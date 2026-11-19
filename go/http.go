// Raw HTTP/1.1 server: one goroutine per connection, pooled buffers, keep-alive.
//
// The workloads are latency-bound on a single connection (2,000 sequential
// reads) and throughput-bound on 50 concurrent clones, so the parser avoids
// allocations: the request line and headers are scanned out of a reusable
// buffer, header values are slices of that buffer, and responses are written
// from pooled byte slices with a single Write per response where possible.
package main

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

const (
	maxHeaderBytes = 64 << 10
	maxBodyBytes   = 64 << 20
)

// request is one parsed HTTP request. Header values point into buf; they are
// only valid until the next read on the connection.
type request struct {
	method  string
	path    string // raw path, query stripped
	rawPath string
	query   string
	proto   string

	// headers, lower-cased names, values trimmed. Small fixed arrays keep the
	// common case allocation-free.
	hdrNames [16]string
	hdrVals  [16][]byte
	hdrExtra map[string][]byte
	nhdr     int

	body     []byte
	bodyBuf  *[]byte

	keepAlive bool
	// scratch for handlers
	params [8]string
	nparam int
}

func (r *request) header(name string) []byte {
	for i := 0; i < r.nhdr; i++ {
		if r.hdrNames[i] == name {
			return r.hdrVals[i]
		}
	}
	if r.hdrExtra != nil {
		return r.hdrExtra[name]
	}
	return nil
}

func (r *request) headerString(name string) string {
	v := r.header(name)
	if v == nil {
		return ""
	}
	return string(v)
}

// response writer: headers are set by the handler, body is one []byte.
type response struct {
	status  int
	ctype   string
	headers [8][2]string
	nhdr    int
	body    []byte
	// noBody: write headers only (e.g. 204)
	noBody bool
	// bodyPool: body came from bufPool and must be returned
	bodyPool bool
}

func (w *response) setHeader(k, v string) {
	if w.nhdr < len(w.headers) {
		w.headers[w.nhdr][0], w.headers[w.nhdr][1] = k, v
		w.nhdr++
	}
}

var (
	bufPool = sync.Pool{New: func() any { b := make([]byte, 0, 64<<10); return &b }}
	reqPool = sync.Pool{New: func() any { return &request{} }}
	resPool = sync.Pool{New: func() any { return &response{} }}
)

func getBuf() *[]byte { return bufPool.Get().(*[]byte) }
func putBuf(b *[]byte) {
	if cap(*b) <= 1<<20 {
		*b = (*b)[:0]
		bufPool.Put(b)
	}
}

type server struct {
	store *Store
}

func newServer() *server { return &server{store: newStore()} }

func (s *server) serve(ln net.Listener) error {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		go s.handleConn(conn)
	}
}

func (s *server) handleConn(conn net.Conn) {
	defer conn.Close()
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
	}
	br := bufio.NewReaderSize(conn, 64<<10)
	bw := bufio.NewWriterSize(conn, 64<<10)
	for {
		req := reqPool.Get().(*request)
		*req = request{}
		ok, cerr := s.readRequest(br, req)
		if !ok {
			reqPool.Put(req)
			if cerr != nil {
				return
			}
			return
		}
		res := resPool.Get().(*response)
		*res = response{status: 200}
		s.dispatch(req, res)
		s.writeResponse(bw, req, res)
		resPool.Put(res)
		keep := req.keepAlive
		if req.bodyBuf != nil {
			putBuf(req.bodyBuf)
		}
		reqPool.Put(req)
		if !keep {
			return
		}
		if err := bw.Flush(); err != nil {
			return
		}
	}
}

// readRequest parses one request from br. Returns ok=false on EOF/close.
func (s *server) readRequest(br *bufio.Reader, req *request) (bool, error) {
	line, err := readLine(br)
	if err != nil {
		return false, err
	}
	if len(line) == 0 {
		// stray CRLF before the request line; read one more
		line, err = readLine(br)
		if err != nil {
			return false, err
		}
		if len(line) == 0 {
			return false, io.EOF
		}
	}
	// METHOD SP PATH SP PROTO
	sp1 := bytes.IndexByte(line, ' ')
	if sp1 < 0 {
		return false, errBadRequest
	}
	sp2 := bytes.IndexByte(line[sp1+1:], ' ')
	if sp2 < 0 {
		return false, errBadRequest
	}
	req.method = string(line[:sp1])
	req.rawPath = string(line[sp1+1 : sp1+1+sp2])
	req.proto = string(line[sp1+1+sp2+1:])
	req.keepAlive = true // HTTP/1.1 default
	if req.proto == "HTTP/1.0" {
		req.keepAlive = false
	}
	// split query
	if qi := strings.IndexByte(req.rawPath, '?'); qi >= 0 {
		req.path = req.rawPath[:qi]
		req.query = req.rawPath[qi+1:]
	} else {
		req.path = req.rawPath
	}
	// headers
	var contentLength = -1
	chunked := false
	for {
		hline, err := readLine(br)
		if err != nil {
			return false, err
		}
		if len(hline) == 0 {
			break
		}
		colon := bytes.IndexByte(hline, ':')
		if colon < 0 {
			continue
		}
		name := lowerASCII(hline[:colon])
		val := bytes.TrimLeft(hline[colon+1:], " \t")
		switch name {
		case "content-length":
			contentLength, _ = strconv.Atoi(string(val))
		case "transfer-encoding":
			if bytes.Contains(lowerASCIIBytes(val), []byte("chunked")) {
				chunked = true
			}
		case "connection":
			if bytes.Contains(lowerASCIIBytes(val), []byte("close")) {
				req.keepAlive = false
			}
		}
		valCopy := append([]byte(nil), val...)
		if req.nhdr < len(req.hdrNames) {
			req.hdrNames[req.nhdr] = name
			req.hdrVals[req.nhdr] = valCopy
			req.nhdr++
		} else {
			if req.hdrExtra == nil {
				req.hdrExtra = make(map[string][]byte)
			}
			req.hdrExtra[name] = valCopy
		}
	}
	// body
	if chunked {
		bp, body, err := readChunked(br)
		if err != nil {
			return false, err
		}
		req.body = body
		req.bodyBuf = bp
	} else if contentLength > 0 {
		if contentLength > maxBodyBytes {
			return false, errBadRequest
		}
		bp := getBuf()
		b := *bp
		if cap(b) < contentLength {
			b = make([]byte, 0, contentLength)
		}
		b = b[:contentLength]
		if _, err := io.ReadFull(br, b); err != nil {
			putBuf(bp)
			return false, err
		}
		req.body = b
		req.bodyBuf = bp
	}
	return true, nil
}

