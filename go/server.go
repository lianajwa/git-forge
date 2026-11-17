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
	"regexp"
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

var gitRouteRe = regexp.MustCompile(`^\/([^/]+)\/([^/]+)\.git\/(info\/refs|git-upload-pack|git-receive-pack)$`)

func (s *forgeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	requestID := "req-" + strconv.FormatInt(atomic.AddInt64(&s.requestCounter, 1), 10)
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("forge error: %v", rec)
			w.Header().Set("content-type", jsonContentType)
			w.Header().Set("x-github-request-id", requestID)
			w.WriteHeader(500)
			_, _ = w.Write(mustJSON(NewOMap().Set("message", "Internal Server Error")))
		}
	}()

	// Split the request target by hand: the suite only ever sends origin-form paths.
	// Anything unusual (dot segments, doubled slashes, backslashes) falls back to URL
	// normalization, the way `new URL` settles it.
	pathname := r.URL.EscapedPath()
	if pathname == "" {
		pathname = "/"
	}
	queryString := r.URL.RawQuery
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
		if r.Method != "GET" {
			s.writeResult(w, requestID, errResult(405, "Method Not Allowed", nil))
			return
		}
		s.writeResult(w, requestID, jsonResult(200, NewOMap().Set("ok", true), nil))
		return
	}

	// Every other surface needs a Bearer token.
	authorization := r.Header.Get("Authorization")
	bearer := ""
	if strings.HasPrefix(authorization, "Bearer ") {
		bearer = authorization[len("Bearer "):]
	}
	if bearer == "" || (s.token != "" && bearer != s.token) {
		s.writeResult(w, requestID, errResult(401, "Unauthorized", map[string]string{"x-github-request-id": requestID}))
		return
	}

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

	// Dispatch under the Forge lock (one request at a time, like the Node event loop),
	// then write the computed answer.
	s.forge.mu.Lock()
	result := s.dispatch(r, pathname, query, body, requestID)
	s.forge.mu.Unlock()
	s.writeResult(w, requestID, result)
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

func (s *forgeServer) dispatch(r *http.Request, pathname string, query func() url.Values, body []byte, requestID string) dispatchResult {
	// Fast path: GET /api/v3/repos/{owner}/{repo}/issues/{number} — the timed read
	// workload (2,000 sequential reads of one issue). It answers exactly as the
	// router does (owner 404, repo 404, issue 404, the cached issue JSON), without
	// segment decoding or re-serialization. Any path needing decoding ('%'), a
	// non-numeric tail, or a trailing slash falls through to the router below.
	if r.Method == "GET" && strings.HasPrefix(pathname, "/api/v3/repos/") && !strings.Contains(pathname, "%") {
		tail := strings.Split(pathname[len("/api/v3/repos/"):], "/")
		if len(tail) == 4 && tail[2] == "issues" && tail[3] != "" {
			owner := tail[0]
			repoName := tail[1]
			var repo *Repo
			if owner == ViewerLogin {
				repo = s.forge.GetRepo(repoName)
			}
			if repo == nil {
				return errResult(404, "Not Found", map[string]string{"x-github-request-id": requestID})
			}
			numFloat, err := strconv.ParseFloat(tail[3], 64)
			var issue *Issue
			if err == nil && !math.IsNaN(numFloat) && numFloat == math.Trunc(numFloat) {
				number := int(numFloat)
				issue = repo.IssueByNumber[number]
				if issue == nil {
					for _, entry := range repo.Issues {
						if entry.Number == number {
							issue = entry
							break
						}
					}
				}
			}
			if issue == nil {
				return errResult(404, "Not Found", map[string]string{"x-github-request-id": requestID})
			}
			return dispatchResult{status: 200, contentType: jsonContentType, body: []byte(IssueJSONText(repo, issue)), headers: map[string]string{"x-github-request-id": requestID}}
		}
	}

	// git smart HTTP: /{owner}/{repo}.git/{info/refs, git-upload-pack, git-receive-pack}
	if match := gitRouteRe.FindStringSubmatch(pathname); match != nil {
		owner, repoName, action := match[1], match[2], match[3]
		if action == "info/refs" {
			if r.Method != "GET" {
				return dispatchResult{status: 405, contentType: jsonContentType, body: mustJSON(NewOMap().Set("message", "Method Not Allowed")), omitRequestID: true}
			}
			service := query().Get("service")
			gitProtocol := r.Header.Get("Git-Protocol")
			result := InfoRefs(s.forge, owner, repoName, service, gitProtocol)
			return dispatchResult{status: result.Status, contentType: result.ContentType, body: result.Body, headers: map[string]string{"x-github-request-id": requestID}}
		}
		if r.Method != "POST" {
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
		if r.Method != "POST" {
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
		if r.Method != "POST" {
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
		host := r.Host
		if host == "" {
			host = "127.0.0.1"
		}
		result := HandleRest(s.forge, r.Method, segments, query(), parsed, parseErr, host)
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
