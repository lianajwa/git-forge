// rest.go — GitHub REST v3: repositories, branches, commits, contents, labels,
// issues, comments, pull requests and merges, plus the wire-protocol rules every
// reply obeys (application/json, a non-empty X-GitHub-Request-Id, `{message}` error
// bodies). All state lives in the shared Forge store, so REST sees what git, GraphQL
// and MCP wrote.

package main

import (
	"encoding/base64"
	"net/url"
	"strconv"
	"strings"
)

// RestResult is one REST answer.
type RestResult struct {
	Status  int
	Body    any // nil means no body (204)
	RawBody []byte
	Headers map[string]string
}

func restOK(body any, status int, headers map[string]string) RestResult {
	return RestResult{Status: status, Body: body, Headers: headers}
}

// restOKRaw is a 200/201 whose body is already JSON bytes (the cached issue JSON).
func restOKRaw(rawBody []byte, status int, headers map[string]string) RestResult {
	return RestResult{Status: status, RawBody: rawBody, Headers: headers}
}

func restErr(status int, message string) RestResult {
	return RestResult{Status: status, Body: struct {
		Message string `json:"message"`
	}{Message: message}}
}

func restNotFound(what string) RestResult {
	if what == "" {
		what = "Not Found"
	}
	return restErr(404, what)
}

// bodyObject returns the JSON object body, or nil when the body is not an object.
func bodyObject(body any, parseErr bool) map[string]any {
	if parseErr {
		return nil
	}
	if obj, ok := body.(map[string]any); ok {
		return obj
	}
	return nil
}

func bodyString(body map[string]any, key string) (string, bool) {
	if body == nil {
		return "", false
	}
	value, ok := body[key].(string)
	return value, ok
}

func bodyBoolean(body map[string]any, key string) (bool, bool) {
	if body == nil {
		return false, false
	}
	value, ok := body[key].(bool)
	return value, ok
}

// ---------------------------------------------------------------------------------------------
// JSON views (struct field order matches the Node object literals)
// ---------------------------------------------------------------------------------------------

type userRef struct {
	Login string `json:"login"`
	ID    int    `json:"id"`
	Type  string `json:"type"`
}

type repoView struct {
	ID              int64   `json:"id"`
	NodeID          string  `json:"node_id"`
	Name            string  `json:"name"`
	FullName        string  `json:"full_name"`
	Owner           userRef `json:"owner"`
	Private         bool    `json:"private"`
	Description     string  `json:"description"`
	DefaultBranch   string  `json:"default_branch"`
	CreatedAt       string  `json:"created_at"`
	UpdatedAt       string  `json:"updated_at"`
	HasIssues       bool    `json:"has_issues"`
	HasPullRequests bool    `json:"has_pull_requests"`
	URL             string  `json:"url"`
	HTMLURL         string  `json:"html_url"`
}

// RepoJSON renders a repository the way the API does.
func RepoJSON(repo *Repo) repoView {
	return repoView{
		ID:              repo.DatabaseID,
		NodeID:          "R_" + strconv.FormatInt(repo.DatabaseID, 10),
		Name:            repo.Name,
		FullName:        ViewerLogin + "/" + repo.Name,
		Owner:           userRef{Login: ViewerLogin, ID: 1000, Type: "User"},
		Private:         repo.IsPrivate,
		Description:     repo.Description,
		DefaultBranch:   DefaultBranch,
		CreatedAt:       repo.CreatedAt,
		UpdatedAt:       repo.CreatedAt,
		HasIssues:       true,
		HasPullRequests: true,
		URL:             "/api/v3/repos/" + ViewerLogin + "/" + repo.Name,
		HTMLURL:         "https://github.com/" + ViewerLogin + "/" + repo.Name,
	}
}

type issueLabelView struct {
	ID    *int64 `json:"id"`
	Name  string `json:"name"`
	Color string `json:"color"`
}

type issueView struct {
	ID        int64            `json:"id"`
	NodeID    string           `json:"node_id"`
	Number    int              `json:"number"`
	Title     string           `json:"title"`
	Body      string           `json:"body"`
	State     string           `json:"state"`
	Labels    []issueLabelView `json:"labels"`
	User      userRef          `json:"user"`
	CreatedAt string           `json:"created_at"`
	UpdatedAt string           `json:"updated_at"`
	URL       string           `json:"url"`
	HTMLURL   string           `json:"html_url"`
}

