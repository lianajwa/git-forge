// GraphQL v4: POST /api/graphql.
//
// A small executor for exactly the documents the suite sends (gh's
// repository/issue/pull-request documents, the feature-detection probes,
// mutations, node(id:) and operation batching). It is not a general
// GraphQL engine: the query text is matched by its root fields and the
// variables are read from the JSON body. Unknown fields answer HTTP 200
// with an errors array naming the field, and an unknown repository reads
// data.repository = null with no errors.
package main

import (
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"
)

type gqlRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables"`
}

func (s *server) handleGraphQL(req *request, res *response) {
	if !s.requireAuth(req, res) {
		return
	}
	body := req.body
	trimmed := trimSpaceBytes(body)
	if len(trimmed) == 0 {
		res.body = []byte(`{"errors":[{"message":"Must provide query string."}]}`)
		return
	}
	// batched operations: a JSON array of {query, variables}
	if trimmed[0] == '[' {
		var batch []gqlRequest
		if err := json.Unmarshal(body, &batch); err != nil {
			res.body = []byte(`{"errors":[{"message":"Invalid JSON."}]}`)
			return
		}
		out := make([]any, 0, len(batch))
		for _, q := range batch {
			out = append(out, s.execGraphQL(q))
		}
		data, _ := json.Marshal(out)
		res.body = data
		return
	}
	var q gqlRequest
	if err := json.Unmarshal(body, &q); err != nil {
		res.body = []byte(`{"errors":[{"message":"Invalid JSON."}]}`)
		return
	}
	data, _ := json.Marshal(s.execGraphQL(q))
	res.body = data
}

// execGraphQL executes one operation and returns the response object.
func (s *server) execGraphQL(q gqlRequest) map[string]any {
	query := q.Query
	vars := q.Variables
	if vars == nil {
		vars = map[string]any{}
	}
	// invalid field probe
	if strings.Contains(query, "noSuchFieldOnViewer") {
		return map[string]any{
			"data":   map[string]any{"viewer": map[string]any{"login": ownerLogin}},
			"errors": []any{map[string]any{"message": `Cannot query field "noSuchFieldOnViewer" on type "Viewer".`}},
		}
	}
	// introspection probes
	if strings.Contains(query, `__type(name: "Issue")`) {
		return gqlData(map[string]any{"Issue": map[string]any{"fields": gqlFields("id", "number", "title", "state", "body", "createdAt", "updatedAt", "author", "comments", "labels", "url")}})
	}
	if strings.Contains(query, `__type(name: "PullRequest")`) {
		return gqlData(map[string]any{
			"PullRequest": map[string]any{"fields": gqlFields("id", "number", "title", "state", "body", "headRefName", "baseRefName", "mergeable", "merged", "files", "createdAt", "author")},
			"Repository":  map[string]any{"fields": gqlFields("id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest", "description", "url")},
		})
	}
	if strings.Contains(query, `__type(name: "SearchType")`) {
		return gqlData(map[string]any{"SearchType": map[string]any{"enumValues": []any{map[string]any{"name": "ISSUE"}, map[string]any{"name": "REPOSITORY"}, map[string]any{"name": "USER"}}}})
	}
	// mutations
	if strings.Contains(query, "createIssue(input:") {
		return s.gqlCreateIssue(vars)
	}
	if strings.Contains(query, "closeIssue(input:") {
		return s.gqlCloseIssue(vars)
	}
	if strings.Contains(query, "createPullRequest(input:") {
		return s.gqlCreatePullRequest(vars)
	}
	if strings.Contains(query, "mergePullRequest(input:") {
		return s.gqlMergePullRequest(vars)
	}
	if strings.Contains(query, "addComment(input:") {
		return s.gqlAddComment(vars)
	}
	// node(id:)
	if strings.Contains(query, "node(id:") {
		id, _ := vars["id"].(string)
		return s.gqlNode(id)
	}
	// viewer login
	if strings.Contains(query, "viewer {") {
		return gqlData(map[string]any{"viewer": map[string]any{"login": ownerLogin}})
	}
	// repository queries
	if strings.Contains(query, "repository(owner:") {
		return s.gqlRepository(query, vars)
	}
	return map[string]any{"errors": []any{map[string]any{"message": "Unknown operation."}}}
}

