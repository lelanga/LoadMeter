package ai

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/lelanga/loadmeter/internal/safety"
)

// Key storage order: OS credential store when available (macOS Keychain via
// `security`, Linux Secret Service via `secret-tool`), otherwise an owner-only
// (0600) file in the Load Meter config directory. Environment variables named by
// ProviderConfig.KeyEnv always take precedence and are never written anywhere.

const service = "loadmeter-performance"

var keyMu sync.Mutex

func keyFile() string { return filepath.Join(safety.ConfigDir(), "keys.json") }

// KeyBackend reports where keys are stored on this machine.
func KeyBackend() string {
	switch {
	case runtime.GOOS == "darwin" && has("security"):
		return "macos-keychain"
	case runtime.GOOS == "linux" && has("secret-tool") && os.Getenv("DBUS_SESSION_BUS_ADDRESS") != "":
		return "secret-service"
	}
	return "file-0600"
}

func has(bin string) bool { _, err := exec.LookPath(bin); return err == nil }

func SetKey(id, key string) error {
	keyMu.Lock()
	defer keyMu.Unlock()
	defer safety.Audit("ai.key_changed", map[string]any{"provider": id, "backend": KeyBackend(), "removed": key == ""})
	switch KeyBackend() {
	case "macos-keychain":
		if key == "" {
			return exec.Command("security", "delete-generic-password", "-s", service, "-a", id).Run()
		}
		// -w with the value as argument is visible in ps briefly; use stdin-less form is not supported by `security`.
		return exec.Command("security", "add-generic-password", "-U", "-s", service, "-a", id, "-w", key).Run()
	case "secret-service":
		if key == "" {
			return exec.Command("secret-tool", "clear", "service", service, "account", id).Run()
		}
		cmd := exec.Command("secret-tool", "store", "--label=Load Meter "+id, "service", service, "account", id)
		cmd.Stdin = strings.NewReader(key)
		return cmd.Run()
	}
	m := readKeyFile()
	if key == "" {
		delete(m, id)
	} else {
		m[id] = key
	}
	if err := os.MkdirAll(safety.ConfigDir(), 0o700); err != nil {
		return err
	}
	b, _ := json.Marshal(m)
	return os.WriteFile(keyFile(), b, 0o600)
}

func GetKey(id string) (string, error) {
	keyMu.Lock()
	defer keyMu.Unlock()
	switch KeyBackend() {
	case "macos-keychain":
		out, err := exec.Command("security", "find-generic-password", "-s", service, "-a", id, "-w").Output()
		return strings.TrimSpace(string(out)), err
	case "secret-service":
		out, err := exec.Command("secret-tool", "lookup", "service", service, "account", id).Output()
		return string(bytes.TrimSpace(out)), err
	}
	if v, ok := readKeyFile()[id]; ok {
		return v, nil
	}
	return "", errors.New("no stored key")
}

func readKeyFile() map[string]string {
	m := map[string]string{}
	b, err := os.ReadFile(keyFile())
	if err == nil {
		_ = json.Unmarshal(b, &m)
	}
	return m
}
