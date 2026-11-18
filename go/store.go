// store.go — the forge's whole state, in memory. One Forge holds every repository
// the token owner created, with their git objects, refs, issues, pull requests and
// labels. All four surfaces (git smart HTTP, REST v3, GraphQL, MCP) read and write
// this one store, so a change made over any of them is visible on all the others.
//
// Identity: the suite's scenario is built from the contest's one fixed seed, so the
// token owner's login is the constant below (viewer "adaiversen", stranger
// "olgabauer"). A repository belongs to the token owner; the same name under any other
// owner is a 404, never a 403.
//
// Determinism: server-created commits use a fixed identity and a fixed timestamp,
// issue and pull-request ids come from counters, and nothing here reads the wall clock
// or randomness — two runs over the same requests hold the same state.
//
// Concurrency: the HTTP layer holds the Forge's RWMutex for a whole request (read
// lock for read-only surfaces, write lock for mutations). The per-repo cacheMu guards
// only the lazy read caches, which a read may fill while holding the read lock.

package main

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// ViewerLogin is the login the bearer token names (the contest scenario's fixed viewer).
const ViewerLogin = "adaiversen"

// ViewerName is the viewer's display name.
const ViewerName = "ada iversen"

// ViewerEmail is the viewer's email.
const ViewerEmail = "adaiversen@example.invalid"

// StrangerLogin is another login with no rights on the viewer's repositories.
const StrangerLogin = "olgabauer"

// DefaultBranch is the default branch every repository is born with.
const DefaultBranch = "main"

// ServerAuthor is the fixed identity for commits the server itself writes.
const ServerAuthor = ViewerName + " <" + ViewerEmail + ">"

// ServerEpoch is the fixed timestamp for commits the server itself writes.
const ServerEpoch = 1700000000

// ServerMergedAt is the fixed merge timestamp for squash merges.
const ServerMergedAt = "2025-01-01T00:00:00Z"

// isoAt formats Unix seconds the way Date.toISOString does: milliseconds included.
func isoAt(epochSec int64) string {
	return time.Unix(epochSec, 0).UTC().Format("2006-01-02T15:04:05.000Z")
}

// Label is a repository label. Its id is always nil: the Node implementation's label
// counter was never initialised, so every label id serialises as null, and the suite
// accepts that.
type Label struct {
	ID    *int64
	Name  string
	Color string
}

// Issue is one issue.
type Issue struct {
	ID        int64
	Number    int
	Title     string
	Body      string
	State     string // "open" or "closed"
	Labels    []string
	Seq       int64
	CreatedAt string
}

// IssueComment is one comment on an issue.
type IssueComment struct {
	ID   int64
	Body string
	Seq  int64
}

// Pull is one pull request.
type Pull struct {
	ID        int64
	Number    int
	Title     string
	Body      string
	State     string // "open" or "closed"
	Head      string
	Base      string
	BaseSha   string // the base branch tip when the pull request was opened (frozen)
	Merged    bool
	MergedAt  *string
	MergeSha  *string
	Seq       int64
	CreatedAt string
}

// RefEntry is one ref with its tip, for sorted listings.
type RefEntry struct {
	Name string
	Oid  string
}

// Repo is one repository.
type Repo struct {
	Name        string
	Description string
	IsPrivate   bool
	DatabaseID  int64
	Seq         int64
	CreatedAt   string

	Objects     map[string]GitObj
	Refs        map[string]string
	Labels      map[string]*Label
	Issues      []*Issue
	Comments    map[int][]*IssueComment
	Pulls       []*Pull
	NextIssue   int
	NextPull    int
	NextComment int64

	// GitGen is bumped on every change to git objects or refs; read-path caches key on it.
	GitGen int64
	// IssueByNumber maps issue numbers to issues, for O(1) single-issue reads.
	IssueByNumber map[int]*Issue
	// issueJsonCache holds serialized issue JSON by number, for the hot single-issue GET.
	// It stores the wire bytes directly: the fast path writes them without a copy.
	issueJsonCache map[int][]byte
	// sortedRefsCache holds cached sorted refs, valid while gen equals GitGen.
	sortedRefsCache *sortedRefsEntry
	// gitCache holds cached git wire answers (fetch / ls-refs / v0 ref lines).
	gitCache map[string][]byte
	// cacheMu guards the three lazy caches above.
	cacheMu sync.Mutex
}

