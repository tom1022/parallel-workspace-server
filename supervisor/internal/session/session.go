package session

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sync"
)

// SendError kinds, mirroring the SessionControl contract in design.md.
const (
	SendWritableClientPresent = "WritableClientPresent"
	SendSessionNotRunning     = "SessionNotRunning"
	SendInputRejected         = "InputRejected"
)

// SendError explains why an input could not be delivered.
type SendError struct {
	Kind     string `json:"kind"`
	ClientID string `json:"clientId,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

func (e *SendError) Error() string {
	if e.ClientID != "" {
		return fmt.Sprintf("supervisor: %s (%s)", e.Kind, e.ClientID)
	}
	if e.Detail != "" {
		return fmt.Sprintf("supervisor: %s: %s", e.Kind, e.Detail)
	}
	return "supervisor: " + e.Kind
}

// SessionClient is one client attached to the session.
type SessionClient struct {
	ID         string `json:"id"`
	Writable   bool   `json:"writable"`
	AttachedAt string `json:"attachedAt"`
}

// OutputChunk carries session output plus the offset to resume from. Seq is the
// byte offset just past this chunk, so a caller replays with no gap or overlap.
type OutputChunk struct {
	Seq  int64  `json:"seq"`
	Data string `json:"data"`
}

// Supervisor exposes the session to processes that are not attached to it.
type Supervisor struct {
	Tmux      *Tmux
	ConfigDir string
	OutputLog string
	// SSHPort is the port SSHEndpoint listens on; zero means DefaultSSHPort.
	SSHPort int

	mu      sync.RWMutex
	failure string
}

// SetFailure records that the hosted process died. The transcript cannot show
// this — it simply stops — so the crash is surfaced through the turn state,
// which is the one channel an observer polls anyway. Hermes Agent has no
// inbound endpoint of its own in this design, so the push notification is
// best-effort and this is what makes the failure reliably observable (2.5).
func (s *Supervisor) SetFailure(detail string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failure = detail
}

// ClearFailure marks the process healthy again after a successful restart.
func (s *Supervisor) ClearFailure() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failure = ""
}

// SendInput delivers text to the session on behalf of a process that is not
// attached (2.7). It refuses while a writable developer client holds the
// session, which is the enforcement point behind 3.5: Hermes Agent's own
// check is advisory, this one is mechanical.
func (s *Supervisor) SendInput(text string) error {
	if !s.Tmux.HasSession() {
		return &SendError{Kind: SendSessionNotRunning}
	}
	clients, err := s.Tmux.ListClients()
	if err != nil {
		return &SendError{Kind: SendInputRejected, Detail: err.Error()}
	}
	if c := firstWritable(clients); c != nil {
		return &SendError{Kind: SendWritableClientPresent, ClientID: c.ID}
	}
	if err := s.Tmux.SendText(text); err != nil {
		return &SendError{Kind: SendInputRejected, Detail: err.Error()}
	}
	return nil
}

// ReadOutput returns everything written after sinceSeq, in order (2.6).
func (s *Supervisor) ReadOutput(sinceSeq int64) ([]OutputChunk, error) {
	f, err := os.Open(s.OutputLog)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	if _, err := f.Seek(sinceSeq, io.SeekStart); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, nil
	}
	return []OutputChunk{{Seq: sinceSeq + int64(len(data)), Data: string(data)}}, nil
}

// ListClients reports who is attached and whether each may write (3.2).
func (s *Supervisor) ListClients() ([]SessionClient, error) {
	clients, err := s.Tmux.ListClients()
	if err != nil {
		return nil, err
	}
	if clients == nil {
		return []SessionClient{}, nil
	}
	return clients, nil
}

// SSHSessionCount publishes how many SSH sessions hold this workspace, so the
// Workspace Controller can fold them into idle detection from its own
// reconcile loop (4.8). It is exposed as readable state rather than pushed:
// the dependency runs Control Plane -> Workspace Runtime, and the supervisor
// never writes the Workspace's status itself.
func (s *Supervisor) SSHSessionCount() (int, error) {
	port := s.SSHPort
	if port == 0 {
		port = DefaultSSHPort
	}
	return SSHSessions(port)
}

// TurnState reports where the session stands in the current turn (2.9, 2.10).
func (s *Supervisor) TurnState() (TurnState, error) {
	s.mu.RLock()
	failure := s.failure
	s.mu.RUnlock()
	if failure != "" {
		return TurnState{Kind: TurnFailed, Detail: failure}, nil
	}
	return TurnStateFrom(s.ConfigDir)
}
