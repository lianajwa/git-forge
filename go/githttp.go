// Git smart HTTP: upload-pack (v2 and v0) and receive-pack (v0).
//
// Wire shapes follow the kit's git-client exactly:
//   - info/refs?service=git-upload-pack answers a v2 capability advertisement
//     ("version 2", ls-refs=unborn, fetch=shallow filter, object-format=sha1)
//     behind the "# service=git-upload-pack" preamble.
//   - info/refs?service=git-receive-pack answers a v0 ref advertisement with
//     report-status, side-band-64k, delete-refs and ofs-delta.
//   - upload-pack v2 fetch answers sections: acknowledgments (NAK/ACK),
//     shallow-info, packfile (sideband band 1).
//   - upload-pack v0 fetch answers NAK/ACK and shallow lines, then the pack
//     as sideband.
//   - receive-pack answers the report-status stream inside sideband band 1.
package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"
)

// --- pkt-lines --------------------------------------------------------------

func pktLine(payload []byte) []byte {
	out := make([]byte, 4+len(payload))
	n := len(payload) + 4
	out[0] = hexDigit(byte(n>>12) & 0xf)
	out[1] = hexDigit(byte(n>>8) & 0xf)
	out[2] = hexDigit(byte(n>>4) & 0xf)
	out[3] = hexDigit(byte(n) & 0xf)
	copy(out[4:], payload)
	return out
}

func pktStr(s string) []byte { return pktLine([]byte(s)) }

var pktFlush = []byte("0000")
var pktDelim = []byte("0001")

// pktSideband wraps data as one sideband packet (band 1 = pack data).
func pktSideband(data []byte) []byte {
	payload := make([]byte, 1+len(data))
	payload[0] = 1
	copy(payload[1:], data)
	return pktLine(payload)
}

// parsePktLines splits a request body into data lines. Returns the lines
// (without trailing \n), plus flush/delim markers as empty strings with kind.
type pkt struct {
	kind string // "data" | "flush" | "delim"
	text string
}

func parsePktLines(body []byte) []pkt {
	var out []pkt
	i := 0
	for i+4 <= len(body) {
		n, err := strconv.ParseInt(string(body[i:i+4]), 16, 32)
		if err != nil || n < 0 {
			break
		}
		if n == 0 {
			out = append(out, pkt{kind: "flush"})
			i += 4
			continue
		}
		if n == 1 {
			out = append(out, pkt{kind: "delim"})
			i += 4
			continue
		}
		if i+int(n) > len(body) {
			break
		}
		out = append(out, pkt{kind: "data", text: string(body[i+4 : i+int(n)])})
		i += int(n)
	}
	return out
}

// --- ref advertisement -------------------------------------------------------

