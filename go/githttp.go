// githttp.go — git smart HTTP: info/refs advertisements (protocol v0 and v2),
// upload-pack (v2 ls-refs/fetch, v0 fetch) and receive-pack (push with report-status).
// The wire shapes follow the git smart-HTTP protocol: pkt-line framing, sideband pack
// delivery, shallow/deepen negotiation, and per-ref push reports.

package main

import (
	"bytes"
	"sort"
	"strconv"
	"strings"
)

const (
	uploadAdvertisement  = "application/x-git-upload-pack-advertisement"
	uploadResult         = "application/x-git-upload-pack-result"
	receiveAdvertisement = "application/x-git-receive-pack-advertisement"
	receiveResult        = "application/x-git-receive-pack-result"
)

// GitHTTPResult is one smart-HTTP answer.
type GitHTTPResult struct {
	Status      int
	ContentType string
	Body        []byte
}

func gitOK(contentType string, body []byte) GitHTTPResult {
	return GitHTTPResult{Status: 200, ContentType: contentType, Body: body}
}

func gitFail(status int, message string) GitHTTPResult {
	return GitHTTPResult{Status: status, ContentType: "application/json; charset=utf-8", Body: mustJSON(NewOMap().Set("message", message))}
}

// V0UploadCaps are the capabilities a v0 upload-pack advertisement carries (after
// NUL on the first line).
const V0UploadCaps = "side-band-64k ofs-delta thin-pack multi_ack shallow symref=HEAD:refs/heads/main"

// V0ReceiveCaps are the capabilities a v0 receive-pack advertisement carries.
const V0ReceiveCaps = "report-status side-band-64k delete-refs ofs-delta symref=HEAD:refs/heads/main"

// v2Advertisement is the v2 capability advertisement (no refs: v2 lists them through
// ls-refs). Built once: it never varies.
var v2Advertisement = mustFramePktStrings([]string{"version 2\n", "ls-refs=unborn\n", "fetch=shallow\n", "object-format=sha1\n", "flush"})

func mustFramePktStrings(items []string) []byte {
	out, err := framePktStrings(items)
	if err != nil {
		panic(err)
	}
	return out
}

// The read-path git cache: a repo's cached wire answers, keyed with its GitGen so any
// object or ref change rebuilds them. Bounded (32 entries, 1 MiB each): when full it
// is cleared and rebuilt, which keeps the hot clone keys live while memory stays small.
// Every cached answer is a pure function of the repo's git state and the request's
// arguments, so a hit answers byte-identically to a fresh build.
const gitCacheMaxEntries = 32
const gitCacheMaxBytes = 1048576

func gitCacheGet(repo *Repo, key string) []byte {
	repo.cacheMu.Lock()
	defer repo.cacheMu.Unlock()
	return repo.gitCache[key]
}

func gitCachePut(repo *Repo, key string, value []byte) {
	if len(value) > gitCacheMaxBytes {
		return
	}
	repo.cacheMu.Lock()
	defer repo.cacheMu.Unlock()
	if len(repo.gitCache) >= gitCacheMaxEntries {
		repo.gitCache = make(map[string][]byte)
	}
	repo.gitCache[key] = value
}

// v0RefLines returns one v0 ref line set, the first carrying the capabilities after a NUL.
func v0RefLines(repo *Repo, forge *Forge, caps string) []byte {
	key := "v0:" + itoa(int(repo.GitGen)) + ":" + caps
	if cached := gitCacheGet(repo, key); cached != nil {
		return cached
	}
	refs := forge.SortedRefs(repo)
	var lines []string
	if len(refs) == 0 {
		lines = append(lines, ZERO_OID+" capabilities^{}\x00"+caps+"\n")
		built := mustFramePktStrings(append(lines, "flush"))
		gitCachePut(repo, key, built)
		return built
	}
	defaultTip, hasTip := forge.BranchTip(repo, DefaultBranch)
	headLine := ZERO_OID + " HEAD\x00" + caps + "\n"
	if hasTip {
		headLine = defaultTip + " HEAD\x00" + caps + "\n"
	}
	lines = append(lines, headLine)
	for _, ref := range refs {
		lines = append(lines, ref.Oid+" "+ref.Name+"\n")
		// An annotated tag peels to its commit on a `^{}` line.
		if object, ok := repo.Objects[ref.Oid]; ok && object.Kind == KindTag {
			if peeled, ok := forge.Peel(repo, ref.Oid); ok {
				lines = append(lines, peeled+" "+ref.Name+"^{}\n")
			}
		}
	}
	built := mustFramePktStrings(append(lines, "flush"))
	gitCachePut(repo, key, built)
	return built
}

