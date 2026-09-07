package gateway

import (
	"encoding/json"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/websocket"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

// stubSupervisor stands in for a workspace's Session Supervisor, including the
// single-writer rule it enforces mechanically: input is refused while some
// other client holds the session read-write.
type stubSupervisor struct {
	mu             sync.Mutex
	output         string
	input          []string
	writableClient bool
	// holders are the takeovers declared by the gateway for clients that reach
	// the session over HTTP instead of attaching to the multiplexer.
	holders map[string]bool
}

const stubAttachedClientID = "/dev/pts/3"

// blockingClientLocked names a writable client other than clientID, if any.
func (s *stubSupervisor) blockingClientLocked(clientID string) string {
	if s.writableClient && clientID != stubAttachedClientID {
		return stubAttachedClientID
	}
	for _, id := range slices.Sorted(maps.Keys(s.holders)) {
		if id != clientID {
			return id
		}
	}
	return ""
}

func (s *stubSupervisor) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /output", func(w http.ResponseWriter, r *http.Request) {
		since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
		s.mu.Lock()
		defer s.mu.Unlock()
		if since >= int64(len(s.output)) {
			writeJSON(w, http.StatusOK, map[string]any{"chunks": []any{}})
			return
		}
		data := s.output[since:]
		writeJSON(w, http.StatusOK, map[string]any{"chunks": []map[string]any{
			{"seq": since + int64(len(data)), "data": data},
		}})
	})
	mux.HandleFunc("POST /input", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Text     string `json:"text"`
			ClientID string `json:"clientId"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.mu.Lock()
		defer s.mu.Unlock()
		if blocker := s.blockingClientLocked(body.ClientID); blocker != "" {
			writeJSON(w, http.StatusConflict, map[string]string{
				"kind":     "WritableClientPresent",
				"clientId": blocker,
			})
			return
		}
		s.input = append(s.input, body.Text)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /clients", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		clients := []map[string]any{}
		if s.writableClient {
			clients = append(clients, map[string]any{"id": stubAttachedClientID, "writable": true, "attachedAt": "2026-09-06T00:00:00Z"})
		}
		for _, id := range slices.Sorted(maps.Keys(s.holders)) {
			clients = append(clients, map[string]any{"id": id, "writable": true, "attachedAt": "2026-09-06T00:00:00Z"})
		}
		writeJSON(w, http.StatusOK, clients)
	})
	mux.HandleFunc("POST /clients/{id}", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		if s.holders == nil {
			s.holders = map[string]bool{}
		}
		s.holders[r.PathValue("id")] = true
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("DELETE /clients/{id}", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		delete(s.holders, r.PathValue("id"))
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /turn", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"state": "AwaitingInput"})
	})
	return mux
}

func (s *stubSupervisor) setWritableClient(writable bool) {
	s.mu.Lock()
	s.writableClient = writable
	s.mu.Unlock()
}

func (s *stubSupervisor) sent() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.input...)
}

func runningPod(workspace, ip string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata": map[string]any{
			"name":      workspace + "-0",
			"namespace": "devplatform-workspaces",
			"labels":    map[string]any{labelWorkspaceName: workspace},
		},
		"status": map[string]any{"phase": "Running", "podIP": ip},
	}}
}

type testRig struct {
	handler    http.Handler
	token      string
	supervisor *stubSupervisor
	store      *Store
	// backendURL is where the stub Session Supervisor listens, so a test can
	// reach it the way Hermes Agent does: directly, not through the gateway.
	backendURL string
}

func newTestRig(t *testing.T, objs ...runtime.Object) *testRig {
	t.Helper()
	verifier, key := newTestVerifier(t)
	ca, _ := newTestCA(t)

	supervisor := &stubSupervisor{output: "hello from tmux"}
	backend := httptest.NewServer(supervisor.handler())
	t.Cleanup(backend.Close)
	host, port, err := net.SplitHostPort(strings.TrimPrefix(backend.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	supervisorPort, _ := strconv.Atoi(port)

	store := newTestStore(t, append(objs, runningPod("feature-login", host))...)
	store.SupervisorPort = supervisorPort

	return &testRig{
		handler:    (&Handler{Store: store, Verifier: verifier, CA: ca, PollInterval: 10 * time.Millisecond}).Routes(),
		token:      signToken(t, key, validClaims()),
		supervisor: supervisor,
		store:      store,
		backendURL: backend.URL,
	}
}

func (r *testRig) do(t *testing.T, method, target string, body string, opts ...func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, reader)
	for _, opt := range opts {
		opt(req)
	}
	rec := httptest.NewRecorder()
	r.handler.ServeHTTP(rec, req)
	return rec
}

func (r *testRig) authorized() func(*http.Request) {
	return func(req *http.Request) { req.Header.Set("Authorization", "Bearer "+r.token) }
}

func withHost(host string) func(*http.Request) {
	return func(req *http.Request) { req.Host = host }
}

func TestBrowserRootServesTerminalForResolvedHost(t *testing.T) {
	rig := newTestRig(t, testWorkspace("feature-login", "feature/login", "feature-login", "Ready"))

	rec := rig.do(t, http.MethodGet, "/", "", withHost("feature-login.fickledev.com"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "/ws") {
		t.Fatal("terminal page does not point at the session socket")
	}
}

func TestBrowserRootRejectsUnknownHost(t *testing.T) {
	rig := newTestRig(t, testWorkspace("feature-login", "feature/login", "feature-login", "Ready"))
	if rec := rig.do(t, http.MethodGet, "/", "", withHost("nope.fickledev.com")); rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestAPIRequiresItsOwnBearerVerification(t *testing.T) {
	rig := newTestRig(t, testWorkspace("feature-login", "feature/login", "feature-login", "Ready"))

	// Reaching the gateway at all means the forward auth chain let the request
	// through; that must not be taken as authorization (4.12).
	for _, target := range []string{
		"/api/workspaces",
		"/api/workspaces/feature-login/session",
	} {
		rec := rig.do(t, http.MethodGet, target, "", withHost("workspaces.fickledev.com"))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s status = %d, want 401", target, rec.Code)
		}
		if rec.Header().Get("Location") != "" {
			t.Errorf("GET %s answered with a redirect", target)
		}
	}
	if rec := rig.do(t, http.MethodDelete, "/api/workspaces/feature-login", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("DELETE status = %d, want 401", rec.Code)
	}
	if rec := rig.do(t, http.MethodPost, "/api/ssh/certificate", `{"workspaceId":"feature-login","publicKey":"x"}`); rec.Code != http.StatusUnauthorized {
		t.Errorf("POST /api/ssh/certificate status = %d, want 401", rec.Code)
	}
}

func TestListWorkspaces(t *testing.T) {
	rig := newTestRig(t, testWorkspace("feature-login", "feature/login", "feature-login", "Ready"))
	rec := rig.do(t, http.MethodGet, "/api/workspaces", "", rig.authorized())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got []WorkspaceSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Branch != "feature/login" {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestCreateAndDeleteWorkspace(t *testing.T) {
	rig := newTestRig(t)
	body := `{"repository":"https://gitea.example.com/tochi/portfolio.git","branch":"feature/login"}`

	rec := rig.do(t, http.MethodPost, "/api/workspaces", body, rig.authorized())
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	if rec := rig.do(t, http.MethodPost, "/api/workspaces", body, rig.authorized()); rec.Code != http.StatusConflict {
		t.Fatalf("duplicate create status = %d, want 409", rec.Code)
	}
	if rec := rig.do(t, http.MethodPost, "/api/workspaces", `{"branch":"x"}`, rig.authorized()); rec.Code != http.StatusBadRequest {
		t.Fatalf("incomplete create status = %d, want 400", rec.Code)
	}
	if rec := rig.do(t, http.MethodDelete, "/api/workspaces/feature-login", "", rig.authorized()); rec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", rec.Code)
	}
	if rec := rig.do(t, http.MethodDelete, "/api/workspaces/feature-login", "", rig.authorized()); rec.Code != http.StatusNotFound {
		t.Fatalf("repeat delete status = %d, want 404", rec.Code)
	}
}

func TestSessionStatus(t *testing.T) {
	rig := newTestRig(t, testWorkspace("feature-login", "feature/login", "feature-login", "Ready"))

	rec := rig.do(t, http.MethodGet, "/api/workspaces/feature-login/session", "", rig.authorized())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["phase"] != "Ready" || got["sessionId"] != "feature-login-session" {
		t.Fatalf("body = %s", rec.Body.String())
	}
	if got["turn"] == nil {
		t.Fatal("session status carries no turn state")
	}

	if rec := rig.do(t, http.MethodGet, "/api/workspaces/absent/session", "", rig.authorized()); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown workspace status = %d, want 404", rec.Code)
	}
}

func TestSessionStatusReportsUnreachableSupervisor(t *testing.T) {
	rig := newTestRig(t, testWorkspace("fix-crash", "fix/crash", "fix-crash", "Provisioning"))
	rec := rig.do(t, http.MethodGet, "/api/workspaces/fix-crash/session", "", rig.authorized())
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
}

func TestIssueSSHCertificate(t *testing.T) {
	rig := newTestRig(t, testWorkspace("feature-login", "feature/login", "feature-login", "Ready"))
	publicKey := newUserKey(t)

	rec := rig.do(t, http.MethodPost, "/api/ssh/certificate",
		`{"workspaceId":"feature-login","publicKey":`+strconv.Quote(publicKey)+`}`, rig.authorized())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var issued IssuedCertificate
	if err := json.Unmarshal(rec.Body.Bytes(), &issued); err != nil {
		t.Fatal(err)
	}
	if issued.Principal != "feature-login" || issued.Certificate == "" {
		t.Fatalf("body = %s", rec.Body.String())
	}

	// A certificate is only ever signed for a workspace that exists, so a
	// caller cannot mint a principal for something the cluster never had.
	rec = rig.do(t, http.MethodPost, "/api/ssh/certificate",
		`{"workspaceId":"absent","publicKey":`+strconv.Quote(publicKey)+`}`, rig.authorized())
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown workspace status = %d, want 404", rec.Code)
	}

	rec = rig.do(t, http.MethodPost, "/api/ssh/certificate",
		`{"workspaceId":"feature-login","publicKey":"garbage"}`, rig.authorized())
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed key status = %d, want 400", rec.Code)
	}
}

func TestIssueSSHCertificateWithoutCA(t *testing.T) {
	rig := newTestRig(t, testWorkspace("feature-login", "feature/login", "feature-login", "Ready"))
	// Replace the handler with one whose CA never synced.
	verifier, key := newTestVerifier(t)
	store := newTestStore(t, testWorkspace("feature-login", "feature/login", "feature-login", "Ready"))
	handler := (&Handler{Store: store, Verifier: verifier}).Routes()

	req := httptest.NewRequest(http.MethodPost, "/api/ssh/certificate",
		strings.NewReader(`{"workspaceId":"feature-login","publicKey":`+strconv.Quote(newUserKey(t))+`}`))
	req.Header.Set("Authorization", "Bearer "+signToken(t, key, validClaims()))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	_ = rig
}

func TestSessionSocketBridgesBothDirections(t *testing.T) {
	rig := newTestRig(t, testWorkspace("feature-login", "feature/login", "feature-login", "Ready"))
	server := httptest.NewServer(rig.handler)
	defer server.Close()

	// The Host header is the only thing that names the workspace on this path,
	// so the handshake targets the workspace hostname while the socket is dialed
	// at the test server's real address.
	config, err := websocket.NewConfig("ws://feature-login.fickledev.com/ws?sessionId=s-bridge", "http://feature-login.fickledev.com")
	if err != nil {
		t.Fatal(err)
	}
	dialer, err := net.Dial("tcp", strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	conn, err := websocket.NewClient(config, dialer)
	if err != nil {
		t.Fatalf("dial /ws: %v", err)
	}
	defer conn.Close()

	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 128)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("read session output: %v", err)
	}
	if string(buf[:n]) != "hello from tmux" {
		t.Fatalf("session output = %q", buf[:n])
	}

	// Input only flows once this connection holds the session read-write (3.4).
	if rec := rig.do(t, http.MethodPost, "/handover", `{"sessionId":"s-bridge"}`, withHost("feature-login.fickledev.com")); rec.Code != http.StatusOK {
		t.Fatalf("handover status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if _, err := conn.Write([]byte("ls\n")); err != nil {
		t.Fatal(err)
	}
	waitForInput(t, rig.supervisor, "ls\n")
}

func TestSessionSocketRejectsUnknownHost(t *testing.T) {
	rig := newTestRig(t)
	rec := rig.do(t, http.MethodGet, "/ws", "", withHost("nope.fickledev.com"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}
