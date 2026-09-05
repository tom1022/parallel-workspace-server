package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// UsageSnapshot is one quota window as Claude Code last saw it.
type UsageSnapshot struct {
	// Kind and Group are Claude Code's own labels; Group separates the session
	// window from the weekly ones, which is the distinction a dispatcher needs
	// to decide between switching model and pausing altogether (7.6).
	Kind             string  `json:"kind"`
	Group            string  `json:"group"`
	UsedPercent      float64 `json:"usedPercent"`
	RemainingPercent float64 `json:"remainingPercent"`
	ResetsAt         string  `json:"resetsAt,omitempty"`
	// Model is set only for a window scoped to one model.
	Model  string `json:"model,omitempty"`
	Active bool   `json:"active"`
	// FetchedAt is when Claude Code last refreshed this reading. The supervisor
	// only reads what is already on disk, so a caller cannot tell how current
	// the numbers are without it.
	FetchedAt string `json:"fetchedAt"`
}

// claudeState is the subset of Claude Code's .claude.json the quota reading
// comes from. Claude Code refreshes it as a side effect of its own traffic.
type claudeState struct {
	CachedUsageUtilization *struct {
		FetchedAtMs int64 `json:"fetchedAtMs"`
		Utilization *struct {
			Limits []struct {
				Kind     string   `json:"kind"`
				Group    string   `json:"group"`
				Percent  *float64 `json:"percent"`
				ResetsAt *string  `json:"resets_at"`
				IsActive bool     `json:"is_active"`
				Scope    *struct {
					Model *struct {
						DisplayName string `json:"display_name"`
					} `json:"model"`
				} `json:"scope"`
			} `json:"limits"`
		} `json:"utilization"`
	} `json:"cachedUsageUtilization"`
}

// Usage reports the quota windows Claude Code publishes in its own local
// state. It is a file read and nothing else, which is what makes it safe to
// poll: answering must not itself consume quota (7.10).
func Usage(configDir string) ([]UsageSnapshot, error) {
	b, err := os.ReadFile(filepath.Join(configDir, ".claude.json"))
	if err != nil {
		// A workspace whose session has not run yet simply has no reading.
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}

	var state claudeState
	if err := json.Unmarshal(b, &state); err != nil {
		return nil, fmt.Errorf("supervisor: parse claude state: %w", err)
	}
	cached := state.CachedUsageUtilization
	if cached == nil || cached.Utilization == nil {
		return nil, nil
	}

	fetchedAt := time.UnixMilli(cached.FetchedAtMs).UTC().Format(time.RFC3339)
	out := make([]UsageSnapshot, 0, len(cached.Utilization.Limits))
	for _, l := range cached.Utilization.Limits {
		if l.Percent == nil {
			continue
		}
		snap := UsageSnapshot{
			Kind:             l.Kind,
			Group:            l.Group,
			UsedPercent:      *l.Percent,
			RemainingPercent: 100 - *l.Percent,
			Active:           l.IsActive,
			FetchedAt:        fetchedAt,
		}
		if l.ResetsAt != nil {
			snap.ResetsAt = *l.ResetsAt
		}
		if l.Scope != nil && l.Scope.Model != nil {
			snap.Model = l.Scope.Model.DisplayName
		}
		out = append(out, snap)
	}
	return out, nil
}
