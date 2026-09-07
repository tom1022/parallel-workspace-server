package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

// ihAgentInput drives the session the way Hermes Agent does: straight at the
// Session Supervisor, naming no client, bypassing the gateway entirely.
func ihAgentInput(t *testing.T, rig *testRig, text string) int {
	t.Helper()
	body := fmt.Sprintf(`{"text":%q}`, text)
	resp, err := http.Post(rig.backendURL+"/input", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("agent input: %v", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

func ihSupervisorClients(t *testing.T, rig *testRig) []struct {
	ID       string `json:"id"`
	Writable bool   `json:"writable"`
} {
	t.Helper()
	resp, err := http.Get(rig.backendURL + "/clients")
	if err != nil {
		t.Fatalf("list clients: %v", err)
	}
	defer resp.Body.Close()
	var clients []struct {
		ID       string `json:"id"`
		Writable bool   `json:"writable"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&clients); err != nil {
		t.Fatalf("decode clients: %v", err)
	}
	return clients
}

// ihWaitForLastInput waits until want is the most recent thing the session
// received, so an assertion never races the bridge's own goroutine.
func ihWaitForLastInput(t *testing.T, rig *testRig, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var sent []string
	for time.Now().Before(deadline) {
		sent = rig.supervisor.sent()
		if len(sent) > 0 && sent[len(sent)-1] == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("last input = %q, want %q", sent, want)
}

// ihWaitForAgentResumed waits for the disconnect to reach the supervisor. The
// gateway reports it after the socket is already gone, so the agent's first
// accepted send is what marks the takeover as actually over.
func ihWaitForAgentResumed(t *testing.T, rig *testRig, text string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	status := 0
	for time.Now().Before(deadline) {
		if status = ihAgentInput(t, rig, text); status == http.StatusNoContent {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("agent input still refused with %d after the browser disconnected", status)
}

func ihTakeOver(t *testing.T, rig *testRig, sessionID string) {
	t.Helper()
	rec := rig.do(t, http.MethodPost, "/handover", fmt.Sprintf(`{"sessionId":%q}`, sessionID), withHost("feature-login.fickledev.com"))
	if rec.Code != http.StatusOK {
		t.Fatalf("handover status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

// 3.5 / 3.6: a browser reaching the session through the shared gateway takes it
// over read-write, and for as long as it holds it the agent's own path into the
// session is closed. The whole point is that the two never type at once, so the
// check runs against the supervisor rather than against the gateway's own
// bookkeeping, which the agent cannot see.
func TestBrowserTakeoverStopsAgentInputAndReleaseResumesIt(t *testing.T) {
	rig := newTestRig(t, testWorkspace("feature-login", "feature/login", "feature-login", "Ready"))
	server := httptest.NewServer(rig.handler)
	defer server.Close()

	if status := ihAgentInput(t, rig, "agent-before"); status != http.StatusNoContent {
		t.Fatalf("agent input before takeover = %d, want 204", status)
	}

	conn := dialSession(t, server, "feature-login.fickledev.com", "s-1")
	if _, err := conn.Write([]byte("watching\n")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if sent := rig.supervisor.sent(); sent[len(sent)-1] != "agent-before" {
		t.Fatalf("a read-only connection drove the session: %q", sent)
	}

	ihTakeOver(t, rig, "s-1")

	// 3.2: the takeover has to be visible in the session's client list, which
	// is the only thing the agent consults before sending.
	clients := ihSupervisorClients(t, rig)
	if len(clients) != 1 || clients[0].ID != "s-1" || !clients[0].Writable {
		t.Fatalf("session clients = %+v, want the browser listed as writable", clients)
	}

	if status := ihAgentInput(t, rig, "agent-during"); status != http.StatusConflict {
		t.Fatalf("agent input during takeover = %d, want 409", status)
	}

	if _, err := conn.Write([]byte("developer\n")); err != nil {
		t.Fatal(err)
	}
	ihWaitForLastInput(t, rig, "developer\n")

	conn.Close()
	ihWaitForAgentResumed(t, rig, "agent-after")

	if clients := ihSupervisorClients(t, rig); len(clients) != 0 {
		t.Fatalf("session clients after disconnect = %+v, want none", clients)
	}
	if got := rig.supervisor.sent(); !slices.Equal(got, []string{"agent-before", "developer\n", "agent-after"}) {
		t.Fatalf("session received %q, want the agent's sends never to overlap the takeover", got)
	}
}

// Interrupting and handing back repeatedly must leave the same clean
// alternation every time: nothing the agent sent may land inside a takeover
// window, and nothing may be lost between them.
func TestRepeatedInterruptAndResumeNeverCollides(t *testing.T) {
	rig := newTestRig(t, testWorkspace("feature-login", "feature/login", "feature-login", "Ready"))
	server := httptest.NewServer(rig.handler)
	defer server.Close()

	var want []string
	for round := 1; round <= 3; round++ {
		agentBefore := fmt.Sprintf("agent-%d\n", round)
		if status := ihAgentInput(t, rig, agentBefore); status != http.StatusNoContent {
			t.Fatalf("round %d: agent input = %d, want 204", round, status)
		}
		want = append(want, agentBefore)

		sessionID := fmt.Sprintf("s-%d", round)
		conn := dialSession(t, server, "feature-login.fickledev.com", sessionID)
		ihTakeOver(t, rig, sessionID)

		if status := ihAgentInput(t, rig, "agent-must-not-land"); status != http.StatusConflict {
			t.Fatalf("round %d: agent input during takeover = %d, want 409", round, status)
		}

		developer := fmt.Sprintf("developer-%d\n", round)
		if _, err := conn.Write([]byte(developer)); err != nil {
			t.Fatal(err)
		}
		ihWaitForLastInput(t, rig, developer)
		want = append(want, developer)

		conn.Close()
		resumed := fmt.Sprintf("agent-resumed-%d\n", round)
		ihWaitForAgentResumed(t, rig, resumed)
		want = append(want, resumed)
	}

	if got := rig.supervisor.sent(); !slices.Equal(got, want) {
		t.Fatalf("session received %q, want %q", got, want)
	}
}

// A second browser cannot take the session over while the first holds it, and
// the refusal leaves the holder untouched.
func TestTakeoverRefusalLeavesTheHolderDriving(t *testing.T) {
	rig := newTestRig(t, testWorkspace("feature-login", "feature/login", "feature-login", "Ready"))
	server := httptest.NewServer(rig.handler)
	defer server.Close()

	first := dialSession(t, server, "feature-login.fickledev.com", "s-1")
	second := dialSession(t, server, "feature-login.fickledev.com", "s-2")
	ihTakeOver(t, rig, "s-1")

	rec := rig.do(t, http.MethodPost, "/handover", `{"sessionId":"s-2"}`, withHost("feature-login.fickledev.com"))
	if rec.Code != http.StatusConflict {
		t.Fatalf("second handover status = %d, want 409", rec.Code)
	}
	if _, err := second.Write([]byte("intruder\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := first.Write([]byte("holder\n")); err != nil {
		t.Fatal(err)
	}
	ihWaitForLastInput(t, rig, "holder\n")
	if got := rig.supervisor.sent(); !slices.Equal(got, []string{"holder\n"}) {
		t.Fatalf("session received %q, want only the holder's input", got)
	}
}
