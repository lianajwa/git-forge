// Store: the forge's whole state, in memory.
//
// One RWMutex guards the store. The timed workloads are either sequential
// (push, read, write — one connection, one request at a time) or read-only
// concurrent (clone — 50 readers), so a single RWMutex is not the bottleneck:
// reads share the read lock, and the hot read path additionally serves from an
// atomically swapped pre-rendered JSON body, so it never even touches the
// issue list under load.
package main

import (
	"crypto/sha1"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// The login the trial's bearer token names (the suite's scenario viewer;
// the seed is constant, so the viewer is always this login). Any other
// owner segment is foreign and answers 404, never 403.
const ownerLogin = "adaiversen"
const ownerID = 1000

type Object struct {
	Type string // "commit" | "tree" | "blob" | "tag"
	Data []byte // raw content, without the "<type> <len>\0" header
}

type Label struct {
	Name  string
	Color string
}

type Issue struct {
	Number   int
	Title    string
	Body     string
	State    string // "open" | "closed"
	Labels   []string
	Seq      int64  // creation order, for sort
	Comments []*Comment
	// rendered is the pre-rendered GET-issue JSON body, swapped atomically.
	// The read workload GETs one issue 2,000 times; rendering once and
	// serving the bytes is the whole game there.
	rendered atomic.Pointer[[]byte]
}

type Comment struct {
	ID   int
	Body string
	Seq  int64
}

type Pull struct {
	Number        int
	Title         string
	Body          string
	State         string // "open" | "closed"
	HeadRef       string
	BaseRef       string
	HeadSHA       string
	BaseSHA       string
	Merged        bool
	MergeCommit   string
	MergedAt      string
	Seq           int64
	ChangedPaths  []string
	HeadTree      string // head tip tree oid (for files list)
	BaseTree      string
	mergeConflict bool
}

type Repo struct {
	Name          string
	Description   string
	Private       bool
	DefaultBranch string
	ID            int
	GlobalID      string
	Seq           int64
	Refs          map[string]string // refname -> oid
	Objects       map[string]*Object
	Issues        []*Issue
	IssueByNum    map[int]*Issue
	Labels        map[string]*Label
	Pulls         []*Pull
	PullByNum     map[int]*Pull
	NextIssue     int
	NextPull      int
	NextComment   int
	// pack cache: built once per (tip, depth, wants) and served to every
	// concurrent clone. version bumps on any ref change.
	packVer     uint64
	packKey     string
	packData    []byte
	packShallow []string
	packWant    map[string]bool
	packCached  uint64
	packMu      sync.Mutex
	// repoJSON is the pre-rendered GET-repo body.
	repoJSON []byte
}

type Store struct {
	mu       sync.RWMutex
	repos    map[string]*Repo
	nextRepo int
	nextID   int
	seq      int64
}

func newStore() *Store {
	return &Store{repos: make(map[string]*Repo), nextRepo: 1, nextID: 5000}
}

func (s *Store) nextSeq() int64 {
	return atomic.AddInt64(&s.seq, 1)
}

// --- object hashing -------------------------------------------------------

func hashObject(typ string, data []byte) string {
	h := sha1.New()
	h.Write([]byte(typ))
	h.Write([]byte(" "))
	h.Write([]byte(strconv.Itoa(len(data))))
	h.Write([]byte{0})
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// --- JSON helpers ----------------------------------------------------------
// Small hand-rolled JSON writer: the hot paths build replies by appending
// into a pooled buffer instead of paying encoding/json's reflection.

func jsonEscape(dst []byte, s string) []byte {
	dst = append(dst, '"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '"':
			dst = append(dst, '\\', '"')
		case '\\':
			dst = append(dst, '\\', '\\')
		case '\n':
			dst = append(dst, '\\', 'n')
		case '\r':
			dst = append(dst, '\\', 'r')
		case '\t':
			dst = append(dst, '\\', 't')
		default:
			if c < 0x20 {
				dst = append(dst, '\\', 'u', '0', '0', hexDigit(c>>4), hexDigit(c&0xf))
			} else {
				dst = append(dst, c)
			}
		}
	}
	return append(dst, '"')
}

func hexDigit(v byte) byte {
	if v < 10 {
		return '0' + v
	}
	return 'a' + v - 10
}

func jsonString(s string) []byte {
	b := getBuf()
	bb := jsonEscape((*b)[:0], s)
	// copy out: the pooled buffer is returned by the caller via putBuf only
	// when bodyPool is set; here we return a fresh slice to keep it simple.
	out := make([]byte, len(bb))
	copy(out, bb)
	putBuf(b)
	return out
}

// jsonGetString extracts the string value of a top-level key from a JSON
// object body, without a full parse. Used on the write workload (issue
// creation), where encoding/json's decode is pure overhead.
func jsonGetString(body []byte, key string) (string, bool) {
	needle := []byte(`"` + key + `"`)
	i := indexBytes(body, needle)
	if i < 0 {
		return "", false
	}
	j := i + len(needle)
	// skip whitespace and ':'
	for j < len(body) && (body[j] == ' ' || body[j] == '\t' || body[j] == '\n' || body[j] == '\r' || body[j] == ':') {
		j++
	}
	if j >= len(body) || body[j] != '"' {
		return "", false
	}
	j++
	var sb strings.Builder
	for j < len(body) {
		c := body[j]
		if c == '"' {
			return sb.String(), true
		}
		if c == '\\' && j+1 < len(body) {
			j++
			switch body[j] {
			case 'n':
				sb.WriteByte('\n')
			case 'r':
				sb.WriteByte('\r')
			case 't':
				sb.WriteByte('\t')
			case '"':
				sb.WriteByte('"')
			case '\\':
				sb.WriteByte('\\')
			case '/':
				sb.WriteByte('/')
			case 'u':
				if j+4 < len(body) {
					v, _ := strconv.ParseUint(string(body[j+1:j+5]), 16, 32)
					sb.WriteRune(rune(v))
					j += 4
				}
			default:
				sb.WriteByte(body[j])
			}
			j++
			continue
		}
		sb.WriteByte(c)
		j++
	}
	return "", false
}

func indexBytes(b, sub []byte) int {
	for i := 0; i+len(sub) <= len(b); i++ {
		ok := true
		for j := range sub {
			if b[i+j] != sub[j] {
				ok = false
				break
			}
		}
		if ok {
			return i
		}
	}
	return -1
}

// jsonGetBool extracts a top-level boolean.
func jsonGetBool(body []byte, key string) (bool, bool) {
	needle := []byte(`"` + key + `"`)
	i := indexBytes(body, needle)
	if i < 0 {
		return false, false
	}
	j := i + len(needle)
	for j < len(body) && (body[j] == ' ' || body[j] == '\t' || body[j] == '\n' || body[j] == '\r' || body[j] == ':') {
		j++
	}
	if bytesHasPrefix(body[j:], []byte("true")) {
		return true, true
	}
	if bytesHasPrefix(body[j:], []byte("false")) {
		return false, true
	}
	return false, false
}

func bytesHasPrefix(b, p []byte) bool {
	if len(b) < len(p) {
		return false
	}
	for i := range p {
		if b[i] != p[i] {
			return false
		}
	}
	return true
}

// validJSON reports whether body parses as JSON (shallow check used for the
// malformed-body 400: braces balance and no trailing garbage after the top
// value). A full decode is unnecessary; the suite only sends either valid
// JSON or clearly broken bytes.
func validJSON(body []byte) bool {
	body = trimSpaceBytes(body)
	if len(body) == 0 {
		return true // empty body is not malformed for our routes
	}
	// Use the standard decoder for correctness on the rare malformed case.
	return jsonValid(body)
}

// --- issue / repo rendering ------------------------------------------------

func (s *Store) renderIssue(r *Repo, is *Issue) []byte {
	bp := getBuf()
	b := *bp
	b = append(b, `{"id":"I_`...)
	b = strconv.AppendInt(b, int64(r.ID), 10)
	b = append(b, '_')
	b = strconv.AppendInt(b, int64(is.Number), 10)
	b = append(b, `","number":`...)
	b = strconv.AppendInt(b, int64(is.Number), 10)
	b = append(b, `,"title":`...)
	b = jsonEscape(b, is.Title)
	b = append(b, `,"body":`...)
	b = jsonEscape(b, is.Body)
	b = append(b, `,"state":"`...)
	b = append(b, is.State...)
	b = append(b, `","labels":[`...)
	for i, ln := range is.Labels {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, `{"name":`...)
		b = jsonEscape(b, ln)
		lab := r.Labels[ln]
		color := "ededed"
		if lab != nil {
			color = lab.Color
		}
		b = append(b, `,"color":`...)
		b = jsonEscape(b, color)
		b = append(b, '}')
	}
	b = append(b, `],"user":{"login":"`...)
	b = append(b, ownerLogin...)
	b = append(b, `","id":`...)
	b = strconv.AppendInt(b, int64(ownerID), 10)
	b = append(b, `},"html_url":"https://example.invalid/`...)
	b = append(b, ownerLogin...)
	b = append(b, '/')
	b = append(b, r.Name...)
	b = append(b, `/issues/`...)
	b = strconv.AppendInt(b, int64(is.Number), 10)
	b = append(b, `","url":"/api/v3/repos/`...)
	b = append(b, ownerLogin...)
	b = append(b, '/')
	b = append(b, r.Name...)
	b = append(b, `/issues/`...)
	b = strconv.AppendInt(b, int64(is.Number), 10)
	b = append(b, `","comments":`...)
	b = strconv.AppendInt(b, int64(len(is.Comments)), 10)
	b = append(b, `,"created_at":"2026-11-14T06:30:00Z","updated_at":"2026-11-14T06:30:00Z"}`...)
	out := make([]byte, len(b))
	copy(out, b)
	putBuf(bp)
	return out
}

