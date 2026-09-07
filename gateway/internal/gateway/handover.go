package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// browserSessions tracks the browser connections this gateway holds open, per
// workspace, and which one of them (at most one) may drive the session.
//
// It is gateway-local state rather than anything durable: a connection cannot
// outlive the process serving its socket, so a restart correctly forgets every
// one of them. The writable flag here is only half of the single-writer rule —
// the Session Supervisor refuses input mechanically whenever a writable client
// holds the session (3.5), and that check stands regardless of what this says.
type browserSessions struct {
	mu sync.Mutex
	// byWorkspace maps workspace name -> session id -> writable.
	byWorkspace map[string]map[string]bool
}

func (b *browserSessions) attach(workspace, id string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.byWorkspace == nil {
		b.byWorkspace = map[string]map[string]bool{}
	}
	if b.byWorkspace[workspace] == nil {
		b.byWorkspace[workspace] = map[string]bool{}
	}
	b.byWorkspace[workspace][id] = false
	return len(b.byWorkspace[workspace])
}

func (b *browserSessions) detach(workspace, id string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.byWorkspace[workspace], id)
	remaining := len(b.byWorkspace[workspace])
	if remaining == 0 {
		delete(b.byWorkspace, workspace)
	}
	return remaining
}

// promote hands the session to id. known is false when id names no live
// connection; ok is false when another connection already holds it.
func (b *browserSessions) promote(workspace, id string) (ok, known bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	sessions := b.byWorkspace[workspace]
	if _, exists := sessions[id]; !exists {
		return false, false
	}
	for other, writable := range sessions {
		if writable && other != id {
			return false, true
		}
	}
	sessions[id] = true
	return true, true
}

func (b *browserSessions) release(workspace, id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, exists := b.byWorkspace[workspace][id]; exists {
		b.byWorkspace[workspace][id] = false
	}
}

func (b *browserSessions) writable(workspace, id string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.byWorkspace[workspace][id]
}

// handover promotes a watching connection to read-write (3.4). It is on the
// browser path, so the forward auth chain in front of the gateway is what
// authenticated the caller.
func (h *Handler) handover(w http.ResponseWriter, r *http.Request) {
	ws, err := h.Store.ResolveHost(r.Context(), r.Host)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	var req struct {
		SessionId string `json:"sessionId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	endpoint, err := h.Store.SessionEndpoint(r.Context(), ws)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	// A writable client the multiplexer knows about — an IDE attached over SSH,
	// say — is invisible to this gateway's own bookkeeping, so it has to be
	// asked. An unanswerable question refuses the promotion: the single-writer
	// rule is not something to guess at.
	if err := h.refuseIfWritableClientPresent(r.Context(), endpoint); err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}

	ok, known := h.sessions.promote(ws.Name, req.SessionId)
	if !known {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such session connection"})
		return
	}
	if !ok {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "another client already holds this session read-write"})
		return
	}
	// A browser reaching the session through this gateway never attaches to the
	// multiplexer, so the supervisor's client list cannot see it. Declaring the
	// takeover there is what makes Hermes Agent's check (3.7) find a writable
	// client and stop sending (3.5). A promotion the supervisor did not record
	// is worse than no promotion at all — it would leave the developer typing
	// into a session the agent is still driving — so it is undone here.
	if err := h.declareWritable(r.Context(), http.MethodPost, endpoint, req.SessionId); err != nil {
		h.sessions.release(ws.Name, req.SessionId)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"writable": true})
}

func (h *Handler) declareWritable(ctx context.Context, method, endpoint, sessionID string) error {
	req, err := http.NewRequestWithContext(ctx, method, endpoint+"/clients/"+url.PathEscape(sessionID), nil)
	if err != nil {
		return err
	}
	resp, err := h.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("gateway: session clients: status %d", resp.StatusCode)
	}
	return nil
}

// releaseWritable hands the session back when the browser holding it goes away,
// which is what lets Hermes Agent resume (3.6). Like the connection report it
// runs on its own context: the request that carried the socket is already over.
func (h *Handler) releaseWritable(endpoint, sessionID string) {
	ctx, cancel := context.WithTimeout(context.Background(), connectionReportTimeout)
	defer cancel()
	if err := h.declareWritable(ctx, http.MethodDelete, endpoint, sessionID); err != nil {
		log.Printf("gateway: releasing write access for %s: %v", sessionID, err)
	}
}

func (h *Handler) refuseIfWritableClientPresent(ctx context.Context, endpoint string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/clients", nil)
	if err != nil {
		return err
	}
	resp, err := h.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("gateway: session clients: status %d", resp.StatusCode)
	}
	var clients []struct {
		ID       string `json:"id"`
		Writable bool   `json:"writable"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&clients); err != nil {
		return err
	}
	for _, c := range clients {
		if c.Writable {
			return fmt.Errorf("gateway: client %s already holds this session read-write", c.ID)
		}
	}
	return nil
}

// reportConnections tells the Workspace Controller how many browsers are on
// this workspace, which is the idle-detection input for the browser route
// (4.8). It runs on its own context because the disconnect report is made
// after the request that carried the connection is already over.
func (h *Handler) reportConnections(workspace string, count int) {
	ctx, cancel := context.WithTimeout(context.Background(), connectionReportTimeout)
	defer cancel()
	if err := h.Store.SetBrowserConnections(ctx, workspace, count); err != nil {
		// Best-effort: losing a report costs idle-detection accuracy, and must
		// not take down a session the developer is using.
		log.Printf("gateway: reporting %d browser connections for %s: %v", count, workspace, err)
	}
}

const connectionReportTimeout = 5 * time.Second

// newSessionID names a connection that arrived without an identifier of its
// own. Such a connection still counts toward idle detection; it simply has no
// name to ask for handover with.
func newSessionID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "anonymous"
	}
	return hex.EncodeToString(b[:])
}
