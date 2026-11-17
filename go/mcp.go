// mcp.go — Model Context Protocol over JSON-RPC 2.0 at /mcp (modern era 2026-07-28).
// Serves server/discover, initialize, tools/list and tools/call with six tools that wrap
// the same Forge store as REST, git and GraphQL. Tool results carry both structuredContent
// and a JSON text block, so either reading path the client prefers works.

package main

import (
	"strconv"
	"strings"
)

// McpResult is the HTTP status plus the JSON-RPC body for one /mcp request.
type McpResult struct {
	Status int
	Body   any
}

const mcpServerName = "git-forge"
const mcpServerVersion = "1.0.0"
const mcpProtocolVersion = "2026-07-28"

type mcpToolDef struct {
	name        string
	description string
	properties  *OMap // name -> property object, in declaration order
	required    []string
}

func mcpToolJSON(tool mcpToolDef) *OMap {
	required := make([]any, 0, len(tool.required))
	for _, name := range tool.required {
		required = append(required, name)
	}
	return NewOMap().
		Set("name", tool.name).
		Set("description", tool.description).
		Set("inputSchema", NewOMap().
			Set("type", "object").
			Set("properties", tool.properties).
			Set("required", required))
}

var mcpTools = []mcpToolDef{
	{
		name:        "create_repo",
		description: "Create a new repository owned by the viewer.",
		properties: NewOMap().
			Set("name", NewOMap().Set("type", "string").Set("description", "The repository name.")).
			Set("description", NewOMap().Set("type", "string").Set("description", "A short description of the repository.")).
			Set("private", NewOMap().Set("type", "boolean").Set("description", "Whether the repository is private.")),
		required: []string{"name"},
	},
	{
		name:        "get_repo",
		description: "Get a repository by owner and name.",
		properties: NewOMap().
			Set("owner", NewOMap().Set("type", "string").Set("description", "The repository owner login.")).
			Set("repo", NewOMap().Set("type", "string").Set("description", "The repository name.")),
		required: []string{"owner", "repo"},
	},
	{
		name:        "create_issue",
		description: "Create an issue in a repository.",
		properties: NewOMap().
			Set("owner", NewOMap().Set("type", "string").Set("description", "The repository owner login.")).
			Set("repo", NewOMap().Set("type", "string").Set("description", "The repository name.")).
			Set("title", NewOMap().Set("type", "string").Set("description", "The issue title.")).
			Set("body", NewOMap().Set("type", "string").Set("description", "The issue body.")).
			Set("labels", NewOMap().Set("type", "array").Set("items", NewOMap().Set("type", "string")).Set("description", "Label names to attach.")),
		required: []string{"owner", "repo", "title"},
	},
	{
		name:        "list_issues",
		description: "List the issues of a repository.",
		properties: NewOMap().
			Set("owner", NewOMap().Set("type", "string").Set("description", "The repository owner login.")).
			Set("repo", NewOMap().Set("type", "string").Set("description", "The repository name.")).
			Set("state", NewOMap().Set("type", "string").Set("enum", []any{"open", "closed", "all"}).Set("description", "Filter by issue state.")),
		required: []string{"owner", "repo"},
	},
	{
		name:        "create_pull_request",
		description: "Open a pull request from one branch onto another.",
		properties: NewOMap().
			Set("owner", NewOMap().Set("type", "string").Set("description", "The repository owner login.")).
			Set("repo", NewOMap().Set("type", "string").Set("description", "The repository name.")).
			Set("title", NewOMap().Set("type", "string").Set("description", "The pull request title.")).
			Set("body", NewOMap().Set("type", "string").Set("description", "The pull request body.")).
			Set("head", NewOMap().Set("type", "string").Set("description", "The head branch name.")).
			Set("base", NewOMap().Set("type", "string").Set("description", "The base branch name.")),
		required: []string{"owner", "repo", "title", "head", "base"},
	},
	{
		name:        "merge_pull_request",
		description: "Squash-merge a pull request.",
		properties: NewOMap().
			Set("owner", NewOMap().Set("type", "string").Set("description", "The repository owner login.")).
			Set("repo", NewOMap().Set("type", "string").Set("description", "The repository name.")).
			Set("pull_number", NewOMap().Set("type", "integer").Set("description", "The pull request number.")),
		required: []string{"owner", "repo", "pull_number"},
	},
}

func mcpRPCResult(id any, result any) McpResult {
	return McpResult{Status: 200, Body: NewOMap().Set("jsonrpc", "2.0").Set("id", id).Set("result", result)}
}