// IssueJSON renders an issue the way the API does.
func IssueJSON(repo *Repo, issue *Issue) issueView {
	labels := make([]issueLabelView, 0, len(issue.Labels))
	for _, name := range issue.Labels {
		color := "ededed"
		if label, ok := repo.Labels[name]; ok {
			color = label.Color
		}
		labels = append(labels, issueLabelView{ID: nil, Name: name, Color: color})
	}
	return issueView{
		ID:        issue.ID,
		NodeID:    "I_" + strconv.FormatInt(issue.ID, 10),
		Number:    issue.Number,
		Title:     issue.Title,
		Body:      issue.Body,
		State:     issue.State,
		Labels:    labels,
		User:      userRef{Login: ViewerLogin, ID: 1000, Type: "User"},
		CreatedAt: issue.CreatedAt,
		UpdatedAt: issue.CreatedAt,
		URL:       "/api/v3/repos/" + ViewerLogin + "/" + repo.Name + "/issues/" + strconv.Itoa(issue.Number),
		HTMLURL:   "https://github.com/" + ViewerLogin + "/" + repo.Name + "/issues/" + strconv.Itoa(issue.Number),
	}
}

// IssueJSONText returns the serialized issue JSON bytes, cached on the repo. An issue's
// JSON is a pure function of the issue and its labels, and labels never change after
// creation while an issue's title, body and state change only through the PATCH route
// (and GraphQL's closeIssue), which both drop the cached entry — so a hit is
// byte-identical to a fresh build. The returned bytes are the cache's own: callers
// must never mutate them (the writer only reads them).
func IssueJSONText(repo *Repo, issue *Issue) []byte {
	repo.cacheMu.Lock()
	defer repo.cacheMu.Unlock()
	if cached, ok := repo.issueJsonCache[issue.Number]; ok {
		return cached
	}
	built := mustJSON(IssueJSON(repo, issue))
	repo.issueJsonCache[issue.Number] = built
	return built
}

// DropIssueJSONCache drops one issue's cached JSON after a mutation (PATCH, GraphQL
// closeIssue).
func DropIssueJSONCache(repo *Repo, issueNumber int) {
	repo.cacheMu.Lock()
	defer repo.cacheMu.Unlock()
	delete(repo.issueJsonCache, issueNumber)
}

type pullRefView struct {
	Ref   string `json:"ref"`
	Sha   string `json:"sha"`
	Label string `json:"label"`
}

type pullView struct {
	ID             int64       `json:"id"`
	NodeID         string      `json:"node_id"`
	Number         int         `json:"number"`
	State          string      `json:"state"`
	Title          string      `json:"title"`
	Body           string      `json:"body"`
	Head           pullRefView `json:"head"`
	Base           pullRefView `json:"base"`
	Merged         bool        `json:"merged"`
	MergedAt       *string     `json:"merged_at"`
	MergeCommitSha *string     `json:"merge_commit_sha"`
	Mergeable      bool        `json:"mergeable"`
	User           userRef     `json:"user"`
	CreatedAt      string      `json:"created_at"`
	UpdatedAt      string      `json:"updated_at"`
	URL            string      `json:"url"`
	HTMLURL        string      `json:"html_url"`
}

// PullJSON renders a pull request the way the API does.
func PullJSON(repo *Repo, pull *Pull, forge *Forge) pullView {
	headSha, _ := forge.BranchTip(repo, pull.Head)
	return pullView{
		ID:             pull.ID,
		NodeID:         "PR_" + strconv.FormatInt(pull.ID, 10),
		Number:         pull.Number,
		State:          pull.State,
		Title:          pull.Title,
		Body:           pull.Body,
		Head:           pullRefView{Ref: pull.Head, Sha: headSha, Label: ViewerLogin + ":" + pull.Head},
		Base:           pullRefView{Ref: pull.Base, Sha: pull.BaseSha, Label: ViewerLogin + ":" + pull.Base},
		Merged:         pull.Merged,
		MergedAt:       pull.MergedAt,
		MergeCommitSha: pull.MergeSha,
		Mergeable:      !forge.PullConflict(repo, pull),
		User:           userRef{Login: ViewerLogin, ID: 1000, Type: "User"},
		CreatedAt:      pull.CreatedAt,
		UpdatedAt:      pull.CreatedAt,
		URL:            "/api/v3/repos/" + ViewerLogin + "/" + repo.Name + "/pulls/" + strconv.Itoa(pull.Number),
		HTMLURL:        "https://github.com/" + ViewerLogin + "/" + repo.Name + "/pull/" + strconv.Itoa(pull.Number),
	}
}