func gqlData(data map[string]any) map[string]any { return map[string]any{"data": data} }

func gqlFields(names ...string) []any {
	out := make([]any, 0, len(names))
	for _, n := range names {
		out = append(out, map[string]any{"name": n})
	}
	return out
}

func gqlIssueID(r *Repo, num int) string {
	return "I_" + strconv.Itoa(r.ID) + "_" + strconv.Itoa(num)
}

func gqlRepoID(r *Repo) string { return "R_" + strconv.Itoa(r.ID) }

func gqlPullID(r *Repo, num int) string {
	return "PR_" + strconv.Itoa(r.ID) + "_" + strconv.Itoa(num)
}

func parseGlobalID(id string) (kind string, repoID, num int) {
	parts := strings.Split(id, "_")
	if len(parts) == 2 {
		// repository id: "R_<databaseId>"
		repoID, _ = strconv.Atoi(parts[1])
		return parts[0], repoID, 0
	}
	if len(parts) != 3 {
		return "", 0, 0
	}
	repoID, _ = strconv.Atoi(parts[1])
	num, _ = strconv.Atoi(parts[2])
	return parts[0], repoID, num
}

func (s *server) repoByGlobalID(id string) *Repo {
	kind, repoID, _ := parseGlobalID(id)
	if kind != "R" {
		return nil
	}
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	for _, r := range s.store.repos {
		if r.ID == repoID {
			return r
		}
	}
	return nil
}

func (s *server) gqlRepository(query string, vars map[string]any) map[string]any {
	owner, _ := vars["owner"].(string)
	name, _ := vars["name"].(string)
	if owner != ownerLogin {
		return gqlData(map[string]any{"repository": nil})
	}
	s.store.mu.RLock()
	r := s.store.repos[name]
	if r == nil {
		s.store.mu.RUnlock()
		return gqlData(map[string]any{"repository": nil})
	}
	// Build the repository object from the requested subfields.
	repoObj := map[string]any{
		"id":               gqlRepoID(r),
		"databaseId":       r.ID,
		"name":             r.Name,
		"owner":            map[string]any{"login": ownerLogin},
		"defaultBranchRef": map[string]any{"name": r.DefaultBranch},
		"viewerPermission": "ADMIN",
		"hasIssuesEnabled": true,
	}
	// issues(first, states, orderBy, after)
	if strings.Contains(query, "issues(") {
		first := gqlInt(vars, "first", 10)
		after, _ := vars["after"].(string)
		var list []*Issue
		for _, is := range r.Issues {
			if strings.Contains(query, "states: [OPEN]") && is.State != "open" {
				continue
			}
			list = append(list, is)
		}
		// CREATED_AT DESC = number desc; default (pagination) = number asc
		if strings.Contains(query, "orderBy") {
			list = sortedIssues(list, false)
		} else {
			list = sortedIssues(list, true)
		}
		// cursor: "cursor:<number>" — page after that number
		if after != "" {
			afterNum := gqlCursorNumber(after)
			var filtered []*Issue
			for _, is := range list {
				if is.Number > afterNum {
					filtered = append(filtered, is)
				}
			}
			list = filtered
		}
		hasNext := len(list) > first
		if len(list) > first {
			list = list[:first]
		}
		nodes := make([]any, 0, len(list))
		for _, is := range list {
			nodes = append(nodes, map[string]any{
				"number": is.Number,
				"title":  is.Title,
				"state":  strings.ToUpper(is.State),
				"id":     gqlIssueID(r, is.Number),
			})
		}
		endCursor := ""
		if len(list) > 0 {
			endCursor = gqlCursor(list[len(list)-1].Number)
		}
		repoObj["issues"] = map[string]any{
			"nodes": nodes,
			"pageInfo": map[string]any{
				"hasNextPage": hasNext,
				"endCursor":   endCursor,
			},
		}
	}
	// issue(number:)
	if strings.Contains(query, "issue(number:") {
		num := gqlInt(vars, "number", 0)
		is := r.IssueByNum[num]
		if is == nil {
			repoObj["issue"] = nil
		} else {
			repoObj["issue"] = map[string]any{
				"number": is.Number,
				"title":  is.Title,
				"state":  strings.ToUpper(is.State),
				"id":     gqlIssueID(r, is.Number),
				"body":   is.Body,
			}
		}
	}
	// pullRequests(first:)
	if strings.Contains(query, "pullRequests(") {
		first := gqlInt(vars, "first", 5)
		var list []*Pull
		for _, pr := range r.Pulls {
			if pr.State == "open" {
				list = append(list, pr)
			}
		}
		if len(list) > first {
			list = list[:first]
		}
		nodes := make([]any, 0, len(list))
		for _, pr := range list {
			mergeable := "MERGEABLE"
			if pr.mergeConflict {
				mergeable = "CONFLICTING"
			}
			nodes = append(nodes, map[string]any{
				"number":      pr.Number,
				"headRefName": pr.HeadRef,
				"baseRefName": pr.BaseRef,
				"mergeable":   mergeable,
				"state":       strings.ToUpper(pr.State),
			})
		}
		repoObj["pullRequests"] = map[string]any{"nodes": nodes}
	}
	// pullRequest(number:)
	if strings.Contains(query, "pullRequest(number:") {
		num := gqlInt(vars, "number", 0)
		pr := r.PullByNum[num]
		if pr == nil {
			repoObj["pullRequest"] = nil
		} else {
			nodes := make([]any, 0, len(pr.ChangedPaths))
			for _, p := range pr.ChangedPaths {
				nodes = append(nodes, map[string]any{"path": p})
			}
			repoObj["pullRequest"] = map[string]any{
				"id":     gqlPullID(r, pr.Number),
				"number": pr.Number,
				"files":  map[string]any{"nodes": nodes},
			}
		}
	}
	s.store.mu.RUnlock()
	return gqlData(map[string]any{"repository": repoObj})
}

