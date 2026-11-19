// MCP: POST /mcp — JSON-RPC 2.0 over Streamable HTTP, revision 2026-07-28.
//
// The client probes server/discover first; a result makes it modern, so
// this server answers discover with its identity, capabilities and supported
// revisions, and also speaks the legacy initialize handshake. tools/list
// offers the six required tools (plus list_repos); every tool has a
// description and an inputSchema of type object, and two tools/list calls
// answer the same list byte for byte (the JSON is built once at startup).
package main

import (
	"encoding/json"
	"strconv"
	"strings"
)

const mcpRevision = "2026-07-28"
const mcpServerName = "git-forge"
const mcpServerVersion = "5.0.0"

var mcpToolsJSON []byte

func init() {
	tools := []any{
		mcpTool("create_repo", "Create a repository.",
			[]string{"name"},
			map[string]any{
				"name":        map[string]any{"type": "string", "description": "Repository name."},
				"owner":       map[string]any{"type": "string", "description": "Owner login."},
				"description": map[string]any{"type": "string", "description": "Repository description."},
				"private":     map[string]any{"type": "boolean", "description": "Whether the repository is private."},
			}),
		mcpTool("get_repo", "Get a repository.",
			[]string{"owner", "repo"},
			map[string]any{
				"owner":     map[string]any{"type": "string", "description": "Owner login."},
				"repo":      map[string]any{"type": "string", "description": "Repository name."},
				"full_name": map[string]any{"type": "string", "description": "owner/repo."},
			}),
		mcpTool("create_issue", "Create an issue.",
			[]string{"owner", "repo", "title"},
			map[string]any{
				"owner":     map[string]any{"type": "string", "description": "Owner login."},
				"repo":      map[string]any{"type": "string", "description": "Repository name."},
				"full_name": map[string]any{"type": "string", "description": "owner/repo."},
				"title":     map[string]any{"type": "string", "description": "Issue title."},
				"body":      map[string]any{"type": "string", "description": "Issue body."},
			}),
		mcpTool("list_issues", "List issues of a repository.",
			[]string{"owner", "repo"},
			map[string]any{
				"owner":     map[string]any{"type": "string", "description": "Owner login."},
				"repo":      map[string]any{"type": "string", "description": "Repository name."},
				"full_name": map[string]any{"type": "string", "description": "owner/repo."},
				"state":     map[string]any{"type": "string", "description": "open, closed or all."},
			}),
		mcpTool("create_pull_request", "Create a pull request.",
			[]string{"owner", "repo", "title", "head", "base"},
			map[string]any{
				"owner":     map[string]any{"type": "string", "description": "Owner login."},
				"repo":      map[string]any{"type": "string", "description": "Repository name."},
				"full_name": map[string]any{"type": "string", "description": "owner/repo."},
				"title":     map[string]any{"type": "string", "description": "Pull request title."},
				"body":      map[string]any{"type": "string", "description": "Pull request body."},
				"head":      map[string]any{"type": "string", "description": "Head branch."},
				"base":      map[string]any{"type": "string", "description": "Base branch."},
			}),
		mcpTool("merge_pull_request", "Merge a pull request.",
			[]string{"owner", "repo", "pull_number"},
			map[string]any{
				"owner":       map[string]any{"type": "string", "description": "Owner login."},
				"repo":        map[string]any{"type": "string", "description": "Repository name."},
				"full_name":   map[string]any{"type": "string", "description": "owner/repo."},
				"pull_number": map[string]any{"type": "integer", "description": "Pull request number."},
			}),
		mcpTool("list_repos", "List the token owner's repositories.",
			[]string{},
			map[string]any{}),
	}
	// tools/list result, built once: byte-identical on every call.
	out, _ := json.Marshal(map[string]any{"tools": tools})
	mcpToolsJSON = out
}

func mcpTool(name, desc string, required []string, props map[string]any) map[string]any {
	return map[string]any{
		"name":        name,
		"description": desc,
		"inputSchema": map[string]any{
			"type":       "object",
			"properties": props,
			"required":   required,
		},
	}
}

type mcpRequest struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      any            `json:"id"`
	Method  string         `json:"method"`
	Params  map[string]any `json:"params"`
}