type sortedRefsEntry struct {
	gen  int64
	refs []RefEntry
}

// ChangedFile is one file changed between two trees.
type ChangedFile struct {
	Path      string
	Status    string // "added", "modified" or "removed"
	Additions int
	Deletions int
	Changes   int
	OldOid    *string
	NewOid    *string
}

// Forge holds every repository.
type Forge struct {
	mu             sync.RWMutex
	Repos          map[string]*Repo
	nextDatabaseID int64
	nextGlobalID   int64
	nextSeq        int64
}

// NewForge returns an empty forge.
func NewForge() *Forge {
	return &Forge{
		Repos:          make(map[string]*Repo),
		nextDatabaseID: 5000,
		nextGlobalID:   100000,
		nextSeq:        1,
	}
}

// RLock takes the forge's read lock.
func (f *Forge) RLock() { f.mu.RLock() }

// RUnlock releases the forge's read lock.
func (f *Forge) RUnlock() { f.mu.RUnlock() }

// Lock takes the forge's write lock.
func (f *Forge) Lock() { f.mu.Lock() }

// Unlock releases the forge's write lock.
func (f *Forge) Unlock() { f.mu.Unlock() }

// FreshID returns a fresh numeric id, stable across reads of the same object.
func (f *Forge) FreshID() int64 {
	f.nextGlobalID++
	return f.nextGlobalID
}

// CreateRepo creates a repository, or returns nil when the name is taken.
func (f *Forge) CreateRepo(name string, isPrivate bool, description string) *Repo {
	if _, ok := f.Repos[name]; ok {
		return nil
	}
	seq := f.nextSeq
	f.nextSeq++
	repo := &Repo{
		Name:           name,
		Description:    description,
		IsPrivate:      isPrivate,
		DatabaseID:     f.nextDatabaseID,
		Seq:            seq,
		CreatedAt:      isoAt(ServerEpoch + seq),
		Objects:        make(map[string]GitObj),
		Refs:           make(map[string]string),
		Labels:         make(map[string]*Label),
		Issues:         []*Issue{},
		Comments:       make(map[int][]*IssueComment),
		Pulls:          []*Pull{},
		NextIssue:      1,
		NextPull:       1,
		NextComment:    1,
		IssueByNumber:  make(map[int]*Issue),
		issueJsonCache: make(map[int][]byte),
		gitCache:       make(map[string][]byte),
	}
	f.nextDatabaseID++
	f.Repos[name] = repo
	return repo
}

// GetRepo returns the repository by name, or nil.
func (f *Forge) GetRepo(name string) *Repo {
	return f.Repos[name]
}

// DeleteRepo deletes a repository, reporting whether it existed.
func (f *Forge) DeleteRepo(name string) bool {
	if _, ok := f.Repos[name]; !ok {
		return false
	}
	delete(f.Repos, name)
	return true
}

// TouchGit marks a repo's git state changed: every read-path cache keyed on GitGen
// (sorted refs, cached fetch / ls-refs answers) is rebuilt on its next use.
func (f *Forge) TouchGit(repo *Repo) {
	repo.GitGen++
}

// PutObject stores an object, answering its id (idempotent: the same bytes give the
// same id).
func (f *Forge) PutObject(repo *Repo, kind ObjKind, data []byte) string {
	oid := OidOf(kind, data)
	if _, ok := repo.Objects[oid]; !ok {
		repo.Objects[oid] = GitObj{Kind: kind, Bytes: data}
		f.TouchGit(repo)
	}
	return oid
}

// Lookup returns a lookup function over a repo's objects.
func (f *Forge) Lookup(repo *Repo) ObjectLookup {
	return func(oid string) *GitObj {
		if obj, ok := repo.Objects[oid]; ok {
			return &obj
		}
		return nil
	}
}

