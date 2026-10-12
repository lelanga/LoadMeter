package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readJSON(t *testing.T, p string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("%s: %v", p, err)
	}
	return m
}

func TestInstallUninstallMerge(t *testing.T) {
	cases := []struct {
		client, goos, rel, key string
	}{
		{"claude-desktop", "linux", ".config/Claude/claude_desktop_config.json", "mcpServers"},
		{"claude-desktop", "darwin", "Library/Application Support/Claude/claude_desktop_config.json", "mcpServers"},
		{"claude-desktop", "windows", "AppData/Claude/claude_desktop_config.json", "mcpServers"},
		{"cursor", "linux", ".cursor/mcp.json", "mcpServers"},
		{"vscode", "linux", ".config/Code/User/mcp.json", "servers"},
		{"vscode", "darwin", "Library/Application Support/Code/User/mcp.json", "servers"},
		{"gemini", "linux", ".gemini/settings.json", "mcpServers"},
		{"windsurf", "linux", ".codeium/windsurf/mcp_config.json", "mcpServers"},
	}
	for _, c := range cases {
		t.Run(c.client+"-"+c.goos, func(t *testing.T) {
			home := t.TempDir()
			appdata := filepath.Join(home, "AppData")
			path := filepath.Join(home, filepath.FromSlash(c.rel))
			ws := t.TempDir()
			o := installOpts{client: c.client, exe: "/opt/phoenix bin/phoenix", workspace: ws, goos: c.goos, home: home, appdata: appdata, out: &bytes.Buffer{}}

			// fresh install creates the file
			if err := installClient(o, false); err != nil {
				t.Fatal(err)
			}
			m := readJSON(t, path)
			e := m[c.key].(map[string]any)["phoenix"].(map[string]any)
			if e["command"] != "/opt/phoenix bin/phoenix" {
				t.Errorf("command %v", e["command"])
			}
			args := e["args"].([]any)
			if len(args) != 3 || args[0] != "mcp" || args[2] != ws {
				t.Errorf("args %v", args)
			}
			if (c.client == "vscode") != (e["type"] == "stdio") {
				t.Errorf("type field wrong for %s: %v", c.client, e["type"])
			}
			if _, err := os.Stat(path + ".bak"); err == nil {
				t.Error("no backup expected for a new file")
			}

			// existing config with other keys and servers is preserved
			existing := `{"theme":"dark","nested":{"a":[1,2]},"` + c.key + `":{"other":{"command":"x","args":["y"]}}}`
			if err := os.WriteFile(path, []byte(existing), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := installClient(o, false); err != nil {
				t.Fatal(err)
			}
			bak, err := os.ReadFile(path + ".bak")
			if err != nil || string(bak) != existing {
				t.Fatalf("backup: %q %v", bak, err)
			}
			m = readJSON(t, path)
			srv := m[c.key].(map[string]any)
			if srv["other"] == nil || srv["phoenix"] == nil || m["theme"] != "dark" || m["nested"] == nil {
				t.Fatalf("merge lost data: %v", m)
			}
			// idempotent re-install
			if err := installClient(o, false); err != nil {
				t.Fatal(err)
			}
			// uninstall removes only phoenix
			if err := installClient(o, true); err != nil {
				t.Fatal(err)
			}
			m = readJSON(t, path)
			srv = m[c.key].(map[string]any)
			if srv["phoenix"] != nil || srv["other"] == nil || m["theme"] != "dark" {
				t.Fatalf("uninstall wrong: %v", m)
			}
			// uninstall again is a no-op
			if err := installClient(o, true); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestInstallDryRunAndErrors(t *testing.T) {
	home := t.TempDir()
	var out bytes.Buffer
	o := installOpts{client: "cursor", exe: "/x/phoenix", workspace: "/w", goos: "linux", home: home, out: &out, dryRun: true}
	if err := installClient(o, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".cursor")); err == nil {
		t.Error("dry run wrote files")
	}
	if !strings.Contains(out.String(), `"phoenix"`) {
		t.Errorf("dry run output: %s", out.String())
	}
	// invalid JSON is never overwritten
	p := filepath.Join(home, ".cursor", "mcp.json")
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, []byte("{ // comment\n}"), 0o644)
	o.dryRun = false
	if err := installClient(o, false); err == nil {
		t.Error("expected error for non-JSON config")
	}
	if b, _ := os.ReadFile(p); string(b) != "{ // comment\n}" {
		t.Errorf("file modified: %q", b)
	}
	o.client = "nope"
	if err := installClient(o, false); err == nil {
		t.Error("unknown client should fail")
	}
	// vscode project-level
	proj := t.TempDir()
	o = installOpts{client: "vscode", exe: "/x/phoenix", workspace: "/w", goos: "linux", home: home, project: proj, out: &out}
	if err := installClient(o, false); err != nil {
		t.Fatal(err)
	}
	if m := readJSON(t, filepath.Join(proj, ".vscode", "mcp.json")); m["servers"] == nil {
		t.Errorf("project config: %v", m)
	}
}

func TestInstallClaudeCode(t *testing.T) {
	var out bytes.Buffer
	var ran []string
	o := installOpts{client: "claude-code", exe: "/opt/my phoenix/phoenix", workspace: "/w", goos: "linux", home: t.TempDir(), out: &out,
		lookPath: func(string) (string, error) { return "/usr/bin/claude", nil },
		runCmd: func(n string, a ...string) ([]byte, error) {
			ran = append(ran, n+" "+strings.Join(a, " "))
			return []byte("ok\n"), nil
		}}
	o.dryRun = true
	if err := installClient(o, false); err != nil {
		t.Fatal(err)
	}
	if len(ran) != 0 || !strings.Contains(out.String(), "claude mcp add phoenix -- '/opt/my phoenix/phoenix' mcp --workspace /w") {
		t.Fatalf("dry-run: ran=%v out=%s", ran, out.String())
	}
	o.dryRun = false
	if err := installClient(o, false); err != nil || len(ran) != 1 || !strings.HasPrefix(ran[0], "claude mcp add phoenix --") {
		t.Fatalf("run: %v %v", err, ran)
	}
	o.lookPath = func(string) (string, error) { return "", os.ErrNotExist }
	ran = nil
	if err := installClient(o, true); err != nil || len(ran) != 0 {
		t.Fatalf("no claude on PATH: %v %v", err, ran)
	}
}