// ---------------------------------------------------------------------------------------------
// pagination
// ---------------------------------------------------------------------------------------------

type restPage struct {
	perPage int
	page    int
}

func pageOf(query url.Values) restPage {
	perPageRaw, err1 := strconv.ParseFloat(query.Get("per_page"), 64)
	if query.Get("per_page") == "" || err1 != nil {
		perPageRaw = 30
	}
	pageRaw, err2 := strconv.ParseFloat(query.Get("page"), 64)
	if query.Get("page") == "" || err2 != nil {
		pageRaw = 1
	}
	perPage := int(perPageRaw)
	if perPage < 1 {
		perPage = 1
	}
	if perPage > 100 {
		perPage = 100
	}
	page := int(pageRaw)
	if page < 1 {
		page = 1
	}
	return restPage{perPage: perPage, page: page}
}

func paginateLen(total int, page restPage) (start, end, totalPages int) {
	totalPages = (total + page.perPage - 1) / page.perPage
	if totalPages < 1 {
		totalPages = 1
	}
	start = (page.page - 1) * page.perPage
	if start > total {
		start = total
	}
	end = start + page.perPage
	if end > total {
		end = total
	}
	return start, end, totalPages
}

// linkHeader builds a GitHub `Link` header for the next/last pages, absolute on this origin.
func linkHeader(host string, path string, query url.Values, page restPage, totalPages int) string {
	build := func(pageNumber int) string {
		next := url.Values{}
		for key, values := range query {
			next[key] = values
		}
		next.Set("page", strconv.Itoa(pageNumber))
		next.Set("per_page", strconv.Itoa(page.perPage))
		return "http://" + host + path + "?" + next.Encode()
	}
	var links []string
	if page.page < totalPages {
		links = append(links, "<"+build(page.page+1)+">; rel=\"next\"")
	}
	if totalPages > 1 {
		links = append(links, "<"+build(totalPages)+">; rel=\"last\"")
	}
	if page.page > 1 {
		links = append(links, "<"+build(1)+">; rel=\"first\"")
	}
	if page.page > 1 {
		links = append(links, "<"+build(page.page-1)+">; rel=\"prev\"")
	}
	if len(links) == 0 {
		return ""
	}
	return strings.Join(links, ", ")
}

// ---------------------------------------------------------------------------------------------
// the router
// ---------------------------------------------------------------------------------------------

