package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"time"

	"golang.org/x/net/websocket"
)

// DefaultPollInterval is how often the session socket asks the Session
// Supervisor for new output. The supervisor exposes the session as an
// offset-addressed log rather than a stream, so the gateway polls it and turns
// the result into the stream a browser terminal expects.
const DefaultPollInterval = 200 * time.Millisecond

// Handler serves both faces of the gateway: the browser terminal and the
// /api/ control surface. Every route the gateway exposes is identified by
// Identity before it reaches application logic (4.1); Verifier's own
// RequireBearer stays as the external-IdP building block Identity wraps
// (IDPAuthenticator), not as a second gate.
type Handler struct {
	Store    *Store
	Verifier *Verifier
	// Identity identifies the caller behind every request this Handler
	// serves. A Handler with a nil or empty Identity refuses every request
	// (4.1's invariant: no route may fall back to being unprotected).
	Identity *Identity
	// CA may be nil while the Infisical-synced signing key is absent; only
	// certificate issuance degrades.
	CA *CertificateAuthority
	// Local wires the self-hosted login/password/version endpoints. Nil
	// disables them (503) without touching Identity's authenticator list —
	// the caller is responsible for keeping the two consistent when local
	// authentication is stopped (4.9).
	Local *LocalAuth
	// Version, MinClientVersion, and MaxClientVersion answer GET /version
	// (11.4). Empty means "unset" — a client that gets no constraint back is
	// expected to hold off judging compatibility rather than refuse to work.
	Version          string
	MinClientVersion string
	MaxClientVersion string
	// PollInterval overrides DefaultPollInterval.
	PollInterval time.Duration
	// HTTPClient talks to the Session Supervisor. Nil means http.DefaultClient.
	HTTPClient *http.Client

	// sessions is the live browser connection bookkeeping behind /handover and
	// the connection reports idle detection reads.
	sessions browserSessions
}

func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /version", h.version)

	mux.HandleFunc("GET /{$}", h.serveTerminal)
	mux.HandleFunc("GET /ws", h.serveSessionSocket)
	mux.Handle("POST /handover", h.Identity.Require(http.HandlerFunc(h.handover)))

	// /auth/login issues a session, so it cannot require one; every other
	// /auth/ route needs an identified caller (400/503 vs. 401 is decided
	// inside each handler, per the API contract's error columns).
	mux.HandleFunc("POST /auth/login", h.login)
	mux.Handle("POST /auth/logout", h.Identity.Require(http.HandlerFunc(h.logout)))
	mux.Handle("GET /auth/me", h.Identity.Require(http.HandlerFunc(h.me)))
	mux.Handle("POST /auth/password", h.Identity.Require(http.HandlerFunc(h.changePassword)))

	api := http.NewServeMux()
	api.HandleFunc("GET /api/workspaces", h.listWorkspaces)
	api.HandleFunc("POST /api/workspaces", h.createWorkspace)
	api.HandleFunc("DELETE /api/workspaces/{id}", h.deleteWorkspace)
	api.HandleFunc("GET /api/workspaces/{id}/session", h.sessionStatus)
	api.HandleFunc("POST /api/ssh/certificate", h.issueCertificate)
	mux.Handle("/api/", h.Identity.Require(api))

	return mux
}

func (h *Handler) client() *http.Client {
	if h.HTTPClient != nil {
		return h.HTTPClient
	}
	return http.DefaultClient
}

func (h *Handler) pollInterval() time.Duration {
	if h.PollInterval > 0 {
		return h.PollInterval
	}
	return DefaultPollInterval
}

// serveTerminal never asks the Store anything before it knows who is
// asking: a plain browser navigation cannot carry the Authorization header
// PresentedToken reads, so most loads of this page arrive unidentified, and
// the response for those has to be the same regardless of which workspace
// hostname it came in on — resolving the host first and discarding the
// result would still leave that hostname's existence and identity to time
// (4.1's "基盤の情報を一切返さない").
func (h *Handler) serveTerminal(w http.ResponseWriter, r *http.Request) {
	if _, err := h.Identity.Identify(r.Context(), r); err != nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(signInPage))
		return
	}

	ws, err := h.Store.ResolveHost(r.Context(), r.Host)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The client is delivered per request rather than installed anywhere, which
	// is what keeps the required path free of setup on the developer's machine
	// (4.1 / 4.2).
	if err := terminalPage.Execute(w, map[string]string{"Workspace": ws.Name, "Branch": ws.Branch}); err != nil {
		return
	}
}

