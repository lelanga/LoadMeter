package tools

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/lelanga/loadmeter/internal/app"
	"github.com/lelanga/loadmeter/internal/service"
)

func TestCapacityToolsConfirm(t *testing.T) {
	t.Setenv("LOADMETER_HOME", t.TempDir())
	s, err := service.New(app.Workspace{Dir: filepath.Join(t.TempDir(), "ws")})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	if _, err := Call(ctx, s, "find_capacity", map[string]any{"file": "x.json"}, "test"); !errors.Is(err, ErrNeedsConfirm) {
		t.Fatalf("without confirm: %v", err)
	}
	if _, err := Call(ctx, s, "find_capacity", map[string]any{"file": "missing.json", "confirm": true, "p95Ms": 100.0}, "test"); !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("missing scenario: %v", err)
	}
	st, err := Call(ctx, s, "capacity_status", map[string]any{}, "test")
	if err != nil || st.(map[string]any)["active"] != false {
		t.Fatalf("status: %v %v", st, err)
	}
}