func mcpRPCError(id any, code int, message string, status int) McpResult {
	return McpResult{Status: status, Body: NewOMap().Set("jsonrpc", "2.0").Set("id", id).Set("error", NewOMap().Set("code", code).Set("message", message))}
}

// mcpToolOk is a successful tool result: JSON text plus the same object as structuredContent.
func mcpToolOk(result any) *OMap {
	text := string(mustJSON(result))
	return NewOMap().
		Set("content", []any{NewOMap().Set("type", "text").Set("text", text)}).
		Set("structuredContent", result).
		Set("isError", false)
}

// mcpToolFail is a failed tool result: isError true with the reason as text.
func mcpToolFail(message string) *OMap {
	return NewOMap().
		Set("content", []any{NewOMap().Set("type", "text").Set("text", message)}).
		Set("isError", true)
}

// mcpPickArg reads an argument by its canonical name, falling back to the suite's synonyms.
func mcpPickArg(args map[string]any, names ...string) any {
	for _, name := range names {
		value, ok := args[name]
		if ok && value != nil {
			return value
		}
	}
	return nil
}

func mcpMissingRequired(args map[string]any, required []string) string {
	for _, name := range required {
		value, ok := args[name]
		if !ok || value == nil {
			return name
		}
		if str, ok := value.(string); ok && str == "" {
			return name
		}
	}
	return ""
}

func mcpCallTool(forge *Forge, name string, argsRaw any) *OMap {
	args, ok := argsRaw.(map[string]any)
	if !ok {
		args = map[string]any{}
	}

	owner, _ := mcpPickArg(args, "owner", "owner_login", "org", "login").(string)
	repoName, _ := mcpPickArg(args, "repo", "repository", "repo_name").(string)

	switch name {
	case "create_repo":
		if missing := mcpMissingRequired(args, []string{"name"}); missing != "" {
			return mcpToolFail("Missing required argument: " + missing)
		}
		repoNameArg, _ := args["name"].(string)
		if forge.GetRepo(repoNameArg) != nil {
			return mcpToolFail("Repository already exists")
		}
		isPrivate, _ := args["private"].(bool)
		description, _ := args["description"].(string)
		repo := forge.CreateRepo(repoNameArg, isPrivate, description)
		if repo == nil {
			return mcpToolFail("Repository already exists")
		}
		return mcpToolOk(RepoJSON(repo))
	case "get_repo":
		if missing := mcpMissingRequired(args, []string{"owner", "repo"}); missing != "" {
			return mcpToolFail("Missing required argument: " + missing)
		}
		if owner != ViewerLogin {
			return mcpToolFail("Not Found")
		}
		repo := forge.GetRepo(repoName)
		if repo == nil {
			return mcpToolFail("Not Found")
		}
		return mcpToolOk(RepoJSON(repo))
	case "create_issue":
		if missing := mcpMissingRequired(args, []string{"owner", "repo", "title"}); missing != "" {
			return mcpToolFail("Missing required argument: " + missing)
		}
		if owner != ViewerLogin {
			return mcpToolFail("Not Found")
		}
		repo := forge.GetRepo(repoName)
		if repo == nil {
			return mcpToolFail("Not Found")
		}
		var labels []string
		if raw, ok := args["labels"].([]any); ok {
			for _, entry := range raw {
				if str, ok := entry.(string); ok {
					labels = append(labels, str)
				}
			}
		}
		title, _ := args["title"].(string)
		body, _ := args["body"].(string)
		created := forge.CreateIssue(repo, title, body, labels)
		if created.Error != "" {
			return mcpToolFail(created.Error)
		}
		return mcpToolOk(IssueJSON(repo, created.Issue))
	case "list_issues":
		if missing := mcpMissingRequired(args, []string{"owner", "repo"}); missing != "" {
			return mcpToolFail("Missing required argument: " + missing)
		}
		if owner != ViewerLogin {
			return mcpToolFail("Not Found")
		}
		repo := forge.GetRepo(repoName)
		if repo == nil {
			return mcpToolFail("Not Found")
		}
		state, _ := args["state"].(string)
		if state == "" {
			state = "open"
		}
		list := make([]*Issue, 0, len(repo.Issues))
		for _, issue := range repo.Issues {
			if state != "all" && issue.State != state {
				continue
			}
			list = append(list, issue)
		}
		sortIssuesBySeq(list, false)
		views := make([]any, 0, len(list))
		for _, issue := range list {
			views = append(views, IssueJSON(repo, issue))
		}
		return mcpToolOk(NewOMap().Set("issues", views))
	case "create_pull_request":
		if missing := mcpMissingRequired(args, []string{"owner", "repo", "title", "head", "base"}); missing != "" {
			return mcpToolFail("Missing required argument: " + missing)
		}
		if owner != ViewerLogin {
			return mcpToolFail("Not Found")
		}
		repo := forge.GetRepo(repoName)
		if repo == nil {
			return mcpToolFail("Not Found")
		}
		head, _ := args["head"].(string)
		base, _ := args["base"].(string)
		title, _ := args["title"].(string)
		body, _ := args["body"].(string)
		created := forge.CreatePull(repo, head, base, title, body)
		if created.Error != "" {
			return mcpToolFail(created.Error)
		}
		return mcpToolOk(PullJSON(repo, created.Pull, forge))
	case "merge_pull_request":
		if missing := mcpMissingRequired(args, []string{"owner", "repo", "pull_number"}); missing != "" {
			return mcpToolFail("Missing required argument: " + missing)
		}
		if owner != ViewerLogin {
			return mcpToolFail("Not Found")
		}
		repo := forge.GetRepo(repoName)
		if repo == nil {
			return mcpToolFail("Not Found")
		}
		number, _ := gqlToInt(mcpPickArg(args, "pull_number", "pr_number", "number"))
		var pull *Pull
		for _, entry := range repo.Pulls {
			if entry.Number == number {
				pull = entry
				break
			}
		}
		if pull == nil {
			return mcpToolFail("Not Found")
		}
		merged := forge.MergeSquash(repo, pull)
		if merged.Error != "" {
			switch merged.Error {
			case "conflict":
				return mcpToolFail("Pull request is not mergeable")
			case "already-merged":
				return mcpToolFail("Pull request is already merged")
			default:
				return mcpToolFail(merged.Error)
			}
		}
		return mcpToolOk(NewOMap().
			Set("merged", true).
			Set("sha", merged.Sha).
			Set("message", "Merged pull request #"+strconv.Itoa(number)).
			Set("number", number))
	default:
		return mcpToolFail("Unknown tool: " + name)
	}
}

