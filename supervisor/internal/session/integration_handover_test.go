package session

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ihServer runs the supervisor's own HTTP surface over a real tmux session, so
// the takeover, the refusal and the resume all travel the contract the Terminal
// Gateway and Hermes Agent actually speak.
func ihServer(t *testing.T) (*Supervisor, *httptest.Server) {
	t.Helper()
	s := newTestSupervisor(t)
	server := httptest.NewServer(s.Handler())
	t.Cleanup(server.Close)
	return s, server
}

func ihPost(t *testing.T, server *httptest.Server, method, path, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, server.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func ihClients(t *testing.T, server *httptest.Server) []SessionClient {
	t.Helper()
	resp := ihPost(t, server, http.MethodGet, "/clients", "")
	var clients []SessionClient
	if err := json.NewDecoder(resp.Body).Decode(&clients); err != nil {
		t.Fatalf("decode clients: %v", err)
	}
	return clients
}

func ihSendError(t *testing.T, resp *http.Response) SendError {
	t.Helper()
	var got SendError
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode send error: %v", err)
	}
	return got
}

// 3.2, 3.5, 3.6: a browser that took the session over through the gateway is
// not attached to the multiplexer, so the supervisor has to be told about it —
// otherwise Hermes Agent's check finds no writable client and types over the
// developer.
func TestGatewayHeldTakeoverBlocksAgentInputUntilReleased(t *testing.T) {
	_, server := ihServer(t)

	if resp := ihPost(t, server, http.MethodPost, "/input", `{"text":"agent-before"}`); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("agent input before takeover = %d, want 204", resp.StatusCode)
	}

	if resp := ihPost(t, server, http.MethodPost, "/clients/browser-1", ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("declare takeover = %d, want 204", resp.StatusCode)
	}

	clients := ihClients(t, server)
	if len(clients) != 1 || clients[0].ID != "browser-1" || !clients[0].Writable {
		t.Fatalf("clients = %+v, want the browser listed as writable", clients)
	}

	resp := ihPost(t, server, http.MethodPost, "/input", `{"text":"agent-during"}`)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("agent input during takeover = %d, want 409", resp.StatusCode)
	}
	if got := ihSendError(t, resp); got.Kind != SendWritableClientPresent || got.ClientID != "browser-1" {
		t.Fatalf("send error = %+v, want WritableClientPresent for browser-1", got)
	}

	// The developer's own keystrokes reach the session through the same
	// endpoint, so the refusal must turn on who is sending rather than on any
	// hold being present at all.
	if resp := ihPost(t, server, http.MethodPost, "/input", `{"text":"developer","clientId":"browser-1"}`); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("developer input during takeover = %d, want 204", resp.StatusCode)
	}

	if resp := ihPost(t, server, http.MethodDelete, "/clients/browser-1", ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("release takeover = %d, want 204", resp.StatusCode)
	}
	if clients := ihClients(t, server); len(clients) != 0 {
		t.Fatalf("clients after release = %+v, want none", clients)
	}
	if resp := ihPost(t, server, http.MethodPost, "/input", `{"text":"agent-after"}`); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("agent input after release = %d, want 204", resp.StatusCode)
	}
}

// A client attached to the multiplexer still blocks the browser that holds the
// gateway-side takeover: the two sources of writable clients are checked
// together, not one instead of the other.
func TestAttachedWritableClientBlocksAGatewayHeldBrowser(t *testing.T) {
	s, server := ihServer(t)
	stop := attachClient(t, s.Tmux, false)
	defer stop()
	waitForClients(t, s.Tmux, 1)

	ihPost(t, server, http.MethodPost, "/clients/browser-1", "")
	resp := ihPost(t, server, http.MethodPost, "/input", `{"text":"developer","clientId":"browser-1"}`)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	if got := ihSendError(t, resp); got.Kind != SendWritableClientPresent || got.ClientID == "browser-1" {
		t.Fatalf("send error = %+v, want the attached client to be the blocker", got)
	}
}