var errBadRequest = &httpError{status: 400, msg: "bad request"}

type httpError struct {
	status int
	msg    string
}

func (e *httpError) Error() string { return e.msg }

// readLine reads one CRLF-terminated line without the CRLF.
func readLine(br *bufio.Reader) ([]byte, error) {
	line, err := br.ReadSlice('\n')
	if err != nil {
		return nil, err
	}
	// trim trailing \r\n / \n
	if n := len(line); n > 0 && line[n-1] == '\n' {
		line = line[:n-1]
		if n > 1 && line[len(line)-1] == '\r' {
			line = line[:len(line)-1]
		}
	}
	return line, nil
}

func readChunked(br *bufio.Reader) (*[]byte, []byte, error) {
	bp := getBuf()
	b := *bp
	fail := func(err error) (*[]byte, []byte, error) {
		putBuf(bp)
		return nil, nil, err
	}
	for {
		line, err := readLine(br)
		if err != nil {
			return fail(err)
		}
		n, err := strconv.ParseInt(string(bytes.TrimSpace(line)), 16, 64)
		if err != nil {
			return fail(errBadRequest)
		}
		if n == 0 {
			// trailer headers until empty line
			for {
				tl, err := readLine(br)
				if err != nil {
					return fail(err)
				}
				if len(tl) == 0 {
					break
				}
			}
			break
		}
		if len(b)+int(n) > maxBodyBytes {
			return fail(errBadRequest)
		}
		start := len(b)
		b = append(b, make([]byte, int(n))...)
		if _, err := io.ReadFull(br, b[start:]); err != nil {
			return fail(err)
		}
		// trailing CRLF
		if _, err := br.Discard(2); err != nil {
			return fail(err)
		}
	}
	*bp = b
	return bp, b, nil
}

func lowerASCII(s []byte) string {
	b := make([]byte, len(s))
	for i, c := range s {
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		b[i] = c
	}
	return string(b)
}

func lowerASCIIBytes(s []byte) []byte {
	b := make([]byte, len(s))
	for i, c := range s {
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		b[i] = c
	}
	return b
}

var statusText = map[int]string{
	200: "OK", 201: "Created", 204: "No Content", 400: "Bad Request",
	401: "Unauthorized", 404: "Not Found", 405: "Method Not Allowed",
	409: "Conflict", 422: "Unprocessable Entity", 500: "Internal Server Error",
}

var reqIDCounter uint64

func atomicAdd(p *uint64, d uint64) uint64 { return atomic.AddUint64(p, d) }

func nextReqID() string {
	return "req-" + strconv.FormatUint(atomicAdd(&reqIDCounter, 1), 36)
}

func (s *server) writeResponse(bw *bufio.Writer, req *request, res *response) {
	text, ok := statusText[res.status]
	if !ok {
		text = "Status"
	}
	var sb strings.Builder
	sb.WriteString("HTTP/1.1 ")
	sb.WriteString(strconv.Itoa(res.status))
	sb.WriteByte(' ')
	sb.WriteString(text)
	sb.WriteString("\r\n")
	ct := res.ctype
	if ct == "" {
		ct = "application/json"
	}
	sb.WriteString("Content-Type: ")
	sb.WriteString(ct)
	sb.WriteString("\r\n")
	sb.WriteString("X-GitHub-Request-Id: ")
	sb.WriteString(nextReqID())
	sb.WriteString("\r\n")
	for i := 0; i < res.nhdr; i++ {
		sb.WriteString(res.headers[i][0])
		sb.WriteString(": ")
		sb.WriteString(res.headers[i][1])
		sb.WriteString("\r\n")
	}
	if res.noBody {
		sb.WriteString("Content-Length: 0\r\n")
	} else {
		sb.WriteString("Content-Length: ")
		sb.WriteString(strconv.Itoa(len(res.body)))
		sb.WriteString("\r\n")
	}
	if req.keepAlive {
		sb.WriteString("Connection: keep-alive\r\n")
	} else {
		sb.WriteString("Connection: close\r\n")
	}
	sb.WriteString("\r\n")
	_, _ = bw.WriteString(sb.String())
	if !res.noBody && len(res.body) > 0 {
		_, _ = bw.Write(res.body)
	}
}
