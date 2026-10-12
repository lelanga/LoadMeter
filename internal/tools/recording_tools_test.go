package tools

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/lelanga/loadmeter/internal/app"
	"github.com/lelanga/loadmeter/internal/service"
)

func TestRecordingToolsConfirmAndLifecycle(t *testing.T) {
	t.Setenv("LOADMETER_HOME", t.TempDir())
	s, err := service.New(app.Workspace{Dir: filepath.Join(t.TempDir(), "ws")})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	if _, err := Call(ctx, s, "start_recording", map[string]any{}, "test"); !errors.Is(err, ErrNeedsConfirm) {
		t.Fatalf("start without confirm: %v", err)
	}
	out, err := Call(ctx, s, "start_recording", map[string]any{"confirm": true, "excludeHosts": []any{"cdn.example"}}, "test")
	if err != nil {
		t.Fatal(err)
	}
	id := out.(*service.RecordingInfo).ID
	if _, err := Call(ctx, s, "mark_transaction", map[string]any{"id": id, "name": "Login"}, "test"); err != nil {
		t.Fatal(err)
	}
	st, err := Call(ctx, s, "recording_status", map[string]any{}, "test")
	if err != nil || st.(map[string]any)["active"] != true {
		t.Fatalf("status: %v %v", st, err)
	}
	res, err := Call(ctx, s, "stop_recording", map[string]any{"id": id}, "test")
	if err != nil || res.(*service.RecordingResult).HARFile == "" {
		t.Fatalf("stop: %v %v", res, err)
	}
}
