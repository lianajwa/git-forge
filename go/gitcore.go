// gitcore.go — the git object layer: SHA-1 object ids, pkt-line framing, packfiles,
// and commit/tree/tag parsing and building. A direct port of the Node implementation:
// the same bytes in give the same bytes out. No wall clock, no randomness.

package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"
	"strconv"
	"strings"
)

// ObjKind is a git object type.
type ObjKind string

const (
	KindCommit ObjKind = "commit"
	KindTree   ObjKind = "tree"
	KindBlob   ObjKind = "blob"
	KindTag    ObjKind = "tag"
)

// GitObj is one git object: its kind and raw bytes.
type GitObj struct {
	Kind  ObjKind
	Bytes []byte
}

// ZERO_OID is the all-zero object id.
const ZERO_OID = "0000000000000000000000000000000000000000"

func isHex40(value string) bool {
	if len(value) != 40 {
		return false
	}
	for i := 0; i < 40; i++ {
		c := value[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// IsOid reports whether value is a 40-hex object id (lowercase, as git writes them).
func IsOid(value string) bool {
	return isHex40(value)
}

// OidOf is git's object id: SHA-1 over `<kind> <size>\0` + the object's bytes.
func OidOf(kind ObjKind, data []byte) string {
	h := sha1.New()
	h.Write([]byte(fmt.Sprintf("%s %d\x00", kind, len(data))))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// ---------------------------------------------------------------------------------------------
// pkt-line framing (git's wire framing: 4 hex length bytes, 0000 flush, 0001 delim, 0002 end)
// ---------------------------------------------------------------------------------------------

// PKT_MAX is the largest pkt-line length git allows.
const PKT_MAX = 65520

// PktKind is a decoded packet's kind.
type PktKind int

const (
	PktData PktKind = iota
	PktFlush
	PktDelim
	PktEnd
)

// Pkt is one decoded pkt-line.
type Pkt struct {
	Kind    PktKind
	Payload []byte
}

const hexDigits = "0123456789abcdef"

// appendPktHeader appends a 4-hex-digit pkt-line length header for a payload of n
// bytes (n counts the payload only; the header covers payload+4). No allocation.
func appendPktHeader(out []byte, n int) []byte {
	total := n + 4
	return append(out, hexDigits[(total>>12)&0xf], hexDigits[(total>>8)&0xf], hexDigits[(total>>4)&0xf], hexDigits[total&0xf])
}

// FramePkt frames one data payload as a pkt-line.
func FramePkt(payload []byte) ([]byte, error) {
	if len(payload) > PKT_MAX-4 {
		return nil, fmt.Errorf("pkt-line payload of %d bytes exceeds the limit", len(payload))
	}
	out := make([]byte, 0, len(payload)+4)
	out = appendPktHeader(out, len(payload))
	out = append(out, payload...)
	return out, nil
}

// FramePktString frames a string payload as a pkt-line.
func FramePktString(payload string) ([]byte, error) {
	return FramePkt([]byte(payload))
}

// FrameFlush returns the flush packet.
func FrameFlush() []byte {
	return []byte("0000")
}

// FrameDelim returns the delim packet.
func FrameDelim() []byte {
	return []byte("0001")
}

// frameEnd returns the end packet.
func frameEnd() []byte {
	return []byte("0002")
}

// PktItem is one item of a framed packet list.
type PktItem struct {
	Kind    PktKind // PktData, PktFlush, PktDelim or PktEnd
	Payload []byte  // set only for PktData
}

// FramePkts frames a list of lines/control packets in order.
func FramePkts(items []PktItem) ([]byte, error) {
	var out []byte
	for _, item := range items {
		switch item.Kind {
		case PktFlush:
			out = append(out, FrameFlush()...)
		case PktDelim:
			out = append(out, FrameDelim()...)
		case PktEnd:
			out = append(out, frameEnd()...)
		default:
			framed, err := FramePkt(item.Payload)
			if err != nil {
				return nil, err
			}
			out = append(out, framed...)
		}
	}
	return out, nil
}

// framePktStrings is a convenience for framing string lines plus control words.
// It sizes the answer once and writes straight into it: no item slice, no per-line
// allocation, one buffer out.
func framePktStrings(items []string) ([]byte, error) {
	total := 0
	for _, item := range items {
		switch item {
		case "flush", "delim", "end":
			total += 4
		default:
			if len(item) > PKT_MAX-4 {
				return nil, fmt.Errorf("pkt-line payload of %d bytes exceeds the limit", len(item))
			}
			total += len(item) + 4
		}
	}
	out := make([]byte, 0, total)
	for _, item := range items {
		switch item {
		case "flush":
			out = append(out, "0000"...)
		case "delim":
			out = append(out, "0001"...)
		case "end":
			out = append(out, "0002"...)
		default:
			out = appendPktHeader(out, len(item))
			out = append(out, item...)
		}
	}
	return out, nil
}

// isPktHead reports whether the 4 bytes are a pkt-line length in hex (either case).
func isPktHead(head []byte) bool {
	if len(head) != 4 {
		return false
	}
	for _, c := range head {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// DecodePkts decodes a whole buffer into packets. When allowRawTail is set, decoding
// stops at the first non-pkt-line header and returns the rest as the tail (a
// receive-pack body keeps its raw pack bytes after the command list's flush packet).
func DecodePkts(buf []byte, allowRawTail bool) ([]Pkt, []byte, error) {
	var packets []Pkt
	at := 0
	for at < len(buf) {
		if at+4 > len(buf) {
			return nil, nil, errors.New("truncated pkt-line header")
		}
		if !isPktHead(buf[at : at+4]) {
			if allowRawTail {
				break
			}
			return nil, nil, fmt.Errorf("bad pkt-line length %q", string(buf[at:at+4]))
		}
		length, err := strconv.ParseInt(string(buf[at:at+4]), 16, 32)
		if err != nil {
			return nil, nil, fmt.Errorf("bad pkt-line length %q", string(buf[at:at+4]))
		}
		switch length {
		case 0:
			packets = append(packets, Pkt{Kind: PktFlush})
			at += 4
			continue
		case 1:
			packets = append(packets, Pkt{Kind: PktDelim})
			at += 4
			continue
		case 2:
			packets = append(packets, Pkt{Kind: PktEnd})
			at += 4
			continue
		}
		if length == 3 || length > PKT_MAX {
			return nil, nil, fmt.Errorf("bad pkt-line length %d", length)
		}
		if at+int(length) > len(buf) {
			return nil, nil, errors.New("truncated pkt-line payload")
		}
		packets = append(packets, Pkt{Kind: PktData, Payload: buf[at+4 : at+int(length)]})
		at += int(length)
	}
	return packets, buf[at:], nil
}

// PktLines returns the data payloads of a packet list, as text lines.
func PktLines(packets []Pkt) []string {
	out := []string{}
	for _, p := range packets {
		if p.Kind == PktData {
			out = append(out, string(p.Payload))
		}
	}
	return out
}

// ---------------------------------------------------------------------------------------------
// trees
// ---------------------------------------------------------------------------------------------

// TreeEntry is one entry of a tree object.
type TreeEntry struct {
	Mode string
	Name string
	Oid  string
}

// ParseTree parses a tree object's bytes into entries, in stored order.
func ParseTree(data []byte) ([]TreeEntry, error) {
	var entries []TreeEntry
	at := 0
	for at < len(data) {
		space := bytes.IndexByte(data[at:], 0x20)
		if space < 0 {
			return nil, errors.New("tree entry has no mode separator")
		}
		space += at
		nul := bytes.IndexByte(data[space+1:], 0x00)
		if nul < 0 {
			return nil, errors.New("tree entry is truncated")
		}
		nul += space + 1
		if nul+21 > len(data) {
			return nil, errors.New("tree entry is truncated")
		}
		entries = append(entries, TreeEntry{
			Mode: string(data[at:space]),
			Name: string(data[space+1 : nul]),
			Oid:  hex.EncodeToString(data[nul+1 : nul+21]),
		})
		at = nul + 21
	}
	return entries, nil
}

// treeSortKey is git's tree sort key: byte order of the name, a directory compared as
// if it ended in '/'.
func treeSortKey(entry TreeEntry) string {
	if entry.Mode == "40000" {
		return entry.Name + "/"
	}
	return entry.Name
}

// BuildTree builds a tree object's bytes from entries, sorted the way git sorts them.
func BuildTree(entries []TreeEntry) ([]byte, error) {
	sorted := make([]TreeEntry, len(entries))
	copy(sorted, entries)
	sort.SliceStable(sorted, func(i, j int) bool {
		return strings.Compare(treeSortKey(sorted[i]), treeSortKey(sorted[j])) < 0
	})
	var buf bytes.Buffer
	for _, entry := range sorted {
		if !IsOid(entry.Oid) {
			return nil, fmt.Errorf("tree entry %s has a bad object id", entry.Name)
		}
		buf.WriteString(entry.Mode + " " + entry.Name + "\x00")
		raw, err := hex.DecodeString(entry.Oid)
		if err != nil {
			return nil, err
		}
		buf.Write(raw)
	}
	return buf.Bytes(), nil
}

// ---------------------------------------------------------------------------------------------
// commits and tags
// ---------------------------------------------------------------------------------------------

// CommitView is a parsed commit object.
type CommitView struct {
	Tree      string
	Parents   []string
	Author    string
	Committer string
	Message   string
}

// ParseCommit parses a commit object's bytes.
func ParseCommit(data []byte) (*CommitView, error) {
	blank := bytes.Index(data, []byte("\n\n"))
	headEnd := len(data)
	if blank >= 0 {
		headEnd = blank
	}
	message := ""
	if blank >= 0 {
		message = string(data[blank+2:])
	}
	view := &CommitView{}
	for _, line := range strings.Split(string(data[:headEnd]), "\n") {
		if line == "" {
			continue
		}
		space := strings.Index(line, " ")
		key := line
		value := ""
		if space >= 0 {
			key = line[:space]
			value = line[space+1:]
		}
		switch key {
		case "tree":
			view.Tree = value
		case "parent":
			view.Parents = append(view.Parents, value)
		case "author":
			view.Author = value
		case "committer":
			view.Committer = value
		}
	}
	if !IsOid(view.Tree) {
		return nil, errors.New("commit has no valid tree header")
	}
	view.Message = message
	return view, nil
}

// TagView is a parsed annotated tag object.
type TagView struct {
	Object  string
	Type    ObjKind
	Tag     string
	Tagger  string
	Message string
}

// ParseTag parses an annotated tag object's bytes.
func ParseTag(data []byte) (*TagView, error) {
	blank := bytes.Index(data, []byte("\n\n"))
	headEnd := len(data)
	if blank >= 0 {
		headEnd = blank
	}
	headers := make(map[string]string)
	for _, line := range strings.Split(string(data[:headEnd]), "\n") {
		if line == "" {
			continue
		}
		space := strings.Index(line, " ")
		if space < 0 {
			return nil, fmt.Errorf("tag header %q has no value", line)
		}
		headers[line[:space]] = line[space+1:]
	}
	object := headers["object"]
	typ := headers["type"]
	if !IsOid(object) {
		return nil, errors.New("tag has no valid object header")
	}
	switch ObjKind(typ) {
	case KindCommit, KindTree, KindBlob, KindTag:
	default:
		return nil, fmt.Errorf("tag has bad type %q", typ)
	}
	message := ""
	if blank >= 0 {
		message = string(data[blank+2:])
	}
	return &TagView{
		Object:  object,
		Type:    ObjKind(typ),
		Tag:     headers["tag"],
		Tagger:  headers["tagger"],
		Message: message,
	}, nil
}

// CommitFields are the fields of a commit object to build.
type CommitFields struct {
	Tree      string
	Parents   []string
	Message   string
	Author    string
	Committer string
	// Timestamp is Unix seconds; callers pass a fixed constant, never the wall clock.
	Timestamp int64
	Timezone  string
}

// BuildCommit builds a commit object's bytes. The message is used verbatim (callers
// end it with \n).
func BuildCommit(fields CommitFields) ([]byte, error) {
	if !IsOid(fields.Tree) {
		return nil, errors.New("buildCommit needs a valid tree id")
	}
	tz := fields.Timezone
	if tz == "" {
		tz = "+0000"
	}
	var lines []string
	lines = append(lines, "tree "+fields.Tree)
	for _, parent := range fields.Parents {
		if !IsOid(parent) {
			return nil, errors.New("buildCommit needs valid parent ids")
		}
		lines = append(lines, "parent "+parent)
	}
	lines = append(lines, fmt.Sprintf("author %s %d %s", fields.Author, fields.Timestamp, tz))
	lines = append(lines, fmt.Sprintf("committer %s %d %s", fields.Committer, fields.Timestamp, tz))
	lines = append(lines, "")
	return []byte(strings.Join(lines, "\n") + "\n" + fields.Message), nil
}

// TagFields are the fields of an annotated tag object to build.
type TagFields struct {
	Object    string
	Type      ObjKind
	Tag       string
	Message   string
	Tagger    string
	Timestamp int64
	Timezone  string
}

// BuildTag builds an annotated tag object's bytes.
func BuildTag(fields TagFields) ([]byte, error) {
	if !IsOid(fields.Object) {
		return nil, errors.New("buildTag needs a valid object id")
	}
	tz := fields.Timezone
	if tz == "" {
		tz = "+0000"
	}
	return []byte(fmt.Sprintf("object %s\ntype %s\ntag %s\ntagger %s %d %s\n\n%s",
		fields.Object, fields.Type, fields.Tag, fields.Tagger, fields.Timestamp, tz, fields.Message)), nil
}

// ---------------------------------------------------------------------------------------------
// packfiles (version 2, no deltas on write; OFS/REF deltas resolved on read)
// ---------------------------------------------------------------------------------------------

var typeCode = map[ObjKind]int{KindCommit: 1, KindTree: 2, KindBlob: 3, KindTag: 4}
var codeType = map[int]ObjKind{1: KindCommit, 2: KindTree, 3: KindBlob, 4: KindTag}

// packObjectHeader encodes one object's pack header: the type/size varint git uses.
func packObjectHeader(kind ObjKind, size int) []byte {
	code := typeCode[kind]
	var out []byte
	remaining := size
	first := (code << 4) | (remaining & 0x0f)
	remaining /= 16
	for remaining > 0 {
		out = append(out, byte(first|0x80))
		first = remaining & 0x7f
		remaining /= 128
	}
	out = append(out, byte(first))
	return out
}

func deflate(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	w := zlib.NewWriter(&buf)
	if _, err := w.Write(data); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// WritePack builds a packfile from plain objects (no deltas). The objects are sorted
// by id first, so the same set always builds the same pack bytes.
func WritePack(objects []GitObj) ([]byte, error) {
	type keyed struct {
		oid string
		obj GitObj
	}
	keyedObjs := make([]keyed, len(objects))
	for i, object := range objects {
		keyedObjs[i] = keyed{oid: OidOf(object.Kind, object.Bytes), obj: object}
	}
	sort.SliceStable(keyedObjs, func(i, j int) bool { return keyedObjs[i].oid < keyedObjs[j].oid })
	var body bytes.Buffer
	body.WriteString("PACK")
	var tmp [4]byte
	putU32(tmp[:], 2)
	body.Write(tmp[:])
	putU32(tmp[:], uint32(len(keyedObjs)))
	body.Write(tmp[:])
	for _, keyedObj := range keyedObjs {
		object := keyedObj.obj
		body.Write(packObjectHeader(object.Kind, len(object.Bytes)))
		deflated, err := deflate(object.Bytes)
		if err != nil {
			return nil, err
		}
		body.Write(deflated)
	}
	h := sha1.New()
	h.Write(body.Bytes())
	trailer := h.Sum(nil)
	out := append(body.Bytes(), trailer...)
	return out, nil
}

func putU32(b []byte, v uint32) {
	b[0] = byte(v >> 24)
	b[1] = byte(v >> 16)
	b[2] = byte(v >> 8)
	b[3] = byte(v)
}

func readU32(b []byte) uint32 {
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

// PackChecksum is the SHA-1 of every pack byte before the 20-byte trailer.
func PackChecksum(pack []byte) (string, error) {
	if len(pack) < 20 {
		return "", errors.New("pack is shorter than its trailer")
	}
	h := sha1.New()
	h.Write(pack[:len(pack)-20])
	return hex.EncodeToString(h.Sum(nil)), nil
}

// VerifyPack reports whether a pack's trailer matches its bytes.
func VerifyPack(pack []byte) bool {
	if len(pack) < 32 {
		return false
	}
	if string(pack[:4]) != "PACK" {
		return false
	}
	trailer := hex.EncodeToString(pack[len(pack)-20:])
	sum, err := PackChecksum(pack)
	if err != nil {
		return false
	}
	return trailer == sum
}

func readPackVarint(buf []byte, start int) (size int, next int, first byte, err error) {
	if start >= len(buf) {
		return 0, 0, 0, errors.New("pack entry has no size header")
	}
	b := buf[start]
	first = b
	size = int(b & 0x0f)
	shift := uint(4)
	at := start + 1
	for b&0x80 != 0 {
		if at >= len(buf) {
			return 0, 0, 0, errors.New("pack entry size varint is truncated")
		}
		b = buf[at]
		at++
		size += int(b&0x7f) << shift
		shift += 7
	}
	return size, at, first, nil
}

func readOfsBase(buf []byte, start int) (baseOffset int, next int, err error) {
	if start >= len(buf) {
		return 0, 0, errors.New("ofs-delta has no base offset")
	}
	b := buf[start]
	at := start + 1
	value := int(b & 0x7f)
	for b&0x80 != 0 {
		if at >= len(buf) {
			return 0, 0, errors.New("ofs-delta base offset is truncated")
		}
		b = buf[at]
		at++
		value = (value+1)*128 + int(b&0x7f)
	}
	return value, at, nil
}

// pooledInflate reuses one zlib reader (with its 32 KiB flate window) across pack
// entries, so unpacking a pack stops allocating a fresh decompressor per object.
// The bytes.Reader is reused too; the output buffer is always fresh (the entry's
// declared size, which the object store keeps).
type pooledInflate struct {
	br *bytes.Reader
	zr io.ReadCloser
}

var inflatePool = sync.Pool{New: func() any { return &pooledInflate{br: bytes.NewReader(nil)} }}

// inflateAt inflates one zlib stream at offset; it returns the bytes and the offset
// just past the stream. The reader counts bytes exactly (bytes.Reader is a
// io.ByteReader, so the flate reader never reads past the stream's end). The
// output buffer is sized to the entry's declared size, so no growing copy happens;
// a final read forces the stream to EOF, consuming and verifying the adler32,
// before the consumed count is taken.
func inflateAt(buf []byte, offset int, size int) ([]byte, int, error) {
	p := inflatePool.Get().(*pooledInflate)
	defer inflatePool.Put(p)
	p.br.Reset(buf[offset:])
	var err error
	if p.zr == nil {
		p.zr, err = zlib.NewReader(p.br)
	} else if rs, ok := p.zr.(zlib.Resetter); ok {
		err = rs.Reset(p.br, nil)
	} else {
		p.zr, err = zlib.NewReader(p.br)
	}
	if err != nil {
		return nil, 0, fmt.Errorf("zlib stream at offset %d did not inflate: %v", offset, err)
	}
	data := make([]byte, size)
	if _, err := io.ReadFull(p.zr, data); err != nil {
		return nil, 0, fmt.Errorf("zlib stream at offset %d did not inflate: %v", offset, err)
	}
	var one [1]byte
	if _, err := p.zr.Read(one[:]); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("zlib stream at offset %d produced more than %d bytes", offset, size)
		}
		return nil, 0, fmt.Errorf("zlib stream at offset %d did not inflate: %v", offset, err)
	}
	if err := p.zr.Close(); err != nil {
		return nil, 0, fmt.Errorf("zlib stream at offset %d did not inflate: %v", offset, err)
	}
	consumed := len(buf[offset:]) - p.br.Len()
	if consumed == 0 {
		return nil, 0, fmt.Errorf("zlib stream at offset %d consumed no bytes", offset)
	}
	return data, offset + consumed, nil
}

// ApplyDelta applies a git delta (copy/insert instruction stream) to a base object's bytes.
func ApplyDelta(base []byte, delta []byte) ([]byte, error) {
	at := 0
	readSize := func() (int, error) {
		value := 0
		shift := uint(0)
		for {
			if at >= len(delta) {
				return 0, errors.New("delta size varint is truncated")
			}
			b := delta[at]
			at++
			value += int(b&0x7f) << shift
			shift += 7
			if b&0x80 == 0 {
				break
			}
		}
		return value, nil
	}
	sourceSize, err := readSize()
	if err != nil {
		return nil, err
	}
	targetSize, err := readSize()
	if err != nil {
		return nil, err
	}
	if sourceSize != len(base) {
		return nil, fmt.Errorf("delta expects a %d-byte base, got %d", sourceSize, len(base))
	}
	out := make([]byte, targetSize)
	written := 0
	for at < len(delta) {
		op := delta[at]
		at++
		if op&0x80 != 0 {
			copyOffset := 0
			copySize := 0
			shift := uint(0)
			for bit := 0; bit < 4; bit++ {
				if op&(1<<uint(bit)) != 0 {
					if at >= len(delta) {
						return nil, errors.New("delta copy is truncated")
					}
					copyOffset += int(delta[at]) << shift
					at++
				}
				shift += 8
			}
			shift = 0
			for bit := 4; bit < 7; bit++ {
				if op&(1<<uint(bit)) != 0 {
					if at >= len(delta) {
						return nil, errors.New("delta copy is truncated")
					}
					copySize += int(delta[at]) << shift
					at++
				}
				shift += 8
			}
			if copySize == 0 {
				copySize = 0x10000
			}
			if copyOffset+copySize > len(base) {
				return nil, errors.New("delta copy is out of the base")
			}
			if written+copySize > targetSize {
				return nil, errors.New("delta writes past its target size")
			}
			copy(out[written:written+copySize], base[copyOffset:copyOffset+copySize])
			written += copySize
		} else if op != 0 {
			literal := int(op & 0x7f)
			if at+literal > len(delta) {
				return nil, errors.New("delta insert is truncated")
			}
			if written+literal > targetSize {
				return nil, errors.New("delta writes past its target size")
			}
			copy(out[written:written+literal], delta[at:at+literal])
			at += literal
			written += literal
		} else {
			return nil, errors.New("delta opcode 0 is invalid")
		}
	}
	if written != targetSize {
		return nil, fmt.Errorf("delta produced %d of %d bytes", written, targetSize)
	}
	return out, nil
}

// PackRead is a parsed packfile with every object resolved.
type PackRead struct {
	Version       int
	DeclaredCount int
	Objects       map[string]GitObj
	DeltaCount    int
}

// ReadPack reads a packfile (version 2 or 3) and resolves every object, including
// OFS_DELTA and REF_DELTA. resolveBase supplies a base object a thin pack leaves out.
// Every object's SHA-1 is recomputed; a mismatch is an error.
func ReadPack(pack []byte, resolveBase func(oid string) *GitObj) (*PackRead, error) {
	if len(pack) < 32 {
		return nil, fmt.Errorf("pack is %d bytes, too short", len(pack))
	}
	if string(pack[:4]) != "PACK" {
		return nil, errors.New("pack does not start with PACK")
	}
	version := int(readU32(pack[4:8]))
	if version != 2 && version != 3 {
		return nil, fmt.Errorf("unsupported pack version %d", version)
	}
	declaredCount := int(readU32(pack[8:12]))
	trailerStart := len(pack) - 20
	sum, err := PackChecksum(pack)
	if err != nil {
		return nil, err
	}
	if hex.EncodeToString(pack[trailerStart:]) != sum {
		return nil, errors.New("pack trailer does not match its bytes")
	}

	type rawEntry struct {
		offset     int
		typeCode   int
		size       int
		data       []byte
		baseOffset int // for ofs-delta: absolute offset of the base
		baseOid    string
		resolved   *GitObj
		oid        string
	}
	var raw []*rawEntry
	byOffset := make(map[int]*rawEntry)
	at := 12
	for index := 0; index < declaredCount; index++ {
		if at >= trailerStart {
			return nil, fmt.Errorf("pack entry %d starts at %d, past the trailer at %d", index, at, trailerStart)
		}
		entryOffset := at
		size, next, first, err := readPackVarint(pack, at)
		if err != nil {
			return nil, err
		}
		at = next
		typeCode := (int(first) >> 4) & 0x07
		entry := &rawEntry{offset: entryOffset, typeCode: typeCode, size: size}
		if typeCode == 6 {
			base, next, err := readOfsBase(pack, at)
			if err != nil {
				return nil, err
			}
			at = next
			if base <= 0 || base > entryOffset {
				return nil, fmt.Errorf("ofs-delta at %d names an invalid base offset %d", entryOffset, base)
			}
			entry.baseOffset = entryOffset - base
		} else if typeCode == 7 {
			if at+20 > trailerStart {
				return nil, errors.New("ref-delta base id is truncated")
			}
			entry.baseOid = hex.EncodeToString(pack[at : at+20])
			at += 20
		} else if _, ok := codeType[typeCode]; !ok {
			return nil, fmt.Errorf("pack entry %d has unknown type code %d", index, typeCode)
		}
		inflated, nextOff, err := inflateAt(pack[:trailerStart], at, size)
		if err != nil {
			return nil, err
		}
		entry.data = inflated
		at = nextOff
		raw = append(raw, entry)
		byOffset[entryOffset] = entry
	}
	if at != trailerStart {
		return nil, fmt.Errorf("pack has %d stray bytes before its trailer", trailerStart-at)
	}

	objects := make(map[string]GitObj)
	resolvedByOid := make(map[string]*rawEntry)
	resolving := make(map[int]bool)
	deltaCount := 0
	var resolve func(entry *rawEntry) (*GitObj, string, error)
	resolve = func(entry *rawEntry) (*GitObj, string, error) {
		if entry.resolved != nil && entry.oid != "" {
			return entry.resolved, entry.oid, nil
		}
		if resolving[entry.offset] {
			return nil, "", fmt.Errorf("delta cycle at pack offset %d", entry.offset)
		}
		resolving[entry.offset] = true
		var kind ObjKind
		var data []byte
		var err error
		switch entry.typeCode {
		case 6:
			baseEntry, ok := byOffset[entry.baseOffset]
			if !ok {
				return nil, "", fmt.Errorf("ofs-delta base at offset %d is not in the pack", entry.baseOffset)
			}
			base, _, err := resolve(baseEntry)
			if err != nil {
				return nil, "", err
			}
			kind = base.Kind
			data, err = ApplyDelta(base.Bytes, entry.data)
			if err != nil {
				return nil, "", err
			}
		case 7:
			wanted := entry.baseOid
			if baseEntry, ok := resolvedByOid[wanted]; ok {
				base, _, err := resolve(baseEntry)
				if err != nil {
					return nil, "", err
				}
				kind = base.Kind
				data, err = ApplyDelta(base.Bytes, entry.data)
				if err != nil {
					return nil, "", err
				}
			} else {
				var external *GitObj
				if resolveBase != nil {
					external = resolveBase(wanted)
				}
				if external == nil {
					return nil, "", fmt.Errorf("ref-delta base %s is neither in the pack nor supplied", wanted)
				}
				kind = external.Kind
				data, err = ApplyDelta(external.Bytes, entry.data)
				if err != nil {
					return nil, "", err
				}
			}
		default:
			kind = codeType[entry.typeCode]
			data = entry.data
		}
		oid := OidOf(kind, data)
		entry.resolved = &GitObj{Kind: kind, Bytes: data}
		entry.oid = oid
		resolvedByOid[oid] = entry
		delete(resolving, entry.offset)
		return entry.resolved, oid, nil
	}
	for _, entry := range raw {
		resolved, oid, err := resolve(entry)
		if err != nil {
			return nil, err
		}
		if entry.typeCode == 6 || entry.typeCode == 7 {
			deltaCount++
		}
		if _, ok := objects[oid]; !ok {
			objects[oid] = *resolved
		}
	}
	return &PackRead{Version: version, DeclaredCount: declaredCount, Objects: objects, DeltaCount: deltaCount}, nil
}

// ---------------------------------------------------------------------------------------------
// reachability and tree paths
// ---------------------------------------------------------------------------------------------

// ObjectLookup looks up an object by id.
type ObjectLookup func(oid string) *GitObj

// CollectReachable returns every object reachable from roots: commit → tree + parents,
// tree → entries (gitlinks skipped), tag → its target, blob → nothing. It throws on a
// missing object.
func CollectReachable(roots []string, lookup ObjectLookup) (map[string]bool, error) {
	seen := make(map[string]bool)
	var queue []string
	for _, root := range roots {
		if IsOid(root) {
			queue = append(queue, root)
		}
	}
	for len(queue) > 0 {
		oid := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if seen[oid] {
			continue
		}
		object := lookup(oid)
		if object == nil {
			return nil, fmt.Errorf("object %s is missing from the store", oid)
		}
		seen[oid] = true
		switch object.Kind {
		case KindCommit:
			commit, err := ParseCommit(object.Bytes)
			if err != nil {
				return nil, err
			}
			queue = append(queue, commit.Tree)
			queue = append(queue, commit.Parents...)
		case KindTag:
			tag, err := ParseTag(object.Bytes)
			if err != nil {
				return nil, err
			}
			queue = append(queue, tag.Object)
		case KindTree:
			entries, err := ParseTree(object.Bytes)
			if err != nil {
				return nil, err
			}
			for _, entry := range entries {
				if entry.Mode == "160000" {
					continue
				}
				queue = append(queue, entry.Oid)
			}
		}
	}
	return seen, nil
}

// WalkCommits returns the commit-generation walk from roots: commit ids in
// newest-first order (parents after).
func WalkCommits(roots []string, lookup ObjectLookup) []string {
	seen := make(map[string]bool)
	var order []string
	var queue []string
	for _, root := range roots {
		if IsOid(root) {
			queue = append(queue, root)
		}
	}
	for i := 0; i < len(queue); i++ {
		oid := queue[i]
		if seen[oid] {
			continue
		}
		object := lookup(oid)
		if object == nil || object.Kind != KindCommit {
			continue
		}
		seen[oid] = true
		order = append(order, oid)
		commit, err := ParseCommit(object.Bytes)
		if err != nil {
			continue
		}
		queue = append(queue, commit.Parents...)
	}
	return order
}

// IsAncestor reports whether ancestor is reachable from tip (or equal to it).
func IsAncestor(ancestor string, tip string, lookup ObjectLookup) bool {
	if ancestor == tip {
		return true
	}
	if ancestor == ZERO_OID {
		return true
	}
	seen := make(map[string]bool)
	queue := []string{tip}
	for len(queue) > 0 {
		oid := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		object := lookup(oid)
		if object == nil || object.Kind != KindCommit {
			continue
		}
		commit, err := ParseCommit(object.Bytes)
		if err != nil {
			continue
		}
		for _, parent := range commit.Parents {
			if parent == ancestor {
				return true
			}
			queue = append(queue, parent)
		}
	}
	return false
}

// PathLookup is a resolved path inside a tree.
type PathLookup struct {
	Kind string // "blob" or "tree"
	Oid  string
}

// LookupPath resolves a slash path inside a tree object; it returns nil when absent.
func LookupPath(lookup ObjectLookup, treeOid string, path string) *PathLookup {
	segments := splitPath(path)
	if len(segments) == 0 {
		return &PathLookup{Kind: "tree", Oid: treeOid}
	}
	current := treeOid
	for index, segment := range segments {
		object := lookup(current)
		if object == nil || object.Kind != KindTree {
			return nil
		}
		entries, err := ParseTree(object.Bytes)
		if err != nil {
			return nil
		}
		var entry *TreeEntry
		for i := range entries {
			if entries[i].Name == segment {
				entry = &entries[i]
				break
			}
		}
		if entry == nil {
			return nil
		}
		if index == len(segments)-1 {
			kind := "blob"
			if entry.Mode == "40000" {
				kind = "tree"
			}
			return &PathLookup{Kind: kind, Oid: entry.Oid}
		}
		if entry.Mode != "40000" {
			return nil
		}
		current = entry.Oid
	}
	return nil
}

func splitPath(path string) []string {
	parts := strings.Split(path, "/")
	out := []string{}
	for _, part := range parts {
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

// SetPath returns a new tree oid with path set to blobOid (or removed when blobOid is
// ""), building the intermediate trees. put stores each new object and answers its
// oid. The input tree may be "" for an empty tree.
func SetPath(lookup ObjectLookup, put func(kind ObjKind, bytes []byte) string, treeOid string, path string, blobOid string) (string, error) {
	segments := splitPath(path)
	if len(segments) == 0 {
		return "", errors.New("setPath needs a non-empty path")
	}
	var build func(current string, depth int) (string, error)
	build = func(current string, depth int) (string, error) {
		name := segments[depth]
		var entries []TreeEntry
		if current != "" {
			object := lookup(current)
			if object != nil {
				parsed, err := ParseTree(object.Bytes)
				if err != nil {
					return "", err
				}
				entries = parsed
			}
		}
		existing := -1
		for i, entry := range entries {
			if entry.Name == name {
				existing = i
				break
			}
		}
		if depth == len(segments)-1 {
			if blobOid == "" {
				if existing >= 0 {
					entries = append(entries[:existing], entries[existing+1:]...)
				}
			} else if existing >= 0 {
				entries[existing] = TreeEntry{Mode: "100644", Name: name, Oid: blobOid}
			} else {
				entries = append(entries, TreeEntry{Mode: "100644", Name: name, Oid: blobOid})
			}
		} else {
			var childOid string
			if existing >= 0 && entries[existing].Mode == "40000" {
				childOid = entries[existing].Oid
			}
			next, err := build(childOid, depth+1)
			if err != nil {
				return "", err
			}
			if next == "" {
				if existing >= 0 {
					entries = append(entries[:existing], entries[existing+1:]...)
				}
			} else if existing >= 0 {
				entries[existing] = TreeEntry{Mode: "40000", Name: name, Oid: next}
			} else {
				entries = append(entries, TreeEntry{Mode: "40000", Name: name, Oid: next})
			}
		}
		if len(entries) == 0 {
			return "", nil
		}
		built, err := BuildTree(entries)
		if err != nil {
			return "", err
		}
		return put(KindTree, built), nil
	}
	oid, err := build(treeOid, 0)
	if err != nil {
		return "", err
	}
	if oid == "" {
		empty, err := BuildTree(nil)
		if err != nil {
			return "", err
		}
		return put(KindTree, empty), nil
	}
	return oid, nil
}

// FlattenTree flattens a tree into {path: blobOid} for every blob beneath it.
func FlattenTree(lookup ObjectLookup, treeOid string, prefix string) map[string]string {
	out := make(map[string]string)
	object := lookup(treeOid)
	if object == nil || object.Kind != KindTree {
		return out
	}
	entries, err := ParseTree(object.Bytes)
	if err != nil {
		return out
	}
	for _, entry := range entries {
		path := entry.Name
		if prefix != "" {
			path = prefix + "/" + entry.Name
		}
		if entry.Mode == "40000" {
			for child, oid := range FlattenTree(lookup, entry.Oid, path) {
				out[child] = oid
			}
		} else {
			out[path] = entry.Oid
		}
	}
	return out
}
