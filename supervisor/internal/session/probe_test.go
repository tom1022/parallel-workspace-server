package session

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func probeConfig(t *testing.T) ProbeConfig {
	t.Helper()
	return ProbeConfig{
		Prompt:    "reply with ok",
		Timeout:   3 * time.Second,
		Poll:      50 * time.Millisecond,
		Workspace: "ws-test",
	}
}

// completeTurnAfter simulates Claude Code writing its execution record: the
// probe must observe the turn through that record, not through the screen.
func completeTurnAfter(t *testing.T, configDir string, d time.Duration) {
	t.Helper()
	go func() {
		time.Sleep(d)
		projects := filepath.Join(configDir, "projects", "-workspace-repo")
		if err := os.MkdirAll(projects, 0o700); err != nil {
			return
		}
		body := lineUserPrompt + "\n" + lineEndTurn + "\n"
		_ = os.WriteFile(filepath.Join(projects, "sess.jsonl"), []byte(body), 0o600)
	}()
}

func TestAuthProbeIssuesItsRequestThroughTheLiveSession(t *testing.T) {
	sup := newTestSupervisor(t)
	health := &Health{}
	completeTurnAfter(t, sup.ConfigDir, 200*time.Millisecond)

	if err := AuthProbe(sup, health, probeConfig(t)); err != nil {
		t.Fatalf("probe: %v", err)
	}

	// The prompt reaching the tmux pane is what distinguishes this from a
	// one-shot batch invocation (5.7).
	b, err := os.ReadFile(filepath.Join(sup.ConfigDir, "typed.txt"))
	if err != nil {
		t.Fatalf("probe input never reached the session: %v", err)
	}
	if !strings.Contains(string(b), "reply with ok") {
		t.Errorf("session received %q, want the probe prompt", b)
	}
	if usable, _ := health.Status(); !usable {
		t.Error("a successful probe must leave the workspace usable")
	}
}

func TestAuthProbeFailureMarksWorkspaceUnusableAndNotifiesHermes(t *testing.T) {
	sup := newTestSupervisor(t)
	health := &Health{}

	var mu sync.Mutex
	var got Event
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		_ = json.NewDecoder(r.Body).Decode(&got)
	}))
	defer srv.Close()

	cfg := probeConfig(t)
	cfg.Timeout = 300 * time.Millisecond
	cfg.HermesURL = srv.URL

	// No transcript is ever written, so the turn never completes.
	if err := AuthProbe(sup, health, cfg); err == nil {
		t.Fatal("expected the probe to fail when the turn never completes")
	}

	usable, detail := health.Status()
	if usable {
		t.Error("a failed probe must mark the workspace unusable (5.8)")
	}
	if detail == "" {
		t.Error("the reason must be recorded, it is the only signal an operator gets")
	}

	mu.Lock()
	defer mu.Unlock()
	if got.Kind != EventAuthProbeFailed {
		t.Errorf("hermes event = %+v, want %s", got, EventAuthProbeFailed)
	}
	if got.Workspace != "ws-test" {
		t.Errorf("event workspace = %q", got.Workspace)
	}
}

func TestAuthProbeFailsWhenTheTurnEndsInFailure(t *testing.T) {
	sup := newTestSupervisor(t)
	health := &Health{}
	sup.SetFailure("claude code exited")

	cfg := probeConfig(t)
	cfg.Timeout = 300 * time.Millisecond
	if err := AuthProbe(sup, health, cfg); err == nil {
		t.Fatal("expected failure when the session itself is down")
	}
	if usable, _ := health.Status(); usable {
		t.Error("workspace must be unusable (5.8)")
	}
}

func TestHealthHandlerPublishesUsability(t *testing.T) {
	health := &Health{}
	srv := httptest.NewServer(health.Handler())
	defer srv.Close()

	var body struct {
		Usable bool   `json:"usable"`
		Detail string `json:"detail"`
	}
	getJSON(t, srv.URL+"/health", &body)
	if !body.Usable {
		t.Error("a fresh workspace must report usable")
	}

	health.MarkUnusable("auth probe failed")
	getJSON(t, srv.URL+"/health", &body)
	if body.Usable {
		t.Error("health must report the workspace unusable once marked (5.8)")
	}
	if body.Detail != "auth probe failed" {
		t.Errorf("detail = %q", body.Detail)
	}
}

func TestHealthHandlerServesUsage(t *testing.T) {
	configDir := writeUsageState(t, usageFixture)
	health := &Health{ConfigDir: configDir}
	srv := httptest.NewServer(health.Handler())
	defer srv.Close()

	var got []UsageSnapshot
	getJSON(t, srv.URL+"/usage", &got)
	if len(got) != 3 {
		t.Fatalf("got %d snapshots, want 3", len(got))
	}
	if got[0].RemainingPercent != 67 {
		t.Errorf("remaining = %v, want 67", got[0].RemainingPercent)
	}
}

// The config area outlives the container, so a finished turn from a previous
// run is already on disk when the probe starts. Treating it as the probe's own
// result would report a broken credential as healthy.
func TestAuthProbeIgnoresATurnFinishedBeforeItStarted(t *testing.T) {
	sup := newTestSupervisor(t)
	health := &Health{}

	projects := filepath.Join(sup.ConfigDir, "projects", "-workspace-repo")
	if err := os.MkdirAll(projects, 0o700); err != nil {
		t.Fatal(err)
	}
	stale := lineUserPrompt + "\n" + lineEndTurn + "\n"
	if err := os.WriteFile(filepath.Join(projects, "sess.jsonl"), []byte(stale), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := probeConfig(t)
	cfg.Timeout = 300 * time.Millisecond
	if err := AuthProbe(sup, health, cfg); err == nil {
		t.Fatal("a stale completed turn must not count as the probe's own request (5.7)")
	}
	if usable, _ := health.Status(); usable {
		t.Error("workspace must be marked unusable (5.8)")
	}
}

// An API failure left in the record by an earlier container must not fail the
// next container's probe: the record lives on the PVC and outlives the process
// that wrote it.
func TestRequestIgnoresAFailureItDidNotProvoke(t *testing.T) {
	sup := newTestSupervisor(t)
	projects := filepath.Join(sup.ConfigDir, "projects", "-workspace-repo")
	if err := os.MkdirAll(projects, 0o700); err != nil {
		t.Fatal(err)
	}
	stale := lineUserPrompt + "\n" + lineAuthError + "\n"
	if err := os.WriteFile(filepath.Join(projects, "sess.jsonl"), []byte(stale), 0o600); err != nil {
		t.Fatal(err)
	}
	completeTurnAfter(t, sup.ConfigDir, 200*time.Millisecond)

	if err := Request(sup, "probe", "reply with ok", 3*time.Second, 50*time.Millisecond); err != nil {
		t.Fatalf("request: %v", err)
	}
}
