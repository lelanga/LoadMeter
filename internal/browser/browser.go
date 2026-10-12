// Package browser finds and launches a local Chromium-family browser
// (Chrome, Chromium, Edge) for script recording through Load Meter's proxy.
package browser

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

// finder holds the environment hooks so tests can simulate any machine.
type finder struct {
	getenv   func(string) string
	lookPath func(string) (string, error)
	exists   func(string) bool
	goos     string
	glob     func(string) ([]string, error)
}

func osFinder() finder {
	return finder{getenv: os.Getenv, lookPath: exec.LookPath, goos: runtime.GOOS, glob: filepath.Glob,
		exists: func(p string) bool { fi, err := os.Stat(p); return err == nil && !fi.IsDir() }}
}

// ErrNotFound is returned when no supported browser is installed.
var ErrNotFound = errors.New("no Chrome, Chromium or Edge browser found")

// Find returns the path of a Chromium-family browser. LOADMETER_BROWSER (a path
// or a command name) takes precedence.
func Find() (string, error) { return osFinder().find() }

func (f finder) find() (string, error) {
	if p := strings.TrimSpace(f.getenv("LOADMETER_BROWSER")); p != "" {
		if f.exists(p) {
			return p, nil
		}
		if lp, err := f.lookPath(p); err == nil {
			return lp, nil
		}
		return "", fmt.Errorf("LOADMETER_BROWSER=%q does not point to an executable browser", p)
	}
	for _, name := range []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "microsoft-edge", "microsoft-edge-stable", "msedge", "chrome"} {
		if lp, err := f.lookPath(name); err == nil {
			return lp, nil
		}
	}
	var cands []string
	switch f.goos {
	case "darwin":
		home := f.getenv("HOME")
		for _, app := range []string{"Google Chrome.app/Contents/MacOS/Google Chrome", "Chromium.app/Contents/MacOS/Chromium",
			"Microsoft Edge.app/Contents/MacOS/Microsoft Edge", "Google Chrome Canary.app/Contents/MacOS/Google Chrome Canary"} {
			cands = append(cands, "/Applications/"+app)
			if home != "" {
				cands = append(cands, filepath.Join(home, "Applications", app))
			}
		}
	case "windows":
		for _, env := range []string{"PROGRAMFILES", "PROGRAMFILES(X86)", "LOCALAPPDATA"} {
			base := f.getenv(env)
			if base == "" {
				continue
			}
			cands = append(cands, filepath.Join(base, `Google\Chrome\Application\chrome.exe`), filepath.Join(base, `Microsoft\Edge\Application\msedge.exe`),
				filepath.Join(base, `Chromium\Application\chrome.exe`))
		}
	default:
		cands = []string{"/usr/bin/google-chrome", "/usr/bin/google-chrome-stable", "/usr/bin/chromium", "/usr/bin/chromium-browser",
			"/snap/bin/chromium", "/usr/bin/microsoft-edge", "/opt/google/chrome/chrome", "/opt/microsoft/msedge/msedge",
			"/var/lib/flatpak/exports/bin/com.google.Chrome"}
		// Playwright-managed Chromium (common on CI machines).
		if f.glob != nil {
			for _, base := range []string{f.getenv("PLAYWRIGHT_BROWSERS_PATH"), "/opt/pw-browsers", filepath.Join(f.getenv("HOME"), ".cache", "ms-playwright")} {
				if base == "" {
					continue
				}
				m, _ := f.glob(filepath.Join(base, "chromium-*", "chrome-linux", "chrome"))
				cands = append(cands, m...)
			}
		}
	}
	for _, c := range cands {
		if f.exists(c) {
			return c, nil
		}
	}
	return "", fmt.Errorf("%w: install Google Chrome, Chromium or Microsoft Edge, or set LOADMETER_BROWSER to the browser executable (you can also set your browser's proxy to the address printed by 'loadmeter record' manually)", ErrNotFound)
}

// Options configure a recording browser.
type Options struct {
	Path      string // browser executable (Find() when empty)
	ProxyAddr string // host:port of the recording proxy
	SPKI      string // base64 sha256 SPKI of the recording CA
	StartURL  string
}

// Session is a launched browser with a throwaway profile.
type Session struct {
	Cmd        *exec.Cmd
	ProfileDir string
	Done       chan struct{} // closed when the browser process exits
}

// Args builds the browser command line. LOADMETER_BROWSER_ARGS (whitespace
// separated) is appended, e.g. "--headless=new" for automated smoke tests.
func Args(o Options, profile string) []string {
	a := []string{
		"--user-data-dir=" + profile,
		"--proxy-server=http://" + o.ProxyAddr,
		"--proxy-bypass-list=<-loopback>", // also send localhost through the proxy
		"--ignore-certificate-errors-spki-list=" + o.SPKI,
		"--no-first-run", "--no-default-browser-check", "--disable-background-networking", "--disable-component-update",
		"--disable-sync", "--disable-default-apps", "--disable-features=OptimizationHints,MediaRouter,Translate",
		"--disable-quic", // QUIC (HTTP/3) bypasses HTTP proxies
	}
	if extra := strings.Fields(os.Getenv("LOADMETER_BROWSER_ARGS")); len(extra) > 0 {
		a = append(a, extra...)
	}
	start := o.StartURL
	if start == "" {
		start = "about:blank"
	}
	return append(a, start)
}

// Launch starts the browser with a fresh temporary profile. The profile is
// deleted when the process exits. Cancel ctx (or call Stop) to close it.
func Launch(ctx context.Context, o Options) (*Session, error) {
	if o.ProxyAddr == "" || o.SPKI == "" {
		return nil, errors.New("browser launch needs the proxy address and CA SPKI")
	}
	if o.StartURL != "" && !strings.HasPrefix(o.StartURL, "http://") && !strings.HasPrefix(o.StartURL, "https://") {
		return nil, fmt.Errorf("start URL must begin with http:// or https://")
	}
	if o.Path == "" {
		p, err := Find()
		if err != nil {
			return nil, err
		}
		o.Path = p
	}
	profile, err := os.MkdirTemp("", "loadmeter-record-profile-")
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, o.Path, Args(o, profile)...)
	cmd.Stdout, cmd.Stderr = nil, nil // browser logs may contain URLs with tokens
	if err := cmd.Start(); err != nil {
		os.RemoveAll(profile)
		return nil, fmt.Errorf("starting %s: %w", o.Path, err)
	}
	s := &Session{Cmd: cmd, ProfileDir: profile, Done: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		// Child processes (renderer, GPU, crashpad) may still write to the
		// profile briefly after the main process exits.
		for i := 0; i < 25; i++ {
			if err := os.RemoveAll(profile); err == nil {
				if _, err := os.Stat(profile); os.IsNotExist(err) {
					break
				}
			}
			time.Sleep(200 * time.Millisecond)
		}
		close(s.Done)
	}()
	return s, nil
}

// Stop terminates the browser (if still running) and waits for cleanup.
func (s *Session) Stop() {
	if s == nil || s.Cmd == nil || s.Cmd.Process == nil {
		return
	}
	select {
	case <-s.Done:
		return
	default:
	}
	// Ask politely first so the browser shuts down its child processes.
	if err := s.Cmd.Process.Signal(syscall.SIGTERM); err == nil {
		select {
		case <-s.Done:
			return
		case <-time.After(5 * time.Second):
		}
	}
	_ = s.Cmd.Process.Kill()
	<-s.Done
}
