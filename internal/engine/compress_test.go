package engine

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lelanga/loadmeter/internal/model"
)

var bombOnce = sync.OnceValue(func() []byte { return gz(make([]byte, 16<<20)) }) // 16 MiB of zeros, ~16 KiB on the wire

func gz(b []byte) []byte {
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	w.Write(b)
	w.Close()
	return buf.Bytes()
}

// encServer serves a JSON body compressed per ?enc= and records Accept-Encoding.
type encServer struct {
	mu  sync.Mutex
	ae  []string
	url string
}

func newEncServer(t *testing.T) *encServer {
	t.Helper()
	es := &encServer{}
	payload := []byte(`{"token":"abc123","pad":"` + strings.Repeat("x", 4000) + `"}`)
	bomb := bombOnce()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		es.mu.Lock()
		es.ae = append(es.ae, r.Header.Get("Accept-Encoding"))
		es.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		var body []byte
		switch r.URL.Query().Get("enc") {
		case "gzip":
			w.Header().Set("Content-Encoding", "gzip")
			body = gz(payload)
		case "deflate-zlib":
			var buf bytes.Buffer
			zw := zlib.NewWriter(&buf)
			zw.Write(payload)
			zw.Close()
			w.Header().Set("Content-Encoding", "deflate")
			body = buf.Bytes()
		case "deflate-raw":
			var buf bytes.Buffer
			fw, _ := flate.NewWriter(&buf, flate.DefaultCompression)
			fw.Write(payload)
			fw.Close()
			w.Header().Set("Content-Encoding", "deflate")
			body = buf.Bytes()
		case "br":
			w.Header().Set("Content-Encoding", "br")
			body = []byte{0x0b, 0x02, 0x80} // opaque: not decodable
		case "corrupt":
			w.Header().Set("Content-Encoding", "gzip")
			body = append(gz(payload)[:20], bytes.Repeat([]byte{0xff}, 50)...)
		case "bomb":
			w.Header().Set("Content-Encoding", "gzip")
			body = bomb
		case "empty":
			w.Header().Set("Content-Encoding", "gzip")
			w.WriteHeader(204)
			return
		default:
			body = payload
		}
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	es.url = srv.URL
	return es
}

func (es *encServer) acceptEncodings() []string {
	es.mu.Lock()
	defer es.mu.Unlock()
	return append([]string(nil), es.ae...)
}

func tokenStep(name, path string) model.Step {
	st := withExtract(get(name, path), model.Extractor{Name: "tok", Type: model.ExJSONPath, Expr: "$.token", Required: true})
	return st
}

func TestCompressionExplicitAcceptEncoding(t *testing.T) {
	t.Parallel()
	es := newEncServer(t)
	for _, enc := range []string{"gzip", "deflate-zlib", "deflate-raw", ""} {
		rec := &recorder{}
		st := tokenStep("r", "/?enc="+enc)
		// Recorded JMX/HAR headers keep an explicit Accept-Encoding with br.
		st.Request.Headers = []model.Header{{Name: "Accept-Encoding", Value: "gzip, deflate, br"}}
		sc := newSc(es.url, perVU(1, 1), st)
		_, snap := runSc(t, sc, opts(rec))
		if snap.Requests.Errors != 0 {
			t.Fatalf("enc=%q: %v", enc, snap.Requests.ErrorKind)
		}
		ex := rec.all()[0]
		if ex.Extracted["tok"] != "abc123" {
			t.Errorf("enc=%q: body not decoded, extracted %v", enc, ex.Extracted)
		}
		if ex.BodyBytes < 4000 {
			t.Errorf("enc=%q: decoded bytes %d", enc, ex.BodyBytes)
		}
		if enc != "" && ex.WireBytes >= ex.BodyBytes {
			t.Errorf("enc=%q: wire bytes %d should be below decoded %d", enc, ex.WireBytes, ex.BodyBytes)
		}
		if enc == "" && ex.WireBytes != ex.BodyBytes {
			t.Errorf("identity: wire %d != body %d", ex.WireBytes, ex.BodyBytes)
		}
		if got := snap.Requests.BytesIn; got < ex.WireBytes || got > ex.WireBytes+1000 {
			t.Errorf("enc=%q: BytesIn %d must count wire bytes (%d) plus headers", enc, got, ex.WireBytes)
		}
	}
	for _, ae := range es.acceptEncodings() {
		if strings.Contains(ae, "br") || !strings.Contains(ae, "gzip") {
			t.Errorf("Accept-Encoding sent %q: br must be dropped, gzip kept", ae)
		}
	}
}

