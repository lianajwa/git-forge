// REST v3: /api/v3 — repositories, issues, pull requests, contents.
//
// Shapes follow the kit's rest-client expectations: every reply is
// application/json with a non-empty X-GitHub-Request-Id (added by the HTTP
// layer), errors are {"message": string}, unknown routes are JSON 404 and a
// malformed JSON body is a 400. The token's owner is always `adaiversen`
// (see store.go); any other owner segment answers 404, never 403.
package main

import (
	"encoding/base64"
	"strconv"
	"strings"
)

func errJSON(msg string) []byte {
	bp := getBuf()
	b := *bp
	b = append(b, `{"message":`...)
	b = jsonEscape(b, msg)
	b = append(b, '}')
	out := make([]byte, len(b))
	copy(out, b)
	putBuf(bp)
	return out
}

func (s *server) restError(res *response, status int, msg string) {
	res.status = status
	res.body = errJSON(msg)
}

// queryParams parses req.query into a map.
func queryParams(q string) map[string]string {
	out := map[string]string{}
	for q != "" {
		kv := q
		if i := strings.IndexByte(q, '&'); i >= 0 {
			kv, q = q[:i], q[i+1:]
		} else {
			q = ""
		}
		if i := strings.IndexByte(kv, '='); i >= 0 {
			out[kv[:i]] = urlUnescape(kv[i+1:])
		} else if kv != "" {
			out[kv] = ""
		}
	}
	return out
}

