package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lelanga/loadmeter/internal/service"
)

func TestImportAccessLogTool(t *testing.T) {
	s := newSvc(t)
	logPath, _ := filepath.Abs(filepath.Join("..", "..", "testdata", "accesslog", "nginx_combined.log"))
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())
	ctx := context.Background()

	// path restriction, like the other import tools
	outside := filepath.Join(t.TempDir(), "a.log")
	os.WriteFile(outside, data, 0o600)
	_, err = Call(ctx, s, "import_access_log", map[string]any{"path": outside, "baseUrl": "http://127.0.0.1:8099"}, "test")
	if err == nil || !strings.Contains(err.Error(), "outside the allowed import folders") {
		t.Errorf("outside path: %v", err)
	}
	if _, err := Call(ctx, s, "import_access_log", map[string]any{"path": "/etc/passwd", "baseUrl": "http://127.0.0.1:8099"}, "test"); err == nil {
		t.Error("read /etc/passwd")
	}
	if _, err := Call(ctx, s, "import_access_log", map[string]any{"path": "x.log"}, "test"); err == nil || !strings.Contains(err.Error(), "missing required") {
		t.Errorf("baseUrl is required: %v", err)
	}

	imports := filepath.Join(s.WS.Dir, "imports")
	in := filepath.Join(imports, "access.log")
	os.WriteFile(in, data, 0o600)

	// a non-loopback target is refused unless the policy already allows it
	_, err = Call(ctx, s, "import_access_log", map[string]any{"path": in, "baseUrl": "https://shop.example.com"}, "test")
	if err == nil || !strings.Contains(err.Error(), "refusing non-loopback") {
		t.Errorf("non-loopback: %v", err)
	}

	out, err := Call(ctx, s, "import_access_log", map[string]any{"path": in, "baseUrl": "http://127.0.0.1:8099", "name": "Log Replay", "scale": 0.5, "top": float64(2)}, "test")
	if err != nil {
		t.Fatal(err)
	}
	res, ok := out.(*service.AccessLogImport)
	if !ok || res.File == "" || res.Profile.Replayable != 6 {
		t.Fatalf("%T %+v", out, out)
	}
	sc, err := s.GetScenario(res.File)
	if err != nil {
		t.Fatal(err)
	}
	if sc.Steps[0].Select != "weighted" || len(sc.Steps[0].Steps) != 2 || sc.Defaults.BaseURL != "http://127.0.0.1:8099" {
		t.Errorf("saved scenario: %+v", sc.Steps[0])
	}
	if p := sc.Validate(); len(p) != 0 {
		t.Errorf("saved scenario invalid: %v", p)
	}
}
