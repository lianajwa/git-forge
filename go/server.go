// server.go — one HTTP server for every surface: /healthz, git smart HTTP,
// REST v3 under /api/v3, GraphQL at /api/graphql and MCP at /mcp.
// Every REST reply is application/json with a non-empty X-GitHub-Request-Id drawn from a
// monotonic counter (no wall clock, no randomness — the state export stays deterministic).
//
// The Go server is single-threaded in effect: one Forge mutex is held for the whole
// dispatch of each request, matching the Node event loop the port replaces. The body is
// read before the lock is taken, so a slow client never stalls other requests.

package main

import (
	"encoding/json"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync/atomic"
)

const maxBodyBytes = 64 * 1024 * 1024

const jsonContentType = "application/json; charset=utf-8"

type forgeServer struct {
	forge *Forge
	// token is the Bearer token that authorizes every call except /healthz.
	// Empty accepts any non-empty token.
	token          string
	requestCounter int64
}

func newForgeServer(forge *Forge, token string) *forgeServer {
	return &forgeServer{forge: forge, token: token}
}

// dispatchResult is one fully computed answer, ready to write.
type dispatchResult struct {
	status        int
	contentType   string // "" means no Content-Type header
	body          []byte // nil means no body
	headers       map[string]string
	omitRequestID bool
}

func jsonResult(status int, body any, headers map[string]string) dispatchResult {
	return dispatchResult{status: status, contentType: jsonContentType, body: mustJSON(body), headers: headers}
}

func errResult(status int, message string, headers map[string]string) dispatchResult {
	return jsonResult(status, NewOMap().Set("message", message), headers)
}

func decodeSegments(restPath string) []string {
	parts := strings.Split(restPath, "/")
	segments := []string{}
	for _, part := range parts {
		if part == "" {
			continue
		}
		decoded, err := url.PathUnescape(part)
		if err != nil {
			return nil
		}
		segments = append(segments, decoded)
	}
	return segments
}

// parseGitRoute splits a git smart-HTTP path /{owner}/{repo}.git/{action} by hand:
// owner and repo hold no slash, action is one of the three smart-HTTP actions.
// It is exactly the route the old gitRouteRe regex matched, without a regexp call.
func parseGitRoute(pathname string) (owner, repo, action string, ok bool) {
	if len(pathname) < 2 || pathname[0] != '/' {
		return "", "", "", false
	}
	rest := pathname[1:]
	i := strings.IndexByte(rest, '/')
	if i <= 0 {
		return "", "", "", false
	}
	owner = rest[:i]
	rest = rest[i+1:]
	j := strings.Index(rest, ".git/")
	if j <= 0 || strings.IndexByte(rest[:j], '/') >= 0 {
		return "", "", "", false
	}
	repo = rest[:j]
	action = rest[j+5:]
	switch action {
	case "info/refs", "git-upload-pack", "git-receive-pack":
		return owner, repo, action, true
	}
	return "", "", "", false
}

func (s *forgeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	requestID := s.nextRequestID()
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("forge error: %v", rec)
			w.Header().Set("content-type", jsonContentType)
			w.Header().Set("x-github-request-id", requestID)
			w.WriteHeader(500)
			_, _ = w.Write(mustJSON(NewOMap().Set("message", "Internal Server Error")))
		}
	}()

	// The body is read before dispatch; GET and HEAD never have one.
	var body []byte
	if r.Method != "GET" && r.Method != "HEAD" {
		data, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
		if err != nil {
			log.Printf("forge error: %v", err)
			s.writeResult(w, requestID, errResult(500, "Internal Server Error", map[string]string{"x-github-request-id": requestID}))
			return
		}
		if len(data) > maxBodyBytes {
			log.Printf("forge error: request body too large")
			s.writeResult(w, requestID, errResult(500, "Internal Server Error", map[string]string{"x-github-request-id": requestID}))
			return
		}
		body = data
	} else {
		body = []byte{}
	}
	result := s.serveParsed(r.Method, r.URL.EscapedPath(), r.URL.RawQuery, r.Host, r.Header.Get("Authorization"), r.Header.Get("Git-Protocol"), body, requestID)
	s.writeResult(w, requestID, result)
}

func (s *forgeServer) nextRequestID() string {
	var buf [24]byte
	b := append(buf[:0], "req-"...)
	b = strconv.AppendInt(b, atomic.AddInt64(&s.requestCounter, 1), 10)
	return string(b)
}

