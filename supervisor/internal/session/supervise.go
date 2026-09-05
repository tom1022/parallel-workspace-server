package session

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"syscall"
	"time"
)

// Event kinds reported to Hermes Agent.
const (
	EventClaudeProcessExited = "ClaudeProcessExited"
)

// Event is a notification sent to Hermes Agent.
type Event struct {
	Kind      string `json:"kind"`
	Workspace string `json:"workspace"`
	Detail    string `json:"detail,omitempty"`
	At        string `json:"at"`
}

// AcquireWorkingDirLock takes an exclusive, non-blocking lock that admits one
// supervisor per working directory (16.3). flock is held by the open file
// description, so the kernel releases it when the process dies however it
// dies — a crashed workspace therefore leaves no stale lock to clear (16.5).
func AcquireWorkingDirLock(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("supervisor: another session already holds %s: %w", path, err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

// NotifyHermes posts an event to Hermes Agent. An unset endpoint is a no-op:
// chat relay is the platform's only notification path, and losing it must not
// take the workspace down with it.
func NotifyHermes(endpoint string, e Event) error {
	if endpoint == "" {
		return nil
	}
	if e.At == "" {
		e.At = time.Now().UTC().Format(time.RFC3339)
	}
	body, err := json.Marshal(e)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(endpoint, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("supervisor: hermes returned %s", resp.Status)
	}
	return nil
}

// CheckProcess reports an abnormal exit of the hosted process (2.5) and brings
// it back, since the session must keep running whenever the workspace is not
// suspended (2.3).
//
// ponytail: restarts immediately with no backoff. A process that fails on
// startup will therefore respawn as fast as it dies; add a delay here if that
// is ever observed.
func CheckProcess(tm *Tmux, workspace string, onExit func(Event)) error {
	if !tm.HasSession() {
		return nil
	}
	dead, status, err := tm.PaneDead()
	if err != nil {
		return err
	}
	if !dead {
		return nil
	}
	onExit(Event{
		Kind:      EventClaudeProcessExited,
		Workspace: workspace,
		Detail:    fmt.Sprintf("exit status %d", status),
		At:        time.Now().UTC().Format(time.RFC3339),
	})
	return tm.Respawn()
}