// InfoRefs answers GET /{owner}/{repo}.git/info/refs?service=git-upload-pack|
// git-receive-pack. The Git-Protocol: version=2 header selects the v2 advertisement
// for upload-pack; receive-pack is always v0.
func InfoRefs(forge *Forge, owner string, repoName string, service string, gitProtocol string) GitHTTPResult {
	if owner != ViewerLogin {
		return gitFail(404, "Not Found")
	}
	repo := forge.GetRepo(repoName)
	if repo == nil {
		return gitFail(404, "Not Found")
	}
	if service == "git-upload-pack" {
		if gitProtocol == "version=2" {
			return gitOK(uploadAdvertisement, v2Advertisement)
		}
		head, _ := FramePktString("# service=git-upload-pack\n")
		body := append(append(append([]byte{}, head...), FrameFlush()...), v0RefLines(repo, forge, V0UploadCaps)...)
		return gitOK(uploadAdvertisement, body)
	}
	if service == "git-receive-pack" {
		head, _ := FramePktString("# service=git-receive-pack\n")
		body := append(append(append([]byte{}, head...), FrameFlush()...), v0RefLines(repo, forge, V0ReceiveCaps)...)
		return gitOK(receiveAdvertisement, body)
	}
	return gitFail(400, "unknown service")
}

// ---------------------------------------------------------------------------------------------
// v2 ls-refs
// ---------------------------------------------------------------------------------------------

// LsRefsArgs are the parsed arguments of a v2 ls-refs request.
type LsRefsArgs struct {
	Peel     bool
	Symrefs  bool
	Unborn   bool
	Prefixes []string
}

func parseLsRefs(lines []string) LsRefsArgs {
	args := LsRefsArgs{}
	for _, line := range lines {
		text := strings.TrimSuffix(line, "\n")
		if text == "" {
			continue
		}
		if strings.HasPrefix(text, "command=") || strings.HasPrefix(text, "agent=") || strings.HasPrefix(text, "object-format=") {
			continue
		}
		switch {
		case text == "peel":
			args.Peel = true
		case text == "symrefs":
			args.Symrefs = true
		case text == "unborn":
			args.Unborn = true
		case strings.HasPrefix(text, "ref-prefix "):
			args.Prefixes = append(args.Prefixes, text[len("ref-prefix "):])
		}
	}
	return args
}

