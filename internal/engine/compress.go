package engine

import (
	"bufio"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Compression handling (JMeter parity): Load Meter, not Go's transport,
// negotiates and decodes content codings, so bodies are decoded the same way
// whether or not the scenario sets Accept-Encoding (JMX/HAR imports keep
// recorded headers), and BytesIn always counts bytes on the wire.

// decodable reports codings Load Meter can decode with the standard library.
func decodable(coding string) bool {
	switch coding {
	case "gzip", "x-gzip", "deflate", "identity":
		return true
	}
	return false
}

// negotiateEncoding adjusts Accept-Encoding in auto mode. Without a header it
// asks for gzip (what Go's transport did implicitly). An explicit header keeps
// its gzip/deflate/identity entries and drops codings that cannot be decoded
// (br, zstd, compress), so responses stay readable for extractors; the
// returned note says what was removed.
func negotiateEncoding(req *http.Request) string {
	if req.Method == http.MethodHead || req.Header.Get("Range") != "" {
		return ""
	}
	ae := req.Header.Values("Accept-Encoding")
	if len(ae) == 0 {
		req.Header.Set("Accept-Encoding", "gzip")
		return ""
	}
	var keep, dropped []string
	for _, v := range ae {
		for _, part := range strings.Split(v, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			name := strings.ToLower(strings.TrimSpace(strings.SplitN(part, ";", 2)[0]))
			if decodable(name) || name == "*" {
				keep = append(keep, part)
			} else {
				dropped = append(dropped, name)
			}
		}
	}
	if len(dropped) == 0 {
		return ""
	}
	if len(keep) == 0 {
		keep = []string{"identity"}
	}
	req.Header.Set("Accept-Encoding", strings.Join(keep, ", "))
	return "Accept-Encoding: removed " + strings.Join(dropped, ", ") + " (no decoder in Load Meter)"
}

type countReader struct {
	r io.Reader
	n int64
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// bodyResult describes one response body read.
type bodyResult struct {
	body      []byte // decoded, at most maxBody bytes
	wire      int64  // body bytes received on the wire
	decoded   int64  // decoded body bytes (lower bound when truncated)
	truncated bool   // decoding stopped at maxBody (zip-bomb guard)
	encoding  string // Content-Encoding that was decoded
	note      string
}

// readBody reads the whole response body from the wire, decoding gzip and
// deflate when decode is set. Decoding stops at maxBody decoded bytes (the
// rest of the wire body is still drained, undecoded, so the connection can be
// reused and the bandwidth is measured), which bounds memory and CPU for
// highly compressed payloads.
func readBody(resp *http.Response, maxBody int64, decode bool) (bodyResult, error) {
	var br bodyResult
	cr := &countReader{r: resp.Body}
	defer resp.Body.Close()

	var codings []string
	for _, v := range resp.Header.Values("Content-Encoding") {
		for _, c := range strings.Split(v, ",") {
			if c = strings.ToLower(strings.TrimSpace(c)); c != "" && c != "identity" {
				codings = append(codings, c)
			}
		}
	}
	canDecode := decode && len(codings) > 0
	for _, c := range codings {
		if !decodable(c) {
			canDecode = false
		}
	}
	if decode && len(codings) > 0 && !canDecode {
		br.note = "response Content-Encoding " + strings.Join(codings, ", ") + " is not decoded (unsupported); body kept raw"
	}
	if !canDecode {
		var err error
		br.body, err = io.ReadAll(io.LimitReader(cr, maxBody))
		if err == nil {
			_, err = io.Copy(io.Discard, cr)
		}
		br.wire, br.decoded = cr.n, cr.n
		return br, err
	}

	// Peek for an empty body (HEAD, 204, 304 with a Content-Encoding header).
	src := bufio.NewReader(cr)
	if _, err := src.Peek(1); err != nil {
		br.wire = cr.n
		if errors.Is(err, io.EOF) {
			return br, nil
		}
		return br, err
	}
	br.encoding = strings.Join(codings, ", ")
	var r io.Reader = src
	// Codings are listed in the order applied: undo them in reverse.
	for i := len(codings) - 1; i >= 0; i-- {
		dr, err := decoder(codings[i], r)
		if err != nil {
			_, _ = io.Copy(io.Discard, cr)
			br.wire = cr.n
			return br, fmt.Errorf("%w (%s): %v", errDecompressed, codings[i], err)
		}
		r = dr
	}
	var err error
	br.body, err = io.ReadAll(io.LimitReader(r, maxBody))
	br.decoded = int64(len(br.body))
	if err != nil {
		_, _ = io.Copy(io.Discard, cr)
		br.wire = cr.n
		if isNetErr(err) {
			return br, err
		}
		return br, fmt.Errorf("%w (%s): %v", errDecompressed, br.encoding, err)
	}
	if br.decoded == maxBody {
		var one [1]byte
		if n, _ := r.Read(one[:]); n > 0 {
			br.truncated = true
			br.note = fmt.Sprintf("decoded body exceeds maxBodyBytes (%d); decoding stopped there", maxBody)
		}
	}
	_, err = io.Copy(io.Discard, cr) // drain the rest of the wire body (undecoded)
	br.wire = cr.n
	return br, err
}

func decoder(coding string, r io.Reader) (io.Reader, error) {
	switch coding {
	case "gzip", "x-gzip":
		return gzip.NewReader(r)
	case "deflate":
		// RFC 9110 "deflate" is zlib-wrapped, but many servers send raw
		// DEFLATE; detect the zlib header like HttpClient does.
		b := bufio.NewReader(r)
		h, err := b.Peek(2)
		if err == nil && h[0]&0x0f == 8 && (uint16(h[0])<<8|uint16(h[1]))%31 == 0 {
			return zlib.NewReader(b)
		}
		return flate.NewReader(b), nil
	}
	return nil, fmt.Errorf("unsupported coding %q", coding)
}

// isNetErr distinguishes transport failures (timeouts, resets) from corrupt
// compressed data while reading a body.
func isNetErr(err error) bool {
	var ne interface{ Timeout() bool }
	if errors.As(err, &ne) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "connection reset") || strings.Contains(msg, "use of closed network connection")
}
