package session

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeTranscript(t *testing.T, lines ...string) string {
	t.Helper()
	dir := t.TempDir()
	projects := filepath.Join(dir, "projects", "-workspace")
	if err := os.MkdirAll(projects, 0o700); err != nil {
		t.Fatal(err)
	}
	body := ""
	for _, l := range lines {
		body += l + "\n"
	}
	if err := os.WriteFile(filepath.Join(projects, "sess.jsonl"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

const (
	lineUserPrompt = `{"type":"user","timestamp":"2026-09-05T10:00:00.000Z","message":{"role":"user","content":"do the thing"}}`
	lineToolUse    = `{"type":"assistant","timestamp":"2026-09-05T10:00:05.000Z","message":{"role":"assistant","model":"claude-opus-5","stop_reason":"tool_use","content":[{"type":"tool_use","name":"Bash"}]}}`
	lineToolResult = `{"type":"user","timestamp":"2026-09-05T10:00:07.000Z","toolUseResult":{},"message":{"role":"user","content":[{"type":"tool_result"}]}}`
	lineEndTurn    = `{"type":"assistant","timestamp":"2026-09-05T10:00:09.000Z","message":{"role":"assistant","model":"claude-opus-5","stop_reason":"end_turn","content":[{"type":"text","text":"done"}]}}`
)

func TestTurnStateIdleWithoutTranscript(t *testing.T) {
	got, err := TurnStateFrom(t.TempDir())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Kind != TurnIdle {
		t.Errorf("kind = %q, want %q", got.Kind, TurnIdle)
	}
}

func TestTurnStateRunningAfterPrompt(t *testing.T) {
	got, err := TurnStateFrom(writeTranscript(t, lineUserPrompt))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Kind != TurnRunning {
		t.Fatalf("kind = %q, want %q", got.Kind, TurnRunning)
	}
	if got.StartedAt == "" {
		t.Error("StartedAt must carry the prompt timestamp")
	}
}

func TestTurnStateAwaitingToolIsDistinctFromCompleted(t *testing.T) {
	awaiting, err := TurnStateFrom(writeTranscript(t, lineUserPrompt, lineToolUse))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if awaiting.Kind != TurnAwaitingTool {
		t.Fatalf("kind = %q, want %q", awaiting.Kind, TurnAwaitingTool)
	}
	if awaiting.Reason != "Bash" {
		t.Errorf("Reason = %q, want the tool name %q", awaiting.Reason, "Bash")
	}

	completed, err := TurnStateFrom(writeTranscript(t, lineUserPrompt, lineToolUse, lineToolResult, lineEndTurn))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if completed.Kind != TurnCompleted {
		t.Fatalf("kind = %q, want %q", completed.Kind, TurnCompleted)
	}
	if completed.Model != "claude-opus-5" {
		t.Errorf("Model = %q, want %q", completed.Model, "claude-opus-5")
	}
	if completed.EndedAt != "2026-09-05T10:00:09.000Z" {
		t.Errorf("EndedAt = %q, want the end_turn timestamp", completed.EndedAt)
	}
}

func TestTurnStateRunningAfterToolResult(t *testing.T) {
	got, err := TurnStateFrom(writeTranscript(t, lineUserPrompt, lineToolUse, lineToolResult))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Kind != TurnRunning {
		t.Errorf("kind = %q, want %q after a tool result returns", got.Kind, TurnRunning)
	}
}

func TestTurnStateIgnoresNonConversationRecords(t *testing.T) {
	noise := `{"type":"file-history-snapshot","timestamp":"2026-09-05T10:00:10.000Z"}`
	meta := `{"type":"user","isMeta":true,"timestamp":"2026-09-05T10:00:11.000Z","message":{"role":"user","content":"<system-reminder/>"}}`
	got, err := TurnStateFrom(writeTranscript(t, lineUserPrompt, lineEndTurn, noise, meta))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Kind != TurnCompleted {
		t.Errorf("kind = %q, want %q; noise and meta records must not advance the turn", got.Kind, TurnCompleted)
	}
}

func TestTurnStateUsesMostRecentlyModifiedTranscript(t *testing.T) {
	dir := t.TempDir()
	projects := filepath.Join(dir, "projects", "-workspace")
	if err := os.MkdirAll(projects, 0o700); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(projects, "old.jsonl")
	fresh := filepath.Join(projects, "new.jsonl")
	if err := os.WriteFile(stale, []byte(lineEndTurn+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fresh, []byte(lineUserPrompt+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}

	got, err := TurnStateFrom(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Kind != TurnRunning {
		t.Errorf("kind = %q, want %q from the newest transcript", got.Kind, TurnRunning)
	}
}

func TestTurnStateSkipsMalformedLines(t *testing.T) {
	got, err := TurnStateFrom(writeTranscript(t, lineUserPrompt, "{not json", lineEndTurn))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Kind != TurnCompleted {
		t.Errorf("kind = %q, want %q", got.Kind, TurnCompleted)
	}
}

// Claude Code records an API failure as its own record type rather than as
// assistant output, and carries the API's error object inside it. These two
// lines differ only in that object: the prose is identical, so a reader that
// went by the message text could not tell them apart.
const (
	lineAuthError = `{"type":"user","isApiErrorMessage":true,"timestamp":"2026-09-05T10:00:11.000Z","message":{"role":"user","content":"API Error: 401 {\"type\":\"error\",\"error\":{\"type\":\"authentication_error\",\"message\":\"invalid x-api-key\"}}"}}`
	lineRateError = `{"type":"user","isApiErrorMessage":true,"timestamp":"2026-09-05T10:00:11.000Z","message":{"role":"user","content":"API Error: 401 {\"type\":\"error\",\"error\":{\"type\":\"rate_limit_error\",\"message\":\"invalid x-api-key\"}}"}}`
)

func TestTurnStateFailedCarriesTheApiErrorKind(t *testing.T) {
	for _, tc := range []struct {
		name string
		line string
		want string
	}{
		{"authentication", lineAuthError, "authentication_error"},
		{"rate limit", lineRateError, "rate_limit_error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := TurnStateFrom(writeTranscript(t, lineUserPrompt, tc.line))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Kind != TurnFailed {
				t.Errorf("kind = %q, want %q", got.Kind, TurnFailed)
			}
			if got.ErrorKind != tc.want {
				t.Errorf("ErrorKind = %q, want %q", got.ErrorKind, tc.want)
			}
		})
	}
}

// A later successful turn must clear the failure: a stale error left on the
// PVC by an earlier container would otherwise halt dispatch forever.
func TestTurnStateFailureIsSupersededByALaterTurn(t *testing.T) {
	got, err := TurnStateFrom(writeTranscript(t, lineUserPrompt, lineAuthError, lineUserPrompt, lineEndTurn))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Kind != TurnCompleted {
		t.Errorf("kind = %q, want %q", got.Kind, TurnCompleted)
	}
	if got.ErrorKind != "" {
		t.Errorf("ErrorKind = %q, want it cleared", got.ErrorKind)
	}
}

// An error record with no decodable API error object still marks the turn
// failed; only the kind is unknown.
func TestTurnStateFailedWithoutAnErrorObject(t *testing.T) {
	line := `{"type":"user","isApiErrorMessage":true,"timestamp":"2026-09-05T10:00:11.000Z","message":{"role":"user","content":"API Error: Connection error."}}`
	got, err := TurnStateFrom(writeTranscript(t, lineUserPrompt, line))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Kind != TurnFailed {
		t.Errorf("kind = %q, want %q", got.Kind, TurnFailed)
	}
	if got.ErrorKind != "" {
		t.Errorf("ErrorKind = %q, want empty", got.ErrorKind)
	}
}