// lsRefsAnswer returns the v2 ls-refs answer: HEAD, every ref, peeled tags, unborn
// HEAD for an empty repo.
func lsRefsAnswer(forge *Forge, repo *Repo, args LsRefsArgs) []byte {
	boolBit := func(b bool) string {
		if b {
			return "1"
		}
		return "0"
	}
	sortedPrefixes := append([]string{}, args.Prefixes...)
	sort.Strings(sortedPrefixes)
	key := "ls:" + itoa(int(repo.GitGen)) + ":" + boolBit(args.Peel) + boolBit(args.Symrefs) + boolBit(args.Unborn) + ":" + strings.Join(sortedPrefixes, ",")
	if cached := gitCacheGet(repo, key); cached != nil {
		return cached
	}
	var lines []string
	keep := func(name string) bool {
		if len(args.Prefixes) == 0 {
			return true
		}
		for _, prefix := range args.Prefixes {
			if strings.HasPrefix(name, prefix) {
				return true
			}
		}
		return false
	}
	refs := forge.SortedRefs(repo)
	defaultTip, hasTip := forge.BranchTip(repo, DefaultBranch)
	if len(refs) == 0 {
		if args.Unborn && keep("HEAD") {
			lines = append(lines, "unborn HEAD symref-target:refs/heads/main\n")
		}
	} else {
		if keep("HEAD") && hasTip {
			lines = append(lines, defaultTip+" HEAD symref-target:refs/heads/main\n")
		}
		for _, ref := range refs {
			if !keep(ref.Name) {
				continue
			}
			line := ref.Oid + " " + ref.Name
			if object, ok := repo.Objects[ref.Oid]; args.Peel && ok && object.Kind == KindTag {
				if peeled, ok := forge.Peel(repo, ref.Oid); ok {
					line += " peeled:" + peeled
				}
			}
			lines = append(lines, line+"\n")
		}
	}
	lines = append(lines, "flush")
	built := mustFramePktStrings(lines)
	gitCachePut(repo, key, built)
	return built
}

// ---------------------------------------------------------------------------------------------
// fetch (v2 and v0 share the pack-building core)
// ---------------------------------------------------------------------------------------------

// FetchArgs are the parsed arguments of a fetch request.
type FetchArgs struct {
	Wants   []string
	Haves   []string
	Shallow []string
	Depth   *int
}

// parseFetchV2 parses the arguments of a v2 fetch request (header lines ignored by prefix).
func parseFetchV2(lines []string) FetchArgs {
	args := FetchArgs{}
	for _, line := range lines {
		text := strings.TrimSuffix(line, "\n")
		switch {
		case strings.HasPrefix(text, "want "):
			args.Wants = append(args.Wants, strings.TrimSpace(text[5:]))
		case strings.HasPrefix(text, "have "):
			args.Haves = append(args.Haves, strings.TrimSpace(text[5:]))
		case strings.HasPrefix(text, "shallow "):
			args.Shallow = append(args.Shallow, strings.TrimSpace(text[8:]))
		case strings.HasPrefix(text, "deepen "):
			depth := atoi(strings.TrimSpace(text[7:]))
			args.Depth = &depth
		}
	}
	return args
}

// parseFetchV0 parses a v0 fetch request (want/have/shallow/deepen across two flushed sections).
func parseFetchV0(lines []string) FetchArgs {
	args := FetchArgs{}
	for _, line := range lines {
		text := strings.TrimSuffix(line, "\n")
		switch {
		case strings.HasPrefix(text, "want "):
			fields := strings.Fields(text[5:])
			if len(fields) > 0 && IsOid(fields[0]) {
				args.Wants = append(args.Wants, fields[0])
			}
		case strings.HasPrefix(text, "have "):
			args.Haves = append(args.Haves, strings.TrimSpace(text[5:]))
		case strings.HasPrefix(text, "shallow "):
			args.Shallow = append(args.Shallow, strings.TrimSpace(text[8:]))
		case strings.HasPrefix(text, "deepen "):
			depth := atoi(strings.TrimSpace(text[7:]))
			args.Depth = &depth
		}
	}
	return args
}

// sidebandPack splits pack bytes into sideband band-1 packets (each well under the
// pkt-line max).
func sidebandPack(pack []byte) []byte {
	var parts [][]byte
	const chunk = 32768
	for at := 0; at < len(pack); at += chunk {
		end := at + chunk
		if end > len(pack) {
			end = len(pack)
		}
		piece := pack[at:end]
		framed, _ := FramePkt(append([]byte{1}, piece...))
		parts = append(parts, framed)
	}
	if len(parts) == 0 {
		framed, _ := FramePkt(append([]byte{1}, pack...))
		parts = append(parts, framed)
	}
	return bytes.Join(parts, nil)
}

