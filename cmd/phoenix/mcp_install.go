package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/lelanga/phoenix/internal/mcp"
	"github.com/lelanga/phoenix/internal/service"
)

// mcpServerName is the key Phoenix uses in every client's config.
const mcpServerName = "phoenix"

var mcpClients = []string{"claude-desktop", "claude-code", "cursor", "vscode", "gemini", "windsurf"}

// runMCPHTTP serves the Streamable HTTP transport on a loopback address.
func runMCPHTTP(svc *service.Service, addr string) error {
	if err := mcp.ValidateLoopbackAddr(addr); err != nil {
		return err
	}
	tok, err := mcp.EnsureToken()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return (&mcp.Server{Svc: svc}).ServeHTTP(ctx, addr, tok, func(a net.Addr) {
		fmt.Printf("phoenix MCP server (Streamable HTTP) on http://%s/mcp\nbearer token (also in %s):\n  %s\nSend: Authorization: Bearer <token>. Press Ctrl+C to stop.\n", a, mcp.TokenPath(), tok)
	})
}

type installOpts struct {
	client, exe, workspace, project string
	dryRun                          bool
	goos, home, appdata             string
	out                             io.Writer
	lookPath                        func(string) (string, error)
	runCmd                          func(name string, args ...string) ([]byte, error)
}

func (o *installOpts) defaults() error {
	if o.out == nil {
		o.out = os.Stdout
	}
	if o.goos == "" {
		o.goos = runtime.GOOS
	}
	if o.home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		o.home = h
	}
	if o.appdata == "" {
		o.appdata = os.Getenv("APPDATA")
	}
	if o.lookPath == nil {
		o.lookPath = exec.LookPath
	}
	if o.runCmd == nil {
		o.runCmd = func(name string, args ...string) ([]byte, error) {
			return exec.Command(name, args...).CombinedOutput()
		}
	}
	if o.exe == "" {
		e, err := os.Executable()
		if err != nil {
			return err
		}
		if r, err := filepath.EvalSymlinks(e); err == nil {
			e = r
		}
		o.exe = e
	}
	if abs, err := filepath.Abs(o.exe); err == nil {
		o.exe = abs
	}
	if o.workspace == "" {
		o.workspace = defaultWorkspace()
	}
	if abs, err := filepath.Abs(o.workspace); err == nil {
		o.workspace = abs
	}
	return nil
}

// clientConfig returns the config file path and the top-level key holding servers.
func (o *installOpts) clientConfig() (path, key string, err error) {
	switch o.client {
	case "claude-desktop":
		switch o.goos {
		case "darwin":
			return filepath.Join(o.home, "Library", "Application Support", "Claude", "claude_desktop_config.json"), "mcpServers", nil
		case "windows":
			if o.appdata == "" {
				return "", "", errors.New("APPDATA is not set")
			}
			return filepath.Join(o.appdata, "Claude", "claude_desktop_config.json"), "mcpServers", nil
		}
		return filepath.Join(o.home, ".config", "Claude", "claude_desktop_config.json"), "mcpServers", nil
	case "cursor":
		return filepath.Join(o.home, ".cursor", "mcp.json"), "mcpServers", nil
	case "gemini":
		return filepath.Join(o.home, ".gemini", "settings.json"), "mcpServers", nil
	case "windsurf":
		return filepath.Join(o.home, ".codeium", "windsurf", "mcp_config.json"), "mcpServers", nil
	case "vscode":
		if o.project != "" {
			p, _ := filepath.Abs(o.project)
			return filepath.Join(p, ".vscode", "mcp.json"), "servers", nil
		}
		switch o.goos {
		case "darwin":
			return filepath.Join(o.home, "Library", "Application Support", "Code", "User", "mcp.json"), "servers", nil
		case "windows":
			if o.appdata == "" {
				return "", "", errors.New("APPDATA is not set")
			}
			return filepath.Join(o.appdata, "Code", "User", "mcp.json"), "servers", nil
		}
		return filepath.Join(o.home, ".config", "Code", "User", "mcp.json"), "servers", nil
	}
	return "", "", fmt.Errorf("unknown client %q (choose one of: %s)", o.client, strings.Join(mcpClients, ", "))
}

func (o *installOpts) entry() map[string]any {
	e := map[string]any{"command": o.exe, "args": []string{"mcp", "--workspace", o.workspace}}
	if o.client == "vscode" {
		e["type"] = "stdio"
	}
	return e
}

func shellQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\"'$`\\&|;<>()*?[]{}!#~") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// mergeServer parses data (empty = new file), sets or removes servers[phoenix]
// and returns the new document. Everything else is preserved untouched.
func mergeServer(data []byte, key string, entry map[string]any, remove bool) (out []byte, changed bool, err error) {
	top := map[string]json.RawMessage{}
	if len(bytes.TrimSpace(data)) > 0 {
		if err := json.Unmarshal(data, &top); err != nil {
			return nil, false, fmt.Errorf("existing config is not plain JSON (%v); edit it by hand instead", err)
		}
	}
	servers := map[string]json.RawMessage{}
	if raw, ok := top[key]; ok && string(bytes.TrimSpace(raw)) != "null" {
		if err := json.Unmarshal(raw, &servers); err != nil {
			return nil, false, fmt.Errorf("%q in the existing config is not an object: %v", key, err)
		}
	}
	if remove {
		if _, ok := servers[mcpServerName]; !ok {
			return data, false, nil
		}
		delete(servers, mcpServerName)
	} else {
		b, _ := json.Marshal(entry)
		servers[mcpServerName] = b
	}
	sb, _ := json.Marshal(servers)
	top[key] = sb
	out, err = json.MarshalIndent(top, "", "  ")
	if err != nil {
		return nil, false, err
	}
	return append(out, '\n'), true, nil
}

func installClient(o installOpts, remove bool) error {
	if err := o.defaults(); err != nil {
		return err
	}
	if o.client == "claude-code" {
		return claudeCodeCmd(o, remove)
	}
	path, key, err := o.clientConfig()
	if err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	existed := err == nil
	out, changed, err := mergeServer(data, key, o.entry(), remove)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if !changed {
		fmt.Fprintf(o.out, "no phoenix entry in %s; nothing to do\n", path)
		return nil
	}
	if o.dryRun {
		fmt.Fprintf(o.out, "dry run: would write %s:\n%s", path, out)
		return nil
	}
	mode := os.FileMode(0o600)
	if existed {
		if fi, err := os.Stat(path); err == nil {
			mode = fi.Mode().Perm()
		}
		if err := os.WriteFile(path+".bak", data, mode); err != nil {
			return fmt.Errorf("backup failed, config left untouched: %w", err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, mode); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	verb := "installed phoenix into"
	if remove {
		verb = "removed phoenix from"
	}
	fmt.Fprintf(o.out, "%s %s", verb, path)
	if existed {
		fmt.Fprintf(o.out, " (backup: %s.bak)", path)
	}
	fmt.Fprintf(o.out, "\nRestart %s to pick it up.\n", o.client)
	return nil
}

func claudeCodeCmd(o installOpts, remove bool) error {
	args := []string{"mcp", "add", mcpServerName, "--", o.exe, "mcp", "--workspace", o.workspace}
	if remove {
		args = []string{"mcp", "remove", mcpServerName}
	}
	parts := []string{"claude"}
	for _, a := range args {
		parts = append(parts, shellQuote(a))
	}
	line := strings.Join(parts, " ")
	fmt.Fprintf(o.out, "%s\n", line)
	if o.dryRun {
		return nil
	}
	if _, err := o.lookPath("claude"); err != nil {
		fmt.Fprintln(o.out, "`claude` is not on PATH: run the command above yourself.")
		return nil
	}
	b, err := o.runCmd("claude", args...)
	fmt.Fprintf(o.out, "%s", b)
	if err != nil {
		return fmt.Errorf("claude: %w", err)
	}
	return nil
}

// cmdMCPInstall handles `phoenix mcp install|uninstall`.
func cmdMCPInstall(args []string) error {
	remove := args[0] == "uninstall"
	fs := flag.NewFlagSet("mcp "+args[0], flag.ContinueOnError)
	var o installOpts
	fs.StringVar(&o.client, "client", "", "claude-desktop|claude-code|cursor|vscode|gemini|windsurf")
	fs.StringVar(&o.workspace, "workspace", "", "workspace directory Phoenix should use (default: the usual workspace)")
	fs.StringVar(&o.project, "project", "", "vscode only: write DIR/.vscode/mcp.json instead of the user-level file")
	fs.BoolVar(&o.dryRun, "dry-run", false, "show what would change without writing")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if o.client == "" {
		sort.Strings(mcpClients)
		return fmt.Errorf("--client is required (one of: %s)", strings.Join(mcpClients, ", "))
	}
	return installClient(o, remove)
}