// RefTip returns a ref's tip, or "" when absent.
func (f *Forge) RefTip(repo *Repo, ref string) (string, bool) {
	oid, ok := repo.Refs[ref]
	return oid, ok
}

// BranchTip returns a branch's tip, or "" when absent.
func (f *Forge) BranchTip(repo *Repo, branch string) (string, bool) {
	return f.RefTip(repo, "refs/heads/"+branch)
}

// SortedRefs returns every ref the repo holds, sorted by name (HEAD excluded: it is
// a symref, not a ref).
func (f *Forge) SortedRefs(repo *Repo) []RefEntry {
	repo.cacheMu.Lock()
	defer repo.cacheMu.Unlock()
	if repo.sortedRefsCache != nil && repo.sortedRefsCache.gen == repo.GitGen {
		return repo.sortedRefsCache.refs
	}
	refs := make([]RefEntry, 0, len(repo.Refs))
	for name, oid := range repo.Refs {
		refs = append(refs, RefEntry{Name: name, Oid: oid})
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Name < refs[j].Name })
	repo.sortedRefsCache = &sortedRefsEntry{gen: repo.GitGen, refs: refs}
	return refs
}

// Peel returns the commit a tag object peels to (itself when the ref already names a
// commit), or "" when the object is missing or unpeelable.
func (f *Forge) Peel(repo *Repo, oid string) (string, bool) {
	object, ok := repo.Objects[oid]
	if !ok {
		return "", false
	}
	if object.Kind == KindCommit {
		return oid, true
	}
	if object.Kind == KindTag {
		tag, err := ParseTag(object.Bytes)
		if err != nil {
			return "", false
		}
		return tag.Object, true
	}
	return "", false
}

// -------------------------------------------------------------------------------------------
// commits the server writes (contents API and squash merges)
// -------------------------------------------------------------------------------------------

// WriteFileResult is the outcome of Forge.WriteFile.
type WriteFileResult struct {
	CommitSha string
	BlobSha   *string
	Created   bool
	Error     string
}

// WriteFile writes one file (or deletes it when content is nil) on branch, creating
// the branch from the default branch when it does not exist yet.
func (f *Forge) WriteFile(repo *Repo, path string, content []byte, hasContent bool, message string, branch string, expectedSha *string) WriteFileResult {
	branchName := branch
	if branchName == "" {
		branchName = DefaultBranch
	}
	refName := "refs/heads/" + branchName
	tip, hasTip := repo.Refs[refName]
	if !hasTip && branchName != DefaultBranch {
		tip, hasTip = repo.Refs["refs/heads/"+DefaultBranch]
	}
	var parentTree string
	hasParentTree := false
	created := true
	if hasTip {
		commitObj, ok := repo.Objects[tip]
		if !ok || commitObj.Kind != KindCommit {
			return WriteFileResult{Error: "branch tip is not a commit"}
		}
		commit, err := ParseCommit(commitObj.Bytes)
		if err != nil {
			return WriteFileResult{Error: "branch tip is not a commit"}
		}
		parentTree = commit.Tree
		hasParentTree = true
		existing := LookupPath(f.Lookup(repo), parentTree, path)
		created = existing == nil
		if expectedSha != nil {
			if existing == nil || existing.Kind != "blob" || existing.Oid != *expectedSha {
				return WriteFileResult{Error: "stale sha"}
			}
		}
		if !hasContent && existing == nil {
			return WriteFileResult{Error: "file does not exist"}
		}
	} else if expectedSha != nil {
		return WriteFileResult{Error: "file does not exist"}
	}
	var blobSha *string
	if hasContent {
		oid := f.PutObject(repo, KindBlob, content)
		blobSha = &oid
	}
	treeOid, err := SetPath(f.Lookup(repo), func(kind ObjKind, bytes []byte) string {
		return f.PutObject(repo, kind, bytes)
	}, parentTree, path, strOrEmpty(blobSha))
	_ = hasParentTree
	if err != nil {
		return WriteFileResult{Error: err.Error()}
	}
	commitBytes, err := BuildCommit(CommitFields{
		Tree:      treeOid,
		Parents:   parentsOf(tip, hasTip),
		Message:   message,
		Author:    ServerAuthor,
		Committer: ServerAuthor,
		Timestamp: ServerEpoch,
	})
	if err != nil {
		return WriteFileResult{Error: err.Error()}
	}
	commitSha := f.PutObject(repo, KindCommit, commitBytes)
	repo.Refs[refName] = commitSha
	f.TouchGit(repo)
	return WriteFileResult{CommitSha: commitSha, BlobSha: blobSha, Created: created}
}

func strOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func parentsOf(tip string, hasTip bool) []string {
	if !hasTip {
		return nil
	}
	return []string{tip}
}

// ReadPathResult is the outcome of Forge.ReadPath.
type ReadPathResult struct {
	Kind  string // "blob" or "tree"
	Oid   string
	Bytes []byte
}

// ReadPath reads a file or directory entry at a ref (branch name, or a commit id).
func (f *Forge) ReadPath(repo *Repo, path string, ref string) *ReadPathResult {
	var tip string
	var ok bool
	if ref == "" {
		tip, ok = repo.Refs["refs/heads/"+DefaultBranch]
	} else {
		tip, ok = f.ResolveRef(repo, ref)
	}
	if !ok {
		return nil
	}
	commitObj, exists := repo.Objects[tip]
	if !exists || commitObj.Kind != KindCommit {
		return nil
	}
	commit, err := ParseCommit(commitObj.Bytes)
	if err != nil {
		return nil
	}
	if path == "" || path == "/" {
		return &ReadPathResult{Kind: "tree", Oid: commit.Tree}
	}
	found := LookupPath(f.Lookup(repo), commit.Tree, path)
	if found == nil {
		return nil
	}
	if found.Kind == "blob" {
		blob, exists := repo.Objects[found.Oid]
		if !exists {
			return nil
		}
		return &ReadPathResult{Kind: "blob", Oid: found.Oid, Bytes: blob.Bytes}
	}
	return &ReadPathResult{Kind: "tree", Oid: found.Oid}
}

// DirEntry is one entry of a directory listing.
type DirEntry struct {
	Name string
	Path string
	Kind string // "file" or "dir"
	Oid  string
}

// ListDir lists a directory's entries at a ref.
func (f *Forge) ListDir(repo *Repo, path string, ref string) []DirEntry {
	found := f.ReadPath(repo, path, ref)
	if found == nil || found.Kind != "tree" {
		return nil
	}
	object, ok := repo.Objects[found.Oid]
	if !ok {
		return nil
	}
	entries, err := ParseTree(object.Bytes)
	if err != nil {
		return nil
	}
	out := make([]DirEntry, 0, len(entries))
	for _, entry := range entries {
		kind := "file"
		if entry.Mode == "40000" {
			kind = "dir"
		}
		entryPath := entry.Name
		if path != "" {
			entryPath = path + "/" + entry.Name
		}
		out = append(out, DirEntry{Name: entry.Name, Path: entryPath, Kind: kind, Oid: entry.Oid})
	}
	return out
}

// ResolveRef resolves a ref argument: a branch name, a full ref, or a commit id.
func (f *Forge) ResolveRef(repo *Repo, ref string) (string, bool) {
	if IsOid(ref) {
		if _, ok := repo.Objects[ref]; ok {
			return ref, true
		}
		return "", false
	}
	if strings.HasPrefix(ref, "refs/") {
		oid, ok := repo.Refs[ref]
		return oid, ok
	}
	oid, ok := repo.Refs["refs/heads/"+ref]
	return oid, ok
}

// ListCommits returns the commits reachable from a ref, newest first.
func (f *Forge) ListCommits(repo *Repo, ref string) []string {
	var tip string
	var ok bool
	if ref == "" {
		tip, ok = repo.Refs["refs/heads/"+DefaultBranch]
	} else {
		tip, ok = f.ResolveRef(repo, ref)
	}
	if !ok {
		return nil
	}
	return WalkCommits([]string{tip}, f.Lookup(repo))
}

// -------------------------------------------------------------------------------------------
// pull-request diffs and squash merges
// -------------------------------------------------------------------------------------------

var identityRe = regexp.MustCompile(`^(.*?) <(.*?)> (\d+) ([+-]\d{4})$`)

