package session

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestWorkingDirLockAdmitsOnlyOneHolder(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "session.lock")

	release, err := AcquireWorkingDirLock(lockPath)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	if _, err := AcquireWorkingDirLock(lockPath); err == nil {
		t.Fatal("a second holder must be refused so one working directory runs one Claude Code process (16.3)")
	}

	release()

	release2, err := AcquireWorkingDirLock(lockPath)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	release2()
}

func TestNotifyHermesPostsTheEvent(t *testing.T) {
	type received struct {
		body map[string]any
		ct   string
	}
	got := make(chan received, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		got <- received{body: m, ct: r.Header.Get("Content-Type")}
	}))
	defer srv.Close()

	err := NotifyHermes(srv.URL, Event{
		Kind:      "ClaudeProcessExited",
		Workspace: "feature-foo",
		Detail:    "exit status 3",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	select {
	case r := <-got:
		if r.body["kind"] != "ClaudeProcessExited" {
			t.Errorf("kind = %v", r.body["kind"])
		}
		if r.body["workspace"] != "feature-foo" {
			t.Errorf("workspace = %v", r.body["workspace"])
		}
		if r.ct != "application/json" {
			t.Errorf("content-type = %q", r.ct)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no notification arrived")
	}
}

func TestNotifyHermesWithoutAConfiguredEndpoint(t *testing.T) {
	if err := NotifyHermes("", Event{Kind: "x"}); err != nil {
		t.Errorf("an unset endpoint must be a no-op, got %v", err)
	}
}

func TestCrashIsDetectedAndTheSessionIsRestored(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	dir := t.TempDir()
	tm := &Tmux{Socket: "devplatform-crash-" + filepath.Base(dir), Session: "workspace"}
	defer tm.Kill()

	marker := filepath.Join(dir, "started")
	// Exits non-zero the first time, then stays up, so both the crash and the
	// restart are observable.
	script := "if [ -e " + marker + " ]; then sleep 300; else touch " + marker + "; exit 3; fi"
	if err := tm.Start(StartConfig{WorkingDir: dir, Command: []string{"sh", "-c", script}}); err != nil {
		t.Fatalf("start: %v", err)
	}

	crashes := make(chan Event, 4)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if err := CheckProcess(tm, "feature-foo", func(e Event) { crashes <- e }); err != nil {
			t.Fatalf("check: %v", err)
		}
		if len(crashes) > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	select {
	case e := <-crashes:
		if e.Kind != EventClaudeProcessExited {
			t.Errorf("kind = %q, want %q", e.Kind, EventClaudeProcessExited)
		}
		if e.Detail == "" {
			t.Error("the exit status must be reported")
		}
	default:
		t.Fatal("the abnormal exit was never detected (2.5)")
	}

	// 2.3: the session must not be left dead just because the process died.
	if !tm.HasSession() {
		t.Fatal("session gone after a crash")
	}
	restored := false
	for time.Now().Before(deadline) {
		dead, _, err := tm.PaneDead()
		if err != nil {
			t.Fatalf("pane dead: %v", err)
		}
		if !dead {
			restored = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !restored {
		t.Error("the process was not restarted after its abnormal exit")
	}
}