func urlUnescape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '%' && i+2 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+3], 16, 8); err == nil {
				b.WriteByte(byte(v))
				i += 2
				continue
			}
		}
		if c == '+' {
			b.WriteByte(' ')
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

// splitRest splits a /api/v3 path into segments (unescaped, '/' inside a
// contents path kept as one trailing segment is handled by callers).
func splitRest(path string) []string {
	path = strings.TrimPrefix(path, "/api/v3")
	path = strings.Trim(path, "/")
	if path == "" {
		return nil
	}
	parts := strings.Split(path, "/")
	for i, p := range parts {
		parts[i] = urlUnescape(p)
	}
	return parts
}

// auth: a non-empty Bearer token is required for every /api/v3 route.
func (s *server) requireAuth(req *request, res *response) bool {
	auth := req.headerString("authorization")
	if !strings.HasPrefix(auth, "Bearer ") || len(strings.TrimSpace(auth[7:])) == 0 {
		s.restError(res, 401, "Requires authentication")
		return false
	}
	return true
}

// getRepo resolves owner/name under the store read lock. Returns nil when
// the owner is foreign or the repo does not exist.
func (s *server) getRepo(owner, name string) *Repo {
	if owner != ownerLogin {
		return nil
	}
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	return s.store.repos[name]
}

// --- repository helpers -------------------------------------------------------

func (s *server) createRepoFrom(name, desc string, priv bool) (*Repo, bool) {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	if _, ok := s.store.repos[name]; ok {
		return nil, false
	}
	s.store.nextID++
	r := &Repo{
		Name:          name,
		Description:   desc,
		Private:       priv,
		DefaultBranch: "main",
		ID:            s.store.nextID,
		Refs:          make(map[string]string),
		Objects:       make(map[string]*Object),
		IssueByNum:    make(map[int]*Issue),
		Labels:        make(map[string]*Label),
		PullByNum:     make(map[int]*Pull),
		NextIssue:     1,
		NextPull:      1,
		NextComment:   1,
	}
	r.GlobalID = "R_" + strconv.Itoa(r.ID)
	r.Seq = s.store.nextSeq()
	s.store.repos[name] = r
	s.store.cacheRepo(r)
	return r, true
}

// makeCommit builds a commit object (author fixed) and stores it.
func (s *server) makeCommit(r *Repo, treeOID string, parents []string, message string) string {
	var b strings.Builder
	b.WriteString("tree " + treeOID + "\n")
	for _, p := range parents {
		b.WriteString("parent " + p + "\n")
	}
	b.WriteString("author adaiversen <adaiversen@example.invalid> 1700000000 +0000\n")
	b.WriteString("committer adaiversen <adaiversen@example.invalid> 1700000000 +0000\n")
	b.WriteString("\n")
	b.WriteString(message)
	data := []byte(b.String())
	oid := hashObject("commit", data)
	if _, ok := r.Objects[oid]; !ok {
		r.Objects[oid] = &Object{Type: "commit", Data: data}
	}
	return oid
}

// commitFile creates/updates a file on a branch: new blob, new tree, new
// commit, ref update. Returns the commit oid and blob oid.
func (s *server) commitFile(r *Repo, branch, path string, content []byte, message string) (string, string) {
	blobOID := hashObject("blob", content)
	if _, ok := r.Objects[blobOID]; !ok {
		r.Objects[blobOID] = &Object{Type: "blob", Data: content}
	}
	refName := "refs/heads/" + branch
	tip, hasTip := r.Refs[refName]
	var baseTree string
	var parents []string
	if hasTip {
		if obj := r.Objects[tip]; obj != nil && obj.Type == "commit" {
			baseTree = parseCommit(obj.Data).Tree
			parents = []string{tip}
		}
	}
	newTree := updateTree(r, baseTree, splitPath(path), blobOID, "100644")
	commitOID := s.makeCommit(r, newTree, parents, message)
	r.Refs[refName] = commitOID
	r.packVer++
	return commitOID, blobOID
}

// deleteFile removes a file from a branch.
func (s *server) deleteFile(r *Repo, branch, path, message string) (string, bool) {
	refName := "refs/heads/" + branch
	tip, hasTip := r.Refs[refName]
	if !hasTip {
		return "", false
	}
	obj := r.Objects[tip]
	if obj == nil || obj.Type != "commit" {
		return "", false
	}
	baseTree := parseCommit(obj.Data).Tree
	if _, data := lookupBlob(r, baseTree, path); data == nil {
		return "", false
	}
	newTree := updateTree(r, baseTree, splitPath(path), "", "100644")
	commitOID := s.makeCommit(r, newTree, []string{tip}, message)
	r.Refs[refName] = commitOID
	r.packVer++
	return commitOID, true
}

// treeAt resolves a ref (branch name, full ref, or oid) to a tree oid.
func (r *Repo) treeAt(ref string) string {
	var tip string
	if ref == "" {
		tip, _ = r.Refs["refs/heads/"+r.DefaultBranch]
	} else if oid, ok := r.Refs["refs/heads/"+ref]; ok {
		tip = oid
	} else if oid, ok := r.Refs[ref]; ok {
		tip = oid
	} else if isOID(ref) {
		tip = ref
	} else {
		return ""
	}
	obj := r.Objects[tip]
	if obj == nil {
		return ""
	}
	if obj.Type == "commit" {
		return parseCommit(obj.Data).Tree
	}
	if obj.Type == "tag" {
		target, _ := parseTag(obj.Data)
		if to := r.Objects[target]; to != nil && to.Type == "commit" {
			return parseCommit(to.Data).Tree
		}
	}
	return ""
}

// --- issue / pull helpers ------------------------------------------------------

func (s *server) createIssueIn(r *Repo, title, body string, labels []string) (*Issue, string) {
	for _, ln := range labels {
		if _, ok := r.Labels[ln]; !ok {
			return nil, ln
		}
	}
	is := &Issue{
		Number: r.NextIssue,
		Title:  title,
		Body:   body,
		State:  "open",
		Labels: append([]string{}, labels...),
		Seq:    s.store.nextSeq(),
	}
	r.NextIssue++
	r.Issues = append(r.Issues, is)
	r.IssueByNum[is.Number] = is
	s.store.cacheIssue(r, is)
	return is, ""
}

// pullConflict reports whether merging head into base would conflict:
// the same path changed on both sides of the merge-base with different
// resulting blobs.
func (s *server) pullConflict(r *Repo, pr *Pull) bool {
	headTip, _ := r.Refs["refs/heads/"+pr.HeadRef]
	baseTip, _ := r.Refs["refs/heads/"+pr.BaseRef]
	if headTip == "" || baseTip == "" {
		return false
	}
	mb := mergeBase(r, headTip, baseTip)
	if mb == "" {
		return false
	}
	mbTree := ""
	if obj := r.Objects[mb]; obj != nil && obj.Type == "commit" {
		mbTree = parseCommit(obj.Data).Tree
	}
	headTree := ""
	if obj := r.Objects[headTip]; obj != nil && obj.Type == "commit" {
		headTree = parseCommit(obj.Data).Tree
	}
	baseTree := ""
	if obj := r.Objects[baseTip]; obj != nil && obj.Type == "commit" {
		baseTree = parseCommit(obj.Data).Tree
	}
	baseDiff := diffTrees(r, mbTree, baseTree)
	headDiff := diffTrees(r, mbTree, headTree)
	headFlat := treeFlat(r, headTree)
	baseFlat := treeFlat(r, baseTree)
	for p := range baseDiff {
		if _, ok := headDiff[p]; ok {
			he, hok := headFlat[p]
			be, bok := baseFlat[p]
			if hok && bok && he.OID != be.OID {
				return true
			}
			if hok != bok {
				return true
			}
		}
	}
	return false
}

func (s *server) createPullIn(r *Repo, title, body, head, base string) (*Pull, string) {
	headTip, ok := r.Refs["refs/heads/"+head]
	if !ok {
		return nil, "head branch not found"
	}
	baseTip, ok := r.Refs["refs/heads/"+base]
	if !ok {
		return nil, "base branch not found"
	}
	headTree, baseTree := "", ""
	if obj := r.Objects[headTip]; obj != nil && obj.Type == "commit" {
		headTree = parseCommit(obj.Data).Tree
	}
	if obj := r.Objects[baseTip]; obj != nil && obj.Type == "commit" {
		baseTree = parseCommit(obj.Data).Tree
	}
	diff := diffTrees(r, baseTree, headTree)
	paths := make([]string, 0, len(diff))
	for p := range diff {
		paths = append(paths, p)
	}
	// sort for determinism
	for i := 0; i < len(paths); i++ {
		for j := i + 1; j < len(paths); j++ {
			if paths[j] < paths[i] {
				paths[i], paths[j] = paths[j], paths[i]
			}
		}
	}
	pr := &Pull{
		Number:       r.NextPull,
		Title:        title,
		Body:         body,
		State:        "open",
		HeadRef:      head,
		BaseRef:      base,
		HeadSHA:      headTip,
		BaseSHA:      baseTip,
		Seq:          s.store.nextSeq(),
		ChangedPaths: paths,
		HeadTree:     headTree,
		BaseTree:     baseTree,
	}
	r.NextPull++
	pr.mergeConflict = s.pullConflict(r, pr)
	r.Pulls = append(r.Pulls, pr)
	r.PullByNum[pr.Number] = pr
	return pr, ""
}

// renderPull builds a PR JSON object.
func (s *server) renderPull(r *Repo, pr *Pull) []byte {
	bp := getBuf()
	b := *bp
	b = append(b, `{"id":"PR_`...)
	b = strconv.AppendInt(b, int64(r.ID), 10)
	b = append(b, '_')
	b = strconv.AppendInt(b, int64(pr.Number), 10)
	b = append(b, `","number":`...)
	b = strconv.AppendInt(b, int64(pr.Number), 10)
	b = append(b, `,"title":`...)
	b = jsonEscape(b, pr.Title)
	b = append(b, `,"body":`...)
	b = jsonEscape(b, pr.Body)
	b = append(b, `,"state":"`...)
	b = append(b, pr.State...)
	b = append(b, `","head":{"ref":`...)
	b = jsonEscape(b, pr.HeadRef)
	b = append(b, `,"sha":`...)
	b = jsonEscape(b, pr.HeadSHA)
	b = append(b, `},"base":{"ref":`...)
	b = jsonEscape(b, pr.BaseRef)
	b = append(b, `,"sha":`...)
	b = jsonEscape(b, pr.BaseSHA)
	b = append(b, `},"merged":`...)
	if pr.Merged {
		b = append(b, "true"...)
	} else {
		b = append(b, "false"...)
	}
	b = append(b, `,"mergeable":`...)
	if pr.mergeConflict {
		b = append(b, "false"...)
	} else {
		b = append(b, "true"...)
	}
	b = append(b, `,"merge_commit_sha":`...)
	if pr.MergeCommit != "" {
		b = jsonEscape(b, pr.MergeCommit)
	} else {
		b = append(b, "null"...)
	}
	b = append(b, `,"merged_at":`...)
	if pr.MergedAt != "" {
		b = jsonEscape(b, pr.MergedAt)
	} else {
		b = append(b, "null"...)
	}
	b = append(b, `,"user":{"login":"`...)
	b = append(b, ownerLogin...)
	b = append(b, `"},"html_url":"https://example.invalid/`...)
	b = append(b, ownerLogin...)
	b = append(b, '/')
	b = append(b, r.Name...)
	b = append(b, `/pull/`...)
	b = strconv.AppendInt(b, int64(pr.Number), 10)
	b = append(b, `"}`...)
	out := make([]byte, len(b))
	copy(out, b)
	putBuf(bp)
	return out
}

// lineDiff counts added/removed lines between two blobs (LCS on lines).
func lineDiff(oldData, newData []byte) (add, del int) {
	oldLines := splitLines(oldData)
	newLines := splitLines(newData)
	// LCS DP on small files
	n, m := len(oldLines), len(newLines)
	if n*m > 1_000_000 {
		// too big: approximate
		return m, n
	}
	dp := make([][]int, n+1)
	for i := range dp {
		dp[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if oldLines[i] == newLines[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else if dp[i+1][j] > dp[i][j+1] {
				dp[i][j] = dp[i+1][j]
			} else {
				dp[i][j] = dp[i][j+1]
			}
		}
	}
	common := dp[0][0]
	return m - common, n - common
}

func splitLines(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	var out []string
	start := 0
	for i := 0; i < len(b); i++ {
		if b[i] == '\n' {
			out = append(out, string(b[start:i]))
			start = i + 1
		}
	}
	if start < len(b) {
		out = append(out, string(b[start:]))
	}
	return out
}

// --- the dispatcher ---------------------------------------------------------

func (s *server) handleREST(req *request, res *response) {
	if !s.requireAuth(req, res) {
		return
	}
	parts := splitRest(req.path)
	if len(parts) == 0 {
		// GET /api/v3/
		if req.method != "GET" {
			s.restError(res, 404, "Not Found")
			return
		}
		res.setHeader("X-OAuth-Scopes", "repo")
		res.body = []byte(`{"current_user_url":"/api/v3/user","repository_url":"/api/v3/repos/{owner}/{repo}"}`)
		return
	}
	switch parts[0] {
	case "meta":
		if req.method == "GET" && len(parts) == 1 {
			res.body = []byte(`{"installed_version":"1.0.0"}`)
			return
		}
	case "user":
		s.handleUserRepos(req, res, parts[1:])
		return
	case "repos":
		s.handleRepos(req, res, parts[1:])
		return
	}
	s.restError(res, 404, "Not Found")
}

func (s *server) handleUserRepos(req *request, res *response, parts []string) {
	if len(parts) == 1 && parts[0] == "repos" {
		switch req.method {
		case "GET":
			s.listUserRepos(req, res)
			return
		case "POST":
			if !validJSON(req.body) {
				s.restError(res, 400, "Invalid JSON")
				return
			}
			name, _ := jsonGetString(req.body, "name")
			if name == "" {
				s.restError(res, 422, "name is required")
				return
			}
			desc, _ := jsonGetString(req.body, "description")
			priv, _ := jsonGetBool(req.body, "private")
			r, ok := s.createRepoFrom(name, desc, priv)
			if !ok {
				s.restError(res, 422, "Repository "+name+" already exists")
				return
			}
			res.status = 201
			res.body = r.repoJSON
			return
		}
	}
	s.restError(res, 404, "Not Found")
}

func (s *server) listUserRepos(req *request, res *response) {
	q := queryParams(req.query)
	perPage := 30
	if v, ok := q["per_page"]; ok {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			perPage = n
		}
	}
	page := 1
	if v, ok := q["page"]; ok {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			page = n
		}
	}
	s.store.mu.RLock()
	names := make([]string, 0, len(s.store.repos))
	for name := range s.store.repos {
		names = append(names, name)
	}
	// creation order: sort by Seq
	sortReposBySeq(s.store, names)
	total := len(names)
	start := (page - 1) * perPage
	if start > total {
		start = total
	}
	end := start + perPage
	if end > total {
		end = total
	}
	var bodies [][]byte
	for _, name := range names[start:end] {
		bodies = append(bodies, s.store.repos[name].repoJSON)
	}
	s.store.mu.RUnlock()
	bp := getBuf()
	b := *bp
	b = append(b, '[')
	for i, body := range bodies {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, body...)
	}
	b = append(b, ']')
	out := make([]byte, len(b))
	copy(out, b)
	putBuf(bp)
	res.body = out
	// Link header when more pages exist
	if end < total {
		host := req.headerString("host")
		if host == "" {
			host = "127.0.0.1"
		}
		base := "http://" + host + "/api/v3/user/repos?per_page=" + strconv.Itoa(perPage)
		lastPage := (total + perPage - 1) / perPage
		link := "<" + base + "&page=" + strconv.Itoa(page+1) + `>; rel="next", <` + base + "&page=" + strconv.Itoa(lastPage) + `>; rel="last"`
		res.setHeader("Link", link)
	}
}