// buildFetchPack returns the pack a fetch delivers, with the shallow bookkeeping for
// depth requests.
func buildFetchPack(forge *Forge, repo *Repo, args FetchArgs) (pack []byte, shallow []string, unshallow []string, err error) {
	if args.Depth != nil && *args.Depth > 0 {
		result := forge.ShallowObjects(repo, args.Wants, args.Haves, args.Shallow, *args.Depth)
		objects := make([]GitObj, 0, len(result.Objects))
		for _, object := range result.Objects {
			objects = append(objects, object)
		}
		pack, err = WritePack(objects)
		if err != nil {
			return nil, nil, nil, err
		}
		return pack, result.Shallow, result.Unshallow, nil
	}
	// A plain fetch: the closure of the wants, minus the closure of the haves.
	wanted := make(map[string]bool)
	for _, want := range args.Wants {
		if !IsOid(want) {
			continue
		}
		if _, ok := repo.Objects[want]; !ok {
			continue
		}
		wanted[want] = true
	}
	wantList := make([]string, 0, len(wanted))
	for want := range wanted {
		wantList = append(wantList, want)
	}
	objects := forge.FreshObjects(repo, wantList, args.Haves)
	list := make([]GitObj, 0, len(objects))
	for _, object := range objects {
		list = append(list, object)
	}
	pack, err = WritePack(list)
	if err != nil {
		return nil, nil, nil, err
	}
	return pack, nil, nil, nil
}

// cacheKeyPart renders one fetch argument list for a cache key: empty for none, the
// single value as-is, and a sorted comma-join only when there are several (sorting
// a 0- or 1-element list is a no-op, so the common fetch skips the copy+sort).
func cacheKeyPart(values []string) string {
	switch len(values) {
	case 0:
		return ""
	case 1:
		return values[0]
	default:
		sorted := append([]string{}, values...)
		sort.Strings(sorted)
		return strings.Join(sorted, ",")
	}
}

// fetchCacheKey is the cache key for a fetch answer: the repo's git generation plus
// every argument that shapes it.
func fetchCacheKey(repo *Repo, version int, args FetchArgs) string {
	depth := ""
	if args.Depth != nil {
		depth = itoa(*args.Depth)
	}
	return "fetch" + itoa(version) + ":" + itoa(int(repo.GitGen)) + ":" +
		cacheKeyPart(args.Wants) + "|" + cacheKeyPart(args.Haves) + "|" + cacheKeyPart(args.Shallow) + "|" + depth
}

// fetchV2Answer returns the v2 fetch answer: acknowledgments, shallow-info, packfile sections.
func fetchV2Answer(forge *Forge, repo *Repo, args FetchArgs) []byte {
	key := fetchCacheKey(repo, 2, args)
	if cached := gitCacheGet(repo, key); cached != nil {
		return cached
	}
	pack, shallow, unshallow, err := buildFetchPack(forge, repo, args)
	if err != nil {
		return nil
	}
	var parts [][]byte
	ack, _ := FramePktString("acknowledgments\n")
	parts = append(parts, ack)
	var held []string
	for _, have := range args.Haves {
		if _, ok := repo.Objects[have]; ok {
			held = append(held, have)
		}
	}
	if len(held) == 0 {
		nak, _ := FramePktString("NAK\n")
		parts = append(parts, nak)
	} else {
		for _, have := range held {
			line, _ := FramePktString("ACK " + have + "\n")
			parts = append(parts, line)
		}
		ready, _ := FramePktString("ready\n")
		parts = append(parts, ready)
	}
	parts = append(parts, FrameDelim())
	if len(shallow) > 0 || len(unshallow) > 0 {
		header, _ := FramePktString("shallow-info\n")
		parts = append(parts, header)
		for _, oid := range shallow {
			line, _ := FramePktString("shallow " + oid + "\n")
			parts = append(parts, line)
		}
		for _, oid := range unshallow {
			line, _ := FramePktString("unshallow " + oid + "\n")
			parts = append(parts, line)
		}
		parts = append(parts, FrameDelim())
	}
	packHeader, _ := FramePktString("packfile\n")
	parts = append(parts, packHeader)
	parts = append(parts, sidebandPack(pack))
	parts = append(parts, FrameFlush())
	built := bytes.Join(parts, nil)
	gitCachePut(repo, key, built)
	return built
}

