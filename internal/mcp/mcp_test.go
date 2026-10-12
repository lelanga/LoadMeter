package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lelanga/phoenix/internal/app"
	"github.com/lelanga/phoenix/internal/service"
)

type client struct {
	t      *testing.T
	w      io.WriteCloser
	msgs   chan map[string]any
	done   chan error
	nextID int
	svc    *service.Service
}

func newSvc(t *testing.T) *service.Service {
	t.Helper()
	t.Setenv("PHOENIX_HOME", t.TempDir())
	s, err := service.New(app.Workspace{Dir: filepath.Join(t.TempDir(), "ws")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func startStdio(t *testing.T) *client {
	t.Helper()
	svc := newSvc(t)
	srv := &Server{Svc: svc}
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	c := &client{t: t, w: inW, msgs: make(chan map[string]any, 256), done: make(chan error, 1), svc: svc}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		err := srv.Serve(ctx, inR, outW, io.Discard)
		outW.Close()
		c.done <- err
	}()
	go func() {
		sc := bufio.NewScanner(outR)
		sc.Buffer(make([]byte, 1<<20), 16<<20)
		for sc.Scan() {
			var m map[string]any
			if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
				t.Errorf("server wrote a non-JSON line to stdout: %q", sc.Text())
				continue
			}
			c.msgs <- m
		}
		close(c.msgs)
	}()
	t.Cleanup(func() {
		inW.Close()
		select {
		case <-c.done:
		case <-time.After(10 * time.Second):
			t.Error("Serve did not return after EOF")
		}
		cancel()
	})
	return c
}

func (c *client) sendRaw(line string) {
	c.t.Helper()
	if _, err := io.WriteString(c.w, line+"\n"); err != nil {
		c.t.Fatal(err)
	}
}

func (c *client) send(method string, params any) int {
	c.t.Helper()
	c.nextID++
	req := map[string]any{"jsonrpc": "2.0", "id": c.nextID, "method": method}
	if params != nil {
		req["params"] = params
	}
	b, _ := json.Marshal(req)
	c.sendRaw(string(b))
	return c.nextID
}

func (c *client) recv(timeout time.Duration) map[string]any {
	c.t.Helper()
	select {
	case m, ok := <-c.msgs:
		if !ok {
			c.t.Fatal("server closed output")
		}
		return m
	case <-time.After(timeout):
		c.t.Fatal("timed out waiting for a message")
	}
	return nil
}

// call sends a request and returns its response, skipping notifications.
func (c *client) call(method string, params any) map[string]any {
	c.t.Helper()
	id := c.send(method, params)
	for {
		m := c.recv(30 * time.Second)
		if m["id"] == nil {
			continue // notification
		}
		if m["id"] != float64(id) {
			c.t.Fatalf("unexpected response id %v (wanted %d): %v", m["id"], id, m)
		}
		return m
	}
}

func result(t *testing.T, m map[string]any) map[string]any {
	t.Helper()
	if m["error"] != nil {
		t.Fatalf("unexpected error: %v", m["error"])
	}
	r, _ := m["result"].(map[string]any)
	if r == nil {
		t.Fatalf("no result: %v", m)
	}
	return r
}

func errCode(m map[string]any) float64 {
	e, _ := m["error"].(map[string]any)
	if e == nil {
		return 0
	}
	c, _ := e["code"].(float64)
	return c
}

func TestInitialize(t *testing.T) {
	c := startStdio(t)
	r := result(t, c.call("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "t", "version": "1"}}))
	if r["protocolVersion"] != "2025-06-18" {
		t.Errorf("version echo: %v", r["protocolVersion"])
	}
	caps, _ := r["capabilities"].(map[string]any)
	if caps["tools"] == nil {
		t.Errorf("tools capability missing: %v", caps)
	}
	info, _ := r["serverInfo"].(map[string]any)
	if info["name"] == "" || info["version"] == "" {
		t.Errorf("serverInfo %v", info)
	}
	if s, _ := r["instructions"].(string); !strings.Contains(s, "confirm") {
		t.Errorf("instructions should explain the confirm rule: %q", s)
	}
	// each supported version is echoed; unknown/missing falls back to a supported one
	for _, v := range []string{"2024-11-05", "2025-03-26", "2025-11-25"} {
		if got := result(t, c.call("initialize", map[string]any{"protocolVersion": v}))["protocolVersion"]; got != v {
			t.Errorf("version %s => %v", v, got)
		}
	}
	for _, v := range []any{"1999-01-01", "", nil} {
		got, _ := result(t, c.call("initialize", map[string]any{"protocolVersion": v}))["protocolVersion"].(string)
		ok := false
		for _, s := range supported {
			ok = ok || s == got
		}
		if !ok {
			t.Errorf("fallback for %v => %q is not a supported version", v, got)
		}
	}
	// initialize without params at all
	if r := result(t, c.call("initialize", nil)); r["protocolVersion"] == nil {
		t.Error("initialize without params")
	}
}

func TestPingAndNotifications(t *testing.T) {
	c := startStdio(t)
	// notifications produce no response: the next message must be the ping reply
	c.sendRaw(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	c.sendRaw(`{"jsonrpc":"2.0","method":"notifications/whatever","params":{}}`)
	c.sendRaw("") // blank lines are ignored
	if r := result(t, c.call("ping", nil)); len(r) != 0 {
		t.Errorf("ping => %v", r)
	}
	select {
	case m := <-c.msgs:
		t.Errorf("unexpected extra message %v", m)
	case <-time.After(100 * time.Millisecond):
	}
	// string IDs are echoed verbatim
	c.sendRaw(`{"jsonrpc":"2.0","id":"abc-1","method":"ping"}`)
	if m := c.recv(5 * time.Second); m["id"] != "abc-1" {
		t.Errorf("string id: %v", m)
	}
}

func TestToolsList(t *testing.T) {
	c := startStdio(t)
	r := result(t, c.call("tools/list", nil))
	list, _ := r["tools"].([]any)
	if len(list) < 15 {
		t.Fatalf("only %d tools", len(list))
	}
	by := map[string]map[string]any{}
	for _, x := range list {
		tl := x.(map[string]any)
		by[tl["name"].(string)] = tl
		if tl["description"] == "" || tl["inputSchema"] == nil {
			t.Errorf("tool %v lacks description/schema", tl["name"])
		}
		sch := tl["inputSchema"].(map[string]any)
		if sch["type"] != "object" {
			t.Errorf("%v schema %v", tl["name"], sch)
		}
	}
	ann := func(name string) map[string]any {
		t.Helper()
		tl := by[name]
		if tl == nil {
			t.Fatalf("tool %s missing", name)
		}
		a, _ := tl["annotations"].(map[string]any)
		return a
	}
	if a := ann("run_test"); a["destructiveHint"] != true || a["readOnlyHint"] != false || a["openWorldHint"] != true {
		t.Errorf("run_test annotations %v", a)
	}
	if a := ann("delete_scenario"); a["destructiveHint"] != true {
		t.Errorf("delete_scenario annotations %v", a)
	}
	if a := ann("list_scenarios"); a["readOnlyHint"] != true || a["destructiveHint"] != false || a["openWorldHint"] != false {
		t.Errorf("list_scenarios annotations %v", a)
	}
	if a := ann("get_results"); a["readOnlyHint"] != true {
		t.Errorf("get_results annotations %v", a)
	}
	// the run_test schema documents the confirm flag and requires file
	props := by["run_test"]["inputSchema"].(map[string]any)["properties"].(map[string]any)
	if props["confirm"] == nil || props["file"] == nil {
		t.Errorf("run_test props %v", props)
	}
	if req, _ := by["run_test"]["inputSchema"].(map[string]any)["required"].([]any); len(req) != 1 || req[0] != "file" {
		t.Errorf("required %v", req)
	}
}

func toolText(t *testing.T, r map[string]any) string {
	t.Helper()
	content, _ := r["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("no content: %v", r)
	}
	first := content[0].(map[string]any)
	if first["type"] != "text" {
		t.Fatalf("content type %v", first["type"])
	}
	return first["text"].(string)
}

func TestToolsCall(t *testing.T) {
	c := startStdio(t)
	// list_scenarios on an empty workspace
	r := result(t, c.call("tools/call", map[string]any{"name": "list_scenarios", "arguments": map[string]any{}}))
	if r["isError"] != false {
		t.Fatalf("isError %v: %v", r["isError"], toolText(t, r))
	}
	var arr []any
	if err := json.Unmarshal([]byte(toolText(t, r)), &arr); err != nil || len(arr) != 0 {
		t.Errorf("list_scenarios text %q: %v", toolText(t, r), err)
	}
	// create_demo => structuredContent for object results
	r = result(t, c.call("tools/call", map[string]any{"name": "create_demo"}))
	if r["isError"] != false {
		t.Fatal(toolText(t, r))
	}
	sc, _ := r["structuredContent"].(map[string]any)
	if sc["file"] != "demo-shop.json" || !strings.HasPrefix(fmt.Sprint(sc["mockUrl"]), "http://127.0.0.1:") {
		t.Errorf("structuredContent %v", sc)
	}
	// now it lists one scenario
	r = result(t, c.call("tools/call", map[string]any{"name": "list_scenarios", "arguments": map[string]any{}}))
	json.Unmarshal([]byte(toolText(t, r)), &arr)
	if len(arr) != 1 {
		t.Errorf("scenarios %v", arr)
	}
	// get_scenario with a missing file => isError result (not a protocol error)
	r = result(t, c.call("tools/call", map[string]any{"name": "get_scenario", "arguments": map[string]any{"file": "nope.json"}}))
	if r["isError"] != true || !strings.HasPrefix(toolText(t, r), "Error: ") {
		t.Errorf("missing scenario: %v", r)
	}
	// argument validation errors are tool errors too
	r = result(t, c.call("tools/call", map[string]any{"name": "get_scenario", "arguments": map[string]any{"file": 7}}))
	if r["isError"] != true || !strings.Contains(toolText(t, r), "must be a string") {
		t.Errorf("type error: %v", r)
	}
	r = result(t, c.call("tools/call", map[string]any{"name": "get_scenario", "arguments": map[string]any{}}))
	if r["isError"] != true || !strings.Contains(toolText(t, r), "missing required") {
		t.Errorf("required: %v", r)
	}
}

func TestRunTestRequiresConfirm(t *testing.T) {
	c := startStdio(t)
	c.call("tools/call", map[string]any{"name": "create_demo"})
	for _, args := range []map[string]any{{"file": "demo-shop.json"}, {"file": "demo-shop.json", "confirm": false}} {
		r := result(t, c.call("tools/call", map[string]any{"name": "run_test", "arguments": args}))
		if r["isError"] != true {
			t.Fatalf("run_test without confirm must be an error: %v", r)
		}
		txt := toolText(t, r)
		if !strings.Contains(txt, "confirm") || !strings.Contains(txt, "human") {
			t.Errorf("confirmation message %q", txt)
		}
	}
	if info := c.svc.ActiveRun(); info.RunID != "" {
		t.Fatalf("a run started anyway: %+v", info)
	}
	r := result(t, c.call("tools/call", map[string]any{"name": "delete_scenario", "arguments": map[string]any{"file": "demo-shop.json"}}))
	if r["isError"] != true {
		t.Error("delete without confirm succeeded")
	}
	// with confirm the run starts (don't wait)
	r = result(t, c.call("tools/call", map[string]any{"name": "run_test", "arguments": map[string]any{"file": "demo-shop.json", "confirm": true, "vus": 1.0, "duration": "1s"}}))
	if r["isError"] != false {
		t.Fatalf("confirmed run failed: %s", toolText(t, r))
	}
	sc, _ := r["structuredContent"].(map[string]any)
	id, _ := sc["runId"].(string)
	if id == "" || sc["status"] != "running" {
		t.Fatalf("%v", sc)
	}
	// wait_for_run returns the finished summary
	r = result(t, c.call("tools/call", map[string]any{"name": "wait_for_run", "arguments": map[string]any{"runId": id, "timeoutSec": 30.0}}))
	if r["isError"] != false {
		t.Fatalf("wait_for_run: %s", toolText(t, r))
	}
	sum, _ := r["structuredContent"].(map[string]any)
	if sum["runId"] != id || sum["status"] != "finished" {
		t.Errorf("summary %v", sum["status"])
	}
}

func TestProgressNotifications(t *testing.T) {
	c := startStdio(t)
	c.call("tools/call", map[string]any{"name": "create_demo"})
	r := result(t, c.call("tools/call", map[string]any{"name": "run_test", "arguments": map[string]any{"file": "demo-shop.json", "confirm": true, "vus": 1.0, "duration": "2s"}}))
	id := r["structuredContent"].(map[string]any)["runId"].(string)
	reqID := c.send("tools/call", map[string]any{"name": "wait_for_run", "arguments": map[string]any{"runId": id},
		"_meta": map[string]any{"progressToken": "tok-1"}})
	var progress []float64
	for {
		m := c.recv(30 * time.Second)
		if m["method"] == "notifications/progress" {
			p := m["params"].(map[string]any)
			if p["progressToken"] != "tok-1" {
				t.Errorf("token %v", p["progressToken"])
			}
			progress = append(progress, p["progress"].(float64))
			continue
		}
		if m["id"] == float64(reqID) {
			if m["error"] != nil {
				t.Fatalf("%v", m["error"])
			}
			break
		}
	}
	if len(progress) == 0 {
		t.Fatal("no progress notifications")
	}
	for i := 1; i < len(progress); i++ {
		if progress[i] <= progress[i-1] {
			t.Errorf("progress must strictly increase: %v", progress)
		}
	}
}

func TestCancelledRequestGetsNoResponse(t *testing.T) {
	c := startStdio(t)
	c.call("tools/call", map[string]any{"name": "create_demo"})
	r := result(t, c.call("tools/call", map[string]any{"name": "run_test", "arguments": map[string]any{"file": "demo-shop.json", "confirm": true, "vus": 1.0, "duration": "20s"}}))
	id := r["structuredContent"].(map[string]any)["runId"].(string)
	waitID := c.send("tools/call", map[string]any{"name": "wait_for_run", "arguments": map[string]any{"runId": id}})
	time.Sleep(300 * time.Millisecond)
	c.sendRaw(fmt.Sprintf(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":%d,"reason":"user"}}`, waitID))
	// the cancelled request must not be answered; a later ping must be
	pingID := c.send("ping", nil)
	for {
		m := c.recv(10 * time.Second)
		if m["id"] == float64(waitID) {
			t.Fatalf("cancelled request was answered: %v", m)
		}
		if m["id"] == float64(pingID) {
			break
		}
	}
	c.svc.StopRun(id)
	a := c.svc.Active(id)
	select {
	case <-a.Run.Done():
	case <-time.After(15 * time.Second):
		t.Fatal("run did not stop")
	}
	a.Wait() // let the summary/report be written before the temp dir is removed
}

func TestProtocolErrors(t *testing.T) {
	c := startStdio(t)
	// unknown method
	m := c.call("does/not/exist", nil)
	if errCode(m) != -32601 || !strings.Contains(fmt.Sprint(m["error"]), "does/not/exist") {
		t.Errorf("unknown method: %v", m)
	}
	// parse error: id null
	c.sendRaw(`{this is not json`)
	pm := c.recv(5 * time.Second)
	if errCode(pm) != -32700 {
		t.Errorf("parse error: %v", pm)
	}
	if id, present := pm["id"]; !present || id != nil {
		t.Errorf("parse error response must carry id:null, got %v (present=%v)", id, present)
	}
	if pm["jsonrpc"] != "2.0" {
		t.Errorf("jsonrpc %v", pm["jsonrpc"])
	}
	// the server survives and still answers
	if r := result(t, c.call("ping", nil)); r == nil {
		t.Error("no ping after parse error")
	}
	// unknown tool => -32602
	m = c.call("tools/call", map[string]any{"name": "format_disk"})
	if errCode(m) != -32602 || !strings.Contains(fmt.Sprint(m["error"]), "unknown tool") {
		t.Errorf("unknown tool: %v", m)
	}
	// invalid params shapes
	m = c.call("tools/call", "not-an-object")
	if errCode(m) != -32602 {
		t.Errorf("invalid params: %v", m)
	}
	m = c.call("tools/call", map[string]any{"name": "list_scenarios", "arguments": "x"})
	if errCode(m) != -32602 {
		t.Errorf("invalid arguments: %v", m)
	}
	// logging/setLevel
	if errCode(c.call("logging/setLevel", map[string]any{"level": "loud"})) != -32602 {
		t.Error("invalid log level accepted")
	}
	result(t, c.call("logging/setLevel", map[string]any{"level": "warning"}))
	// no response is an error for a request with a JSON-RPC error AND result at once
	for _, bad := range []string{`{"jsonrpc":"2.0","id":99,"method":"tools/list"}`} {
		c.sendRaw(bad)
		mm := c.recv(5 * time.Second)
		if mm["error"] != nil && mm["result"] != nil {
			t.Errorf("both result and error: %v", mm)
		}
	}
}

func TestResourcesAndPrompts(t *testing.T) {
	c := startStdio(t)
	c.call("tools/call", map[string]any{"name": "create_demo"})
	r := result(t, c.call("resources/list", nil))
	res, _ := r["resources"].([]any)
	foundDemo := false
	for _, x := range res {
		if x.(map[string]any)["uri"] == "phoenix://scenarios/demo-shop.json" {
			foundDemo = true
		}
	}
	if !foundDemo {
		t.Errorf("scenario resource missing: %v", res)
	}
	r = result(t, c.call("resources/read", map[string]any{"uri": "phoenix://scenarios/demo-shop.json"}))
	contents, _ := r["contents"].([]any)
	if len(contents) != 1 || !strings.Contains(fmt.Sprint(contents[0]), "baseUrl") {
		t.Errorf("read: %v", r)
	}
	// traversal and unknown URIs are protocol errors, not file reads
	for _, uri := range []string{"phoenix://scenarios/../../etc/passwd", "phoenix://scenarios/%2e%2e%2fx.json", "file:///etc/passwd", "phoenix://runs/../x/summary", "phoenix://scenarios/missing.json", ""} {
		m := c.call("resources/read", map[string]any{"uri": uri})
		if m["error"] == nil {
			t.Errorf("resources/read %q succeeded: %v", uri, m)
		}
	}
	if r := result(t, c.call("resources/templates/list", nil)); r["resourceTemplates"] == nil {
		t.Error("templates")
	}
	if r := result(t, c.call("prompts/list", nil)); r["prompts"] == nil {
		t.Error("prompts")
	}
}

func TestEOFEndsServe(t *testing.T) {
	svc := newSvc(t)
	srv := &Server{Svc: svc}
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n" + `{"jsonrpc":"2.0","id":2,"method":"tools/list"}` + "\n")
	var out bytes.Buffer
	if err := srv.Serve(context.Background(), in, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 responses, got %d: %q", len(lines), out.String())
	}
	ids := map[string]bool{}
	for _, ln := range lines {
		var m map[string]any
		if err := json.Unmarshal([]byte(ln), &m); err != nil {
			t.Fatalf("non-JSON output %q", ln)
		}
		ids[fmt.Sprint(m["id"])] = true
	}
	if !ids["1"] || !ids["2"] {
		t.Errorf("ids %v", ids)
	}
}

// ---------- HTTP transport ----------

func TestHTTPTransportSecurity(t *testing.T) {
	svc := newSvc(t)
	srv := &Server{Svc: svc}
	const token = "0123456789abcdef0123456789abcdef"
	ts := httptest.NewServer(srv.HTTPHandler(token))
	defer ts.Close()
	post := func(body string, hdr map[string]string) (*http.Response, string) {
		req, _ := http.NewRequest("POST", ts.URL+"/mcp", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		for k, v := range hdr {
			if k == "Host" {
				req.Host = v
				continue
			}
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}
	ping := `{"jsonrpc":"2.0","id":1,"method":"ping"}`
	auth := map[string]string{"Authorization": "Bearer " + token}
	tests := []struct {
		name string
		hdr  map[string]string
		want int
	}{
		{"no auth", nil, 401},
		{"wrong token", map[string]string{"Authorization": "Bearer nope"}, 401},
		{"basic scheme", map[string]string{"Authorization": "Basic " + token}, 401},
		{"raw token without scheme", map[string]string{"Authorization": token}, 401},
		{"ok", auth, 200},
		{"foreign host", map[string]string{"Authorization": "Bearer " + token, "Host": "evil.example.com"}, 403},
		{"rebinding host", map[string]string{"Authorization": "Bearer " + token, "Host": "localhost.evil.com:80"}, 403},
		{"foreign origin", map[string]string{"Authorization": "Bearer " + token, "Origin": "https://evil.example.com"}, 403},
		{"loopback origin", map[string]string{"Authorization": "Bearer " + token, "Origin": "http://localhost:3000"}, 200},
	}
	for _, tc := range tests {
		resp, body := post(ping, tc.hdr)
		if resp.StatusCode != tc.want {
			t.Errorf("%s: %d (want %d) %s", tc.name, resp.StatusCode, tc.want, body)
		}
	}
	if resp, _ := post(ping, nil); resp.Header.Get("WWW-Authenticate") == "" {
		t.Error("401 should carry WWW-Authenticate")
	}
	// protocol-level cases over HTTP
	resp, body := post(`{nope`, auth)
	if resp.StatusCode != 400 || !strings.Contains(body, "-32700") {
		t.Errorf("parse error: %d %s", resp.StatusCode, body)
	}
	resp, body = post(`[{"jsonrpc":"2.0","id":1,"method":"ping"}]`, auth)
	if resp.StatusCode != 400 || !strings.Contains(body, "-32600") {
		t.Errorf("batch: %d %s", resp.StatusCode, body)
	}
	resp, _ = post(`{"jsonrpc":"2.0","method":"notifications/initialized"}`, auth)
	if resp.StatusCode != 202 {
		t.Errorf("notification: %d", resp.StatusCode)
	}
	resp, body = post(`{"jsonrpc":"2.0","id":3,"method":"nope"}`, auth)
	if resp.StatusCode != 200 || !strings.Contains(body, "-32601") {
		t.Errorf("unknown method: %d %s", resp.StatusCode, body)
	}
	// session handling
	resp, body = post(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`, auth)
	sid := resp.Header.Get("Mcp-Session-Id")
	if resp.StatusCode != 200 || sid == "" || !strings.Contains(body, "protocolVersion") {
		t.Fatalf("initialize: %d sid=%q %s", resp.StatusCode, sid, body)
	}
	withSid := map[string]string{"Authorization": "Bearer " + token, "Mcp-Session-Id": sid}
	if resp, _ := post(ping, withSid); resp.StatusCode != 200 {
		t.Errorf("valid session: %d", resp.StatusCode)
	}
	if resp, _ := post(ping, map[string]string{"Authorization": "Bearer " + token, "Mcp-Session-Id": "bogus"}); resp.StatusCode != 404 {
		t.Errorf("bogus session: %d", resp.StatusCode)
	}
	req, _ := http.NewRequest("DELETE", ts.URL+"/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Mcp-Session-Id", sid)
	if dr, _ := http.DefaultClient.Do(req); dr == nil || dr.StatusCode != 204 {
		t.Errorf("delete session: %v", dr)
	}
	if resp, _ := post(ping, withSid); resp.StatusCode != 404 {
		t.Errorf("ended session: %d", resp.StatusCode)
	}
	// GET is not supported
	greq, _ := http.NewRequest("GET", ts.URL+"/mcp", nil)
	greq.Header.Set("Authorization", "Bearer "+token)
	if gr, _ := http.DefaultClient.Do(greq); gr == nil || gr.StatusCode != 405 {
		t.Errorf("GET: %v", gr)
	}
	// tool call over HTTP, confirmation still enforced
	resp, body = post(`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"run_test","arguments":{"file":"x.json"}}}`, auth)
	if resp.StatusCode != 200 || !strings.Contains(body, `"isError":true`) {
		t.Errorf("run_test over HTTP: %d %s", resp.StatusCode, body)
	}
}

func TestValidateLoopbackAddrAndToken(t *testing.T) {
	good := []string{"127.0.0.1:0", "localhost:8080", "[::1]:9"}
	bad := []string{"0.0.0.0:8080", ":8080", "192.168.1.5:80", "example.com:80", "8080", ""}
	for _, a := range good {
		if err := ValidateLoopbackAddr(a); err != nil {
			t.Errorf("%q: %v", a, err)
		}
	}
	for _, a := range bad {
		if err := ValidateLoopbackAddr(a); err == nil {
			t.Errorf("%q accepted", a)
		}
	}
	t.Setenv("PHOENIX_HOME", t.TempDir())
	tok, err := EnsureToken()
	if err != nil || len(tok) < 32 {
		t.Fatalf("%q %v", tok, err)
	}
	again, _ := EnsureToken()
	if again != tok {
		t.Error("token must be stable")
	}
}