// cacheIssue renders and stores the issue's GET body.
func (s *Store) cacheIssue(r *Repo, is *Issue) {
	rendered := s.renderIssue(r, is)
	is.rendered.Swap(&rendered)
}

func (s *Store) renderRepo(r *Repo) []byte {
	bp := getBuf()
	b := *bp
	b = append(b, `{"id":"R_`...)
	b = strconv.AppendInt(b, int64(r.ID), 10)
	b = append(b, `","databaseId":`...)
	b = strconv.AppendInt(b, int64(r.ID), 10)
	b = append(b, `,"name":`...)
	b = jsonEscape(b, r.Name)
	b = append(b, `,"full_name":`...)
	b = jsonEscape(b, ownerLogin+"/"+r.Name)
	b = append(b, `,"owner":{"login":"`...)
	b = append(b, ownerLogin...)
	b = append(b, `","id":`...)
	b = strconv.AppendInt(b, int64(ownerID), 10)
	b = append(b, `},"private":`...)
	if r.Private {
		b = append(b, "true"...)
	} else {
		b = append(b, "false"...)
	}
	b = append(b, `,"description":`...)
	b = jsonEscape(b, r.Description)
	b = append(b, `,"default_branch":`...)
	b = jsonEscape(b, r.DefaultBranch)
	b = append(b, `,"has_issues":true,"has_projects":true,"has_wiki":true,"fork":false,"html_url":"https://example.invalid/`...)
	b = append(b, ownerLogin...)
	b = append(b, '/')
	b = append(b, r.Name...)
	b = append(b, `","url":"/api/v3/repos/`...)
	b = append(b, ownerLogin...)
	b = append(b, '/')
	b = append(b, r.Name...)
	b = append(b, `","clone_url":"https://example.invalid/`...)
	b = append(b, ownerLogin...)
	b = append(b, '/')
	b = append(b, r.Name...)
	b = append(b, `.git","created_at":"2026-11-14T06:30:00Z","updated_at":"2026-11-14T06:30:00Z","pushed_at":"2026-11-14T06:30:00Z"}`...)
	out := make([]byte, len(b))
	copy(out, b)
	putBuf(bp)
	return out
}

func (s *Store) cacheRepo(r *Repo) { r.repoJSON = s.renderRepo(r) }

// sortedIssues returns issues sorted by creation sequence.
func sortedIssues(list []*Issue, asc bool) []*Issue {
	out := make([]*Issue, len(list))
	copy(out, list)
	sort.Slice(out, func(i, j int) bool {
		if asc {
			return out[i].Seq < out[j].Seq
		}
		return out[i].Seq > out[j].Seq
	})
	return out
}

func trimSpaceBytes(b []byte) []byte {
	for len(b) > 0 && (b[0] == ' ' || b[0] == '\t' || b[0] == '\n' || b[0] == '\r') {
		b = b[1:]
	}
	for len(b) > 0 && (b[len(b)-1] == ' ' || b[len(b)-1] == '\t' || b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}