// serveParsed is the transport-neutral core shared by the net/http adapter and
// the raw-TCP server: path normalisation, /healthz, auth, then dispatch under
// the Forge lock. The body is already fully read (bounded by maxBodyBytes).
// pathname is the raw (still percent-escaped) path, "" meaning "/".
func (s *forgeServer) serveParsed(method, pathname, queryString, host, authorization, gitProtocol string, body []byte, requestID string) dispatchResult {
	// Split the request target by hand: the suite only ever sends origin-form paths.
	// Anything unusual (dot segments, doubled slashes, backslashes) falls back to URL
	// normalization, the way `new URL` settles it.
	if pathname == "" {
		pathname = "/"
	}
	if strings.Contains(pathname, "/./") || strings.Contains(pathname, "/../") || strings.Contains(pathname, "//") || strings.Contains(pathname, "\\") {
		pathname = path.Clean(strings.ReplaceAll(pathname, "\\", "/"))
	}
	// The query is parsed lazily: most routes never read it.
	var queryValues url.Values
	queryParsed := false
	query := func() url.Values {
		if !queryParsed {
			queryValues, _ = url.ParseQuery(queryString)
			queryParsed = true
		}
		return queryValues
	}

	// /healthz answers without a token; the harness pings it twice at boot.
	if pathname == "/healthz" {
		if method != "GET" {
			return errResult(405, "Method Not Allowed", nil)
		}
		return jsonResult(200, NewOMap().Set("ok", true), nil)
	}

	// Every other surface needs a Bearer token.
	bearer := ""
	if strings.HasPrefix(authorization, "Bearer ") {
		bearer = authorization[len("Bearer "):]
	}
	if bearer == "" || (s.token != "" && bearer != s.token) {
		return errResult(401, "Unauthorized", map[string]string{"x-github-request-id": requestID})
	}

	// Dispatch under the Forge lock (one request at a time, like the Node event loop).
	// The deferred unlock keeps the lock releasable if dispatch panics, so the
	// transport's recover can still answer 500 and the server stays up.
	s.forge.mu.Lock()
	defer s.forge.mu.Unlock()
	result := s.dispatch(method, gitProtocol, host, pathname, query, body, requestID)
	return result
}

func (s *forgeServer) writeResult(w http.ResponseWriter, requestID string, res dispatchResult) {
	headers := w.Header()
	if res.contentType != "" {
		headers.Set("content-type", res.contentType)
	}
	if !res.omitRequestID {
		headers.Set("x-github-request-id", requestID)
	}
	for key, value := range res.headers {
		headers.Set(key, value)
	}
	w.WriteHeader(res.status)
	if res.body != nil {
		_, _ = w.Write(res.body)
	}
}

