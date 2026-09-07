package testrun

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/tom1022/gitops-apps/apps/devplatform/supervisor/internal/repocfg"
)

var declared = repocfg.Config{
	App:           []string{"app"},
	UnitTest:      []string{"unit"},
	E2ETest:       []string{"e2e"},
	E2ETestFilter: []string{"e2e", "--grep"},
}

type fakeProc struct {
	events []string
	argv   [][]string
	fail   map[string]bool
}

func newRunner(t *testing.T, cfg repocfg.Config) (*Runner, *fakeProc) {
	t.Helper()
	f := &fakeProc{fail: map[string]bool{}}
	r := &Runner{
		WorkingDir:   t.TempDir(),
		ArtifactRoot: t.TempDir(),
		Config:       cfg,
		Exec: func(_ context.Context, argv []string) (string, error) {
			f.events = append(f.events, "exec:"+argv[0])
			f.argv = append(f.argv, argv)
			if f.fail[argv[0]] {
				return "boom", errors.New("exit status 1")
			}
			return "ok", nil
		},
		Serve: func(_ context.Context, argv []string) (func(), error) {
			f.events = append(f.events, "serve:"+argv[0])
			return func() { f.events = append(f.events, "stop") }, nil
		},
	}
	return r, f
}

func TestExecuteRunsBothSuitesWithTheAppUpOnlyForE2E(t *testing.T) {
	r, f := newRunner(t, declared)

	res, err := r.Execute(context.Background(), Run{Id: "run-1"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"exec:unit", "serve:app", "exec:e2e", "stop"}
	if !reflect.DeepEqual(f.events, want) {
		t.Errorf("events = %v, want %v", f.events, want)
	}
	if !res.Passed {
		t.Errorf("res = %+v, want a passing run", res)
	}
	if res.Failures == nil {
		t.Error("Failures must be an empty slice, not nil")
	}
}

func TestExecuteRunsWithoutAnySessionState(t *testing.T) {
	// 9.10: the run must not depend on a developer being attached. Nothing in
	// the runner is given a session, so a run with only a working directory
	// and a declaration has to complete.
	r, _ := newRunner(t, declared)
	if _, err := r.Execute(context.Background(), Run{Id: "detached"}); err != nil {
		t.Fatalf("a run with no attached client must still complete: %v", err)
	}
}

func TestExecuteStopsTheAppWhenTheE2ESuiteFails(t *testing.T) {
	r, f := newRunner(t, declared)
	f.fail["e2e"] = true

	res, err := r.Execute(context.Background(), Run{Id: "run-1"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Passed {
		t.Error("a failing suite must not report a passing run")
	}
	if f.events[len(f.events)-1] != "stop" {
		t.Errorf("events = %v, want the app stopped even after a failure", f.events)
	}
}

func TestExecuteSkipsTheAppWhenNoE2ESuiteIsDeclared(t *testing.T) {
	r, f := newRunner(t, repocfg.Config{App: []string{"app"}, UnitTest: []string{"unit"}})

	if _, err := r.Execute(context.Background(), Run{Id: "run-1"}); err != nil {
		t.Fatal(err)
	}
	want := []string{"exec:unit"}
	if !reflect.DeepEqual(f.events, want) {
		t.Errorf("events = %v, want %v", f.events, want)
	}
}

func TestExecutePassesWhenTheRepositoryDeclaresNoSuites(t *testing.T) {
	r, f := newRunner(t, repocfg.Config{})

	res, err := r.Execute(context.Background(), Run{Id: "run-1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.events) != 0 {
		t.Errorf("events = %v, want nothing run", f.events)
	}
	if !res.Passed {
		t.Error("a repository with no declared suites has nothing failing")
	}
}

func TestExecuteReplacesAnInterruptedRunOfTheSameId(t *testing.T) {
	r, _ := newRunner(t, declared)
	dir := r.Dir("run-1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, "e2e.log")
	if err := os.WriteFile(stale, []byte("half-written"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := r.Execute(context.Background(), Run{Id: "run-1"}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(stale)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) == "half-written" {
		t.Error("the interrupted run's artifacts survived a full re-run of the same id")
	}
}

func TestExecuteWritesEachSuiteOutputUnderTheRunId(t *testing.T) {
	r, _ := newRunner(t, declared)
	if _, err := r.Execute(context.Background(), Run{Id: "run-7"}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{StepUnit, StepE2E} {
		b, err := os.ReadFile(filepath.Join(r.Dir("run-7"), name+".log"))
		if err != nil {
			t.Fatalf("%s output was not captured: %v", name, err)
		}
		if string(b) != "ok" {
			t.Errorf("%s.log = %q, want the command's output", name, b)
		}
	}
}

func TestExecuteFilteredRerunNarrowsToTheNamedTests(t *testing.T) {
	r, f := newRunner(t, declared)

	res, err := r.Execute(context.Background(), Run{Id: "run-2", Attempt: 1, OnlyTests: []string{"checkout works"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Attempt != 1 {
		t.Errorf("Attempt = %d, want the attempt carried through", res.Attempt)
	}
	want := []string{"serve:app", "exec:e2e"}
	if !reflect.DeepEqual(f.events[:len(want)], want) {
		t.Errorf("events = %v, want a re-run to skip the unit suite", f.events)
	}
	wantArgv := []string{"e2e", "--grep", "checkout works"}
	if !reflect.DeepEqual(f.argv[0], wantArgv) {
		t.Errorf("argv = %v, want %v", f.argv[0], wantArgv)
	}
}

func TestExecuteFilteredRerunFallsBackToTheWholeSuite(t *testing.T) {
	cfg := declared
	cfg.E2ETestFilter = nil
	r, f := newRunner(t, cfg)

	if _, err := r.Execute(context.Background(), Run{Id: "run-3", OnlyTests: []string{"checkout works"}}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.argv[0], []string{"e2e"}) {
		t.Errorf("argv = %v, want the whole suite when no filter is declared", f.argv)
	}
}

func TestExecuteRequiresARunId(t *testing.T) {
	r, _ := newRunner(t, declared)
	if _, err := r.Execute(context.Background(), Run{}); err == nil {
		t.Fatal("an unidentified run cannot be idempotent; it must be refused")
	}
}
