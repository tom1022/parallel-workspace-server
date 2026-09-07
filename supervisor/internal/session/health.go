package session

import (
	"net/http"
	"os"
	"path/filepath"
	"sync"
)

// unusablePrefix names the files the mark is kept in, so a process that is not
// the one serving /health can set it — the database bootstrap runs in the init
// container and is gone by the time anyone asks (8.7). One file per Source: a
// check that passes clears only its own verdict, never another check's.
const unusablePrefix = "unusable-"

// Health is the workspace's usability flag. It is separate from the turn state
// on purpose: a turn can fail and be retried, whereas an unusable workspace
// must not be handed work at all until an operator looks at it (5.8).
type Health struct {
	// ConfigDir is where the quota reading is served from, and where the
	// unusable marks are kept.
	ConfigDir string
	// Source names the check this Health speaks for. Serving /health leaves it
	// empty, which reports every check's mark and sets none.
	Source string

	mu       sync.RWMutex
	unusable string
}

// MarkUnusable records that the workspace must not be given work, with the
// reason. Chat relay is the platform's only push path and it can be down, so
// this durable, pollable mark — not the notification — is the real signal.
func (h *Health) MarkUnusable(detail string) {
	h.mu.Lock()
	h.unusable = detail
	h.mu.Unlock()
	if path := h.markPath(); path != "" {
		// Best-effort: the in-memory mark above still answers this process's
		// own /health, so a full volume must not turn a reported failure into
		// a panic.
		_ = os.WriteFile(path, []byte(detail), 0o600)
	}
}

// ClearUnusable withdraws this check's mark, which is what makes a failure
// recoverable by the check passing on a later start rather than only by an
// operator deleting the workspace.
func (h *Health) ClearUnusable() {
	h.mu.Lock()
	h.unusable = ""
	h.mu.Unlock()
	if path := h.markPath(); path != "" {
		_ = os.Remove(path)
	}
}

// Status reports whether the workspace may be handed work, and why not.
func (h *Health) Status() (bool, string) {
	h.mu.RLock()
	detail := h.unusable
	h.mu.RUnlock()
	if detail != "" || h.ConfigDir == "" {
		return detail == "", detail
	}
	marks, _ := filepath.Glob(filepath.Join(h.ConfigDir, unusablePrefix+"*"))
	for _, path := range marks {
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		return false, string(b)
	}
	return true, ""
}

// Handler publishes usability and the quota reading to processes outside the
// workspace (5.8, 7.10).
func (h *Health) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		usable, detail := h.Status()
		writeJSONResponse(w, http.StatusOK, map[string]any{"usable": usable, "detail": detail})
	})

	mux.HandleFunc("GET /usage", func(w http.ResponseWriter, r *http.Request) {
		snapshots, err := Usage(h.ConfigDir)
		if err != nil {
			writeJSONResponse(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if snapshots == nil {
			snapshots = []UsageSnapshot{}
		}
		writeJSONResponse(w, http.StatusOK, snapshots)
	})

	return mux
}

func (h *Health) markPath() string {
	if h.ConfigDir == "" || h.Source == "" {
		return ""
	}
	return filepath.Join(h.ConfigDir, unusablePrefix+h.Source)
}