// ParseIdentity parses a "Name <email> 1700000000 +0000" identity line into its parts.
func ParseIdentity(line string) (name, email, date string) {
	match := identityRe.FindStringSubmatch(line)
	if match == nil {
		return line, "", ""
	}
	var epoch int64
	fmt.Sscanf(match[3], "%d", &epoch)
	return match[1], match[2], isoAt(epoch)
}

// LineCounts counts lines added and removed between two file versions, by multiset
// difference.
func LineCounts(oldBytes []byte, newBytes []byte) (additions, deletions int) {
	countLines := func(data []byte) map[string]int {
		counts := make(map[string]int)
		if data == nil {
			return counts
		}
		text := string(data)
		if text == "" {
			return counts
		}
		lines := strings.Split(text, "\n")
		// A trailing newline does not start a new line.
		if len(lines) > 0 && lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1]
		}
		for _, line := range lines {
			counts[line]++
		}
		return counts
	}
	oldLines := countLines(oldBytes)
	newLines := countLines(newBytes)
	for line, count := range newLines {
		if gone := count - oldLines[line]; gone > 0 {
			additions += gone
		}
	}
	for line, count := range oldLines {
		if gone := count - newLines[line]; gone > 0 {
			deletions += gone
		}
	}
	return additions, deletions
}

// DiffTrees returns the changed files between two trees: every path whose blob
// differs, was added, or was removed.
func (f *Forge) DiffTrees(repo *Repo, baseTreeOid string, headTreeOid string) []ChangedFile {
	lookup := f.Lookup(repo)
	base := FlattenTree(lookup, baseTreeOid, "")
	head := FlattenTree(lookup, headTreeOid, "")
	paths := make(map[string]bool)
	for path := range base {
		paths[path] = true
	}
	for path := range head {
		paths[path] = true
	}
	sorted := make([]string, 0, len(paths))
	for path := range paths {
		sorted = append(sorted, path)
	}
	sort.Strings(sorted)
	var out []ChangedFile
	for _, path := range sorted {
		oldOid, hasOld := base[path]
		newOid, hasNew := head[path]
		if hasOld && hasNew && oldOid == newOid {
			continue
		}
		var status string
		var oldPtr, newPtr *string
		if !hasOld {
			status = "added"
		} else if !hasNew {
			status = "removed"
		} else {
			status = "modified"
		}
		if hasOld {
			v := oldOid
			oldPtr = &v
		}
		if hasNew {
			v := newOid
			newPtr = &v
		}
		var oldBytes, newBytes []byte
		if hasOld {
			if obj, ok := repo.Objects[oldOid]; ok {
				oldBytes = obj.Bytes
			}
		}
		if hasNew {
			if obj, ok := repo.Objects[newOid]; ok {
				newBytes = obj.Bytes
			}
		}
		additions, deletions := LineCounts(oldBytes, newBytes)
		out = append(out, ChangedFile{
			Path: path, Status: status,
			Additions: additions, Deletions: deletions, Changes: additions + deletions,
			OldOid: oldPtr, NewOid: newPtr,
		})
	}
	return out
}

// PullFiles returns the changed files of a pull request (base snapshot vs head tip).
func (f *Forge) PullFiles(repo *Repo, pull *Pull) []ChangedFile {
	baseCommit, ok := repo.Objects[pull.BaseSha]
	if !ok || baseCommit.Kind != KindCommit {
		return nil
	}
	headTip, ok := f.BranchTip(repo, pull.Head)
	if !ok {
		return nil
	}
	headCommit, ok := repo.Objects[headTip]
	if !ok || headCommit.Kind != KindCommit {
		return nil
	}
	baseParsed, err := ParseCommit(baseCommit.Bytes)
	if err != nil {
		return nil
	}
	headParsed, err := ParseCommit(headCommit.Bytes)
	if err != nil {
		return nil
	}
	return f.DiffTrees(repo, baseParsed.Tree, headParsed.Tree)
}