// fetchV0Answer returns the v0 fetch answer: NAK/ACK/shallow lines, then the sideband pack.
func fetchV0Answer(forge *Forge, repo *Repo, args FetchArgs) []byte {
	key := fetchCacheKey(repo, 0, args)
	if cached := gitCacheGet(repo, key); cached != nil {
		return cached
	}
	pack, shallow, unshallow, err := buildFetchPack(forge, repo, args)
	if err != nil {
		return nil
	}
	var parts [][]byte
	var held []string
	for _, have := range args.Haves {
		if _, ok := repo.Objects[have]; ok {
			held = append(held, have)
		}
	}
	if len(held) == 0 {
		nak, _ := FramePktString("NAK\n")
		parts = append(parts, nak)
	} else {
		for _, have := range held {
			line, _ := FramePktString("ACK " + have + "\n")
			parts = append(parts, line)
		}
	}
	for _, oid := range shallow {
		line, _ := FramePktString("shallow " + oid + "\n")
		parts = append(parts, line)
	}
	for _, oid := range unshallow {
		line, _ := FramePktString("unshallow " + oid + "\n")
		parts = append(parts, line)
	}
	parts = append(parts, sidebandPack(pack))
	parts = append(parts, FrameFlush())
	built := bytes.Join(parts, nil)
	gitCachePut(repo, key, built)
	return built
}

// ---------------------------------------------------------------------------------------------
// upload-pack POST (ls-refs / fetch) and receive-pack POST (push)
// ---------------------------------------------------------------------------------------------

// UploadPack answers POST /{owner}/{repo}.git/git-upload-pack. The first line selects
// the shape: a v2 `command=ls-refs` or `command=fetch` request, or a v0 want/have request.
func UploadPack(forge *Forge, owner string, repoName string, body []byte) GitHTTPResult {
	if owner != ViewerLogin {
		return gitFail(404, "Not Found")
	}
	repo := forge.GetRepo(repoName)
	if repo == nil {
		return gitFail(404, "Not Found")
	}
	packets, _, err := DecodePkts(body, false)
	if err != nil {
		return gitFail(400, "malformed pkt-line stream")
	}
	lines := PktLines(packets)
	first := ""
	if len(lines) > 0 {
		first = strings.TrimSuffix(lines[0], "\n")
	}
	if first == "command=ls-refs" {
		return gitOK(uploadResult, lsRefsAnswer(forge, repo, parseLsRefs(lines)))
	}
	if first == "command=fetch" {
		return gitOK(uploadResult, fetchV2Answer(forge, repo, parseFetchV2(lines)))
	}
	// v0: want/have/shallow/deepen lines in two flushed sections.
	return gitOK(uploadResult, fetchV0Answer(forge, repo, parseFetchV0(lines)))
}

// PushCommand is one receive-pack ref update.
type PushCommand struct {
	OldOid string
	NewOid string
	Ref    string
}