// serveSessionSocket identifies the caller before the handshake is even
// hijacked: the credential rides the Sec-WebSocket-Protocol negotiation
// (PresentedToken), so it is already on the request by the time this runs,
// and an unidentified request never reaches websocket.Handler at all — no
// connection is established for it (4.1).
func (h *Handler) serveSessionSocket(w http.ResponseWriter, r *http.Request) {
	if _, err := h.Identity.Identify(r.Context(), r); err != nil {
		w.Header().Set("WWW-Authenticate", `Bearer realm="devplatform-gateway"`)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	ws, err := h.Store.ResolveHost(r.Context(), r.Host)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	endpoint, err := h.Store.SessionEndpoint(r.Context(), ws)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	sessionID := r.URL.Query().Get("sessionId")
	if sessionID == "" {
		sessionID = newSessionID()
	}
	websocket.Handler(func(conn *websocket.Conn) {
		h.reportConnections(ws.Name, h.sessions.attach(ws.Name, sessionID))
		defer func() {
			if h.sessions.writable(ws.Name, sessionID) {
				h.releaseWritable(endpoint, sessionID)
			}
			h.reportConnections(ws.Name, h.sessions.detach(ws.Name, sessionID))
		}()
		// Terminal output is arbitrary bytes; framing it as text would let a
		// non-UTF-8 escape sequence make a browser drop the connection.
		conn.PayloadType = websocket.BinaryFrame
		h.bridge(conn, endpoint, ws.Name, sessionID)
	}).ServeHTTP(w, r)
}

// bridge couples one browser socket to a workspace's session: output polled out
// of the supervisor goes down the socket, and everything typed goes back in
// through the supervisor's input endpoint, which is where the single-writer rule
// is enforced. Output flows to every connection; input only from the one that
// asked for and was granted handover (3.3 / 3.4).
func (h *Handler) bridge(conn *websocket.Conn, endpoint, workspace, sessionID string) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		defer cancel()
		buf := make([]byte, 4096)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				return
			}
			if n == 0 || !h.sessions.writable(workspace, sessionID) {
				continue
			}
			if err := h.sendInput(ctx, endpoint, sessionID, string(buf[:n])); err != nil {
				return
			}
		}
	}()

	ticker := time.NewTicker(h.pollInterval())
	defer ticker.Stop()
	var since int64
	for {
		chunks, err := h.readOutput(ctx, endpoint, since)
		if err != nil {
			return
		}
		for _, chunk := range chunks {
			if _, err := conn.Write([]byte(chunk.Data)); err != nil {
				return
			}
			since = chunk.Seq
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

type outputChunk struct {
	Seq  int64  `json:"seq"`
	Data string `json:"data"`
}

func (h *Handler) readOutput(ctx context.Context, endpoint string, since int64) ([]outputChunk, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/output?since=%d", endpoint, since), nil)
	if err != nil {
		return nil, err
	}
	resp, err := h.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("gateway: session output: status %d", resp.StatusCode)
	}
	var body struct {
		Chunks []outputChunk `json:"chunks"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	return body.Chunks, nil
}

func (h *Handler) sendInput(ctx context.Context, endpoint, sessionID, text string) error {
	// Naming the sender is what distinguishes the developer who took the
	// session over from a third party: the supervisor refuses input from
	// anyone but the client holding it.
	payload, err := json.Marshal(map[string]string{"text": text, "clientId": sessionID})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+"/input", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

func (h *Handler) listWorkspaces(w http.ResponseWriter, r *http.Request) {
	summaries, err := h.Store.List(r.Context())
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, summaries)
}

func (h *Handler) createWorkspace(w http.ResponseWriter, r *http.Request) {
	var req CreateWorkspaceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	created, err := h.Store.Create(r.Context(), req)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (h *Handler) deleteWorkspace(w http.ResponseWriter, r *http.Request) {
	if err := h.Store.Delete(r.Context(), r.PathValue("id")); err != nil {
		writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) sessionStatus(w http.ResponseWriter, r *http.Request) {
	ws, err := h.Store.Find(r.Context(), r.PathValue("id"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	endpoint, err := h.Store.SessionEndpoint(r.Context(), ws)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	turn, err := h.readTurn(r.Context(), endpoint)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"name":        ws.Name,
		"workspaceId": ws.WorkspaceId,
		"sessionId":   ws.SessionId,
		"phase":       ws.Phase,
		"turn":        turn,
	})
}

func (h *Handler) readTurn(ctx context.Context, endpoint string) (json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/turn", nil)
	if err != nil {
		return nil, err
	}
	resp, err := h.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("gateway: session turn state: status %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

func (h *Handler) issueCertificate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		WorkspaceId string `json:"workspaceId"`
		PublicKey   string `json:"publicKey"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if h.CA == nil || h.CA.Signer == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "no ssh certificate authority configured"})
		return
	}
	// Signing only for a workspace that exists is what keeps the principal
	// namespace tied to real workspaces (4.11).
	ws, err := h.Store.Find(r.Context(), req.WorkspaceId)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	principal := ws.WorkspaceId
	if principal == "" {
		principal = ws.Name
	}
	issued, err := h.CA.Issue(principal, req.PublicKey)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, issued)
}

func writeStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "workspace not found"})
	case errors.Is(err, ErrAlreadyExists):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "workspace already exists for this branch"})
	case errors.Is(err, ErrInvalid):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	default:
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
	}
}

// signInPage is what an unidentified GET /{$} gets instead of the terminal:
// no workspace name, branch, or any other detail of what this hostname
// serves (4.1). It carries no workspace-specific content, so it is a plain
// constant rather than a template.
const signInPage = `<!doctype html>
<html lang="en">
<head><meta charset="utf-8"><title>Sign in required</title></head>
<body>
<p>Authentication required. Obtain a session (POST /auth/login or your
configured identity provider) and present it as a Bearer credential.</p>
</body>
</html>
`

var terminalPage = template.Must(template.New("terminal").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>{{.Workspace}} — {{.Branch}}</title>
<link rel="stylesheet" href="https://cdnjs.cloudflare.com/ajax/libs/xterm/5.3.0/xterm.min.css">
<style>
html,body{margin:0;height:100%;background:#000;color:#ccc;font:12px system-ui,sans-serif}
#bar{height:24px;display:flex;align-items:center;gap:8px;padding:0 8px}
#terminal{height:calc(100% - 24px)}
</style>
</head>
<body>
<div id="bar"><span id="mode">read-only</span><button id="takeover">take over</button></div>
<div id="terminal"></div>
<script src="https://cdnjs.cloudflare.com/ajax/libs/xterm/5.3.0/xterm.min.js"></script>
<script src="https://cdnjs.cloudflare.com/ajax/libs/xterm-addon-fit/0.8.0/xterm-addon-fit.min.js"></script>
<script>
const term = new Terminal({ convertEol: false });
const fit = new FitAddon.FitAddon();
term.loadAddon(fit);
term.open(document.getElementById('terminal'));
fit.fit();
addEventListener('resize', () => fit.fit());

// The connection names itself so /handover has something to promote; it stays
// read-only until it does (3.3).
const sessionId = (crypto.randomUUID ? crypto.randomUUID() : String(Date.now()) + Math.random());
const socket = new WebSocket((location.protocol === 'https:' ? 'wss://' : 'ws://') + location.host + '/ws?sessionId=' + sessionId);
socket.binaryType = 'arraybuffer';
socket.onmessage = (event) => term.write(new Uint8Array(event.data));
socket.onclose = () => term.write('\r\n[session disconnected]\r\n');
term.onData((data) => socket.readyState === WebSocket.OPEN && socket.send(data));

const mode = document.getElementById('mode');
const takeover = document.getElementById('takeover');
takeover.onclick = async () => {
  const res = await fetch('/handover', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ sessionId }),
  });
  if (res.ok) {
    mode.textContent = 'read-write';
    takeover.disabled = true;
  } else {
    mode.textContent = res.status === 409 ? 'read-only (another client holds the session)' : 'read-only (handover failed)';
  }
};
</script>
</body>
</html>
`))
