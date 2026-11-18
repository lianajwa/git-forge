// rawhttp.go — hand-rolled HTTP/1.1 server on a raw TCP accept loop.
//
// The kit's undici client and the dev harness speak plain HTTP/1.1 with
// Content-Length bodies on keep-alive connections. net/http spends 60-70% of
// server CPU on the hot workloads (finishRequest/readRequest/syscalls in the
// pprof), so this file replaces it with a minimal parser that only extracts
// what dispatch needs: method, path, query, host, Authorization, Git-Protocol,
// Connection, Content-Length / Transfer-Encoding / Expect, and the body.
//
// Behaviour contract (must match the net/http adapter in server.go):
//   - origin-form and absolute-form targets; anything else is a 400.
//   - Content-Length bodies, chunked bodies (trailers skipped), Expect:
//     100-continue answered before the body is read.
//   - keep-alive by default on HTTP/1.1, close on `Connection: close` and on
//     HTTP/1.0 without `keep-alive`; one response per request, single Write.
//   - HEAD answers carry Content-Length of the would-be body but no body bytes
//     (what net/http does).
//   - a malformed request line or header answers 400 JSON and closes the
//     connection; the server keeps serving afterwards.
//   - dispatch panics are recovered per request as a 500 (the Forge lock is
//     released by serveParsed's deferred unlock).
// FORGE_NETHTTP=1 in main.go keeps the net/http server for A/B + fallback.

package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	rawReadTimeout  = 60 * time.Second
	rawWriteTimeout = 30 * time.Second
	maxRequestLine  = 8192
	maxHeaderLine   = 8192
	maxHeaderTotal  = 65536
)

// rawReq is the transport-neutral request: everything serveParsed needs.
type rawReq struct {
	method        string
	pathname      string
	queryString   string
	host          string
	authorization string
	gitProtocol   string
	keepAlive     bool
}

func serveRaw(listener net.Listener, s *forgeServer) error {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return err
		}
		go handleRawConn(conn, s)
	}
}

func handleRawConn(conn net.Conn, s *forgeServer) {
	defer conn.Close()
	br := bufio.NewReaderSize(conn, 4096)
	for {
		req, body, ok := readRawRequest(br, conn)
		if !ok {
			return
		}
		requestID := s.nextRequestID()
		result := func() (res dispatchResult) {
			defer func() {
				if rec := recover(); rec != nil {
					log.Printf("forge error: %v", rec)
					res = errResult(500, "Internal Server Error", map[string]string{"x-github-request-id": requestID})
				}
			}()
			return s.serveParsed(req.method, req.pathname, req.queryString, req.host, req.authorization, req.gitProtocol, body, requestID)
		}()
		if !writeRawResponse(conn, req.method, requestID, result, req.keepAlive) {
			return
		}
		if !req.keepAlive {
			return
		}
	}
}