// HandleRest handles one REST call. segments are the decoded path parts after /api/v3;
// query holds the query string; body is the parsed JSON body (nil when there was no
// body); parseErr reports a malformed JSON body. It returns the status, body and any
// extra headers.
func HandleRest(forge *Forge, method string, segments []string, query url.Values, body any, parseErr bool, host string) RestResult {
	if parseErr {
		return restErr(400, "Invalid JSON")
	}
	path := "/api/v3"
	if len(segments) > 0 {
		path += "/" + strings.Join(segments, "/")
	}

	// API root and meta.
	if len(segments) == 0 {
		if method == "GET" {
			return restOK(NewOMap().
				Set("current_user_url", "/api/v3/user").
				Set("rate_limit_url", "/api/v3/rate_limit").
				Set("repository_url", "/api/v3/repos/{owner}/{repo}"),
				200, map[string]string{"x-oauth-scopes": "repo, read:org"})
		}
		return restNotFound("")
	}
	if len(segments) == 1 && segments[0] == "meta" && method == "GET" {
		return restOK(NewOMap().Set("installed_version", "forge 1.0.0"), 200, nil)
	}

	// The token's own user.
	if len(segments) == 1 && segments[0] == "user" && method == "GET" {
		return restOK(NewOMap().
			Set("login", ViewerLogin).
			Set("id", 1000).
			Set("name", ViewerName).
			Set("email", ViewerEmail).
			Set("type", "User"), 200, nil)
	}

	// /user/repos: list and create.
	if len(segments) == 2 && segments[0] == "user" && segments[1] == "repos" {
		if method == "GET" {
			page := pageOf(query)
			all := make([]*Repo, 0, len(forge.Repos))
			for _, repo := range forge.Repos {
				all = append(all, repo)
			}
			sortReposBySeq(all)
			start, end, totalPages := paginateLen(len(all), page)
			headers := map[string]string{}
			if link := linkHeader(host, path, query, page, totalPages); link != "" {
				headers["link"] = link
			}
			views := make([]repoView, 0, end-start)
			for _, repo := range all[start:end] {
				views = append(views, RepoJSON(repo))
			}
			return restOK(views, 200, headers)
		}
		if method == "POST" {
			input := bodyObject(body, false)
			name, ok := bodyString(input, "name")
			if !ok || name == "" {
				return restErr(422, "name is required")
			}
			if strings.Contains(name, "/") {
				return restErr(422, "invalid repository name")
			}
			if forge.GetRepo(name) != nil {
				return restErr(422, "Repository already exists")
			}
			isPrivate, _ := bodyBoolean(input, "private")
			description, _ := bodyString(input, "description")
			repo := forge.CreateRepo(name, isPrivate, description)
			if repo == nil {
				return restErr(422, "Repository already exists")
			}
			return restOK(RepoJSON(repo), 201, nil)
		}
		return restNotFound("")
	}

	// /repos/{owner}/{name}[/...]: everything else lives under a repository.
	if len(segments) >= 3 && segments[0] == "repos" {
		owner := segments[1]
		name := segments[2]
		if owner != ViewerLogin {
			return restNotFound("Not Found")
		}
		repo := forge.GetRepo(name)
		if repo == nil {
			return restNotFound("Not Found")
		}
		return handleRepo(forge, repo, method, segments[3:], query, body, host, path)
	}

	return restNotFound("")
}

func sortReposBySeq(repos []*Repo) {
	for i := 1; i < len(repos); i++ {
		for j := i; j > 0 && repos[j].Seq < repos[j-1].Seq; j-- {
			repos[j], repos[j-1] = repos[j-1], repos[j]
		}
	}
}

// ---------------------------------------------------------------------------------------------
// repository-scoped routes
// ---------------------------------------------------------------------------------------------

