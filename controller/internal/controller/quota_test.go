package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
)

// 7.6: the scope decides between switching model and stopping altogether, so
// it has to come out of the reading's own fields.
func TestExhaustedScope(t *testing.T) {
	full := UsageSnapshot{Kind: "five_hour", Group: "session", RemainingPercent: 60, Active: true}
	session := UsageSnapshot{Kind: "five_hour", Group: "session", RemainingPercent: 0, Active: true}
	weekly := UsageSnapshot{Kind: "seven_day", Group: "weekly", RemainingPercent: 0, Active: true}
	model := UsageSnapshot{Kind: "seven_day_opus", Group: "weekly", Model: "Opus", RemainingPercent: 0, Active: true}
	inactive := UsageSnapshot{Kind: "five_hour", Group: "session", RemainingPercent: 0, Active: false}

	for _, tc := range []struct {
		name  string
		snaps []UsageSnapshot
		want  QuotaScope
		hit   bool
	}{
		{"nothing exhausted", []UsageSnapshot{full}, "", false},
		{"session window", []UsageSnapshot{full, session}, QuotaScopeSession, true},
		{"weekly window", []UsageSnapshot{full, weekly}, QuotaScopeWeekly, true},
		{"model window", []UsageSnapshot{full, model}, QuotaScopeModel, true},
		// Switching model cannot rescue a workspace whose account-wide window
		// is gone, so the wider scope has to win.
		{"model and session together", []UsageSnapshot{model, session}, QuotaScopeSession, true},
		{"model and weekly together", []UsageSnapshot{model, weekly}, QuotaScopeWeekly, true},
		{"inactive window is not a limit in effect", []UsageSnapshot{full, inactive}, "", false},
		{"no reading at all", nil, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, hit := exhaustedScope(tc.snaps, 0)
			if hit != tc.hit || got != tc.want {
				t.Errorf("exhaustedScope = %q/%v, want %q/%v", got, hit, tc.want, tc.hit)
			}
		})
	}
}

// 7.11: dispatch is judged on the remaining balance, so the threshold has to
// bite before the window is completely gone.
func TestExhaustedScopeHonoursTheRemainingThreshold(t *testing.T) {
	low := []UsageSnapshot{{Kind: "five_hour", Group: "session", RemainingPercent: 3, Active: true}}
	if _, hit := exhaustedScope(low, 0); hit {
		t.Error("3% remaining with a 0 threshold must not count as exhausted")
	}
	if _, hit := exhaustedScope(low, 5); !hit {
		t.Error("3% remaining with a 5 threshold must count as exhausted")
	}
}

func TestIsAuthError(t *testing.T) {
	for kind, want := range map[string]bool{
		"authentication_error": true,
		"permission_error":     true,
		"rate_limit_error":     false,
		"overloaded_error":     false,
		"":                     false,
	} {
		if got := isAuthError(kind); got != want {
			t.Errorf("isAuthError(%q) = %v, want %v", kind, got, want)
		}
	}
}

// The acceptance criterion for 7.2: the judgement must rest on the structured
// reading alone. Here the wire carries prose that says the quota is gone while
// every structured field says it is not; a reader that went by the text would
// hold, and holding is the wrong answer.
func TestSupervisorUsageObserverIgnoresTheRenderedText(t *testing.T) {
	srv := usageServer(t,
		[]UsageSnapshot{{Kind: "five_hour", Group: "session", RemainingPercent: 80, Active: true}},
		map[string]any{
			"kind":   "Completed",
			"detail": "Claude usage limit reached. Your limit will reset at 3pm. API Error: 401 authentication_error",
		})

	reading, err := observeAt(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	if reading.ErrorKind != "" {
		t.Errorf("ErrorKind = %q, want empty: only errorKind may be read, never detail", reading.ErrorKind)
	}
	if scope, hit := exhaustedScope(reading.Snapshots, 0); hit {
		t.Errorf("held on %q despite 80%% remaining: the decision followed the prose", scope)
	}
}

func TestSupervisorUsageObserverReadsTheStructuredErrorKind(t *testing.T) {
	srv := usageServer(t,
		[]UsageSnapshot{{Kind: "five_hour", Group: "session", RemainingPercent: 80, Active: true}},
		map[string]any{"kind": "Failed", "errorKind": "authentication_error", "detail": "invalid x-api-key"})

	reading, err := observeAt(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	if !isAuthError(reading.ErrorKind) {
		t.Errorf("ErrorKind = %q, want an auth error", reading.ErrorKind)
	}
	if len(reading.Snapshots) != 1 || reading.Snapshots[0].RemainingPercent != 80 {
		t.Errorf("snapshots = %+v, want the served reading", reading.Snapshots)
	}
}

func usageServer(t *testing.T, snaps []UsageSnapshot, turn map[string]any) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /usage", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(snaps)
	})
	mux.HandleFunc("GET /turn", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(turn)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// observeAt drives the observer against a stand-in supervisor, bypassing the
// Pod lookup that resolves a workspace to this address in production.
func observeAt(ctx context.Context, base string) (UsageReading, error) {
	u, err := url.Parse(base)
	if err != nil {
		return UsageReading{}, err
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		return UsageReading{}, err
	}
	o := &SupervisorUsageObserver{Port: port}
	return o.observe(ctx, u.Hostname())
}
