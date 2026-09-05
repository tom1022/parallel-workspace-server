package session

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReadOutputIsChronologicalAndResumable(t *testing.T) {
	log := filepath.Join(t.TempDir(), "output.log")
	if err := os.WriteFile(log, []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &Supervisor{OutputLog: log}

	chunks, err := s.ReadOutput(0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chunks) != 1 || chunks[0].Data != "first" {
		t.Fatalf("chunks = %+v, want the whole log", chunks)
	}
	seq := chunks[0].Seq

	f, err := os.OpenFile(log, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("second"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	chunks, err = s.ReadOutput(seq)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chunks) != 1 || chunks[0].Data != "second" {
		t.Fatalf("chunks = %+v, want only what was appended after seq %d", chunks, seq)
	}

	chunks, err = s.ReadOutput(chunks[0].Seq)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chunks) != 0 {
		t.Errorf("chunks = %+v, want none once caught up", chunks)
	}
}

func TestReadOutputBeforeAnyOutput(t *testing.T) {
	s := &Supervisor{OutputLog: filepath.Join(t.TempDir(), "missing.log")}
	chunks, err := s.ReadOutput(0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chunks) != 0 {
		t.Errorf("chunks = %+v, want none", chunks)
	}
}

// newTestSupervisor starts a real tmux session hosting a trivial shell so the
// client and input paths run against the multiplexer rather than a stand-in.
func newTestSupervisor(t *testing.T) *Supervisor {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	dir := t.TempDir()
	tm := &Tmux{Socket: "devplatform-test-" + filepath.Base(dir), Session: "workspace"}
	t.Cleanup(func() { _ = tm.Kill() })

	s := &Supervisor{
		Tmux:      tm,
		ConfigDir: dir,
		OutputLog: filepath.Join(dir, "output.log"),
	}
	err := tm.Start(StartConfig{
		WorkingDir:   dir,
		Command:      []string{"sh", "-c", "cat > typed.txt"},
		OutputLog:    s.OutputLog,
		HistoryLimit: 1000,
	})
	if err != nil {
		t.Fatalf("start session: %v", err)
	}
	return s
}

// attachClient connects a client through a pty, since tmux refuses to attach
// without one. Returns a stop func.
func attachClient(t *testing.T, tm *Tmux, readOnly bool) func() {
	t.Helper()
	if _, err := exec.LookPath("script"); err != nil {
		t.Skip("script(1) not available to allocate a pty")
	}
	attach := "tmux -L " + tm.Socket + " attach -t " + tm.Session
	if readOnly {
		attach += " -r"
	}
	cmd := exec.Command("script", "-q", "-c", attach, "/dev/null")
	cmd.Stdout = nil
	if err := cmd.Start(); err != nil {
		t.Fatalf("attach client: %v", err)
	}
	waitForClients(t, tm, -1)
	return func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}
}

// waitForClients polls until the client count reaches want, or any change when
// want is negative.
func waitForClients(t *testing.T, tm *Tmux, want int) []SessionClient {
	t.Helper()
	var last []SessionClient
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		clients, err := tm.ListClients()
		if err != nil {
			t.Fatalf("list clients: %v", err)
		}
		last = clients
		if want < 0 && len(clients) > 0 {
			return clients
		}
		if want >= 0 && len(clients) == want {
			return clients
		}
		time.Sleep(50 * time.Millisecond)
	}
	return last
}

func TestSessionSurvivesWithNoClientsAndRetainsScreen(t *testing.T) {
	s := newTestSupervisor(t)

	stop := attachClient(t, s.Tmux, true)
	if err := s.SendInput("hello-from-supervisor"); err != nil {
		t.Fatalf("send input: %v", err)
	}
	stop()

	waitForClients(t, s.Tmux, 0)
	if !s.Tmux.HasSession() {
		t.Fatal("session must outlive every client (2.3)")
	}

	// The pane's rendered content is what a reattaching client sees restored.
	out, err := s.Tmux.run("capture-pane", "-p", "-t", s.Tmux.Session)
	if err != nil {
		t.Fatalf("capture pane: %v", err)
	}
	if !strings.Contains(out, "hello-from-supervisor") {
		t.Errorf("pane content lost after detach; got %q", out)
	}
}