// readRawRequest parses one request off the connection. It returns ok=false
// when the connection should be closed (EOF, timeout, malformed request —
// after answering 400 where the request line was readable).
func readRawRequest(br *bufio.Reader, conn net.Conn) (*rawReq, []byte, bool) {
	conn.SetReadDeadline(time.Now().Add(rawReadTimeout))
	line, err := readCappedLine(br, maxRequestLine)
	if err != nil {
		return nil, nil, false
	}
	// Request line: METHOD SP TARGET SP VERSION (exactly 3 fields).
	sp1 := strings.IndexByte(line, ' ')
	sp2 := -1
	if sp1 >= 0 {
		sp2 = strings.IndexByte(line[sp1+1:], ' ')
		if sp2 >= 0 {
			sp2 += sp1 + 1
		}
	}
	if sp1 <= 0 || sp2 <= sp1+1 || sp2 == len(line)-1 {
		writeRawError(conn, 400, "Bad Request")
		return nil, nil, false
	}
	method := line[:sp1]
	target := line[sp1+1 : sp2]
	version := line[sp2+1:]
	if version != "HTTP/1.1" && version != "HTTP/1.0" {
		writeRawError(conn, 400, "Bad Request")
		return nil, nil, false
	}
	if !isToken(method) {
		writeRawError(conn, 400, "Bad Request")
		return nil, nil, false
	}
	req := &rawReq{method: method, keepAlive: version == "HTTP/1.1"}
	pathname, queryString, host, ok := splitTarget(target)
	if !ok {
		writeRawError(conn, 400, "Bad Request")
		return nil, nil, false
	}
	req.pathname, req.queryString, req.host = pathname, queryString, host

	// Headers: only the ones dispatch needs are kept; the rest are skipped.
	var contentLength = -1
	chunked := false
	expectContinue := false
	total := 0
	for {
		hdr, err := readCappedLine(br, maxHeaderLine)
		if err != nil {
			return nil, nil, false
		}
		total += len(hdr) + 2
		if total > maxHeaderTotal {
			writeRawError(conn, 400, "Bad Request")
			return nil, nil, false
		}
		if hdr == "" {
			break
		}
		colon := strings.IndexByte(hdr, ':')
		if colon <= 0 {
			writeRawError(conn, 400, "Bad Request")
			return nil, nil, false
		}
		name := lowerASCII(strings.TrimRight(hdr[:colon], " \t"))
		value := strings.Trim(hdr[colon+1:], " \t")
		switch name {
		case "content-length":
			if contentLength < 0 {
				if n, err := strconv.Atoi(value); err == nil && n >= 0 {
					contentLength = n
				} else {
					writeRawError(conn, 400, "Bad Request")
					return nil, nil, false
				}
			}
		case "transfer-encoding":
			if strings.Contains(lowerASCII(value), "chunked") {
				chunked = true
			}
		case "expect":
			if strings.EqualFold(value, "100-continue") {
				expectContinue = true
			}
		case "connection":
			lv := lowerASCII(value)
			if strings.Contains(lv, "close") {
				req.keepAlive = false
			} else if strings.Contains(lv, "keep-alive") {
				req.keepAlive = true
			}
		case "authorization":
			if req.authorization == "" {
				req.authorization = value
			}
		case "git-protocol":
			if req.gitProtocol == "" {
				req.gitProtocol = value
			}
		case "host":
			if req.host == "" {
				req.host = value
			}
		}
	}

	if expectContinue {
		if _, err := conn.Write([]byte("HTTP/1.1 100 Continue\r\n\r\n")); err != nil {
			return nil, nil, false
		}
	}
	body, tooLarge, ok := readRawBody(br, conn, chunked, contentLength)
	if !ok {
		return nil, nil, false
	}
	if tooLarge {
		writeRawError(conn, 500, "Internal Server Error")
		return nil, nil, false
	}
	return req, body, true
}

// splitTarget splits a request target into path, query and (for absolute-form)
// host. Origin-form must start with "/"; "*" (OPTIONS *) becomes path "*".
func splitTarget(target string) (pathname, queryString, host string, ok bool) {
	if target == "*" {
		return "*", "", "", true
	}
	if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") {
		rest := target[strings.Index(target, "://")+3:]
		slash := strings.IndexByte(rest, '/')
		if slash < 0 {
			return "/", "", rest, true
		}
		host = rest[:slash]
		target = rest[slash:]
	}
	if len(target) == 0 || target[0] != '/' {
		return "", "", "", false
	}
	if i := strings.IndexByte(target, '?'); i >= 0 {
		return target[:i], target[i+1:], host, true
	}
	return target, "", host, true
}