func (s *forgeServer) dispatch(method, gitProtocol, host string, pathname string, query func() url.Values, body []byte, requestID string) dispatchResult {
	// Fast path: GET /api/v3/repos/{owner}/{repo}/issues/{number} — the timed read
	// workload (2,000 sequential reads of one issue). It answers exactly as the
	// router does (owner 404, repo 404, issue 404, the cached issue JSON bytes),
	// without segment decoding or re-serialization. Any path needing decoding ('%'),
	// a non-numeric tail, or a trailing slash falls through to the router below.
	// The segments are found by hand (no Split allocation); the number parses as
	// an int, with the float route kept for the router's Number semantics.
	if method == "GET" && strings.HasPrefix(pathname, "/api/v3/repos/") && !strings.Contains(pathname, "%") {
		rest := pathname[len("/api/v3/repos/"):]
		i1 := strings.IndexByte(rest, '/')
		i2 := -1
		i3 := -1
		if i1 >= 0 {
			i2 = strings.IndexByte(rest[i1+1:], '/')
			if i2 >= 0 {
				i2 += i1 + 1
				i3 = strings.IndexByte(rest[i2+1:], '/')
				if i3 >= 0 {
					i3 += i2 + 1
				}
			}
		}
		if i1 > 0 && i2 > i1+1 && i3 > i2+1 && strings.IndexByte(rest[i3+1:], '/') < 0 {
			owner := rest[:i1]
			repoName := rest[i1+1 : i2]
			if rest[i2+1:i3] == "issues" {
				numberTail := rest[i3+1:]
				if numberTail != "" {
					var repo *Repo
					if owner == ViewerLogin {
						repo = s.forge.GetRepo(repoName)
					}
					if repo == nil {
						return errResult(404, "Not Found", map[string]string{"x-github-request-id": requestID})
					}
					number, err := strconv.Atoi(numberTail)
					var issue *Issue
					if err == nil {
						issue = repo.IssueByNumber[number]
					} else if numFloat, ferr := strconv.ParseFloat(numberTail, 64); ferr == nil && !math.IsNaN(numFloat) && numFloat == math.Trunc(numFloat) {
						issue = repo.IssueByNumber[int(numFloat)]
					}
					if issue == nil {
						for _, entry := range repo.Issues {
							if entry.Number == number {
								issue = entry
								break
							}
						}
					}
					if issue == nil {
						return errResult(404, "Not Found", map[string]string{"x-github-request-id": requestID})
					}
					// writeResult sets x-github-request-id itself; no headers map needed.
					return dispatchResult{status: 200, contentType: jsonContentType, body: IssueJSONText(repo, issue)}
				}
			}
		}
	}

	// git smart HTTP: /{owner}/{repo}.git/{info/refs, git-upload-pack, git-receive-pack}
	if owner, repoName, action, ok := parseGitRoute(pathname); ok {
		if action == "info/refs" {
			if method != "GET" {
				return dispatchResult{status: 405, contentType: jsonContentType, body: mustJSON(NewOMap().Set("message", "Method Not Allowed")), omitRequestID: true}
			}
			service := query().Get("service")
			gitProtocol := gitProtocol
			result := InfoRefs(s.forge, owner, repoName, service, gitProtocol)
			return dispatchResult{status: result.Status, contentType: result.ContentType, body: result.Body, headers: map[string]string{"x-github-request-id": requestID}}
		}
		if method != "POST" {
			return dispatchResult{status: 405, contentType: jsonContentType, body: mustJSON(NewOMap().Set("message", "Method Not Allowed")), omitRequestID: true}
		}
		var result GitHTTPResult
		if action == "git-upload-pack" {
			result = UploadPack(s.forge, owner, repoName, body)
		} else {
			result = ReceivePack(s.forge, owner, repoName, body)
		}
		return dispatchResult{status: result.Status, contentType: result.ContentType, body: result.Body, headers: map[string]string{"x-github-request-id": requestID}}
	}

	// GraphQL.
	if pathname == "/api/graphql" {
		if method != "POST" {
			return errResult(405, "Method Not Allowed", map[string]string{"x-github-request-id": requestID})
		}
		var parsed any
		if len(body) == 0 {
			parsed = map[string]any{}
		} else if err := json.Unmarshal(body, &parsed); err != nil {
			return dispatchResult{status: 400, contentType: jsonContentType, body: mustJSON(NewOMap().Set("errors", []any{NewOMap().Set("message", "Invalid JSON")})), headers: map[string]string{"x-github-request-id": requestID}}
		}
		status, resultBody := HandleGraphQL(s.forge, parsed)
		return jsonResult(status, resultBody, map[string]string{"x-github-request-id": requestID})
	}

	// MCP.
	if pathname == "/mcp" {
		if method != "POST" {
			return errResult(405, "Method Not Allowed", map[string]string{"x-github-request-id": requestID})
		}
		var parsed any
		if len(body) == 0 {
			parsed = map[string]any{}
		} else if err := json.Unmarshal(body, &parsed); err != nil {
			return dispatchResult{status: 400, contentType: jsonContentType, body: mustJSON(NewOMap().Set("jsonrpc", "2.0").Set("id", nil).Set("error", NewOMap().Set("code", -32700).Set("message", "Parse error"))), headers: map[string]string{"x-github-request-id": requestID}}
		}
		result := HandleMcp(s.forge, parsed)
		return jsonResult(result.Status, result.Body, map[string]string{"x-github-request-id": requestID})
	}

	// REST v3 under /api/v3.
	if pathname == "/api/v3" || strings.HasPrefix(pathname, "/api/v3/") {
		segments := decodeSegments(strings.TrimPrefix(pathname, "/api/v3"))
		if segments == nil {
			return errResult(400, "Invalid URL encoding", map[string]string{"x-github-request-id": requestID})
		}
		var parsed any
		parseErr := false
		if len(body) == 0 {
			parsed = nil
		} else if err := json.Unmarshal(body, &parsed); err != nil {
			parseErr = true
		}
		if host == "" {
			host = "127.0.0.1"
		}
		result := HandleRest(s.forge, method, segments, query(), parsed, parseErr, host)
		headers := map[string]string{"x-github-request-id": requestID}
		for key, value := range result.Headers {
			headers[key] = value
		}
		if result.RawBody != nil {
			return dispatchResult{status: result.Status, contentType: jsonContentType, body: result.RawBody, headers: headers}
		}
		if result.Body == nil {
			return dispatchResult{status: result.Status, headers: headers}
		}
		return jsonResult(result.Status, result.Body, headers)
	}

	return errResult(404, "Not Found", map[string]string{"x-github-request-id": requestID})
}