func TestCompressionDefaultsAndOff(t *testing.T) {
	t.Parallel()
	es := newEncServer(t)
	rec := &recorder{}
	sc := newSc(es.url, perVU(1, 1), tokenStep("r", "/?enc=gzip"))
	_, snap := runSc(t, sc, opts(rec))
	if snap.Requests.Errors != 0 || es.acceptEncodings()[0] != "gzip" {
		t.Fatalf("auto without header must ask for gzip and decode: %v %v", snap.Requests.ErrorKind, es.acceptEncodings())
	}
	if rec.all()[0].ContentEncoding != "gzip" {
		t.Errorf("content encoding %q", rec.all()[0].ContentEncoding)
	}
	// compression=off: nothing added, body kept raw.
	es2 := newEncServer(t)
	rec2 := &recorder{}
	sc = newSc(es2.url, perVU(1, 1), withExtract(get("r", "/?enc=gzip"), model.Extractor{Name: "tok", Type: model.ExRegex, Expr: `abc(\d+)`}))
	sc.Defaults.Compression = "off"
	runSc(t, sc, opts(rec2))
	if ae := es2.acceptEncodings()[0]; ae != "" {
		t.Errorf("off must not add Accept-Encoding, sent %q", ae)
	}
	if ex := rec2.all()[0]; ex.Extracted["tok"] != "" || ex.WireBytes != ex.BodyBytes {
		t.Errorf("off must keep the raw body: %+v", ex.Extracted)
	}
}

func TestCompressionUnsupportedCorruptEmpty(t *testing.T) {
	t.Parallel()
	es := newEncServer(t)
	rec := &recorder{}
	sc := newSc(es.url, perVU(1, 1), get("br", "/?enc=br"), get("corrupt", "/?enc=corrupt"), get("empty", "/?enc=empty"))
	_, snap := runSc(t, sc, opts(rec))
	br := rec.byStep("br")[0]
	if !br.OK || len(br.Notes) == 0 || !strings.Contains(br.Notes[0], "not decoded") {
		t.Errorf("br must stay raw with a note: ok=%v notes=%v", br.OK, br.Notes)
	}
	c := rec.byStep("corrupt")[0]
	if c.OK || snap.Requests.ErrorKind["decompression"] != 1 {
		t.Errorf("corrupt gzip must fail as decompression: %v %v", c.Error, snap.Requests.ErrorKind)
	}
	if e := rec.byStep("empty")[0]; !e.OK {
		t.Errorf("empty gzip body must be fine: %v", e.Error)
	}
}

func TestCompressionBombBounded(t *testing.T) {
	t.Parallel()
	es := newEncServer(t)
	rec := &recorder{}
	sc := newSc(es.url, perVU(1, 1), get("bomb", "/?enc=bomb"))
	sc.Defaults.MaxBodyBytes = 64 << 10
	start := time.Now()
	_, snap := runSc(t, sc, opts(rec))
	ex := rec.all()[0]
	if !ex.OK || ex.BodyBytes != 64<<10 {
		t.Fatalf("bomb: ok=%v decoded=%d err=%s", ex.OK, ex.BodyBytes, ex.Error)
	}
	if ex.WireBytes != int64(len(bombOnce())) {
		t.Errorf("all wire bytes must be drained and counted: %d", ex.WireBytes)
	}
	if len(ex.Notes) == 0 || !strings.Contains(strings.Join(ex.Notes, ";"), "maxBodyBytes") {
		t.Errorf("truncation must be noted: %v", ex.Notes)
	}
	if snap.Requests.BytesIn > 1<<20 {
		t.Errorf("BytesIn counts wire bytes, got %d", snap.Requests.BytesIn)
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("decoding was not bounded: %v", time.Since(start))
	}
}

func TestNegotiateEncoding(t *testing.T) {
	cases := []struct{ method, in, want string }{
		{"GET", "", "gzip"},
		{"GET", "gzip, deflate, br", "gzip, deflate"},
		{"GET", "br", "identity"},
		{"GET", "gzip;q=1.0, zstd;q=0.5, *;q=0", "gzip;q=1.0, *;q=0"},
		{"HEAD", "", ""},
	}
	for _, c := range cases {
		req, _ := http.NewRequest(c.method, "http://127.0.0.1/", nil)
		if c.in != "" {
			req.Header.Set("Accept-Encoding", c.in)
		}
		negotiateEncoding(req)
		if got := req.Header.Get("Accept-Encoding"); got != c.want {
			t.Errorf("%s %q -> %q want %q", c.method, c.in, got, c.want)
		}
	}
}

func TestReadBodyChainedCodings(t *testing.T) {
	inner := gz([]byte("hello world"))
	outer := gz(inner)
	resp := &http.Response{Header: http.Header{"Content-Encoding": {"gzip, gzip"}}, Body: io.NopCloser(bytes.NewReader(outer))}
	br, err := readBody(resp, 1<<20, true)
	if err != nil || string(br.body) != "hello world" || br.wire != int64(len(outer)) {
		t.Fatalf("%q %v wire=%d", br.body, err, br.wire)
	}
}
