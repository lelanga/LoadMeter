package service

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lelanga/loadmeter/internal/app"
	"github.com/lelanga/loadmeter/internal/mockserver"
	"github.com/lelanga/loadmeter/internal/model"
	"github.com/lelanga/loadmeter/internal/safety"
)

func TestCapacityLifecycle(t *testing.T) {
	t.Setenv("LOADMETER_HOME", t.TempDir())
	s, err := New(app.Workspace{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: mockserver.New("cap").Handler(), ReadHeaderTimeout: 5 * time.Second}
	go srv.Serve(ln)
	defer srv.Close()
	sc := &model.Scenario{Version: model.SchemaVersion, Name: "cap", Defaults: model.Defaults{BaseURL: "http://" + ln.Addr().String()},
		Workload: model.Workload{Executor: model.ExecConstantVUs, VUs: 1, Duration: model.Duration(time.Second), GracefulStop: model.Duration(time.Second)},
		Steps:    []model.Step{{Type: model.StepRequest, Name: "items", Request: &model.Request{Method: "GET", URL: "/api/items"}}}}
	if _, err := s.SaveScenario("cap.json", sc, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartCapacity(CapacityRequest{File: "cap.json"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("no criteria: %v", err)
	}
	remote := *sc
	remote.Defaults.BaseURL = "http://not-allowed.example"
	if _, err := s.SaveScenario("remote.json", &remote, "test"); err != nil {
		t.Fatal(err)
	}
	var hna *safety.ErrHostNotAllowed
	if _, err := s.StartCapacity(CapacityRequest{File: "remote.json", P95Ms: 100, MaxRate: 5, Confirm: true}); !errors.As(err, &hna) {
		t.Fatalf("remote host: %v", err)
	}
	zero := 0
	info, err := s.StartCapacity(CapacityRequest{File: "cap.json", P95Ms: 500, StartRate: 10, MaxRate: 10, Warmup: "200ms", Hold: "1s", RefineSteps: &zero})
	if err != nil {
		t.Fatal(err)
	}
	if info.State != "running" {
		t.Fatalf("state %s", info.State)
	}
	if _, err := s.StartCapacity(CapacityRequest{File: "cap.json", P95Ms: 500}); !errors.Is(err, ErrBusy) {
		t.Fatalf("second search: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	done := s.WaitCapacity(ctx)
	if done.State != "finished" || done.Result == nil || len(done.Steps) != 1 || done.Result.StopReason != "max-rate" {
		t.Fatalf("%+v", done)
	}
	if _, err := os.Stat(filepath.Join(done.ResultDir, "capacity.json")); err != nil {
		t.Fatal(err)
	}
	if r, err := s.CapacityResult(done.ID); err != nil || r.MaxSustainableRate != 10 {
		t.Fatalf("load: %v %v", r, err)
	}
	if ids, _ := s.ListCapacity(); len(ids) != 1 {
		t.Fatalf("list %v", ids)
	}
	if _, err := s.CapacityResult("../x"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad id: %v", err)
	}
}