// HandleMcp handles one JSON-RPC request at /mcp. Unknown tools and unknown methods
// answer with JSON-RPC errors (never HTTP 500); a malformed JSON body is answered by
// the caller.
func HandleMcp(forge *Forge, body any) McpResult {
	request, ok := body.(map[string]any)
	if !ok {
		return mcpRPCError(nil, -32600, "Invalid Request", 400)
	}
	id, ok := request["id"]
	if !ok {
		id = nil
	}
	method, _ := request["method"].(string)
	params, _ := request["params"].(map[string]any)
	if params == nil {
		params = map[string]any{}
	}

	if method == "server/discover" {
		serverInfo := NewOMap().Set("name", mcpServerName).Set("version", mcpServerVersion)
		return mcpRPCResult(id, NewOMap().
			Set("supportedVersions", []any{mcpProtocolVersion, "2025-06-18"}).
			Set("capabilities", NewOMap().Set("tools", NewOMap())).
			Set("serverInfo", serverInfo).
			Set("_meta", NewOMap().Set("io.modelcontextprotocol/serverInfo", serverInfo)))
	}
	if method == "initialize" {
		return mcpRPCResult(id, NewOMap().
			Set("protocolVersion", mcpProtocolVersion).
			Set("capabilities", NewOMap().Set("tools", NewOMap())).
			Set("serverInfo", NewOMap().Set("name", mcpServerName).Set("version", mcpServerVersion)))
	}
	if method == "notifications/initialized" || strings.HasPrefix(method, "notifications/") {
		return mcpRPCResult(id, NewOMap())
	}
	if method == "ping" {
		return mcpRPCResult(id, NewOMap())
	}
	if method == "tools/list" {
		tools := make([]any, 0, len(mcpTools))
		for _, tool := range mcpTools {
			tools = append(tools, mcpToolJSON(tool))
		}
		return mcpRPCResult(id, NewOMap().Set("tools", tools))
	}
	if method == "tools/call" {
		toolName, _ := params["name"].(string)
		known := false
		for _, tool := range mcpTools {
			if tool.name == toolName {
				known = true
				break
			}
		}
		if !known {
			return mcpRPCError(id, -32602, "Unknown tool: "+toolName, 200)
		}
		args, _ := params["arguments"]
		if args == nil {
			args = map[string]any{}
		}
		result := mcpCallTool(forge, toolName, args)
		return mcpRPCResult(id, result)
	}
	return mcpRPCError(id, -32601, "Method not found: "+method, 200)
}