func sortReposBySeq(st *Store, names []string) {
	for i := 0; i < len(names); i++ {
		for j := i + 1; j < len(names); j++ {
			if st.repos[names[j]].Seq < st.repos[names[i]].Seq {
				names[i], names[j] = names[j], names[i]
			}
		}
	}
}

func (s *server) handleRepos(req *request, res *response, parts []string) {
	if len(parts) < 2 {
		s.restError(res, 404, "Not Found")
		return
	}
	owner, name := parts[0], parts[1]
	// repo-scoped routes
	if len(parts) == 2 {
		switch req.method {
		case "GET":
			r := s.getRepo(owner, name)
			if r == nil {
				s.restError(res, 404, "Not Found")
				return
			}
			res.body = r.repoJSON
			return
		case "PATCH":
			if !validJSON(req.body) {
				s.restError(res, 400, "Invalid JSON")
				return
			}
			s.store.mu.Lock()
			r := s.store.repos[name]
			if owner != ownerLogin || r == nil {
				s.store.mu.Unlock()
				s.restError(res, 404, "Not Found")
				return
			}
			if desc, ok := jsonGetString(req.body, "description"); ok {
				r.Description = desc
			}
			s.store.cacheRepo(r)
			body := r.repoJSON
			s.store.mu.Unlock()
			res.body = body
			return
		case "DELETE":
			s.store.mu.Lock()
			r := s.store.repos[name]
			if owner != ownerLogin || r == nil {
				s.store.mu.Unlock()
				s.restError(res, 404, "Not Found")
				return
			}
			delete(s.store.repos, name)
			s.store.mu.Unlock()
			res.status = 204
			res.noBody = true
			return
		}
		s.restError(res, 404, "Not Found")
		return
	}
	// sub-resources: need the repo
	r := s.getRepo(owner, name)
	if r == nil {
		s.restError(res, 404, "Not Found")
		return
	}
	rest := parts[2:]
	switch rest[0] {
	case "branches":
		if req.method == "GET" && len(rest) == 1 {
			s.store.mu.RLock()
			names := make([]string, 0, len(r.Refs))
			for ref := range r.Refs {
				if strings.HasPrefix(ref, "refs/heads/") {
					names = append(names, strings.TrimPrefix(ref, "refs/heads/"))
				}
			}
			for i := 0; i < len(names); i++ {
				for j := i + 1; j < len(names); j++ {
					if names[j] < names[i] {
						names[i], names[j] = names[j], names[i]
					}
				}
			}
			bp := getBuf()
			b := *bp
			b = append(b, '[')
			for i, bn := range names {
				if i > 0 {
					b = append(b, ',')
				}
				b = append(b, `{"name":`...)
				b = jsonEscape(b, bn)
				b = append(b, `,"commit":{"sha":`...)
				b = jsonEscape(b, r.Refs["refs/heads/"+bn])
				b = append(b, `},"protected":false}`...)
			}
			b = append(b, ']')
			out := make([]byte, len(b))
			copy(out, b)
			putBuf(bp)
			s.store.mu.RUnlock()
			res.body = out
			return
		}
	case "git":
		if len(rest) == 2 && rest[1] == "refs" && req.method == "POST" {
			if !validJSON(req.body) {
				s.restError(res, 400, "Invalid JSON")
				return
			}
			ref, _ := jsonGetString(req.body, "ref")
			sha, _ := jsonGetString(req.body, "sha")
			if ref == "" || sha == "" {
				s.restError(res, 422, "ref and sha are required")
				return
			}
			s.store.mu.Lock()
			if _, ok := r.Objects[sha]; !ok {
				s.store.mu.Unlock()
				s.restError(res, 422, "object not found")
				return
			}
			r.Refs[ref] = sha
			r.packVer++
			s.store.mu.Unlock()
			bp := getBuf()
			b := *bp
			b = append(b, `{"ref":`...)
			b = jsonEscape(b, ref)
			b = append(b, `,"object":{"sha":`...)
			b = jsonEscape(b, sha)
			b = append(b, `,"type":"commit"}}`...)
			out := make([]byte, len(b))
			copy(out, b)
			putBuf(bp)
			res.status = 201
			res.body = out
			return
		}
	case "commits":
		if req.method == "GET" && len(rest) == 1 {
			q := queryParams(req.query)
			s.store.mu.RLock()
			tip := r.treeAt(q["sha"])
			_ = tip
			var refTip string
			if q["sha"] != "" {
				if oid, ok := r.Refs["refs/heads/"+q["sha"]]; ok {
					refTip = oid
				} else if oid, ok := r.Refs[q["sha"]]; ok {
					refTip = oid
				} else if isOID(q["sha"]) {
					refTip = q["sha"]
				}
			} else {
				refTip, _ = r.Refs["refs/heads/"+r.DefaultBranch]
			}
			commits := commitList(r, refTip)
			bp := getBuf()
			b := *bp
			b = append(b, '[')
			for i, oid := range commits {
				if i > 0 {
					b = append(b, ',')
				}
				obj := r.Objects[oid]
				msg := ""
				author := ""
				if obj != nil {
					cv := parseCommit(obj.Data)
					msg = strings.TrimRight(cv.Message, "\n")
					author = cv.Author
				}
				name, email := parseAuthor(author)
				b = append(b, `{"sha":`...)
				b = jsonEscape(b, oid)
				b = append(b, `,"commit":{"message":`...)
				b = jsonEscape(b, msg)
				b = append(b, `,"author":{"name":`...)
				b = jsonEscape(b, name)
				b = append(b, `,"email":`...)
				b = jsonEscape(b, email)
				b = append(b, `,"date":"2026-11-14T06:30:00Z"},"committer":{"name":`...)
				b = jsonEscape(b, name)
				b = append(b, `,"email":`...)
				b = jsonEscape(b, email)
				b = append(b, `,"date":"2026-11-14T06:30:00Z"}}}`...)
			}
			b = append(b, ']')
			out := make([]byte, len(b))
			copy(out, b)
			putBuf(bp)
			s.store.mu.RUnlock()
			res.body = out
			return
		}
	case "contents":
		s.handleContents(req, res, r, rest[1:])
		return
	case "labels":
		if req.method == "POST" && len(rest) == 1 {
			if !validJSON(req.body) {
				s.restError(res, 400, "Invalid JSON")
				return
			}
			name, _ := jsonGetString(req.body, "name")
			color, _ := jsonGetString(req.body, "color")
			if name == "" {
				s.restError(res, 422, "name is required")
				return
			}
			s.store.mu.Lock()
			r.Labels[name] = &Label{Name: name, Color: color}
			s.store.mu.Unlock()
			bp := getBuf()
			b := *bp
			b = append(b, `{"name":`...)
			b = jsonEscape(b, name)
			b = append(b, `,"color":`...)
			b = jsonEscape(b, color)
			b = append(b, `,"description":""}`...)
			out := make([]byte, len(b))
			copy(out, b)
			putBuf(bp)
			res.status = 201
			res.body = out
			return
		}
	case "issues":
		s.handleIssues(req, res, r, rest[1:])
		return
	case "pulls":
		s.handlePulls(req, res, r, rest[1:])
		return
	}
	s.restError(res, 404, "Not Found")
}

