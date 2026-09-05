package session

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHTTPExposesOutputInputClientsAndTurn(t *testing.T) {
	s := newTestSupervisor(t)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	// Input from a process that is not attached to the session (2.7).
	resp, err := http.Post(srv.URL+"/input", "application/json", strings.NewReader(`{"text":"http-driven"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("POST /input = %d, want 204", resp.StatusCode)
	}

	// Output read back by that same unattached process (2.6).
	var out struct {
		Chunks []OutputChunk `json:"chunks"`
	}
	getJSON(t, srv.URL+"/output?since=0", &out)
	joined := ""
	for _, c := range out.Chunks {
		joined += c.Data
	}
	if !strings.Contains(joined, "http-driven") {
		t.Errorf("output %q does not contain the injected input", joined)
	}

	var clients []SessionClient
	getJSON(t, srv.URL+"/clients", &clients)
	if clients == nil {
		t.Error("GET /clients must return a list, not null")
	}

	var turn TurnState
	getJSON(t, srv.URL+"/turn", &turn)
	if turn.Kind == "" {
		t.Error("GET /turn must return a turn kind")
	}
}

func TestHTTPInputConflictIsStructured(t *testing.T) {
	s := newTestSupervisor(t)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	stop := attachClient(t, s.Tmux, false)
	defer stop()
	waitForClients(t, s.Tmux, 1)

	resp, err := http.Post(srv.URL+"/input", "application/json", strings.NewReader(`{"text":"nope"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("POST /input = %d, want 409", resp.StatusCode)
	}
	var sendErr SendError
	if err := json.NewDecoder(resp.Body).Decode(&sendErr); err != nil {
		t.Fatal(err)
	}
	if sendErr.Kind != SendWritableClientPresent {
		t.Errorf("kind = %q, want %q", sendErr.Kind, SendWritableClientPresent)
	}
}

func getJSON(t *testing.T, url string, into any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200", url, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
		t.Fatalf("decode %s: %v", url, err)
	}
}

func TestHTTPBrowserVerificationReportsUnavailableAsAnOrdinaryAnswer(t *testing.T) {
	s := newTestSupervisor(t)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	var got BrowserVerification
	getJSON(t, srv.URL+"/browser-verification", &got)
	if got.Available {
		t.Fatal("no client is attached, so interactive browser verification must be unavailable")
	}
	if got.Reason == "" {
		t.Error("an unavailable result must say why")
	}
}