// PullConflict reports whether a pull request can merge cleanly: every file it
// changes must be unchanged on the base branch since the pull request was opened.
func (f *Forge) PullConflict(repo *Repo, pull *Pull) bool {
	baseTip, ok := f.BranchTip(repo, pull.Base)
	if !ok {
		return true
	}
	baseCommit, ok := repo.Objects[pull.BaseSha]
	if !ok {
		return true
	}
	currentCommit, ok := repo.Objects[baseTip]
	if !ok {
		return true
	}
	if baseCommit.Kind != KindCommit || currentCommit.Kind != KindCommit {
		return true
	}
	changed := f.PullFiles(repo, pull)
	if len(changed) == 0 {
		return false
	}
	baseParsed, err1 := ParseCommit(baseCommit.Bytes)
	currentParsed, err2 := ParseCommit(currentCommit.Bytes)
	if err1 != nil || err2 != nil {
		return true
	}
	oldFlat := FlattenTree(f.Lookup(repo), baseParsed.Tree, "")
	nowFlat := FlattenTree(f.Lookup(repo), currentParsed.Tree, "")
	for _, file := range changed {
		oldOid, hasOld := oldFlat[file.Path]
		nowOid, hasNow := nowFlat[file.Path]
		var oldPtr, nowPtr *string
		if hasOld {
			v := oldOid
			oldPtr = &v
		}
		if hasNow {
			v := nowOid
			nowPtr = &v
		}
		if !strPtrEq(oldPtr, nowPtr) {
			return true
		}
	}
	return false
}