// readCappedLine reads one CRLF- (or LF-) terminated line, at most max bytes
// excluding the terminator. The returned line has no trailing \r.
func readCappedLine(br *bufio.Reader, max int) (string, error) {
	var buf []byte
	for {
		frag, err := br.ReadSlice('\n')
		if len(frag) > 0 {
			if len(buf)+len(frag) > max+1 {
				return "", fmt.Errorf("line too long")
			}
			buf = append(buf, frag...)
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil {
			return "", err
		}
		line := string(buf)
		line = strings.TrimSuffix(line, "\n")
		line = strings.TrimSuffix(line, "\r")
		return line, nil
	}
}

// readRawBody reads the request body per the framing headers. Chunked wins
// over Content-Length (RFC 7230 §3.3.3). tooLarge reports a body over
// maxBodyBytes: the caller answers 500 and closes, matching the net/http path
// (which reads up to the limit, then answers 500).
func readRawBody(br *bufio.Reader, conn net.Conn, chunked bool, contentLength int) (body []byte, tooLarge bool, ok bool) {
	if chunked {
		return readChunkedBody(br, conn)
	}
	if contentLength < 0 {
		return []byte{}, false, true
	}
	if contentLength == 0 {
		return []byte{}, false, true
	}
	if contentLength > maxBodyBytes {
		// Drain up to the limit so the client sees the 500, then close.
		_, _ = io.CopyN(io.Discard, br, int64(maxBodyBytes)+1)
		return nil, true, true
	}
	body = make([]byte, contentLength)
	if _, err := io.ReadFull(br, body); err != nil {
		return nil, false, false
	}
	return body, false, true
}

func readChunkedBody(br *bufio.Reader, conn net.Conn) ([]byte, bool, bool) {
	var body []byte
	for {
		line, err := readCappedLine(br, maxHeaderLine)
		if err != nil {
			return nil, false, false
		}
		if i := strings.IndexByte(line, ';'); i >= 0 {
			line = line[:i]
		}
		size, err := strconv.ParseInt(strings.TrimSpace(line), 16, 64)
		if err != nil || size < 0 {
			writeRawError(conn, 400, "Bad Request")
			return nil, false, false
		}
		if size == 0 {
			// Trailers until an empty line.
			for {
				trailer, err := readCappedLine(br, maxHeaderLine)
				if err != nil || trailer == "" {
					return body, false, err == nil
				}
			}
		}
		if int64(len(body))+size > int64(maxBodyBytes)+1 {
			return nil, true, true
		}
		chunk := make([]byte, size+2)
		if _, err := io.ReadFull(br, chunk); err != nil {
			return nil, false, false
		}
		body = append(body, chunk[:size]...)
	}
}

// writeRawResponse serialises one dispatchResult in a single Write: status
// line, headers (content-type, request id, any result headers, content-length,
// connection when closing), then the body — omitted for HEAD, whose
// Content-Length still reports the would-be body size. Returns false when the
// write failed and the connection should be closed.
func writeRawResponse(conn net.Conn, method, requestID string, res dispatchResult, keepAlive bool) bool {
	reason := http.StatusText(res.status)
	if reason == "" {
		reason = "Status"
	}
	var buf bytes.Buffer
	buf.WriteString("HTTP/1.1 ")
	buf.WriteString(strconv.Itoa(res.status))
	buf.WriteString(" ")
	buf.WriteString(reason)
	buf.WriteString("\r\n")
	headers := map[string]string{}
	if res.contentType != "" {
		headers["content-type"] = res.contentType
	}
	if !res.omitRequestID {
		headers["x-github-request-id"] = requestID
	}
	for key, value := range res.headers {
		headers[lowerASCII(key)] = value
	}
	headers["content-length"] = strconv.Itoa(len(res.body))
	if !keepAlive {
		headers["connection"] = "close"
	}
	// Deterministic order for the known keys keeps responses byte-stable across
	// runs; any other result header (e.g. x-oauth-scopes) is emitted too.
	for _, key := range []string{"content-type", "x-github-request-id", "link", "x-oauth-scopes", "content-length", "connection"} {
		if value, ok := headers[key]; ok {
			buf.WriteString(key)
			buf.WriteString(": ")
			buf.WriteString(value)
			buf.WriteString("\r\n")
			delete(headers, key)
		}
	}
	for key, value := range headers {
		buf.WriteString(key)
		buf.WriteString(": ")
		buf.WriteString(value)
		buf.WriteString("\r\n")
	}
	buf.WriteString("\r\n")
	if method != "HEAD" && len(res.body) > 0 {
		buf.Write(res.body)
	}
	conn.SetWriteDeadline(time.Now().Add(rawWriteTimeout))
	_, err := conn.Write(buf.Bytes())
	return err == nil
}

// writeRawError answers a bare 400/500 with the same JSON shape dispatch
// returns, then the caller closes the connection.
func writeRawError(conn net.Conn, status int, message string) {
	body := mustJSON(NewOMap().Set("message", message))
	var buf bytes.Buffer
	buf.WriteString("HTTP/1.1 ")
	buf.WriteString(strconv.Itoa(status))
	buf.WriteString(" ")
	buf.WriteString(http.StatusText(status))
	buf.WriteString("\r\ncontent-type: ")
	buf.WriteString(jsonContentType)
	buf.WriteString("\r\ncontent-length: ")
	buf.WriteString(strconv.Itoa(len(body)))
	buf.WriteString("\r\nconnection: close\r\n\r\n")
	buf.Write(body)
	conn.SetWriteDeadline(time.Now().Add(rawWriteTimeout))
	_, _ = conn.Write(buf.Bytes())
}

func isToken(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c <= ' ' || c >= 0x7f || strings.IndexByte("()<>@,;:\\\"/[]?={}", c) >= 0 {
			return false
		}
	}
	return true
}

func lowerASCII(s string) string {
	hasUpper := false
	for i := 0; i < len(s); i++ {
		if s[i] >= 'A' && s[i] <= 'Z' {
			hasUpper = true
			break
		}
	}
	if !hasUpper {
		return s
	}
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}
