package browser

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// AppSession is a browser window in "app mode" (no tabs or address bar).
type AppSession struct {
	Cmd  *exec.Cmd
	Done chan struct{} // closed when the browser process exits
}

// LaunchApp opens url in a standalone app-style window using Chrome, Chromium
// or Edge. profile is a persistent user-data directory: it keeps UI
// preferences between runs and, being separate from the user's own browser
// profile, makes the process live exactly as long as the window does.
func LaunchApp(ctx context.Context, url, profile string) (*AppSession, error) {
	if !strings.HasPrefix(url, "http://127.0.0.1:") && !strings.HasPrefix(url, "http://localhost:") {
		return nil, fmt.Errorf("app window only opens loopback URLs")
	}
	path, err := Find()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(profile, 0o700); err != nil {
		return nil, err
	}
	args := []string{
		"--user-data-dir=" + profile, "--app=" + url, "--window-size=1360,900",
		"--no-first-run", "--no-default-browser-check", "--disable-sync", "--disable-default-apps",
		"--disable-features=Translate,MediaRouter",
	}
	if extra := strings.Fields(os.Getenv("LOADMETER_BROWSER_ARGS")); len(extra) > 0 {
		args = append(args, extra...)
	}
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Stdout, cmd.Stderr = nil, nil
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting %s: %w", path, err)
	}
	s := &AppSession{Cmd: cmd, Done: make(chan struct{})}
	go func() { _ = cmd.Wait(); close(s.Done) }()
	return s, nil
}