// sortedRefs returns ref names sorted, with HEAD first when present.
func sortedRefs(r *Repo) []string {
	names := make([]string, 0, len(r.Refs)+1)
	for name := range r.Refs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// refOID returns the oid of a ref, resolving HEAD to the default branch.
func (r *Repo) refOID(name string) (string, bool) {
	if name == "HEAD" {
		oid, ok := r.Refs["refs/heads/"+r.DefaultBranch]
		return oid, ok
	}
	oid, ok := r.Refs[name]
	return oid, ok
}

// v2Advertisement is the upload-pack v2 capability advertisement body.
func v2Advertisement() []byte {
	var b bytes.Buffer
	b.Write(pktStr("# service=git-upload-pack\n"))
	b.Write(pktFlush)
	b.Write(pktStr("version 2\n"))
	b.Write(pktStr("ls-refs=unborn\n"))
	b.Write(pktStr("fetch=shallow filter\n"))
	b.Write(pktStr("object-format=sha1\n"))
	b.Write(pktStr("agent=forge-v5/1.0\n"))
	b.Write(pktFlush)
	return b.Bytes()
}

// v0Advertisement is a v0 ref advertisement for service (upload-pack or
// receive-pack). capabilities are advertised on the first line after NUL.
func v0Advertisement(r *Repo, service string, caps string) []byte {
	var b bytes.Buffer
	b.Write(pktStr("# service=" + service + "\n"))
	b.Write(pktFlush)
	names := sortedRefs(r)
	headOID, headOK := r.refOID("HEAD")
	if len(names) == 0 {
		// empty repository: capabilities^{} pseudo-ref
		b.Write(pktStr(zeroOID + " capabilities^{}\x00" + caps + "\n"))
		b.Write(pktFlush)
		return b.Bytes()
	}
	first := true
	writeRef := func(oid, name string) {
		line := oid + " " + name
		if first {
			line += "\x00" + caps
			first = false
		}
		// HEAD carries the symref capability value, not a line suffix; the
		// client reads symref from the capabilities map.
		b.Write(pktStr(line + "\n"))
	}
	if headOK {
		writeRef(headOID, "HEAD")
	}
	for _, name := range names {
		oid := r.Refs[name]
		writeRef(oid, name)
		// peeled line for annotated tags
		if strings.HasPrefix(name, "refs/tags/") {
			if obj := r.Objects[oid]; obj != nil && obj.Type == "tag" {
				target, _ := parseTag(obj.Data)
				if target != "" {
					b.Write(pktStr(target + " " + name + "^{}\n"))
				}
			}
		}
	}
	b.Write(pktFlush)
	return b.Bytes()
}

// --- pack building -----------------------------------------------------------

// buildPackFor returns the pack bytes for the objects reachable from tips,
// excluding anything reachable from haves, limited to depth generations
// (depth < 0 = full). Also returns the shallow boundary oids and the full
// reachable set (used for correct unshallow lines).
func buildPackFor(r *Repo, tips []string, haves []string, depth int, clientShallow []string) ([]byte, []string, map[string]bool) {
	want, shallow := reachable(r, tips, depth)
	if len(haves) > 0 {
		var stopAt map[string]bool
		if len(clientShallow) > 0 {
			stopAt = make(map[string]bool, len(clientShallow))
			for _, sh := range clientShallow {
				stopAt[sh] = true
			}
		}
		haveSet, _ := reachableStop(r, haves, -1, stopAt)
		for oid := range haveSet {
			delete(want, oid)
		}
	}
	oids := make([]string, 0, len(want))
	for oid := range want {
		oids = append(oids, oid)
	}
	sort.Strings(oids)
	objs := make([]*Object, 0, len(oids))
	for _, oid := range oids {
		if o := r.Objects[oid]; o != nil {
			objs = append(objs, o)
		}
	}
	return writePack(objs), shallow, want
}

// packCacheKey identifies a cached clone pack.
func packCacheKey(tips []string, depth int, haves int) string {
	var sb strings.Builder
	for _, t := range tips {
		sb.WriteString(t)
		sb.WriteByte(',')
	}
	sb.WriteString(strconv.Itoa(depth))
	sb.WriteByte(',')
	sb.WriteString(strconv.Itoa(haves))
	return sb.String()
}

// --- upload-pack v2 -----------------------------------------------------------

type fetchArgs struct {
	wants    []string
	haves    []string
	shallow  []string
	depth    int // -1 = none
	done     bool
	thinPack bool
}

func parseFetchV2(lines []pkt) fetchArgs {
	var fa fetchArgs
	fa.depth = -1
	inArgs := false
	for _, p := range lines {
		if p.kind == "delim" {
			inArgs = true
			continue
		}
		if p.kind != "data" {
			continue
		}
		if !inArgs {
			continue // command=/agent=/object-format= lines
		}
		text := strings.TrimSuffix(p.text, "\n")
		switch {
		case strings.HasPrefix(text, "want "):
			fa.wants = append(fa.wants, text[5:])
		case strings.HasPrefix(text, "have "):
			fa.haves = append(fa.haves, text[5:])
		case strings.HasPrefix(text, "shallow "):
			fa.shallow = append(fa.shallow, text[8:])
		case strings.HasPrefix(text, "deepen "):
			n, _ := strconv.Atoi(text[7:])
			fa.depth = n
		case text == "done":
			fa.done = true
		case text == "thin-pack":
			fa.thinPack = true
		}
	}
	return fa
}

// handleUploadPackV2 serves one POST /git-upload-pack (v2 command request).
func handleUploadPackV2(s *Store, r *Repo, body []byte) []byte {
	lines := parsePktLines(body)
	var cmd string
	for _, p := range lines {
		if p.kind == "data" && strings.HasPrefix(p.text, "command=") {
			cmd = strings.TrimSuffix(p.text[len("command="):], "\n")
			break
		}
	}
	switch cmd {
	case "ls-refs":
		return lsRefsV2(s, r, lines)
	case "fetch":
		return fetchV2(s, r, lines)
	default:
		// unknown command: empty response
		return pktFlush
	}
}

func lsRefsV2(s *Store, r *Repo, lines []pkt) []byte {
	// parse args after delim: peel, symrefs, unborn, ref-prefix X
	var prefixes []string
	peel, symrefs, unborn := false, false, false
	inArgs := false
	for _, p := range lines {
		if p.kind == "delim" {
			inArgs = true
			continue
		}
		if p.kind != "data" || !inArgs {
			continue
		}
		text := strings.TrimSuffix(p.text, "\n")
		switch {
		case text == "peel":
			peel = true
		case text == "symrefs":
			symrefs = true
		case text == "unborn":
			unborn = true
		case strings.HasPrefix(text, "ref-prefix "):
			prefixes = append(prefixes, text[len("ref-prefix "):])
		}
	}
	_ = unborn
	var b bytes.Buffer
	keep := func(name string) bool {
		if len(prefixes) == 0 {
			return true
		}
		for _, pre := range prefixes {
			if strings.HasPrefix(name, pre) {
				return true
			}
		}
		return false
	}
	names := sortedRefs(r)
	if len(names) == 0 {
		if keep("HEAD") {
			head := "unborn HEAD"
			if symrefs {
				head += " symref-target:refs/heads/" + r.DefaultBranch
			}
			b.Write(pktStr(head + "\n"))
		}
		b.Write(pktFlush)
		return b.Bytes()
	}
	headOID, headOK := r.refOID("HEAD")
	if headOK && keep("HEAD") {
		line := headOID + " HEAD"
		if symrefs {
			line += " symref-target:refs/heads/" + r.DefaultBranch
		}
		b.Write(pktStr(line + "\n"))
	}
	for _, name := range names {
		if !keep(name) {
			continue
		}
		oid := r.Refs[name]
		line := oid + " " + name
		if peel && strings.HasPrefix(name, "refs/tags/") {
			if obj := r.Objects[oid]; obj != nil && obj.Type == "tag" {
				target, _ := parseTag(obj.Data)
				if target != "" {
					line += " peeled:" + target
				}
			}
		}
		b.Write(pktStr(line + "\n"))
	}
	b.Write(pktFlush)
	return b.Bytes()
}

func fetchV2(s *Store, r *Repo, lines []pkt) []byte {
	fa := parseFetchV2(lines)
	var b bytes.Buffer
	// acknowledgments section (only when the client offered haves)
	if len(fa.haves) > 0 {
		b.Write(pktStr("acknowledgments\n"))
		acked := false
		for _, h := range fa.haves {
			if _, ok := r.Objects[h]; ok {
				// ACK only when the have is an ancestor of a want
				for _, w := range fa.wants {
					if isAncestor(r, h, w) {
						b.Write(pktStr("ACK " + h + "\n"))
						acked = true
						break
					}
				}
			}
		}
		if !acked {
			b.Write(pktStr("NAK\n"))
		}
		b.Write(pktDelim)
	}
	// shallow-info section
	depth := fa.depth
	if depth >= 0 {
		b.Write(pktStr("shallow-info\n"))
		// boundary oids are computed by buildPackFor below; emit after.
	}
	pack, shallow, wantSet := fetchPack(r, fa.wants, fa.haves, depth, fa.shallow)
	if depth >= 0 {
		for _, sh := range shallow {
			b.Write(pktStr("shallow " + sh + "\n"))
		}
		shallowSet := make(map[string]bool, len(shallow))
		for _, sh := range shallow {
			shallowSet[sh] = true
		}
		for _, old := range fa.shallow {
			// unshallow only when the old boundary is now fully included:
			// in the pack and no longer a boundary.
			if wantSet[old] && !shallowSet[old] {
				b.Write(pktStr("unshallow " + old + "\n"))
			}
		}
		b.Write(pktDelim)
	}
	// packfile section
	b.Write(pktStr("packfile\n"))
	writePackSideband(&b, pack)
	b.Write(pktFlush)
	return b.Bytes()
}

// fetchPack builds (or serves from cache) the pack for a fetch. The clone
// workload fires 50 concurrent identical fetches; the pack for a tip is
// built once and cached under the repo's pack version, so the concurrent
// clones all serve the same bytes with zero rebuild.
func fetchPack(r *Repo, wants, haves []string, depth int, clientShallow []string) ([]byte, []string, map[string]bool) {
	if len(haves) == 0 && len(wants) > 0 {
		key := packCacheKey(wants, depth, 0)
		r.packMu.Lock()
		if r.packKey == key && r.packData != nil && r.packCached == r.packVer {
			pack, shallow, wantSet := r.packData, r.packShallow, r.packWant
			r.packMu.Unlock()
			return pack, shallow, wantSet
		}
		ver := r.packVer
		r.packMu.Unlock()
		pack, shallow, wantSet := buildPackFor(r, wants, haves, depth, nil)
		r.packMu.Lock()
		r.packKey = key
		r.packData = pack
		r.packShallow = shallow
		r.packWant = wantSet
		r.packCached = ver
		r.packMu.Unlock()
		return pack, shallow, wantSet
	}
	return buildPackFor(r, wants, haves, depth, clientShallow)
}

// writePackSideband writes pack bytes as sideband band-1 pkt-lines.
func writePackSideband(b *bytes.Buffer, pack []byte) {
	const maxData = 65520 - 4 - 1
	for len(pack) > 0 {
		n := len(pack)
		if n > maxData {
			n = maxData
		}
		b.Write(pktSideband(pack[:n]))
		pack = pack[n:]
	}
}

// --- upload-pack v0 -----------------------------------------------------------

func handleUploadPackV0(s *Store, r *Repo, body []byte) []byte {
	lines := parsePktLines(body)
	// two sections: wants (with shallow/deepen), flush, haves + done, flush
	var wants, haves, shallow []string
	depth := -1
	section := 0
	for _, p := range lines {
		if p.kind == "flush" {
			section++
			continue
		}
		if p.kind != "data" {
			continue
		}
		text := strings.TrimSuffix(p.text, "\n")
		// first want line may carry capabilities after NUL
		if nul := strings.IndexByte(text, 0); nul >= 0 {
			text = text[:nul]
		}
		switch {
		case strings.HasPrefix(text, "want "):
			wants = append(wants, text[5:])
		case strings.HasPrefix(text, "have "):
			haves = append(haves, text[5:])
		case strings.HasPrefix(text, "shallow "):
			shallow = append(shallow, text[8:])
		case strings.HasPrefix(text, "deepen "):
			n, _ := strconv.Atoi(text[7:])
			depth = n
		}
		_ = section
	}
	var b bytes.Buffer
	// NAK / ACK lines (plain pkt-lines, not sideband)
	if len(haves) > 0 {
		acked := false
		for _, h := range haves {
			if _, ok := r.Objects[h]; ok {
				for _, w := range wants {
					if isAncestor(r, h, w) {
						b.Write(pktStr("ACK " + h + "\n"))
						acked = true
						break
					}
				}
			}
		}
		if !acked {
			b.Write(pktStr("NAK\n"))
		}
	}
	pack, sh, wantSet := fetchPack(r, wants, haves, depth, shallow)
	if depth >= 0 {
		for _, s := range sh {
			b.Write(pktStr("shallow " + s + "\n"))
		}
		shallowSet := make(map[string]bool, len(sh))
		for _, s := range sh {
			shallowSet[s] = true
		}
		for _, old := range shallow {
			if wantSet[old] && !shallowSet[old] {
				b.Write(pktStr("unshallow " + old + "\n"))
			}
		}
	}
	writePackSideband(&b, pack)
	b.Write(pktFlush)
	return b.Bytes()
}

// --- receive-pack -------------------------------------------------------------

// handleReceivePack serves POST /git-receive-pack. Returns the sideband
// report body. The caller holds the store write lock.
func handleReceivePack(s *Store, r *Repo, body []byte) []byte {
	lines := parsePktLines(body)
	type cmd struct {
		oldOID, newOID, ref string
	}
	var cmds []cmd
	packStart := -1
	for i, p := range lines {
		if p.kind == "flush" {
			packStart = i
			break
		}
		if p.kind != "data" {
			continue
		}
		text := strings.TrimSuffix(p.text, "\n")
		if nul := strings.IndexByte(text, 0); nul >= 0 {
			text = text[:nul]
		}
		parts := strings.SplitN(text, " ", 3)
		if len(parts) != 3 {
			continue
		}
		cmds = append(cmds, cmd{oldOID: parts[0], newOID: parts[1], ref: parts[2]})
	}
	// pack bytes follow the flush pkt-line in the raw body
	var pack []byte
	if packStart >= 0 {
		// recompute byte offset of the flush packet
		off := 0
		for i := 0; i <= packStart; i++ {
			n, err := strconv.ParseInt(string(body[off:off+4]), 16, 32)
			if err != nil {
				break
			}
			if n == 0 {
				off += 4
			} else {
				off += int(n)
			}
		}
		if off < len(body) {
			pack = body[off:]
		}
	}
	// ingest objects
	unpackOK := true
	if len(pack) > 0 {
		objs, err := readPackFull(pack, func(oid string) *Object { return r.Objects[oid] })
		if err != nil {
			unpackOK = false
		} else {
			for oid, o := range objs {
				if _, ok := r.Objects[oid]; !ok {
					r.Objects[oid] = o
				}
			}
		}
	}
	// apply commands
	var report bytes.Buffer
	if unpackOK {
		report.WriteString("unpack ok\n")
	} else {
		report.WriteString("unpack error\n")
	}
	for _, c := range cmds {
		ok := false
		reason := ""
		if !unpackOK {
			reason = "unpacker error"
		} else if c.newOID == zeroOID {
			// delete
			cur, exists := r.Refs[c.ref]
			if !exists {
				reason = "remote ref does not exist"
			} else if c.oldOID != zeroOID && cur != c.oldOID {
				reason = "non-fast-forward"
			} else {
				delete(r.Refs, c.ref)
				ok = true
			}
		} else {
			// create or update: the new object must exist
			if _, exists := r.Objects[c.newOID]; !exists {
				reason = "object not found"
			} else {
				cur, exists := r.Refs[c.ref]
				if !exists {
					if c.oldOID != zeroOID {
						reason = "non-fast-forward"
					} else {
						r.Refs[c.ref] = c.newOID
						ok = true
					}
				} else if cur != c.oldOID {
					reason = "non-fast-forward"
				} else if !isAncestor(r, cur, c.newOID) {
					reason = "non-fast-forward"
				} else {
					r.Refs[c.ref] = c.newOID
					ok = true
				}
			}
		}
		if ok {
			report.WriteString("ok " + c.ref + "\n")
		} else {
			report.WriteString("ng " + c.ref + " " + reason + "\n")
		}
	}
	if len(cmds) > 0 {
		r.packVer++
	}
	// frame the report as sideband band-1 pkt-lines
	var out bytes.Buffer
	// split the report into pkt-lines (each line its own packet)
	for _, line := range strings.SplitAfter(report.String(), "\n") {
		if line == "" {
			continue
		}
		out.Write(pktSideband([]byte(line)))
	}
	out.Write(pktFlush)
	return out.Bytes()
}

// sha1Hex is a small helper kept here for pack checksums.
func sha1Hex(b []byte) string {
	sum := sha1.Sum(b)
	return hex.EncodeToString(sum[:])
}
