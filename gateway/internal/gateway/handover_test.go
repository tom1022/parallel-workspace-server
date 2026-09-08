package gateway

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/websocket"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// dialSession opens the browser socket the way the terminal page does: at the
// workspace's hostname, carrying the session identifier /handover names and
// presenting token the way a browser has to at handshake time — as a
// WebSocket sub-protocol, since it cannot set a custom header here (4.1).
func dialSession(t *testing.T, server *httptest.Server, host, sessionID, token string) *websocket.Conn {
	t.Helper()
	target := "ws://" + host + "/ws"
	if sessionID != "" {
		target += "?sessionId=" + sessionID
	}
	config, err := websocket.NewConfig(target, "http://"+host)
	if err != nil {
		t.Fatal(err)
	}
	config.Protocol = []string{"bearer." + token}
	dialer, err := net.Dial("tcp", strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	conn, err := websocket.NewClient(config, dialer)
	if err != nil {
		t.Fatalf("dial /ws: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	// Draining the first output chunk proves the bridge is up before the test
	// starts asserting on what it does with input.
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Read(make([]byte, 128)); err != nil {
		t.Fatalf("read session output: %v", err)
	}
	return conn
}

func waitForInput(t *testing.T, sup *stubSupervisor, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if sent := sup.sent(); len(sent) > 0 {
			if sent[0] != want {
				t.Fatalf("input delivered = %q, want %q", sent[0], want)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("input %q never reached the session", want)
}

// 3.3: a browser connection watches by default; it does not drive the session
// until handover is asked for.
func TestSessionSocketIsReadOnlyUntilHandover(t *testing.T) {
	rig := newTestRig(t, testWorkspace("feature-login", "feature/login", "feature-login", "Ready"))
	server := httptest.NewServer(rig.handler)
	defer server.Close()

	conn := dialSession(t, server, "feature-login.fickledev.com", "s-1", rig.token)
	if _, err := conn.Write([]byte("rm -rf /\n")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if sent := rig.supervisor.sent(); len(sent) != 0 {
		t.Fatalf("read-only connection drove the session: %q", sent)
	}

	rec := rig.do(t, http.MethodPost, "/handover", `{"sessionId":"s-1"}`, withHost("feature-login.fickledev.com"), rig.authorized())
	if rec.Code != http.StatusOK {
		t.Fatalf("handover status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"writable":true`) {
		t.Fatalf("handover body = %s", rec.Body.String())
	}

	if _, err := conn.Write([]byte("ls\n")); err != nil {
		t.Fatal(err)
	}
	waitForInput(t, rig.supervisor, "ls\n")
}

// 409: only one client may hold the session read-write.
func TestHandoverRefusedWhileAnotherBrowserHoldsTheSession(t *testing.T) {
	rig := newTestRig(t, testWorkspace("feature-login", "feature/login", "feature-login", "Ready"))
	server := httptest.NewServer(rig.handler)
	defer server.Close()

	dialSession(t, server, "feature-login.fickledev.com", "s-1", rig.token)
	dialSession(t, server, "feature-login.fickledev.com", "s-2", rig.token)

	if rec := rig.do(t, http.MethodPost, "/handover", `{"sessionId":"s-1"}`, withHost("feature-login.fickledev.com"), rig.authorized()); rec.Code != http.StatusOK {
		t.Fatalf("first handover status = %d, want 200", rec.Code)
	}
	if rec := rig.do(t, http.MethodPost, "/handover", `{"sessionId":"s-2"}`, withHost("feature-login.fickledev.com"), rig.authorized()); rec.Code != http.StatusConflict {
		t.Fatalf("second handover status = %d, want 409", rec.Code)
	}
}

// The writable client may be an IDE attached over SSH rather than a browser,
// and the session multiplexer is the only place that knows about it.
func TestHandoverRefusedWhileAWritableSessionClientIsAttached(t *testing.T) {
	rig := newTestRig(t, testWorkspace("feature-login", "feature/login", "feature-login", "Ready"))
	rig.supervisor.setWritableClient(true)
	server := httptest.NewServer(rig.handler)
	defer server.Close()

	dialSession(t, server, "feature-login.fickledev.com", "s-1", rig.token)
	if rec := rig.do(t, http.MethodPost, "/handover", `{"sessionId":"s-1"}`, withHost("feature-login.fickledev.com"), rig.authorized()); rec.Code != http.StatusConflict {
		t.Fatalf("handover status = %d, want 409", rec.Code)
	}
}

func TestHandoverRejectsUnknownSessionAndHost(t *testing.T) {
	rig := newTestRig(t, testWorkspace("feature-login", "feature/login", "feature-login", "Ready"))
	if rec := rig.do(t, http.MethodPost, "/handover", `{"sessionId":"nope"}`, withHost("feature-login.fickledev.com"), rig.authorized()); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown session status = %d, want 404", rec.Code)
	}
	if rec := rig.do(t, http.MethodPost, "/handover", `{"sessionId":"s-1"}`, withHost("absent.fickledev.com"), rig.authorized()); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown host status = %d, want 404", rec.Code)
	}
}

// 4.8: connect and disconnect are reported to the Workspace Controller, which
// is what keeps a watched workspace out of idle detection.
func TestConnectAndDisconnectAreReportedToTheController(t *testing.T) {
	rig := newTestRig(t, testWorkspace("feature-login", "feature/login", "feature-login", "Ready"))
	server := httptest.NewServer(rig.handler)
	defer server.Close()

	conn := dialSession(t, server, "feature-login.fickledev.com", "s-1", rig.token)
	waitForBrowserConnections(t, rig, "1")

	conn.Close()
	waitForBrowserConnections(t, rig, "")
}

func waitForBrowserConnections(t *testing.T, rig *testRig, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var got string
	for time.Now().Before(deadline) {
		obj, err := rig.store.Dynamic.Resource(workspaceGVR).Namespace(rig.store.Namespace).
			Get(context.Background(), "feature-login", metav1.GetOptions{})
		if err != nil {
			t.Fatalf("get Workspace: %v", err)
		}
		got = obj.GetAnnotations()[annotationBrowserConnections]
		if got == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("browser-connections annotation = %q, want %q", got, want)
}
