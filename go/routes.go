// Dispatch: route one request to the right surface.
//
//   GET  /healthz                                   the runner's boot probe
//   GET  /{owner}/{repo}.git/info/refs?service=...  smart-HTTP advertisement
//   POST /{owner}/{repo}.git/git-upload-pack        upload-pack (v2 or v0)
//   POST /{owner}/{repo}.git/git-receive-pack       receive-pack (v0)
//   /api/v3/...                                     REST v3
//   POST /api/graphql                               GraphQL v4
//   POST /mcp                                       MCP (JSON-RPC 2.0)
//
// Git smart HTTP is unauthenticated in this suite; the other three surfaces
// require a Bearer token.
package main

import (
	"strings"
)

func (s *server) dispatch(req *request, res *response) {
	path := req.path
	// health probe
	if path == "/healthz" {
		res.body = []byte(`{"ok":true}`)
		return
	}
	// git smart HTTP: /{owner}/{repo}.git/...
	if strings.HasPrefix(path, "/") && strings.Contains(path, ".git/") {
		s.handleGit(req, res)
		return
	}
	if strings.HasPrefix(path, "/api/v3") {
		s.handleREST(req, res)
		return
	}
	if path == "/api/graphql" {
		if req.method == "POST" {
			s.handleGraphQL(req, res)
		} else {
			s.restError(res, 404, "Not Found")
		}
		return
	}
	if path == "/mcp" {
		if req.method == "POST" {
			s.handleMCP(req, res)
		} else {
			s.restError(res, 404, "Not Found")
		}
		return
	}
	s.restError(res, 404, "Not Found")
}

// handleGit serves the smart-HTTP endpoints.
func (s *server) handleGit(req *request, res *response) {
	// path: /{owner}/{repo}.git/<suffix>
	rest := strings.TrimPrefix(req.path, "/")
	gi := strings.Index(rest, ".git/")
	if gi < 0 {
		s.restError(res, 404, "Not Found")
		return
	}
	ownerRepo := rest[:gi]
	suffix := rest[gi+5:]
	slash := strings.Index(ownerRepo, "/")
	if slash < 0 {
		s.restError(res, 404, "Not Found")
		return
	}
	owner := urlUnescape(ownerRepo[:slash])
	name := urlUnescape(ownerRepo[slash+1:])
	if owner != ownerLogin {
		s.restError(res, 404, "Not Found")
		return
	}
	switch {
	case suffix == "info/refs" && req.method == "GET":
		q := queryParams(req.query)
		service := q["service"]
		s.store.mu.RLock()
		r := s.store.repos[name]
		if r == nil {
			s.store.mu.RUnlock()
			s.restError(res, 404, "Not Found")
			return
		}
		var body []byte
		var ctype string
		switch service {
		case "git-upload-pack":
			body = v2Advertisement()
			ctype = "application/x-git-upload-pack-advertisement"
		case "git-receive-pack":
			body = v0Advertisement(r, "git-receive-pack", "report-status side-band-64k delete-refs ofs-delta agent=forge-v5/1.0")
			ctype = "application/x-git-receive-pack-advertisement"
		default:
			s.store.mu.RUnlock()
			s.restError(res, 404, "Not Found")
			return
		}
		s.store.mu.RUnlock()
		res.ctype = ctype
		res.body = body
		return
	case suffix == "git-upload-pack" && req.method == "POST":
		s.store.mu.RLock()
		r := s.store.repos[name]
		if r == nil {
			s.store.mu.RUnlock()
			s.restError(res, 404, "Not Found")
			return
		}
		// v2 when the client asked for it; v0 otherwise.
		proto := req.headerString("git-protocol")
		var body []byte
		if strings.Contains(proto, "version=2") {
			body = handleUploadPackV2(s.store, r, req.body)
		} else {
			body = handleUploadPackV0(s.store, r, req.body)
		}
		s.store.mu.RUnlock()
		res.ctype = "application/x-git-upload-pack-result"
		res.body = body
		return
	case suffix == "git-receive-pack" && req.method == "POST":
		s.store.mu.Lock()
		r := s.store.repos[name]
		if r == nil {
			s.store.mu.Unlock()
			s.restError(res, 404, "Not Found")
			return
		}
		body := handleReceivePack(s.store, r, req.body)
		s.store.mu.Unlock()
		res.ctype = "application/x-git-receive-pack-result"
		res.body = body
		return
	}
	s.restError(res, 404, "Not Found")
}
