package session

import (
	"bufio"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Turn kinds. The states mirror the SessionControl contract in design.md.
const (
	TurnIdle         = "Idle"
	TurnRunning      = "Running"
	TurnAwaitingTool = "AwaitingTool"
	TurnCompleted    = "Completed"
	TurnFailed       = "Failed"
)

// TurnState is a snapshot of where the session is in the current turn.
type TurnState struct {
	Kind      string `json:"kind"`
	StartedAt string `json:"startedAt,omitempty"`
	Reason    string `json:"reason,omitempty"`
	Model     string `json:"model,omitempty"`
	EndedAt   string `json:"endedAt,omitempty"`
	Detail    string `json:"detail,omitempty"`
	// ErrorKind is the Anthropic API's own error type for a failed turn
	// ("authentication_error", "rate_limit_error", ...). It exists so a caller
	// can act on the failure without reading Detail, whose wording is the
	// rendered message and not a contract (7.5).
	ErrorKind string `json:"errorKind,omitempty"`
}

// transcriptRecord is the subset of Claude Code's JSONL record shape the turn
// state is derived from. Everything else in the record is ignored on purpose:
// the file format carries far more than this and pinning to a wide struct
// would break on unrelated additions.
type transcriptRecord struct {
	Type    string `json:"type"`
	IsMeta  bool   `json:"isMeta"`
	IsError bool   `json:"isApiErrorMessage"`
	Time    string `json:"timestamp"`
	Message *struct {
		Model      string          `json:"model"`
		StopReason string          `json:"stop_reason"`
		Content    json.RawMessage `json:"content"`
	} `json:"message"`
}

// apiError is the Anthropic API's error envelope as Claude Code embeds it in
// the record it writes for a failed request.
type apiError struct {
	Type  string `json:"type"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

type contentBlock struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

// TurnStateFrom derives the turn state from Claude Code's structured execution
// record under configDir, never from the rendered terminal (2.9). The record
// distinguishes a finished turn (stop_reason "end_turn") from a turn parked on
// tool execution (stop_reason "tool_use"), which is the distinction 2.10 asks
// for and which the screen cannot supply reliably.
func TurnStateFrom(configDir string) (TurnState, error) {
	path, err := latestTranscript(configDir)
	if err != nil {
		return TurnState{}, err
	}
	if path == "" {
		return TurnState{Kind: TurnIdle}, nil
	}

	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return TurnState{Kind: TurnIdle}, nil
		}
		return TurnState{}, err
	}
	defer f.Close()

	state := TurnState{Kind: TurnIdle}
	sc := bufio.NewScanner(f)
	// Tool results and pasted context routinely exceed bufio's 64 KiB default,
	// and a skipped long line would strand the state on a stale record.
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		var rec transcriptRecord
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			continue
		}
		if next, ok := advance(rec); ok {
			state = next
		}
	}
	if err := sc.Err(); err != nil {
		return TurnState{}, err
	}
	return state, nil
}

// advance maps one record to the turn state it implies, reporting false for
// records that say nothing about the turn.
func advance(rec transcriptRecord) (TurnState, bool) {
	if rec.Message == nil || rec.IsMeta {
		return TurnState{}, false
	}
	// Checked before the type switch: Claude Code files the error under the
	// "user" type, so the flag is the only thing separating a failed request
	// from the prompt that provoked it.
	if rec.IsError {
		kind, detail := apiErrorFrom(rec.Message.Content)
		return TurnState{Kind: TurnFailed, ErrorKind: kind, Detail: detail, EndedAt: rec.Time}, true
	}

	switch rec.Type {
	case "user":
		// Both a fresh prompt and a returning tool result hand control back to
		// the model, so both mean the turn is running.
		return TurnState{Kind: TurnRunning, StartedAt: rec.Time}, true
	case "assistant":
		switch rec.Message.StopReason {
		case "end_turn", "stop_sequence", "max_tokens":
			return TurnState{Kind: TurnCompleted, Model: rec.Message.Model, EndedAt: rec.Time}, true
		case "tool_use":
			return TurnState{Kind: TurnAwaitingTool, Reason: toolName(rec.Message.Content), StartedAt: rec.Time}, true
		}
	}
	return TurnState{}, false
}

// apiErrorFrom pulls the API's error object out of the record's content. The
// content is the rendered message with the error envelope appended, so the
// object is located by decoding from the first brace rather than by matching
// the prose around it: the wording changes between versions, the envelope's
// shape does not.
func apiErrorFrom(content json.RawMessage) (kind, detail string) {
	var text string
	if err := json.Unmarshal(content, &text); err != nil {
		return "", ""
	}
	i := strings.IndexByte(text, '{')
	if i < 0 {
		return "", ""
	}
	var e apiError
	if err := json.NewDecoder(strings.NewReader(text[i:])).Decode(&e); err != nil || e.Error == nil {
		return "", ""
	}
	return e.Error.Type, e.Error.Message
}

func toolName(content json.RawMessage) string {
	var blocks []contentBlock
	if err := json.Unmarshal(content, &blocks); err != nil {
		return ""
	}
	for _, b := range blocks {
		if b.Type == "tool_use" {
			return b.Name
		}
	}
	return ""
}

// latestTranscript returns the most recently modified transcript below
// configDir. One workspace runs one session (2.2), so the newest file is that
// session's record; older files are leftovers from earlier container starts.
func latestTranscript(configDir string) (string, error) {
	root := filepath.Join(configDir, "projects")
	var newest string
	var newestMod int64
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return fs.SkipAll
			}
			return err
		}
		if d.IsDir() || filepath.Ext(path) != ".jsonl" {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if mod := info.ModTime().UnixNano(); mod >= newestMod {
			newest, newestMod = path, mod
		}
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	return newest, nil
}
