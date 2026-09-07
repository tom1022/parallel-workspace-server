package session

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const procNetTCPSample = `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000:0016 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 8001 1 0000000000000000 100 0 0 10 0
   1: 0100007F:0016 0200000A:C350 01 00000000:00000000 00:00000000 00000000     0        0 8002 1 0000000000000000 20 4 30 10 -1
   2: 0100007F:0016 0200000A:C351 01 00000000:00000000 00:00000000 00000000     0        0 8003 1 0000000000000000 20 4 30 10 -1
   3: 0100007F:0016 0200000A:C352 06 00000000:00000000 00:00000000 00000000     0        0 8004 1 0000000000000000 20 4 30 10 -1
   4: 0100007F:2253 0200000A:C353 01 00000000:00000000 00:00000000 00000000     0        0 8005 1 0000000000000000 20 4 30 10 -1
`

func TestCountEstablishedCountsOnlyLiveSessionsOnThePort(t *testing.T) {
	got, err := countEstablished(strings.NewReader(procNetTCPSample), 22)
	if err != nil {
		t.Fatal(err)
	}
	// The listening socket (0A), the closing one (06) and the supervisor's own
	// port 8787 connection must not read as SSH sessions.
	if got != 2 {
		t.Fatalf("countEstablished = %d, want 2", got)
	}
}

func TestCountEstablishedIgnoresGarbageLines(t *testing.T) {
	got, err := countEstablished(strings.NewReader("header\nnonsense\n   1: 0100007F:zzzz 0200000A:C350 01\n"), 22)
	if err != nil {
		t.Fatal(err)
	}
	if got != 0 {
		t.Fatalf("countEstablished = %d, want 0", got)
	}
}

func TestSSHSessionsEndpointPublishesTheCount(t *testing.T) {
	sup := &Supervisor{Tmux: &Tmux{Socket: "unused", Session: "unused"}}
	rec := httptest.NewRecorder()
	sup.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ssh-sessions", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Count *int `json:"count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Count == nil {
		t.Fatalf("body = %s, want a count", rec.Body.String())
	}
}
