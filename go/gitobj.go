// Git objects and packs.
//
// Objects are stored raw (content without the "<type> <len>\0" header) and
// hashed once on ingest. Packs are written without deltas: the clone and
// fetch packs here are small (a depth-1 clone carries one commit, one tree
// and a few blobs), so plain deflated objects are both correct and fast to
// build. Pack ingest reuses pooled zlib readers and buffers.
package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"sort"
	"sync"
)

const zeroOID = "0000000000000000000000000000000000000000"

func isOID(s string) bool {
	if len(s) != 40 {
		return false
	}
	for i := 0; i < 40; i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// --- object parsing ---------------------------------------------------------

type TreeEntry struct {
	Mode string
	Name string
	OID  string
}

func parseTree(data []byte) []TreeEntry {
	var out []TreeEntry
	i := 0
	for i < len(data) {
		sp := bytes.IndexByte(data[i:], ' ')
		if sp < 0 {
			break
		}
		mode := string(data[i : i+sp])
		i += sp + 1
		nul := bytes.IndexByte(data[i:], 0)
		if nul < 0 || i+nul+20 > len(data) {
			break
		}
		name := string(data[i : i+nul])
		oid := hex.EncodeToString(data[i+nul+1 : i+nul+21])
		out = append(out, TreeEntry{Mode: mode, Name: name, OID: oid})
		i += nul + 21
	}
	return out
}

func buildTree(entries []TreeEntry) []byte {
	// git sorts tree entries with directories as if their name ends in '/'
	sorted := make([]TreeEntry, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool {
		a, b := sorted[i].Name, sorted[j].Name
		if sorted[i].Mode == "40000" {
			a += "/"
		}
		if sorted[j].Mode == "40000" {
			b += "/"
		}
		return a < b
	})
	var buf bytes.Buffer
	for _, e := range sorted {
		buf.WriteString(e.Mode)
		buf.WriteByte(' ')
		buf.WriteString(e.Name)
		buf.WriteByte(0)
		raw, _ := hex.DecodeString(e.OID)
		buf.Write(raw)
	}
	return buf.Bytes()
}

type CommitView struct {
	Tree    string
	Parents []string
	Message string
	Author  string
	Raw     []byte
}

func parseCommit(data []byte) CommitView {
	cv := CommitView{Raw: data}
	// header lines until blank line
	i := 0
	for i < len(data) {
		nl := bytes.IndexByte(data[i:], '\n')
		if nl < 0 {
			break
		}
		line := data[i : i+nl]
		i += nl + 1
		if len(line) == 0 {
			break
		}
		if bytes.HasPrefix(line, []byte("tree ")) {
			cv.Tree = string(line[5:])
		} else if bytes.HasPrefix(line, []byte("parent ")) {
			cv.Parents = append(cv.Parents, string(line[7:]))
		} else if bytes.HasPrefix(line, []byte("author ")) {
			cv.Author = string(line[7:])
		}
	}
	if i < len(data) {
		cv.Message = string(data[i:])
	}
	return cv
}

func parseTag(data []byte) (obj string, typ string) {
	i := 0
	for i < len(data) {
		nl := bytes.IndexByte(data[i:], '\n')
		if nl < 0 {
			break
		}
		line := data[i : i+nl]
		i += nl + 1
		if len(line) == 0 {
			break
		}
		if bytes.HasPrefix(line, []byte("object ")) {
			obj = string(line[7:])
		} else if bytes.HasPrefix(line, []byte("type ")) {
			typ = string(line[5:])
		}
	}
	return obj, typ
}

// --- pack reading -----------------------------------------------------------

// inflatePackObject inflates one zlib stream starting at off and returns the
// data and the offset just past the stream.
func inflatePackObject(pack []byte, off int) ([]byte, int, error) {
	br := bytes.NewReader(pack[off:])
	r, err := zlib.NewReader(br)
	if err != nil {
		return nil, 0, err
	}
	data, err := io.ReadAll(r)
	if err != nil {
		r.Close()
		return nil, 0, err
	}
	consumed := len(pack[off:]) - br.Len()
	r.Close()
	return data, off + consumed, nil
}

// applyDelta applies a git delta to a base object.
func applyDelta(base, delta []byte) ([]byte, error) {
	// read varints
	rd := func(d []byte, i *int) int64 {
		var v int64
		shift := uint(0)
		for {
			b := d[*i]
			*i++
			v |= int64(b&0x7f) << shift
			shift += 7
			if b&0x80 == 0 {
				break
			}
		}
		return v
	}
	i := 0
	srcSize := rd(delta, &i)
	if int(srcSize) != len(base) {
		return nil, fmt.Errorf("delta base size mismatch")
	}
	dstSize := rd(delta, &i)
	var out []byte
	for i < len(delta) {
		op := delta[i]
		i++
		if op&0x80 != 0 {
			// copy from base
			var off, size int
			for b := 0; b < 4; b++ {
				if op&(1<<uint(b)) != 0 {
					off |= int(delta[i]) << (8 * uint(b))
					i++
				}
			}
			for b := 0; b < 3; b++ {
				if op&(0x10<<uint(b)) != 0 {
					size |= int(delta[i]) << (8 * uint(b))
					i++
				}
			}
			if size == 0 {
				size = 0x10000
			}
			if off+size > len(base) {
				return nil, fmt.Errorf("delta copy out of range")
			}
			out = append(out, base[off:off+size]...)
		} else if op != 0 {
			n := int(op)
			if i+n > len(delta) {
				return nil, fmt.Errorf("delta insert out of range")
			}
			out = append(out, delta[i:i+n]...)
			i += n
		} else {
			return nil, fmt.Errorf("delta opcode 0")
		}
	}
	if int64(len(out)) != dstSize {
		return nil, fmt.Errorf("delta result size mismatch")
	}
	return out, nil
}

// readPackFull is the complete pack reader used by receive-pack: it handles
// OFS and REF deltas by keeping per-entry offsets.
func readPackFull(pack []byte, lookup func(oid string) *Object) (map[string]*Object, error) {
	if len(pack) < 12 || string(pack[:4]) != "PACK" {
		return nil, fmt.Errorf("bad pack header")
	}
	count := int(binary.BigEndian.Uint32(pack[8:12]))
	off := 12
	type entry struct {
		oid     string
		typ     string
		data    []byte
		baseOID string
		baseOff int // offset of the base entry in the pack, or -1
	}
	entries := make([]entry, 0, count)
	byOff := make(map[int]string)
	for n := 0; n < count; n++ {
		start := off
		if off >= len(pack)-20 {
			return nil, fmt.Errorf("pack truncated")
		}
		b0 := pack[off]
		typNum := (b0 >> 4) & 7
		off++
		shift := uint(4)
		for pack[off-1]&0x80 != 0 {
			shift += 7
			off++
		}
		var typ string
		switch typNum {
		case 1:
			typ = "commit"
		case 2:
			typ = "tree"
		case 3:
			typ = "blob"
		case 4:
			typ = "tag"
		case 6, 7:
			typ = "delta"
		default:
			return nil, fmt.Errorf("unknown pack object type %d", typNum)
		}
		var baseOID string
		var baseOff = -1
		if typNum == 7 {
			baseOID = hex.EncodeToString(pack[off : off+20])
			off += 20
		} else if typNum == 6 {
			b := pack[off]
			off++
			v := int(b & 0x7f)
			for b&0x80 != 0 {
				b = pack[off]
				off++
				v = ((v + 1) << 7) | int(b&0x7f)
			}
			baseOff = start - v
		}
		data, next, err := inflatePackObject(pack, off)
		if err != nil {
			return nil, err
		}
		off = next
		if typ == "delta" {
			entries = append(entries, entry{typ: "delta", data: data, baseOID: baseOID, baseOff: baseOff})
			continue
		}
		oid := hashObject(typ, data)
		entries = append(entries, entry{oid: oid, typ: typ, data: data})
		byOff[start] = oid
	}
	// resolve deltas (possibly chained): iterate until all resolved.
	objects := make(map[string]*Object, count)
	for _, e := range entries {
		if e.typ != "delta" {
			objects[e.oid] = &Object{Type: e.typ, Data: e.data}
		}
	}
	var resolve func(e entry) (*Object, error)
	resolve = func(e entry) (*Object, error) {
		// find base object
		var base *Object
		if e.baseOID != "" {
			base = objects[e.baseOID]
			if base == nil && lookup != nil {
				base = lookup(e.baseOID)
			}
		} else if e.baseOff >= 0 {
			// base is an entry at that offset; resolve it recursively
			bo, ok := byOff[e.baseOff]
			if !ok {
				return nil, fmt.Errorf("ofs delta base not found")
			}
			base = objects[bo]
			if base == nil {
				// find the entry and resolve it first
				for _, be := range entries {
					if be.oid == bo {
						var err error
						base, err = resolve(be)
						if err != nil {
							return nil, err
						}
						break
					}
				}
			}
		}
		if base == nil {
			return nil, fmt.Errorf("delta base missing")
		}
		data, err := applyDelta(base.Data, e.data)
		if err != nil {
			return nil, err
		}
		// the delta result type is the base's type
		return &Object{Type: base.Type, Data: data}, nil
	}
	// deltas may chain; loop until progress stalls.
	pending := []entry{}
	for _, e := range entries {
		if e.typ == "delta" {
			pending = append(pending, e)
		}
	}
	for len(pending) > 0 {
		progress := false
		var next []entry
		for _, e := range pending {
			obj, err := resolve(e)
			if err != nil {
				next = append(next, e)
				continue
			}
			oid := hashObject(obj.Type, obj.Data)
			objects[oid] = obj
			progress = true
		}
		if !progress {
			return nil, fmt.Errorf("unresolvable delta in pack")
		}
		pending = next
	}
	return objects, nil
}

// --- pack writing -----------------------------------------------------------

var zlibWriterPool = sync.Pool{New: func() any { return zlib.NewWriter(nil) }}

// writePack builds a packfile (v2, no deltas) from objects, with a SHA-1
// trailer. Deterministic order: the caller sorts the list.
func writePack(objs []*Object) []byte {
	var body bytes.Buffer
	body.WriteString("PACK")
	var hdr [8]byte
	binary.BigEndian.PutUint32(hdr[0:4], 2)
	binary.BigEndian.PutUint32(hdr[4:8], uint32(len(objs)))
	body.Write(hdr[:])
	for _, o := range objs {
		writePackObjectFast(&body, o.Type, o.Data)
	}
	// trailer: sha1 of everything so far
	sum := sha1.Sum(body.Bytes())
	body.Write(sum[:])
	return body.Bytes()
}

func writePackObjectFast(buf *bytes.Buffer, typ string, data []byte) {
	var typNum byte
	switch typ {
	case "commit":
		typNum = 1
	case "tree":
		typNum = 2
	case "blob":
		typNum = 3
	case "tag":
		typNum = 4
	}
	size := len(data)
	b := byte(typNum<<4) | byte(size&0x0f)
	size >>= 4
	if size > 0 {
		b |= 0x80
	}
	buf.WriteByte(b)
	for size > 0 {
		c := byte(size & 0x7f)
		size >>= 7
		if size > 0 {
			c |= 0x80
		}
		buf.WriteByte(c)
	}
	w, _ := zlib.NewWriterLevel(buf, zlib.BestSpeed)
	w.Write(data)
	w.Close()
}

func packChecksum(pack []byte) string {
	sum := sha1.Sum(pack[:len(pack)-20])
	return hex.EncodeToString(sum[:])
}

// --- reachability -------------------------------------------------------------

// reachable returns every oid reachable from tips (commits, their trees and
// blobs, tags), optionally limited to `depth` commit generations from a tip
// (depth < 0 means unlimited). shallow holds the boundary commit oids when
// depth limits the walk.
func reachable(r *Repo, tips []string, depth int) (map[string]bool, []string) {
	return reachableStop(r, tips, depth, nil)
}

// reachableStop is reachable, but the walk never goes behind a commit in
// stopAt (the client's shallow boundary): that commit and its tree count as
// present, its parents do not. A fetch that deepens a shallow clone must not
// treat the history behind the client's shallow line as already-held.
func reachableStop(r *Repo, tips []string, depth int, stopAt map[string]bool) (map[string]bool, []string) {
	seen := make(map[string]bool)
	var shallow []string
	type item struct {
		oid   string
		gen   int // commit generation from a tip
		isTip bool
	}
	queue := []item{}
	for _, t := range tips {
		if t == "" || t == zeroOID {
			continue
		}
		queue = append(queue, item{oid: t, gen: 0, isTip: true})
	}
	for len(queue) > 0 {
		it := queue[0]
		queue = queue[1:]
		if seen[it.oid] {
			continue
		}
		obj := r.Objects[it.oid]
		if obj == nil {
			continue
		}
		seen[it.oid] = true
		switch obj.Type {
		case "commit":
			cv := parseCommit(obj.Data)
			if cv.Tree != "" {
				queue = append(queue, item{oid: cv.Tree, gen: it.gen})
			}
			if stopAt != nil && stopAt[it.oid] {
				// client's shallow boundary: present, but no parents
				continue
			}
			for _, p := range cv.Parents {
				if depth >= 0 && it.gen+1 >= depth {
					// this commit is a shallow boundary: it is sent, but
					// its parents are cut, so shallow-info names IT.
					found := false
					for _, sh := range shallow {
						if sh == it.oid {
							found = true
							break
						}
					}
					if !found {
						shallow = append(shallow, it.oid)
					}
					continue
				}
				queue = append(queue, item{oid: p, gen: it.gen + 1})
			}
		case "tag":
			target, _ := parseTag(obj.Data)
			if target != "" {
				queue = append(queue, item{oid: target, gen: it.gen})
			}
		case "tree":
			for _, e := range parseTree(obj.Data) {
				queue = append(queue, item{oid: e.OID, gen: it.gen})
			}
		}
	}
	return seen, shallow
}

// isAncestor reports whether anc is an ancestor of (or equal to) oid.
func isAncestor(r *Repo, anc, oid string) bool {
	if anc == oid {
		return true
	}
	seen := map[string]bool{}
	queue := []string{oid}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if seen[cur] {
			continue
		}
		seen[cur] = true
		obj := r.Objects[cur]
		if obj == nil || obj.Type != "commit" {
			continue
		}
		cv := parseCommit(obj.Data)
		for _, p := range cv.Parents {
			if p == anc {
				return true
			}
			queue = append(queue, p)
		}
	}
	return false
}