func handleRepo(forge *Forge, repo *Repo, method string, tail []string, query url.Values, body any, host string, path string) RestResult {
	// The repository itself.
	if len(tail) == 0 {
		if method == "GET" {
			return restOK(RepoJSON(repo), 200, nil)
		}
		if method == "PATCH" {
			input := bodyObject(body, false)
			if description, ok := bodyString(input, "description"); ok {
				repo.Description = description
			}
			if isPrivate, ok := bodyBoolean(input, "private"); ok {
				repo.IsPrivate = isPrivate
			}
			return restOK(RepoJSON(repo), 200, nil)
		}
		if method == "DELETE" {
			forge.DeleteRepo(repo.Name)
			return RestResult{Status: 204}
		}
		return restNotFound("")
	}

	// Branches.
	if tail[0] == "branches" && len(tail) == 1 && method == "GET" {
		var names []string
		for ref := range repo.Refs {
			if strings.HasPrefix(ref, "refs/heads/") {
				names = append(names, ref[len("refs/heads/"):])
			}
		}
		sortStrings(names)
		branches := make([]*OMap, 0, len(names))
		for _, name := range names {
			tip, _ := repo.Refs["refs/heads/"+name]
			branches = append(branches, NewOMap().
				Set("name", name).
				Set("commit", NewOMap().Set("sha", tip)).
				Set("protected", false))
		}
		return restOK(branches, 200, nil)
	}
	if tail[0] == "branches" && len(tail) == 2 && method == "GET" {
		tip, ok := forge.BranchTip(repo, tail[1])
		if !ok {
			return restNotFound("Branch not found")
		}
		return restOK(NewOMap().
			Set("name", tail[1]).
			Set("commit", NewOMap().Set("sha", tip)).
			Set("protected", false), 200, nil)
	}

	// Commits: ?sha=<branch or commit>; newest first.
	if tail[0] == "commits" && len(tail) == 1 && method == "GET" {
		shaParam := query.Get("sha")
		var tip string
		var ok bool
		if shaParam == "" {
			tip, ok = forge.BranchTip(repo, DefaultBranch)
		} else if IsOid(shaParam) {
			_, ok = repo.Objects[shaParam]
			tip = shaParam
		} else {
			tip, ok = forge.BranchTip(repo, shaParam)
		}
		if !ok {
			return restNotFound("No commit found for SHA")
		}
		_ = tip
		page := pageOf(query)
		walk := forge.ListCommits(repo, shaParam)
		start, end, totalPages := paginateLen(len(walk), page)
		headers := map[string]string{}
		if link := linkHeader(host, path, query, page, totalPages); link != "" {
			headers["link"] = link
		}
		entries := make([]*OMap, 0, end-start)
		for _, oid := range walk[start:end] {
			object := repo.Objects[oid]
			parsed, err := ParseCommit(object.Bytes)
			if err != nil {
				continue
			}
			authorName, authorEmail, authorDate := ParseIdentity(parsed.Author)
			committerName, committerEmail, committerDate := ParseIdentity(parsed.Committer)
			parents := make([]*OMap, 0, len(parsed.Parents))
			for _, parent := range parsed.Parents {
				parents = append(parents, NewOMap().Set("sha", parent))
			}
			entries = append(entries, NewOMap().
				Set("sha", oid).
				Set("commit", NewOMap().
					Set("message", parsed.Message).
					Set("author", NewOMap().Set("name", authorName).Set("email", authorEmail).Set("date", authorDate)).
					Set("committer", NewOMap().Set("name", committerName).Set("email", committerEmail).Set("date", committerDate))).
				Set("author", NewOMap().Set("login", ViewerLogin).Set("id", 1000).Set("type", "User")).
				Set("parents", parents))
		}
		return restOK(entries, 200, headers)
	}

	// git refs: create a ref, or read one branch ref.
	if tail[0] == "git" && len(tail) >= 2 && tail[1] == "refs" && len(tail) == 2 && method == "POST" {
		input := bodyObject(body, false)
		ref, refOK := bodyString(input, "ref")
		sha, shaOK := bodyString(input, "sha")
		if !refOK || !shaOK {
			return restErr(422, "ref and sha are required")
		}
		if !strings.HasPrefix(ref, "refs/heads/") && !strings.HasPrefix(ref, "refs/tags/") {
			return restErr(422, "invalid ref")
		}
		if _, exists := repo.Refs[ref]; exists {
			return restErr(422, "Reference already exists")
		}
		target, exists := repo.Objects[sha]
		if !exists || !IsOid(sha) {
			return restErr(422, "Invalid SHA")
		}
		repo.Refs[ref] = sha
		forge.TouchGit(repo)
		return restOK(NewOMap().
			Set("ref", ref).
			Set("object", NewOMap().Set("sha", sha).Set("type", string(target.Kind))), 201, nil)
	}
	if tail[0] == "git" && len(tail) >= 2 && tail[1] == "refs" && len(tail) >= 3 && tail[2] == "heads" && len(tail) == 4 && method == "GET" {
		tip, ok := forge.BranchTip(repo, tail[3])
		if !ok {
			return restNotFound("Branch not found")
		}
		return restOK(NewOMap().
			Set("ref", "refs/heads/"+tail[3]).
			Set("object", NewOMap().Set("sha", tip).Set("type", "commit")), 200, nil)
	}

	// contents: files and directories, base64 through PUT.
	if tail[0] == "contents" {
		contentPath := strings.Join(tail[1:], "/")
		ref := query.Get("ref")
		if method == "GET" {
			read := forge.ReadPath(repo, contentPath, ref)
			if read == nil {
				return restNotFound("Not Found")
			}
			if read.Kind == "blob" {
				bytes := read.Bytes
				if bytes == nil {
					bytes = []byte{}
				}
				name := contentPath
				if idx := strings.LastIndex(contentPath, "/"); idx >= 0 {
					name = contentPath[idx+1:]
				}
				return restOK(NewOMap().
					Set("type", "file").
					Set("encoding", "base64").
					Set("content", base64.StdEncoding.EncodeToString(bytes)).
					Set("sha", read.Oid).
					Set("size", len(bytes)).
					Set("name", name).
					Set("path", contentPath), 200, nil)
			}
			dirEntries := forge.ListDir(repo, contentPath, ref)
			if dirEntries == nil {
				return restNotFound("Not Found")
			}
			entries := make([]*OMap, 0, len(dirEntries))
			for _, entry := range dirEntries {
				size := 0
				if entry.Kind == "file" {
					if obj, ok := repo.Objects[entry.Oid]; ok {
						size = len(obj.Bytes)
					}
				}
				entries = append(entries, NewOMap().
					Set("type", entry.Kind).
					Set("name", entry.Name).
					Set("path", entry.Path).
					Set("sha", entry.Oid).
					Set("size", size))
			}
			return restOK(entries, 200, nil)
		}
		if method == "PUT" {
			input := bodyObject(body, false)
			content, ok := bodyString(input, "content")
			message, hasMessage := bodyString(input, "message")
			if !hasMessage {
				message = "Update file"
			}
			branch, _ := bodyString(input, "branch")
			sha, hasSha := bodyString(input, "sha")
			if !ok {
				return restErr(422, "content is required")
			}
			cleaned := strings.Map(func(r rune) rune {
				if r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\v' || r == '\f' {
					return -1
				}
				return r
			}, content)
			decoded, err := base64.StdEncoding.DecodeString(cleaned)
			if err != nil {
				return restErr(422, "content is not valid base64")
			}
			var expectedSha *string
			if hasSha {
				expectedSha = &sha
			}
			result := forge.WriteFile(repo, contentPath, decoded, true, message, branch, expectedSha)
			if result.Error != "" {
				switch result.Error {
				case "stale sha":
					return restErr(409, "sha does not match")
				case "file does not exist":
					return restErr(404, "Not Found")
				default:
					return restErr(422, result.Error)
				}
			}
			fileName := contentPath
			if idx := strings.LastIndex(contentPath, "/"); idx >= 0 {
				fileName = contentPath[idx+1:]
			}
			status := 200
			if result.Created {
				status = 201
			}
			return restOK(NewOMap().
				Set("content", NewOMap().
					Set("name", fileName).
					Set("path", contentPath).
					Set("sha", strOrNil(result.BlobSha)).
					Set("size", len(decoded)).
					Set("type", "file").
					Set("encoding", "base64")).
				Set("commit", NewOMap().Set("sha", result.CommitSha).Set("message", message)),
				status, nil)
		}
		if method == "DELETE" {
			input := bodyObject(body, false)
			message, hasMessage := bodyString(input, "message")
			if !hasMessage {
				message = "Delete file"
			}
			branch, _ := bodyString(input, "branch")
			sha, hasSha := bodyString(input, "sha")
			var expectedSha *string
			if hasSha {
				expectedSha = &sha
			}
			result := forge.WriteFile(repo, contentPath, nil, false, message, branch, expectedSha)
			if result.Error != "" {
				switch result.Error {
				case "stale sha":
					return restErr(409, "sha does not match")
				case "file does not exist":
					return restErr(404, "Not Found")
				default:
					return restErr(422, result.Error)
				}
			}
			return restOK(NewOMap().Set("commit", NewOMap().Set("sha", result.CommitSha).Set("message", message)), 200, nil)
		}
		return restNotFound("")
	}

	// labels.
	if tail[0] == "labels" && len(tail) == 1 {
		if method == "GET" {
			labels := make([]*OMap, 0, len(repo.Labels))
			for _, label := range repo.Labels {
				labels = append(labels, NewOMap().
					Set("id", label.ID).
					Set("name", label.Name).
					Set("color", label.Color).
					Set("description", ""))
			}
			return restOK(labels, 200, nil)
		}
		if method == "POST" {
			input := bodyObject(body, false)
			name, ok := bodyString(input, "name")
			if !ok || name == "" {
				return restErr(422, "name is required")
			}
			color, hasColor := bodyString(input, "color")
			if !hasColor {
				color = "ededed"
			}
			if existing, ok := repo.Labels[name]; ok {
				return restOK(NewOMap().
					Set("id", existing.ID).
					Set("name", existing.Name).
					Set("color", existing.Color).
					Set("description", ""), 201, nil)
			}
			label := &Label{ID: nil, Name: name, Color: color}
			repo.Labels[name] = label
			return restOK(NewOMap().
				Set("id", label.ID).
				Set("name", name).
				Set("color", color).
				Set("description", ""), 201, nil)
		}
		return restNotFound("")
	}

	// issues and comments.
	if tail[0] == "issues" && len(tail) == 1 {
		if method == "GET" {
			state := query.Get("state")
			if state == "" {
				state = "open"
			}
			direction := query.Get("direction")
			if direction == "" {
				direction = "desc"
			}
			list := make([]*Issue, 0, len(repo.Issues))
			for _, issue := range repo.Issues {
				if state != "all" && issue.State != state {
					continue
				}
				list = append(list, issue)
			}
			sortIssuesBySeq(list, direction == "asc")
			page := pageOf(query)
			start, end, totalPages := paginateLen(len(list), page)
			headers := map[string]string{}
			if link := linkHeader(host, path, query, page, totalPages); link != "" {
				headers["link"] = link
			}
			views := make([]issueView, 0, end-start)
			for _, issue := range list[start:end] {
				views = append(views, IssueJSON(repo, issue))
			}
			return restOK(views, 200, headers)
		}
		if method == "POST" {
			input := bodyObject(body, false)
			title, ok := bodyString(input, "title")
			if !ok || title == "" {
				return restErr(422, "title is required")
			}
			bodyText, _ := bodyString(input, "body")
			var labels []string
			if raw, ok := input["labels"].([]any); ok {
				for _, entry := range raw {
					if str, ok := entry.(string); ok {
						labels = append(labels, str)
					}
				}
			}
			created := forge.CreateIssue(repo, title, bodyText, labels)
			if created.Error != "" {
				return restErr(422, created.Error)
			}
			return restOK(IssueJSON(repo, created.Issue), 201, nil)
		}
		return restNotFound("")
	}
	if tail[0] == "issues" && len(tail) == 2 {
		number, _ := strconv.Atoi(tail[1])
		issue := repo.IssueByNumber[number]
		if issue == nil {
			for _, entry := range repo.Issues {
				if entry.Number == number {
					issue = entry
					break
				}
			}
		}
		if issue == nil {
			return restNotFound("Not Found")
		}
		if method == "GET" {
			return restOKRaw(IssueJSONText(repo, issue), 200, nil)
		}
		if method == "PATCH" {
			input := bodyObject(body, false)
			if state, ok := bodyString(input, "state"); ok && (state == "open" || state == "closed") {
				issue.State = state
			}
			if title, ok := bodyString(input, "title"); ok {
				issue.Title = title
			}
			if bodyText, ok := bodyString(input, "body"); ok {
				issue.Body = bodyText
			}
			DropIssueJSONCache(repo, issue.Number)
			return restOKRaw(IssueJSONText(repo, issue), 200, nil)
		}
		return restNotFound("")
	}
	if tail[0] == "issues" && len(tail) == 3 && tail[2] == "comments" {
		number, _ := strconv.Atoi(tail[1])
		var issue *Issue
		for _, entry := range repo.Issues {
			if entry.Number == number {
				issue = entry
				break
			}
		}
		if issue == nil {
			return restNotFound("Not Found")
		}
		if method == "GET" {
			comments := repo.Comments[number]
			views := make([]*OMap, 0, len(comments))
			for _, comment := range comments {
				views = append(views, NewOMap().
					Set("id", comment.ID).
					Set("body", comment.Body).
					Set("user", NewOMap().Set("login", ViewerLogin).Set("id", 1000).Set("type", "User")).
					Set("created_at", issue.CreatedAt))
			}
			return restOK(views, 200, nil)
		}
		if method == "POST" {
			input := bodyObject(body, false)
			text, ok := bodyString(input, "body")
			if !ok || text == "" {
				return restErr(422, "body is required")
			}
			comment := forge.AddComment(repo, number, text)
			if comment == nil {
				return restNotFound("Not Found")
			}
			return restOK(NewOMap().
				Set("id", comment.ID).
				Set("body", comment.Body).
				Set("user", NewOMap().Set("login", ViewerLogin).Set("id", 1000).Set("type", "User")).
				Set("created_at", issue.CreatedAt), 201, nil)
		}
		return restNotFound("")
	}

	// pull requests.
	if tail[0] == "pulls" && len(tail) == 1 {
		if method == "GET" {
			state := query.Get("state")
			if state == "" {
				state = "open"
			}
			list := make([]*Pull, 0, len(repo.Pulls))
			for _, pull := range repo.Pulls {
				if state != "all" && pull.State != state {
					continue
				}
				list = append(list, pull)
			}
			sortPullsBySeqDesc(list)
			page := pageOf(query)
			start, end, totalPages := paginateLen(len(list), page)
			headers := map[string]string{}
			if link := linkHeader(host, path, query, page, totalPages); link != "" {
				headers["link"] = link
			}
			views := make([]pullView, 0, end-start)
			for _, pull := range list[start:end] {
				views = append(views, PullJSON(repo, pull, forge))
			}
			return restOK(views, 200, headers)
		}
		if method == "POST" {
			input := bodyObject(body, false)
			head, headOK := bodyString(input, "head")
			base, hasBase := bodyString(input, "base")
			if !hasBase {
				base = DefaultBranch
			}
			title, titleOK := bodyString(input, "title")
			if !headOK || !titleOK || title == "" {
				return restErr(422, "head and title are required")
			}
			bodyText, _ := bodyString(input, "body")
			created := forge.CreatePull(repo, head, base, title, bodyText)
			if created.Error != "" {
				return restErr(422, created.Error)
			}
			return restOK(PullJSON(repo, created.Pull, forge), 201, nil)
		}
		return restNotFound("")
	}
	if tail[0] == "pulls" && len(tail) == 2 {
		number, _ := strconv.Atoi(tail[1])
		var pull *Pull
		for _, entry := range repo.Pulls {
			if entry.Number == number {
				pull = entry
				break
			}
		}
		if pull == nil {
			return restNotFound("Not Found")
		}
		if method == "GET" {
			return restOK(PullJSON(repo, pull, forge), 200, nil)
		}
		return restNotFound("")
	}
	if tail[0] == "pulls" && len(tail) == 3 && tail[2] == "files" && method == "GET" {
		number, _ := strconv.Atoi(tail[1])
		var pull *Pull
		for _, entry := range repo.Pulls {
			if entry.Number == number {
				pull = entry
				break
			}
		}
		if pull == nil {
			return restNotFound("Not Found")
		}
		files := forge.PullFiles(repo, pull)
		views := make([]*OMap, 0, len(files))
		for _, file := range files {
			views = append(views, NewOMap().
				Set("filename", file.Path).
				Set("status", file.Status).
				Set("additions", file.Additions).
				Set("deletions", file.Deletions).
				Set("changes", file.Changes))
		}
		return restOK(views, 200, nil)
	}
	if tail[0] == "pulls" && len(tail) == 3 && tail[2] == "merge" && method == "PUT" {
		number, _ := strconv.Atoi(tail[1])
		var pull *Pull
		for _, entry := range repo.Pulls {
			if entry.Number == number {
				pull = entry
				break
			}
		}
		if pull == nil {
			return restNotFound("Not Found")
		}
		merged := forge.MergeSquash(repo, pull)
		if merged.Error != "" {
			switch merged.Error {
			case "conflict":
				return restErr(405, "Pull request is not mergeable")
			case "already-merged":
				return restErr(405, "Pull request is already merged")
			default:
				return restErr(422, merged.Error)
			}
		}
		return restOK(NewOMap().
			Set("merged", true).
			Set("sha", merged.Sha).
			Set("message", "Merged pull request #"+strconv.Itoa(number)), 200, nil)
	}

	return restNotFound("")
}

func strOrNil(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

func sortIssuesBySeq(list []*Issue, asc bool) {
	for i := 1; i < len(list); i++ {
		for j := i; j > 0; j-- {
			less := list[j].Seq < list[j-1].Seq
			if !asc {
				less = list[j].Seq > list[j-1].Seq
			}
			if !less {
				break
			}
			list[j], list[j-1] = list[j-1], list[j]
		}
	}
}

func sortPullsBySeqDesc(list []*Pull) {
	for i := 1; i < len(list); i++ {
		for j := i; j > 0 && list[j].Seq > list[j-1].Seq; j-- {
			list[j], list[j-1] = list[j-1], list[j]
		}
	}
}
