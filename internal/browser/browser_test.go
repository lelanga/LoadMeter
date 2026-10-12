package browser

import (
	"errors"
	"os/exec"
	"strings"
	"testing"
)

func none() finder {
	return finder{getenv: func(string) string { return "" }, lookPath: func(string) (string, error) { return "", exec.ErrNotFound },
		exists: func(string) bool { return false }, goos: "linux", glob: func(string) ([]string, error) { return nil, nil }}
}

func TestFindNoBrowserGivesClearError(t *testing.T) {
	for _, goos := range []string{"linux", "darwin", "windows"} {
		f := none()
		f.goos = goos
		_, err := f.find()
		if !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "LOADMETER_BROWSER") {
			t.Errorf("%s: err = %v", goos, err)
		}
	}
}

func TestFindEnvAndPathAndInstallDirs(t *testing.T) {
	f := none()
	f.getenv = func(k string) string {
		if k == "LOADMETER_BROWSER" {
			return "/nope/chrome"
		}
		return ""
	}
	if _, err := f.find(); err == nil || !strings.Contains(err.Error(), "/nope/chrome") {
		t.Errorf("bad LOADMETER_BROWSER: %v", err)
	}
	f = none()
	f.lookPath = func(n string) (string, error) {
		if n == "chromium" {
			return "/usr/local/bin/chromium", nil
		}
		return "", exec.ErrNotFound
	}
	if p, err := f.find(); err != nil || p != "/usr/local/bin/chromium" {
		t.Errorf("PATH lookup: %q %v", p, err)
	}
	f = none()
	f.goos = "darwin"
	f.exists = func(p string) bool { return p == "/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge" }
	if p, err := f.find(); err != nil || !strings.Contains(p, "Edge") {
		t.Errorf("mac install dir: %q %v", p, err)
	}
	f = none()
	f.glob = func(pat string) ([]string, error) {
		if strings.HasPrefix(pat, "/opt/pw-browsers/") {
			return []string{"/opt/pw-browsers/chromium-1/chrome-linux/chrome"}, nil
		}
		return nil, nil
	}
	f.exists = func(p string) bool { return strings.HasPrefix(p, "/opt/pw-browsers/") }
	if p, err := f.find(); err != nil || !strings.HasPrefix(p, "/opt/pw-browsers/") {
		t.Errorf("playwright chromium: %q %v", p, err)
	}
}

func TestArgs(t *testing.T) {
	t.Setenv("LOADMETER_BROWSER_ARGS", "--headless=new --no-sandbox")
	a := Args(Options{ProxyAddr: "127.0.0.1:9999", SPKI: "abc=", StartURL: "http://127.0.0.1:1/x"}, "/tmp/prof")
	j := strings.Join(a, " ")
	for _, want := range []string{"--user-data-dir=/tmp/prof", "--proxy-server=http://127.0.0.1:9999", "--proxy-bypass-list=<-loopback>",
		"--ignore-certificate-errors-spki-list=abc=", "--headless=new", "--no-sandbox"} {
		if !strings.Contains(j, want) {
			t.Errorf("missing %s in %s", want, j)
		}
	}
	if a[len(a)-1] != "http://127.0.0.1:1/x" {
		t.Errorf("start URL must be last: %v", a)
	}
}