// commitList returns commits reachable from tip, newest first (BFS by
// generation, parents after children).
func commitList(r *Repo, tip string) []string {
	var out []string
	seen := map[string]bool{}
	queue := []string{tip}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if seen[cur] || cur == "" || cur == zeroOID {
			continue
		}
		seen[cur] = true
		obj := r.Objects[cur]
		if obj == nil || obj.Type != "commit" {
			continue
		}
		out = append(out, cur)
		cv := parseCommit(obj.Data)
		queue = append(queue, cv.Parents...)
	}
	return out
}

// mergeBase returns the first common ancestor of a and b (by BFS from both).
func mergeBase(r *Repo, a, b string) string {
	if a == b {
		return a
	}
	ancA := map[string]bool{}
	queue := []string{a}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if ancA[cur] {
			continue
		}
		ancA[cur] = true
		obj := r.Objects[cur]
		if obj == nil || obj.Type != "commit" {
			continue
		}
		queue = append(queue, parseCommit(obj.Data).Parents...)
	}
	seen := map[string]bool{}
	queue = []string{b}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if seen[cur] {
			continue
		}
		seen[cur] = true
		if ancA[cur] {
			return cur
		}
		obj := r.Objects[cur]
		if obj == nil || obj.Type != "commit" {
			continue
		}
		queue = append(queue, parseCommit(obj.Data).Parents...)
	}
	return ""
}