func parseAuthor(a string) (name, email string) {
	// "Name <email> ts tz"
	lt := strings.IndexByte(a, '<')
	gt := strings.IndexByte(a, '>')
	if lt < 0 || gt < 0 {
		return a, ""
	}
	return strings.TrimSpace(a[:lt]), a[lt+1 : gt]
}

// --- contents ---------------------------------------------------------------

func (s *server) handleContents(req *request, res *response, r *Repo, parts []string) {
	path := strings.Join(parts, "/")
	q := queryParams(req.query)
	switch req.method {
	case "GET":
		if len(parts) == 0 {
			s.restError(res, 404, "Not Found")
			return
		}
		s.store.mu.RLock()
		tree := r.treeAt(q["ref"])
		// directory?
		if s.contentsIsDir(r, tree, path) {
			entries := s.contentsDirEntries(r, tree, path)
			s.store.mu.RUnlock()
			res.body = entries
			return
		}
		oid, data := lookupBlob(r, tree, path)
		if data == nil {
			s.store.mu.RUnlock()
			s.restError(res, 404, "Not Found")
			return
		}
		bp := getBuf()
		b := *bp
		b = append(b, `{"type":"file","encoding":"base64","content":`...)
		b = jsonEscape(b, base64.StdEncoding.EncodeToString(data))
		b = append(b, `,"sha":`...)
		b = jsonEscape(b, oid)
		b = append(b, `,"size":`...)
		b = strconv.AppendInt(b, int64(len(data)), 10)
		b = append(b, `,"name":`...)
		b = jsonEscape(b, baseName(path))
		b = append(b, `,"path":`...)
		b = jsonEscape(b, path)
		b = append(b, `}`...)
		out := make([]byte, len(b))
		copy(out, b)
		putBuf(bp)
		s.store.mu.RUnlock()
		res.body = out
		return
	case "PUT":
		if !validJSON(req.body) {
			s.restError(res, 400, "Invalid JSON")
			return
		}
		msg, _ := jsonGetString(req.body, "message")
		contentB64, _ := jsonGetString(req.body, "content")
		branch, _ := jsonGetString(req.body, "branch")
		if branch == "" {
			branch = r.DefaultBranch
		}
		sha, hasSHA := jsonGetString(req.body, "sha")
		data, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(contentB64, "\n", ""))
		if err != nil {
			s.restError(res, 400, "Invalid base64 content")
			return
		}
		s.store.mu.Lock()
		// stale sha check
		tree := r.treeAt(branch)
		curOID, _ := lookupBlob(r, tree, path)
		isUpdate := curOID != ""
		if hasSHA && isUpdate && sha != curOID {
			s.store.mu.Unlock()
			s.restError(res, 409, "sha does not match")
			return
		}
		if hasSHA && !isUpdate {
			// updating a file that does not exist: treat as create
			isUpdate = false
		}
		commitOID, blobOID := s.commitFile(r, branch, path, data, msg)
		s.store.mu.Unlock()
		bp := getBuf()
		b := *bp
		b = append(b, `{"content":{"name":`...)
		b = jsonEscape(b, baseName(path))
		b = append(b, `,"path":`...)
		b = jsonEscape(b, path)
		b = append(b, `,"sha":`...)
		b = jsonEscape(b, blobOID)
		b = append(b, `,"size":`...)
		b = strconv.AppendInt(b, int64(len(data)), 10)
		b = append(b, `,"type":"file","encoding":"base64"},"commit":{"sha":`...)
		b = jsonEscape(b, commitOID)
		b = append(b, `,"message":`...)
		b = jsonEscape(b, msg)
		b = append(b, `}}`...)
		out := make([]byte, len(b))
		copy(out, b)
		putBuf(bp)
		if isUpdate {
			res.status = 200
		} else {
			res.status = 201
		}
		res.body = out
		return
	case "DELETE":
		if len(req.body) > 0 && !validJSON(req.body) {
			s.restError(res, 400, "Invalid JSON")
			return
		}
		msg, _ := jsonGetString(req.body, "message")
		branch, _ := jsonGetString(req.body, "branch")
		if branch == "" {
			branch = r.DefaultBranch
		}
		s.store.mu.Lock()
		commitOID, ok := s.deleteFile(r, branch, path, msg)
		s.store.mu.Unlock()
		if !ok {
			s.restError(res, 404, "Not Found")
			return
		}
		bp := getBuf()
		b := *bp
		b = append(b, `{"commit":{"sha":`...)
		b = jsonEscape(b, commitOID)
		b = append(b, `}}`...)
		out := make([]byte, len(b))
		copy(out, b)
		putBuf(bp)
		res.body = out
		return
	}
	s.restError(res, 404, "Not Found")
}

