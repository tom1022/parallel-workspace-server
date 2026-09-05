package session

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// clientFormat is the source of truth for the attached-client list. tmux
// tracks read/write capability per client itself, so 3.2's "writable or not"
// comes straight from it and never from scraping the rendered screen.
const clientFormat = "#{client_name}|#{client_readonly}|#{client_created}"

// Tmux drives one named tmux session over a dedicated socket.
type Tmux struct {
	// Socket isolates this supervisor's server from any other tmux on the
	// host; without it a stray user server would be shared.
	Socket  string
	Session string
}

func (t *Tmux) args(extra ...string) []string {
	return append([]string{"-L", t.Socket}, extra...)
}

func (t *Tmux) run(extra ...string) (string, error) {
	cmd := exec.Command("tmux", t.args(extra...)...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("tmux %s: %w: %s", strings.Join(extra, " "), err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}

// HasSession reports whether the session is alive.
func (t *Tmux) HasSession() bool {
	_, err := t.run("has-session", "-t", t.Session)
	return err == nil
}

// StartConfig describes the process the session hosts.
type StartConfig struct {
	WorkingDir string
	Command    []string
	Env        []string
	// OutputLog receives every byte the pane renders, so an external process
	// can read the session's output without attaching to it (2.6).
	OutputLog string
	// HistoryLimit is how many scrollback lines the pane retains across
	// detach/reattach cycles (2.4).
	HistoryLimit int
}

// Start creates the detached session. Detached is the point: the process runs
// regardless of whether any client is attached (2.1, 2.3).
func (t *Tmux) Start(cfg StartConfig) error {
	if t.HasSession() {
		return nil
	}
	args := []string{"new-session", "-d", "-s", t.Session, "-c", cfg.WorkingDir}
	for _, e := range cfg.Env {
		args = append(args, "-e", e)
	}
	args = append(args, cfg.Command...)
	if _, err := t.run(args...); err != nil {
		return err
	}
	// remain-on-exit keeps the pane (and its contents) after the hosted process
	// dies, which is both how the crash is observable at all and how the last
	// screen survives for the operator to read.
	if _, err := t.run("set-option", "-t", t.Session, "remain-on-exit", "on"); err != nil {
		return err
	}
	if cfg.HistoryLimit > 0 {
		if _, err := t.run("set-option", "-t", t.Session, "history-limit", strconv.Itoa(cfg.HistoryLimit)); err != nil {
			return err
		}
	}
	if cfg.OutputLog != "" {
		// -o would toggle an existing pipe off; this always establishes one.
		if _, err := t.run("pipe-pane", "-t", t.Session, fmt.Sprintf("cat >> %s", shellQuote(cfg.OutputLog))); err != nil {
			return err
		}
	}
	return nil
}

// inputBuffer is a named tmux buffer reserved for injected input, so pasting
// never consumes whatever the developer last copied.
const inputBuffer = "devplatform-input"

// SendText types text into the session and submits it.
//
// It pastes rather than using send-keys because send-keys resolves a "current
// client" for the target session and is refused outright when that client is
// read-only — which is exactly the state 3.3 puts a watching developer in, and
// would leave Hermes Agent unable to drive the session (2.7). paste-buffer
// does no such client resolution. Pasting also sidesteps key-name parsing, so
// a literal "Enter" or "C-c" inside text stays literal.
func (t *Tmux) SendText(text string) error {
	load := exec.Command("tmux", t.args("load-buffer", "-b", inputBuffer, "-")...)
	load.Stdin = strings.NewReader(strings.TrimSuffix(text, "\n") + "\n")
	var stderr strings.Builder
	load.Stderr = &stderr
	if err := load.Run(); err != nil {
		return fmt.Errorf("tmux load-buffer: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	// -d drops the buffer once pasted. Not -p: bracketed paste would deliver
	// the trailing newline as text rather than as the submit it has to be.
	_, err := t.run("paste-buffer", "-d", "-b", inputBuffer, "-t", t.Session)
	return err
}

// ListClients returns the currently attached clients.
func (t *Tmux) ListClients() ([]SessionClient, error) {
	out, err := t.run("list-clients", "-t", t.Session, "-F", clientFormat)
	if err != nil {
		// A session with no clients at all is not an error condition.
		if !t.HasSession() {
			return nil, err
		}
		return nil, nil
	}
	return parseClients(out)
}

// PaneDead reports whether the hosted process has exited and, if so, its exit
// status. remain-on-exit keeps the pane around long enough to observe this.
func (t *Tmux) PaneDead() (bool, int, error) {
	out, err := t.run("display-message", "-p", "-t", t.Session, "#{pane_dead}|#{pane_dead_status}")
	if err != nil {
		return false, 0, err
	}
	parts := strings.SplitN(strings.TrimSpace(out), "|", 2)
	if parts[0] != "1" {
		return false, 0, nil
	}
	status := 0
	if len(parts) == 2 {
		status, _ = strconv.Atoi(parts[1])
	}
	return true, status, nil
}

// Respawn restarts the hosted process in the existing pane, preserving the
// session (and therefore attached clients and the output pipe) across it.
func (t *Tmux) Respawn() error {
	_, err := t.run("respawn-pane", "-k", "-t", t.Session)
	return err
}

// Kill tears the whole tmux server down. Only the supervisor's own shutdown
// path and tests use it.
func (t *Tmux) Kill() error {
	_, err := t.run("kill-server")
	return err
}

// Attach replaces the current process with a tmux client. readOnly is the
// default for developer connections (3.3); a writable client is only handed
// out on an explicit handover request (3.4).
func (t *Tmux) Attach(readOnly bool) error {
	bin, err := exec.LookPath("tmux")
	if err != nil {
		return err
	}
	extra := []string{"attach-session", "-t", t.Session}
	if readOnly {
		extra = append(extra, "-r")
	}
	argv := append([]string{"tmux"}, t.args(extra...)...)
	return syscallExec(bin, argv, os.Environ())
}

func parseClients(out string) ([]SessionClient, error) {
	var clients []SessionClient
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, "|", 3)
		if len(fields) != 3 {
			return nil, fmt.Errorf("supervisor: unparsable client line %q", line)
		}
		attachedAt := ""
		if secs, err := strconv.ParseInt(fields[2], 10, 64); err == nil {
			attachedAt = time.Unix(secs, 0).UTC().Format(time.RFC3339)
		}
		clients = append(clients, SessionClient{
			ID:         fields[0],
			Writable:   fields[1] == "0",
			AttachedAt: attachedAt,
		})
	}
	return clients, nil
}

func firstWritable(clients []SessionClient) *SessionClient {
	for i := range clients {
		if clients[i].Writable {
			return &clients[i]
		}
	}
	return nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