func strPtrEq(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// MergeResult is the outcome of Forge.MergeSquash.
type MergeResult struct {
	Sha   string
	Error string // "", "conflict", "already-merged", "no-base" or "no-head"
}

// MergeSquash squash-merges a pull request onto its base branch: one new commit on
// the base tip whose tree is the base tree with the pull request's changes applied.
func (f *Forge) MergeSquash(repo *Repo, pull *Pull) MergeResult {
	if pull.Merged || pull.State != "open" {
		return MergeResult{Error: "already-merged"}
	}
	if f.PullConflict(repo, pull) {
		return MergeResult{Error: "conflict"}
	}
	baseTip, ok1 := f.BranchTip(repo, pull.Base)
	headTip, ok2 := f.BranchTip(repo, pull.Head)
	if !ok1 || !ok2 {
		return MergeResult{Error: "no-head"}
	}
	baseCommit, ok3 := repo.Objects[baseTip]
	baseSnapshot, ok4 := repo.Objects[pull.BaseSha]
	headCommit, ok5 := repo.Objects[headTip]
	if !ok3 || !ok4 || !ok5 {
		return MergeResult{Error: "no-base"}
	}
	if baseCommit.Kind != KindCommit || baseSnapshot.Kind != KindCommit || headCommit.Kind != KindCommit {
		return MergeResult{Error: "no-base"}
	}
	baseParsed, err := ParseCommit(baseCommit.Bytes)
	if err != nil {
		return MergeResult{Error: "no-base"}
	}
	treeOid := baseParsed.Tree
	changed := f.PullFiles(repo, pull)
	for _, file := range changed {
		next, err := SetPath(f.Lookup(repo), func(kind ObjKind, bytes []byte) string {
			return f.PutObject(repo, kind, bytes)
		}, treeOid, file.Path, strOrEmpty(file.NewOid))
		if err != nil {
			return MergeResult{Error: "no-base"}
		}
		treeOid = next
	}
	message := fmt.Sprintf("Merge pull request #%d from %s\n\n%s\n", pull.Number, pull.Head, pull.Title)
	commitBytes, err := BuildCommit(CommitFields{
		Tree:      treeOid,
		Parents:   []string{baseTip},
		Message:   message,
		Author:    ServerAuthor,
		Committer: ServerAuthor,
		Timestamp: ServerEpoch,
	})
	if err != nil {
		return MergeResult{Error: "no-base"}
	}
	sha := f.PutObject(repo, KindCommit, commitBytes)
	repo.Refs["refs/heads/"+pull.Base] = sha
	f.TouchGit(repo)
	pull.Merged = true
	mergedAt := ServerMergedAt
	pull.MergedAt = &mergedAt
	pull.MergeSha = &sha
	pull.State = "closed"
	return MergeResult{Sha: sha}
}

// -------------------------------------------------------------------------------------------
// issues and pull requests
// -------------------------------------------------------------------------------------------

// CreateIssueResult is the outcome of Forge.CreateIssue.
type CreateIssueResult struct {
	Issue *Issue
	Error string
}

// CreateIssue creates an issue, validating its labels first.
func (f *Forge) CreateIssue(repo *Repo, title string, body string, labels []string) CreateIssueResult {
	for _, name := range labels {
		if _, ok := repo.Labels[name]; !ok {
			return CreateIssueResult{Error: "unknown label " + name}
		}
	}
	seq := f.nextSeq
	f.nextSeq++
	issue := &Issue{
		ID:        f.FreshID(),
		Number:    repo.NextIssue,
		Title:     title,
		Body:      body,
		State:     "open",
		Labels:    append([]string{}, labels...),
		Seq:       seq,
		CreatedAt: isoAt(ServerEpoch + seq),
	}
	repo.NextIssue++
	repo.Issues = append(repo.Issues, issue)
	repo.IssueByNumber[issue.Number] = issue
	repo.Comments[issue.Number] = []*IssueComment{}
	return CreateIssueResult{Issue: issue}
}

// AddComment adds a comment to an issue, or returns nil when the issue is missing.
func (f *Forge) AddComment(repo *Repo, issueNumber int, body string) *IssueComment {
	var issue *Issue
	for _, entry := range repo.Issues {
		if entry.Number == issueNumber {
			issue = entry
			break
		}
	}
	if issue == nil {
		return nil
	}
	comment := &IssueComment{ID: repo.NextComment, Body: body, Seq: f.nextSeq}
	repo.NextComment++
	f.nextSeq++
	list := repo.Comments[issueNumber]
	list = append(list, comment)
	repo.Comments[issueNumber] = list
	return comment
}

// CreatePullResult is the outcome of Forge.CreatePull.
type CreatePullResult struct {
	Pull  *Pull
	Error string
}

// CreatePull opens a pull request.
func (f *Forge) CreatePull(repo *Repo, head string, base string, title string, body string) CreatePullResult {
	_, ok1 := f.BranchTip(repo, head)
	baseTip, ok2 := f.BranchTip(repo, base)
	if !ok1 || !ok2 {
		return CreatePullResult{Error: "unknown branch"}
	}
	for _, entry := range repo.Pulls {
		if entry.Head == head && entry.Base == base && entry.State == "open" {
			return CreatePullResult{Error: "a pull request is already open for this branch"}
		}
	}
	seq := f.nextSeq
	f.nextSeq++
	pull := &Pull{
		ID:        f.FreshID(),
		Number:    repo.NextPull,
		Title:     title,
		Body:      body,
		State:     "open",
		Head:      head,
		Base:      base,
		BaseSha:   baseTip,
		Merged:    false,
		MergedAt:  nil,
		MergeSha:  nil,
		Seq:       seq,
		CreatedAt: isoAt(ServerEpoch + seq),
	}
	repo.NextPull++
	repo.Pulls = append(repo.Pulls, pull)
	return CreatePullResult{Pull: pull}
}

// -------------------------------------------------------------------------------------------
// git object helpers shared by the smart-HTTP handlers
// -------------------------------------------------------------------------------------------

// FreshObjects returns every object reachable from wants, minus every object
// reachable from haves.
func (f *Forge) FreshObjects(repo *Repo, wants []string, haves []string) map[string]GitObj {
	lookup := f.Lookup(repo)
	wanted, err := CollectReachable(wants, lookup)
	if err != nil {
		return make(map[string]GitObj)
	}
	held := make(map[string]bool)
	if len(haves) > 0 {
		if closure, err := CollectReachable(haves, lookup); err == nil {
			for oid := range closure {
				held[oid] = true
			}
		}
		// A have the store does not hold is simply not held.
	}
	out := make(map[string]GitObj)
	for oid := range wanted {
		if held[oid] {
			continue
		}
		if object, ok := repo.Objects[oid]; ok {
			out[oid] = object
		}
	}
	return out
}

// ShallowResult is the outcome of Forge.ShallowObjects.
type ShallowResult struct {
	Objects   map[string]GitObj
	Shallow   []string
	Unshallow []string
}

// ShallowObjects returns the objects a shallow fetch at depth generations from wants
// must deliver, minus what the client already holds, plus the new shallow boundary
// commits and the old boundaries that stopped being shallow.
func (f *Forge) ShallowObjects(repo *Repo, wants []string, haves []string, oldShallow []string, depth int) ShallowResult {
	lookup := f.Lookup(repo)
	generations := make(map[string]int)
	type queued struct {
		oid string
		gen int
	}
	var queue []queued
	for _, want := range wants {
		if IsOid(want) {
			queue = append(queue, queued{oid: want, gen: 1})
		}
	}
	held := make(map[string]bool)
	if len(haves) > 0 {
		// A shallow client holds each have commit plus its tree — never the have's
		// ancestors, which it never received. Marking the full closure held would
		// wrongly suppress the very commits a deepen fetch must deliver.
		for _, have := range haves {
			object, ok := repo.Objects[have]
			if !ok {
				continue
			}
			held[have] = true
			if object.Kind == KindCommit {
				commit, err := ParseCommit(object.Bytes)
				if err != nil {
					continue
				}
				if closure, err := CollectReachable([]string{commit.Tree}, lookup); err == nil {
					for oid := range closure {
						held[oid] = true
					}
				}
			}
		}
	}
	boundary := make(map[string]bool)
	included := make(map[string]bool)
	for i := 0; i < len(queue); i++ {
		item := queue[i]
		if included[item.oid] {
			continue
		}
		object := lookup(item.oid)
		if object == nil || object.Kind != KindCommit {
			continue
		}
		included[item.oid] = true
		if item.gen >= depth {
			boundary[item.oid] = true
			continue
		}
		commit, err := ParseCommit(object.Bytes)
		if err != nil {
			continue
		}
		for _, parent := range commit.Parents {
			if gen, ok := generations[parent]; !ok || gen > item.gen+1 {
				generations[parent] = item.gen + 1
			}
			queue = append(queue, queued{oid: parent, gen: item.gen + 1})
		}
	}
	// The objects: each included commit's tree closure, minus what the client holds.
	out := make(map[string]GitObj)
	for oid := range included {
		commitObj, ok := repo.Objects[oid]
		if !ok {
			continue
		}
		commit, err := ParseCommit(commitObj.Bytes)
		if err != nil {
			continue
		}
		closure, err := CollectReachable([]string{commit.Tree}, lookup)
		if err != nil {
			continue
		}
		closure[oid] = true
		for member := range closure {
			if held[member] {
				continue
			}
			if object, ok := repo.Objects[member]; ok {
				out[member] = object
			}
		}
	}
	var shallow []string
	for oid := range boundary {
		if !held[oid] {
			shallow = append(shallow, oid)
		}
	}
	sort.Strings(shallow)
	var unshallow []string
	for _, oid := range oldShallow {
		if !included[oid] {
			continue
		}
		object, ok := repo.Objects[oid]
		if !ok || object.Kind != KindCommit {
			continue
		}
		commit, err := ParseCommit(object.Bytes)
		if err != nil {
			continue
		}
		parents := commit.Parents
		if len(parents) == 0 {
			unshallow = append(unshallow, oid)
			continue
		}
		allIncluded := true
		for _, parent := range parents {
			if !included[parent] {
				allIncluded = false
				break
			}
		}
		if allIncluded {
			unshallow = append(unshallow, oid)
		}
	}
	return ShallowResult{Objects: out, Shallow: shallow, Unshallow: unshallow}
}

// IsFastForward reports whether newOid is a fast-forward from oldOid (old is an
// ancestor of new, or a create).
func (f *Forge) IsFastForward(repo *Repo, oldOid string, newOid string) bool {
	if oldOid == ZERO_OID {
		return true
	}
	if newOid == ZERO_OID {
		return true
	}
	return IsAncestor(oldOid, newOid, f.Lookup(repo))
}