// ReceivePack answers POST /{owner}/{repo}.git/git-receive-pack. A pkt-framed command
// list, a flush, then the raw pack bytes. Each command is validated (old tip matches,
// new tip fast-forwards) and the report answers one ok/ng line per ref, inside
// sideband band 1.
func ReceivePack(forge *Forge, owner string, repoName string, body []byte) GitHTTPResult {
	if owner != ViewerLogin {
		return gitFail(404, "Not Found")
	}
	repo := forge.GetRepo(repoName)
	if repo == nil {
		return gitFail(404, "Not Found")
	}
	packets, tail, err := DecodePkts(body, true)
	if err != nil {
		return gitFail(400, "malformed pkt-line stream")
	}
	var commands []PushCommand
	for _, packet := range packets {
		if packet.Kind != PktData {
			continue
		}
		text := strings.TrimSuffix(string(packet.Payload), "\n")
		if nul := strings.IndexByte(text, 0); nul >= 0 {
			text = text[:nul]
		}
		parts := strings.Split(text, " ")
		if len(parts) < 3 || !IsOid(parts[0]) || !IsOid(parts[1]) {
			continue
		}
		commands = append(commands, PushCommand{OldOid: parts[0], NewOid: parts[1], Ref: parts[2]})
	}
	if len(commands) == 0 {
		return gitFail(400, "no push commands")
	}

	// Unpack first: the pack's objects land in the store even when a ref is refused.
	unpackOK := true
	unpackDetail := "ok"
	if len(tail) > 0 {
		read, err := ReadPack(tail, func(oid string) *GitObj {
			if obj, ok := repo.Objects[oid]; ok {
				return &obj
			}
			return nil
		})
		if err != nil {
			unpackOK = false
			unpackDetail = err.Error()
		} else {
			stored := false
			for oid, object := range read.Objects {
				if _, ok := repo.Objects[oid]; !ok {
					repo.Objects[oid] = object
					stored = true
				}
			}
			if stored {
				forge.TouchGit(repo)
			}
		}
	}

	unpackLine := "unpack ok\n"
	if !unpackOK {
		unpackLine = "unpack " + unpackDetail + "\n"
	}
	reportLines := []string{unpackLine}
	for _, command := range commands {
		var verdictOK bool
		var verdictReason string
		decided := false
		switch {
		case !strings.HasPrefix(command.Ref, "refs/"):
			verdictOK, verdictReason, decided = false, "invalid ref", true
		case !unpackOK:
			verdictOK, verdictReason, decided = false, "unpack failed", true
		}
		if !decided {
			current, hasCurrent := repo.Refs[command.Ref]
			if command.NewOid == ZERO_OID {
				// A delete: the old tip must match.
				if !hasCurrent {
					verdictOK, verdictReason = false, "non-fast-forward"
				} else if current != command.OldOid {
					verdictOK, verdictReason = false, "non-fast-forward"
				} else {
					verdictOK = true
				}
			} else {
				target, hasTarget := repo.Objects[command.NewOid]
				switch {
				case !hasTarget || (target.Kind != KindCommit && target.Kind != KindTag):
					verdictOK, verdictReason = false, "object not found"
				case !hasCurrent:
					if command.OldOid == ZERO_OID {
						verdictOK = true
					} else {
						verdictOK, verdictReason = false, "non-fast-forward"
					}
				case command.OldOid == ZERO_OID:
					verdictOK, verdictReason = false, "already exists"
				case current != command.OldOid:
					verdictOK, verdictReason = false, "non-fast-forward"
				case !forge.IsFastForward(repo, command.OldOid, command.NewOid):
					verdictOK, verdictReason = false, "non-fast-forward"
				default:
					verdictOK = true
				}
			}
		}
		if verdictOK {
			if command.NewOid == ZERO_OID {
				delete(repo.Refs, command.Ref)
			} else {
				repo.Refs[command.Ref] = command.NewOid
			}
			forge.TouchGit(repo)
			reportLines = append(reportLines, "ok "+command.Ref+"\n")
		} else {
			reason := verdictReason
			if reason == "" {
				reason = "failed"
			}
			reportLines = append(reportLines, "ng "+command.Ref+" "+reason+"\n")
		}
	}

	reportBody := mustFramePktStrings(reportLines)
	sideband, _ := FramePkt(append([]byte{1}, reportBody...))
	return gitOK(receiveResult, append(append([]byte{}, sideband...), FrameFlush()...))
}

func itoa(v int) string {
	return strconv.Itoa(v)
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}