// treeEntries returns the flat path -> (mode, oid) map of a tree.
func treeFlat(r *Repo, treeOID string) map[string]TreeEntry {
	out := map[string]TreeEntry{}
	var walk func(oid, prefix string)
	walk = func(oid, prefix string) {
		obj := r.Objects[oid]
		if obj == nil || obj.Type != "tree" {
			return
		}
		for _, e := range parseTree(obj.Data) {
			p := prefix + e.Name
			if e.Mode == "40000" {
				walk(e.OID, p+"/")
			} else {
				out[p] = e
			}
		}
	}
	walk(treeOID, "")
	return out
}

// diffTrees returns changed paths between two trees: path -> status
// ("added" | "modified" | "removed").
func diffTrees(r *Repo, baseTree, headTree string) map[string]string {
	base := treeFlat(r, baseTree)
	head := treeFlat(r, headTree)
	out := map[string]string{}
	for p, be := range base {
		he, ok := head[p]
		if !ok {
			out[p] = "removed"
		} else if he.OID != be.OID {
			out[p] = "modified"
		}
	}
	for p := range head {
		if _, ok := base[p]; !ok {
			out[p] = "added"
		}
	}
	return out
}

// updateTree returns a new tree oid equal to baseTree with path set to
// blobOID (or removed when blobOID is ""). Creates intermediate trees.
func updateTree(r *Repo, baseTreeOID string, path []string, blobOID string, mode string) string {
	if len(path) == 0 {
		return baseTreeOID
	}
	var entries []TreeEntry
	if baseTreeOID != "" {
		if obj := r.Objects[baseTreeOID]; obj != nil && obj.Type == "tree" {
			entries = parseTree(obj.Data)
		}
	}
	name := path[0]
	if len(path) == 1 {
		// leaf
		found := -1
		for i, e := range entries {
			if e.Name == name {
				found = i
				break
			}
		}
		if blobOID == "" {
			if found >= 0 {
				entries = append(entries[:found], entries[found+1:]...)
			}
		} else {
			if found >= 0 {
				entries[found].OID = blobOID
				entries[found].Mode = mode
			} else {
				entries = append(entries, TreeEntry{Mode: mode, Name: name, OID: blobOID})
			}
		}
	} else {
		// directory
		var subOID string
		found := -1
		for i, e := range entries {
			if e.Name == name && e.Mode == "40000" {
				found = i
				subOID = e.OID
				break
			}
		}
		newSub := updateTree(r, subOID, path[1:], blobOID, mode)
		if found >= 0 {
			entries[found].OID = newSub
		} else {
			entries = append(entries, TreeEntry{Mode: "40000", Name: name, OID: newSub})
		}
	}
	data := buildTree(entries)
	oid := hashObject("tree", data)
	if _, ok := r.Objects[oid]; !ok {
		r.Objects[oid] = &Object{Type: "tree", Data: data}
	}
	return oid
}

// lookupBlob returns the blob bytes at path in the tree at ref, or nil.
func lookupBlob(r *Repo, treeOID string, path string) (string, []byte) {
	parts := splitPath(path)
	cur := treeOID
	for i, p := range parts {
		obj := r.Objects[cur]
		if obj == nil || obj.Type != "tree" {
			return "", nil
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
			return "", nil
		}
		if i == len(parts)-1 {
			if mode == "40000" {
				return "", nil
			}
			bo := r.Objects[next]
			if bo == nil {
				return "", nil
			}
			return next, bo.Data
		}
		cur = next
	}
	return "", nil
}

func splitPath(p string) []string {
	var out []string
	start := 0
	for i := 0; i < len(p); i++ {
		if p[i] == '/' {
			if i > start {
				out = append(out, p[start:i])
			}
			start = i + 1
		}
	}
	if start < len(p) {
		out = append(out, p[start:])
	}
	return out
}