func TestSendInputWorksWithoutAnyAttachedClient(t *testing.T) {
	s := newTestSupervisor(t)
	if clients, _ := s.Tmux.ListClients(); len(clients) != 0 {
		t.Fatalf("expected no clients, got %+v", clients)
	}
	if err := s.SendInput("no-client-input"); err != nil {
		t.Fatalf("send input: %v", err)
	}
}

func TestReadOnlyAndWritableClientsAreDistinguishable(t *testing.T) {
	s := newTestSupervisor(t)
	stopRO := attachClient(t, s.Tmux, true)
	defer stopRO()
	stopRW := attachClient(t, s.Tmux, false)
	defer stopRW()

	clients := waitForClients(t, s.Tmux, 2)
	if len(clients) != 2 {
		t.Fatalf("got %d clients, want 2: %+v", len(clients), clients)
	}
	var ro, rw int
	for _, c := range clients {
		if c.Writable {
			rw++
		} else {
			ro++
		}
	}
	if ro != 1 || rw != 1 {
		t.Errorf("got %d read-only and %d writable clients, want 1 of each: %+v", ro, rw, clients)
	}
}

func TestSendInputRefusesWhileAWritableClientIsAttached(t *testing.T) {
	s := newTestSupervisor(t)
	stop := attachClient(t, s.Tmux, false)
	defer stop()
	waitForClients(t, s.Tmux, 1)

	err := s.SendInput("should-be-rejected")
	var sendErr *SendError
	if !errors.As(err, &sendErr) {
		t.Fatalf("err = %v, want a *SendError", err)
	}
	if sendErr.Kind != SendWritableClientPresent {
		t.Errorf("kind = %q, want %q", sendErr.Kind, SendWritableClientPresent)
	}
	if sendErr.ClientID == "" {
		t.Error("the blocking client must be identified")
	}
}

func TestSendInputOnDeadSession(t *testing.T) {
	s := newTestSupervisor(t)
	if err := s.Tmux.Kill(); err != nil {
		t.Fatal(err)
	}
	err := s.SendInput("anything")
	var sendErr *SendError
	if !errors.As(err, &sendErr) {
		t.Fatalf("err = %v, want a *SendError", err)
	}
	if sendErr.Kind != SendSessionNotRunning {
		t.Errorf("kind = %q, want %q", sendErr.Kind, SendSessionNotRunning)
	}
}

func TestPaneDeathIsObservable(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	dir := t.TempDir()
	tm := &Tmux{Socket: "devplatform-dead-" + filepath.Base(dir), Session: "workspace"}
	defer tm.Kill()
	if err := tm.Start(StartConfig{WorkingDir: dir, Command: []string{"sh", "-c", "exit 3"}}); err != nil {
		t.Fatalf("start: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		dead, status, err := tm.PaneDead()
		if err != nil {
			t.Fatalf("pane dead: %v", err)
		}
		if dead {
			if status != 3 {
				t.Errorf("exit status = %d, want 3", status)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("pane death was never observed")
}

func TestTurnStateReportsAFailedProcess(t *testing.T) {
	s := &Supervisor{ConfigDir: t.TempDir()}

	state, err := s.TurnState()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state.Kind != TurnIdle {
		t.Fatalf("kind = %q, want %q before any failure", state.Kind, TurnIdle)
	}

	s.SetFailure("exit status 3")
	state, err = s.TurnState()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state.Kind != TurnFailed {
		t.Errorf("kind = %q, want %q after a crash (2.5)", state.Kind, TurnFailed)
	}
	if state.Detail != "exit status 3" {
		t.Errorf("detail = %q, want the exit status", state.Detail)
	}

	s.ClearFailure()
	state, _ = s.TurnState()
	if state.Kind != TurnIdle {
		t.Errorf("kind = %q, want %q once restarted", state.Kind, TurnIdle)
	}
}