func baseName(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

func (s *server) contentsIsDir(r *Repo, treeOID, path string) bool {
	parts := splitPath(path)
	cur := treeOID
	for i, p := range parts {
		obj := r.Objects[cur]
		if obj == nil || obj.Type != "tree" {
			return false
		}
		var next string
		var mode string
		for _, e := range parseTree(obj.Data) {
			if e.Name == p {
				next, mode = e.OID, e.Mode
				break
			}
		}
		if next == "" {
			return false
		}
		// only the final segment being a tree makes the path a directory;
		// an intermediate tree just lets the walk continue
		if i == len(parts)-1 {
			return mode == "40000"
		}
		if mode != "40000" {
			return false
		}
		cur = next
	}
	return false
}

func (s *server) contentsDirEntries(r *Repo, treeOID, path string) []byte {
	parts := splitPath(path)
	cur := treeOID
	for _, p := range parts {
		obj := r.Objects[cur]
		if obj == nil || obj.Type != "tree" {
			return []byte("[]")
		}
		for _, e := range parseTree(obj.Data) {
			if e.Name == p {
				cur = e.OID
				break
			}
		}
	}
	obj := r.Objects[cur]
	if obj == nil || obj.Type != "tree" {
		return []byte("[]")
	}
	bp := getBuf()
	b := *bp
	b = append(b, '[')
	entries := parseTree(obj.Data)
	for i, e := range entries {
		if i > 0 {
			b = append(b, ',')
		}
		typ := "file"
		size := 0
		if e.Mode == "40000" {
			typ = "dir"
		} else if bo := r.Objects[e.OID]; bo != nil {
			size = len(bo.Data)
		}
		fullPath := path + "/" + e.Name
		b = append(b, `{"type":`...)
		b = jsonEscape(b, typ)
		b = append(b, `,"name":`...)
		b = jsonEscape(b, e.Name)
		b = append(b, `,"path":`...)
		b = jsonEscape(b, fullPath)
		b = append(b, `,"sha":`...)
		b = jsonEscape(b, e.OID)
		b = append(b, `,"size":`...)
		b = strconv.AppendInt(b, int64(size), 10)
		b = append(b, `}`...)
	}
	b = append(b, ']')
	out := make([]byte, len(b))
	copy(out, b)
	putBuf(bp)
	return out
}

// --- issues ------------------------------------------------------------------

func (s *server) handleIssues(req *request, res *response, r *Repo, parts []string) {
	if len(parts) == 0 {
		switch req.method {
		case "POST":
			if !validJSON(req.body) {
				s.restError(res, 400, "Invalid JSON")
				return
			}
			title, _ := jsonGetString(req.body, "title")
			body, _ := jsonGetString(req.body, "body")
			labels := jsonGetStringArray(req.body, "labels")
			s.store.mu.Lock()
			is, bad := s.createIssueIn(r, title, body, labels)
			if is == nil {
				s.store.mu.Unlock()
				s.restError(res, 422, "Label "+bad+" does not exist")
				return
			}
			rendered := *is.rendered.Load()
			s.store.mu.Unlock()
			res.status = 201
			res.body = rendered
			return
		case "GET":
			q := queryParams(req.query)
			state := q["state"]
			if state == "" {
				state = "open"
			}
			direction := q["direction"]
			asc := direction == "asc"
			s.store.mu.RLock()
			var list []*Issue
			for _, is := range r.Issues {
				if state == "all" || is.State == state {
					list = append(list, is)
				}
			}
			list = sortedIssues(list, asc)
			bp := getBuf()
			b := *bp
			b = append(b, '[')
			for i, is := range list {
				if i > 0 {
					b = append(b, ',')
				}
				b = append(b, *is.rendered.Load()...)
			}
			b = append(b, ']')
			out := make([]byte, len(b))
			copy(out, b)
			putBuf(bp)
			s.store.mu.RUnlock()
			res.body = out
			return
		}
		s.restError(res, 404, "Not Found")
		return
	}
	// /issues/{n} or /issues/{n}/comments
	num, err := strconv.Atoi(parts[0])
	if err != nil || num <= 0 {
		s.restError(res, 404, "Not Found")
		return
	}
	if len(parts) == 1 {
		switch req.method {
		case "GET":
			// HOT PATH: the read workload GETs one issue 2,000 times.
			// Serve the pre-rendered bytes; one map lookup, no marshal.
			s.store.mu.RLock()
			is := r.IssueByNum[num]
			if is == nil {
				s.store.mu.RUnlock()
				s.restError(res, 404, "Not Found")
				return
			}
			body := *is.rendered.Load()
			s.store.mu.RUnlock()
			res.body = body
			return
		case "PATCH":
			if !validJSON(req.body) {
				s.restError(res, 400, "Invalid JSON")
				return
			}
			s.store.mu.Lock()
			is := r.IssueByNum[num]
			if is == nil {
				s.store.mu.Unlock()
				s.restError(res, 404, "Not Found")
				return
			}
			if st, ok := jsonGetString(req.body, "state"); ok {
				is.State = st
			}
			if t, ok := jsonGetString(req.body, "title"); ok {
				is.Title = t
			}
			if bd, ok := jsonGetString(req.body, "body"); ok {
				is.Body = bd
			}
			s.store.cacheIssue(r, is)
			rendered := *is.rendered.Load()
			s.store.mu.Unlock()
			res.body = rendered
			return
		}
		s.restError(res, 404, "Not Found")
		return
	}
	if len(parts) == 2 && parts[1] == "comments" {
		switch req.method {
		case "GET":
			s.store.mu.RLock()
			is := r.IssueByNum[num]
			if is == nil {
				s.store.mu.RUnlock()
				s.restError(res, 404, "Not Found")
				return
			}
			bp := getBuf()
			b := *bp
			b = append(b, '[')
			for i, c := range is.Comments {
				if i > 0 {
					b = append(b, ',')
				}
				b = append(b, `{"id":`...)
				b = strconv.AppendInt(b, int64(c.ID), 10)
				b = append(b, `,"body":`...)
				b = jsonEscape(b, c.Body)
				b = append(b, `,"user":{"login":"`...)
				b = append(b, ownerLogin...)
				b = append(b, `"},"created_at":"2026-11-14T06:30:00Z"}`...)
			}
			b = append(b, ']')
			out := make([]byte, len(b))
			copy(out, b)
			putBuf(bp)
			s.store.mu.RUnlock()
			res.body = out
			return
		case "POST":
			if !validJSON(req.body) {
				s.restError(res, 400, "Invalid JSON")
				return
			}
			body, _ := jsonGetString(req.body, "body")
			s.store.mu.Lock()
			is := r.IssueByNum[num]
			if is == nil {
				s.store.mu.Unlock()
				s.restError(res, 404, "Not Found")
				return
			}
			c := &Comment{ID: r.NextComment, Body: body, Seq: s.store.nextSeq()}
			r.NextComment++
			is.Comments = append(is.Comments, c)
			s.store.cacheIssue(r, is)
			s.store.mu.Unlock()
			bp := getBuf()
			b := *bp
			b = append(b, `{"id":`...)
			b = strconv.AppendInt(b, int64(c.ID), 10)
			b = append(b, `,"body":`...)
			b = jsonEscape(b, body)
			b = append(b, `,"user":{"login":"`...)
			b = append(b, ownerLogin...)
			b = append(b, `"},"created_at":"2026-11-14T06:30:00Z"}`...)
			out := make([]byte, len(b))
			copy(out, b)
			putBuf(bp)
			res.status = 201
			res.body = out
			return
		}
	}
	s.restError(res, 404, "Not Found")
}

// jsonGetStringArray extracts a top-level array of strings.
func jsonGetStringArray(body []byte, key string) []string {
	needle := []byte(`"` + key + `"`)
	i := indexBytes(body, needle)
	if i < 0 {
		return nil
	}
	j := i + len(needle)
	for j < len(body) && (body[j] == ' ' || body[j] == '\t' || body[j] == '\n' || body[j] == '\r' || body[j] == ':') {
		j++
	}
	if j >= len(body) || body[j] != '[' {
		return nil
	}
	j++
	var out []string
	for j < len(body) {
		// skip whitespace/commas
		for j < len(body) && (body[j] == ' ' || body[j] == '\t' || body[j] == '\n' || body[j] == '\r' || body[j] == ',') {
			j++
		}
		if j >= len(body) || body[j] == ']' {
			break
		}
		if body[j] != '"' {
			break
		}
		j++
		var sb strings.Builder
		for j < len(body) && body[j] != '"' {
			if body[j] == '\\' && j+1 < len(body) {
				j++
				sb.WriteByte(body[j])
				j++
				continue
			}
			sb.WriteByte(body[j])
			j++
		}
		if j < len(body) {
			j++ // closing quote
		}
		out = append(out, sb.String())
	}
	return out
}

// --- pulls --------------------------------------------------------------------

func (s *server) handlePulls(req *request, res *response, r *Repo, parts []string) {
	if len(parts) == 0 {
		switch req.method {
		case "POST":
			if !validJSON(req.body) {
				s.restError(res, 400, "Invalid JSON")
				return
			}
			title, _ := jsonGetString(req.body, "title")
			body, _ := jsonGetString(req.body, "body")
			head, _ := jsonGetString(req.body, "head")
			base, _ := jsonGetString(req.body, "base")
			if base == "" {
				base = r.DefaultBranch
			}
			s.store.mu.Lock()
			pr, msg := s.createPullIn(r, title, body, head, base)
			if pr == nil {
				s.store.mu.Unlock()
				s.restError(res, 422, msg)
				return
			}
			rendered := s.renderPull(r, pr)
			s.store.mu.Unlock()
			res.status = 201
			res.body = rendered
			return
		case "GET":
			q := queryParams(req.query)
			state := q["state"]
			if state == "" {
				state = "open"
			}
			s.store.mu.RLock()
			var list []*Pull
			for _, pr := range r.Pulls {
				if state == "all" || pr.State == state {
					list = append(list, pr)
				}
			}
			bp := getBuf()
			b := *bp
			b = append(b, '[')
			for i, pr := range list {
				if i > 0 {
					b = append(b, ',')
				}
				b = append(b, s.renderPull(r, pr)...)
			}
			b = append(b, ']')
			out := make([]byte, len(b))
			copy(out, b)
			putBuf(bp)
			s.store.mu.RUnlock()
			res.body = out
			return
		}
		s.restError(res, 404, "Not Found")
		return
	}
	num, err := strconv.Atoi(parts[0])
	if err != nil || num <= 0 {
		s.restError(res, 404, "Not Found")
		return
	}
	if len(parts) == 1 {
		if req.method == "GET" {
			s.store.mu.RLock()
			pr := r.PullByNum[num]
			if pr == nil {
				s.store.mu.RUnlock()
				s.restError(res, 404, "Not Found")
				return
			}
			rendered := s.renderPull(r, pr)
			s.store.mu.RUnlock()
			res.body = rendered
			return
		}
		s.restError(res, 404, "Not Found")
		return
	}
	if len(parts) == 2 && parts[1] == "files" && req.method == "GET" {
		s.store.mu.RLock()
		pr := r.PullByNum[num]
		if pr == nil {
			s.store.mu.RUnlock()
			s.restError(res, 404, "Not Found")
			return
		}
		diff := diffTrees(r, pr.BaseTree, pr.HeadTree)
		baseFlat := treeFlat(r, pr.BaseTree)
		headFlat := treeFlat(r, pr.HeadTree)
		paths := make([]string, 0, len(diff))
		for p := range diff {
			paths = append(paths, p)
		}
		for i := 0; i < len(paths); i++ {
			for j := i + 1; j < len(paths); j++ {
				if paths[j] < paths[i] {
					paths[i], paths[j] = paths[j], paths[i]
				}
			}
		}
		bp := getBuf()
		b := *bp
		b = append(b, '[')
		for i, p := range paths {
			if i > 0 {
				b = append(b, ',')
			}
			status := diff[p]
			add, del := 0, 0
			var oldData, newData []byte
			if be, ok := baseFlat[p]; ok {
				if bo := r.Objects[be.OID]; bo != nil {
					oldData = bo.Data
				}
			}
			if he, ok := headFlat[p]; ok {
				if ho := r.Objects[he.OID]; ho != nil {
					newData = ho.Data
				}
			}
			add, del = lineDiff(oldData, newData)
			b = append(b, `{"filename":`...)
			b = jsonEscape(b, p)
			b = append(b, `,"status":`...)
			b = jsonEscape(b, status)
			b = append(b, `,"additions":`...)
			b = strconv.AppendInt(b, int64(add), 10)
			b = append(b, `,"deletions":`...)
			b = strconv.AppendInt(b, int64(del), 10)
			b = append(b, `,"changes":`...)
			b = strconv.AppendInt(b, int64(add+del), 10)
			b = append(b, `}`...)
		}
		b = append(b, ']')
		out := make([]byte, len(b))
		copy(out, b)
		putBuf(bp)
		s.store.mu.RUnlock()
		res.body = out
		return
	}
	if len(parts) == 2 && parts[1] == "merge" && req.method == "PUT" {
		if len(req.body) > 0 && !validJSON(req.body) {
			s.restError(res, 400, "Invalid JSON")
			return
		}
		s.store.mu.Lock()
		pr := r.PullByNum[num]
		if pr == nil {
			s.store.mu.Unlock()
			s.restError(res, 404, "Not Found")
			return
		}
		if pr.Merged || pr.State == "closed" {
			s.store.mu.Unlock()
			s.restError(res, 405, "Pull request already merged or closed")
			return
		}
		// recompute conflict at merge time (base may have moved)
		pr.mergeConflict = s.pullConflict(r, pr)
		if pr.mergeConflict {
			s.store.mu.Unlock()
			s.restError(res, 405, "Merge conflict")
			return
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
		bp := getBuf()
		b := *bp
		b = append(b, `{"sha":`...)
		b = jsonEscape(b, mergeSHA)
		b = append(b, `,"merged":true,"message":`...)
		b = jsonEscape(b, "Pull request successfully merged")
		b = append(b, `}`...)
		out := make([]byte, len(b))
		copy(out, b)
		putBuf(bp)
		res.body = out
		return
	}
	s.restError(res, 404, "Not Found")
}