func (s *server) handleMCP(req *request, res *response) {
	if !s.requireAuth(req, res) {
		return
	}
	var m mcpRequest
	if err := json.Unmarshal(req.body, &m); err != nil {
		res.status = 400
		res.body = []byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"Parse error"}}`)
		return
	}
	// notifications carry no id and get no JSON-RPC response.
	if m.ID == nil {
		if m.Method == "notifications/initialized" {
			res.status = 202
			res.body = []byte(`{}`)
			return
		}
		res.status = 202
		res.body = []byte(`{}`)
		return
	}
	idJSON, _ := json.Marshal(m.ID)
	switch m.Method {
	case "server/discover":
		res.setHeader("Mcp-Session-Id", "forge-v5-session")
		res.body = []byte(`{"jsonrpc":"2.0","id":` + string(idJSON) + `,"result":` + mcpDiscoverResult() + `}`)
	case "initialize":
		res.setHeader("Mcp-Session-Id", "forge-v5-session")
		res.body = []byte(`{"jsonrpc":"2.0","id":` + string(idJSON) + `,"result":{"protocolVersion":"` + mcpRevision + `","capabilities":{"tools":{}},"serverInfo":{"name":"` + mcpServerName + `","version":"` + mcpServerVersion + `"}}} `)
	case "tools/list":
		res.setHeader("Mcp-Session-Id", "forge-v5-session")
		res.body = []byte(`{"jsonrpc":"2.0","id":` + string(idJSON) + `,"result":` + string(mcpToolsJSON) + `}`)
	case "tools/call":
		res.setHeader("Mcp-Session-Id", "forge-v5-session")
		res.body = s.mcpCall(m, idJSON)
	default:
		res.body = []byte(`{"jsonrpc":"2.0","id":` + string(idJSON) + `,"error":{"code":-32601,"message":"Method not found"}}`)
	}
}

func mcpDiscoverResult() string {
	return `{"protocolVersion":"` + mcpRevision + `","supportedVersions":["` + mcpRevision + `","2025-11-25"],"serverInfo":{"name":"` + mcpServerName + `","version":"` + mcpServerVersion + `"},"capabilities":{"tools":{}},"_meta":{"io.modelcontextprotocol/serverInfo":{"name":"` + mcpServerName + `","version":"` + mcpServerVersion + `"},"io.modelcontextprotocol/protocolVersion":"` + mcpRevision + `"}}`
}

// mcpCall executes a tools/call and returns the full JSON-RPC response body.
func (s *server) mcpCall(m mcpRequest, idJSON []byte) []byte {
	name, _ := m.Params["name"].(string)
	args, _ := m.Params["arguments"].(map[string]any)
	if args == nil {
		args = map[string]any{}
	}
	fail := func(msg string) []byte {
		body, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0", "id": m.ID,
			"result": map[string]any{
				"content": []any{map[string]any{"type": "text", "text": msg}},
				"isError": true,
			},
		})
		return body
	}
	// required-argument check per tool
	required := map[string][]string{
		"create_repo":         {"name"},
		"get_repo":            {"owner", "repo"},
		"create_issue":        {"owner", "repo", "title"},
		"list_issues":         {"owner", "repo"},
		"create_pull_request": {"owner", "repo", "title", "head", "base"},
		"merge_pull_request":  {"owner", "repo", "pull_number"},
	}
	// normalize the tool name (a server prefix is allowed; match suffix)
	norm := strings.ToLower(name)
	for _, cand := range []string{"create_repo", "get_repo", "create_issue", "list_issues", "create_pull_request", "merge_pull_request", "list_repos"} {
		if norm == cand || strings.HasSuffix(norm, "__"+cand) || strings.HasSuffix(norm, cand) {
			name = cand
			break
		}
	}
	req, ok := required[name]
	if !ok && name != "list_repos" {
		body, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0", "id": m.ID,
			"error": map[string]any{"code": -32601, "message": "Unknown tool: " + name},
		})
		return body
	}
	// resolve owner/repo (accept full_name "owner/repo" too)
	owner, repo := mcpArgStr(args, "owner"), mcpArgStr(args, "repo")
	if repo == "" {
		if fn := mcpArgStr(args, "full_name"); fn != "" {
			if i := strings.IndexByte(fn, '/'); i >= 0 {
				owner, repo = fn[:i], fn[i+1:]
			}
		}
	}
	for _, r := range req {
		switch r {
		case "owner":
			if owner == "" {
				return fail("missing required argument: owner")
			}
		case "repo":
			if repo == "" {
				return fail("missing required argument: repo")
			}
		default:
			if _, present := args[r]; !present {
				return fail("missing required argument: " + r)
			}
		}
	}
	okResult := func(obj any) []byte {
		text, _ := json.Marshal(obj)
		body, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0", "id": m.ID,
			"result": map[string]any{
				"content":           []any{map[string]any{"type": "text", "text": string(text)}},
				"structuredContent": obj,
				"isError":           false,
			},
		})
		return body
	}
	switch name {
	case "create_repo":
		rname := mcpArgStr(args, "name")
		desc := mcpArgStr(args, "description")
		priv, _ := args["private"].(bool)
		r, created := s.createRepoFrom(rname, desc, priv)
		if !created {
			return fail("Repository " + rname + " already exists")
		}
		return okResult(map[string]any{
			"id": r.ID, "name": r.Name, "full_name": ownerLogin + "/" + r.Name,
			"private": r.Private, "default_branch": r.DefaultBranch,
		})
	case "get_repo":
		r := s.getRepo(owner, repo)
		if r == nil {
			return fail("Repository not found")
		}
		return okResult(map[string]any{
			"id": r.ID, "name": r.Name, "full_name": ownerLogin + "/" + r.Name,
			"private": r.Private, "default_branch": r.DefaultBranch,
		})
	case "create_issue":
		r := s.getRepo(owner, repo)
		if r == nil {
			return fail("Repository not found")
		}
		s.store.mu.Lock()
		is, bad := s.createIssueIn(r, mcpArgStr(args, "title"), mcpArgStr(args, "body"), nil)
		s.store.mu.Unlock()
		if is == nil {
			return fail("Label " + bad + " does not exist")
		}
		return okResult(map[string]any{
			"id": is.Number, "number": is.Number, "title": is.Title,
			"body": is.Body, "state": is.State,
		})
	case "list_issues":
		r := s.getRepo(owner, repo)
		if r == nil {
			return fail("Repository not found")
		}
		state := mcpArgStr(args, "state")
		s.store.mu.RLock()
		var list []any
		for _, is := range r.Issues {
			if state == "" || state == "all" || is.State == state {
				list = append(list, map[string]any{
					"id": is.Number, "number": is.Number, "title": is.Title,
					"body": is.Body, "state": is.State,
				})
			}
		}
		s.store.mu.RUnlock()
		if list == nil {
			list = []any{}
		}
		return okResult(list)
	case "create_pull_request":
		r := s.getRepo(owner, repo)
		if r == nil {
			return fail("Repository not found")
		}
		s.store.mu.Lock()
		pr, msg := s.createPullIn(r, mcpArgStr(args, "title"), mcpArgStr(args, "body"), mcpArgStr(args, "head"), mcpArgStr(args, "base"))
		s.store.mu.Unlock()
		if pr == nil {
			return fail(msg)
		}
		return okResult(map[string]any{
			"id": pr.Number, "number": pr.Number, "title": pr.Title,
			"state": pr.State, "head": pr.HeadRef, "base": pr.BaseRef,
		})
	case "merge_pull_request":
		r := s.getRepo(owner, repo)
		if r == nil {
			return fail("Repository not found")
		}
		num := mcpArgInt(args, "pull_number")
		s.store.mu.Lock()
		pr := r.PullByNum[num]
		if pr == nil {
			s.store.mu.Unlock()
			return fail("Pull request not found")
		}
		if pr.Merged || pr.State == "closed" {
			s.store.mu.Unlock()
			return fail("Pull request already merged or closed")
		}
		pr.mergeConflict = s.pullConflict(r, pr)
		if pr.mergeConflict {
			s.store.mu.Unlock()
			return fail("Merge conflict")
		}
		headTip, _ := r.Refs["refs/heads/"+pr.HeadRef]
		baseTip, _ := r.Refs["refs/heads/"+pr.BaseRef]
		headTree := ""
		if obj := r.Objects[headTip]; obj != nil && obj.Type == "commit" {
			headTree = parseCommit(obj.Data).Tree
		}
		var parents []string
		if baseTip != "" {
			parents = []string{baseTip}
		}
		mergeSHA := s.makeCommit(r, headTree, parents, pr.Title+" (#"+strconv.Itoa(pr.Number)+")")
		r.Refs["refs/heads/"+pr.BaseRef] = mergeSHA
		r.packVer++
		pr.Merged = true
		pr.State = "closed"
		pr.MergeCommit = mergeSHA
		pr.MergedAt = "2026-11-14T06:30:00Z"
		s.store.mu.Unlock()
		return okResult(map[string]any{
			"id": pr.Number, "number": pr.Number, "merged": true, "state": "closed",
			"sha": mergeSHA,
		})
	case "list_repos":
		s.store.mu.RLock()
		var list []any
		for _, r := range s.store.repos {
			list = append(list, map[string]any{"id": r.ID, "name": r.Name, "full_name": ownerLogin + "/" + r.Name})
		}
		s.store.mu.RUnlock()
		if list == nil {
			list = []any{}
		}
		return okResult(list)
	}
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": m.ID,
		"error": map[string]any{"code": -32601, "message": "Unknown tool: " + name},
	})
	return body
}

func mcpArgStr(args map[string]any, key string) string {
	if v, ok := args[key].(string); ok {
		return v
	}
	return ""
}

func mcpArgInt(args map[string]any, key string) int {
	switch v := args[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case string:
		n, _ := strconv.Atoi(v)
		return n
	}
	return 0
}