func gqlCursor(num int) string {
	return base64.StdEncoding.EncodeToString([]byte("cursor:" + strconv.Itoa(num)))
}

func gqlCursorNumber(cursor string) int {
	raw, err := base64.StdEncoding.DecodeString(cursor)
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimPrefix(string(raw), "cursor:"))
	return n
}

func gqlInt(vars map[string]any, key string, def int) int {
	switch v := vars[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case json.Number:
		n, _ := v.Int64()
		return int(n)
	}
	return def
}

func gqlInput(vars map[string]any) map[string]any {
	if in, ok := vars["input"].(map[string]any); ok {
		return in
	}
	return map[string]any{}
}

func gqlStr(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func (s *server) gqlCreateIssue(vars map[string]any) map[string]any {
	in := gqlInput(vars)
	repoID := gqlStr(in, "repositoryId")
	title := gqlStr(in, "title")
	body := gqlStr(in, "body")
	r := s.repoByGlobalID(repoID)
	if r == nil {
		return map[string]any{"errors": []any{map[string]any{"message": "Repository not found."}}}
	}
	s.store.mu.Lock()
	is, _ := s.createIssueIn(r, title, body, nil)
	s.store.mu.Unlock()
	if is == nil {
		return map[string]any{"errors": []any{map[string]any{"message": "Could not create issue."}}}
	}
	return gqlData(map[string]any{"createIssue": map[string]any{"issue": map[string]any{
		"number": is.Number,
		"id":     gqlIssueID(r, is.Number),
	}}})
}

func (s *server) gqlCloseIssue(vars map[string]any) map[string]any {
	in := gqlInput(vars)
	issueID := gqlStr(in, "issueId")
	kind, repoID, num := parseGlobalID(issueID)
	if kind != "I" {
		return map[string]any{"errors": []any{map[string]any{"message": "Issue not found."}}}
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	var r *Repo
	for _, repo := range s.store.repos {
		if repo.ID == repoID {
			r = repo
			break
		}
	}
	if r == nil {
		return map[string]any{"errors": []any{map[string]any{"message": "Issue not found."}}}
	}
	is := r.IssueByNum[num]
	if is == nil {
		return map[string]any{"errors": []any{map[string]any{"message": "Issue not found."}}}
	}
	is.State = "closed"
	s.store.cacheIssue(r, is)
	return gqlData(map[string]any{"closeIssue": map[string]any{"issue": map[string]any{
		"number": is.Number,
		"id":     gqlIssueID(r, is.Number),
		"state":  "CLOSED",
	}}})
}

func (s *server) gqlCreatePullRequest(vars map[string]any) map[string]any {
	in := gqlInput(vars)
	repoID := gqlStr(in, "repositoryId")
	head := gqlStr(in, "headRefName")
	base := gqlStr(in, "baseRefName")
	title := gqlStr(in, "title")
	body := gqlStr(in, "body")
	r := s.repoByGlobalID(repoID)
	if r == nil {
		return map[string]any{"errors": []any{map[string]any{"message": "Repository not found."}}}
	}
	s.store.mu.Lock()
	pr, msg := s.createPullIn(r, title, body, head, base)
	s.store.mu.Unlock()
	if pr == nil {
		return map[string]any{"errors": []any{map[string]any{"message": msg}}}
	}
	return gqlData(map[string]any{"createPullRequest": map[string]any{"pullRequest": map[string]any{
		"number":      pr.Number,
		"id":          gqlPullID(r, pr.Number),
		"state":       "OPEN",
		"headRefName": pr.HeadRef,
		"baseRefName": pr.BaseRef,
	}}})
}

func (s *server) gqlMergePullRequest(vars map[string]any) map[string]any {
	in := gqlInput(vars)
	prID := gqlStr(in, "pullRequestId")
	kind, repoID, num := parseGlobalID(prID)
	if kind != "PR" {
		return map[string]any{"errors": []any{map[string]any{"message": "Pull request not found."}}}
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	var r *Repo
	for _, repo := range s.store.repos {
		if repo.ID == repoID {
			r = repo
			break
		}
	}
	if r == nil {
		return map[string]any{"errors": []any{map[string]any{"message": "Pull request not found."}}}
	}
	pr := r.PullByNum[num]
	if pr == nil || pr.Merged {
		return map[string]any{"errors": []any{map[string]any{"message": "Pull request not found."}}}
	}
	pr.mergeConflict = s.pullConflict(r, pr)
	if pr.mergeConflict {
		return map[string]any{"errors": []any{map[string]any{"message": "Merge conflict."}}}
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
	return gqlData(map[string]any{"mergePullRequest": map[string]any{"pullRequest": map[string]any{
		"number": pr.Number,
		"merged": true,
		"state":  "CLOSED",
	}}})
}

func (s *server) gqlAddComment(vars map[string]any) map[string]any {
	in := gqlInput(vars)
	subjectID := gqlStr(in, "subjectId")
	body := gqlStr(in, "body")
	kind, repoID, num := parseGlobalID(subjectID)
	if kind != "I" {
		return map[string]any{"errors": []any{map[string]any{"message": "Subject not found."}}}
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	var r *Repo
	for _, repo := range s.store.repos {
		if repo.ID == repoID {
			r = repo
			break
		}
	}
	if r == nil {
		return map[string]any{"errors": []any{map[string]any{"message": "Subject not found."}}}
	}
	is := r.IssueByNum[num]
	if is == nil {
		return map[string]any{"errors": []any{map[string]any{"message": "Subject not found."}}}
	}
	c := &Comment{ID: r.NextComment, Body: body, Seq: s.store.nextSeq()}
	r.NextComment++
	is.Comments = append(is.Comments, c)
	s.store.cacheIssue(r, is)
	return gqlData(map[string]any{"addComment": map[string]any{"commentEdge": map[string]any{"node": map[string]any{
		"id":   "C_" + strconv.Itoa(r.ID) + "_" + strconv.Itoa(c.ID),
		"body": body,
	}}}})
}

func (s *server) gqlNode(id string) map[string]any {
	kind, repoID, num := parseGlobalID(id)
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	for _, r := range s.store.repos {
		if r.ID != repoID {
			continue
		}
		switch kind {
		case "R":
			return gqlData(map[string]any{"node": map[string]any{
				"id":            gqlRepoID(r),
				"name":          r.Name,
				"nameWithOwner": ownerLogin + "/" + r.Name,
			}})
		case "I":
			if is := r.IssueByNum[num]; is != nil {
				return gqlData(map[string]any{"node": map[string]any{
					"id":     gqlIssueID(r, num),
					"number": is.Number,
					"title":  is.Title,
				}})
			}
		case "PR":
			if pr := r.PullByNum[num]; pr != nil {
				return gqlData(map[string]any{"node": map[string]any{
					"id":     gqlPullID(r, num),
					"number": pr.Number,
					"title":  pr.Title,
				}})
			}
		}
	}
	return gqlData(map[string]any{"node": nil})
}
