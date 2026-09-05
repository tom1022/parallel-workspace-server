package session

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// personalConfigEntries are developer-authored configuration that would enter
// Claude Code's system prompt and diverge it from the other workspaces. The
// config area lives on a PVC that outlives the container, and a developer with
// a writable session can write into it, so these are cleared on every start
// rather than only at first provisioning (7.15).
//
// ponytail: skills/ is deliberately not in this list — a later task
// provisions a platform-owned skill bundle there, and clearing it here would
// fight that. Revisit if developer-authored skills turn up in practice.
var personalConfigEntries = []string{
	"CLAUDE.md",
	"settings.local.json",
	"plugins",
}

// canonicalSettings is the settings.json every workspace gets, byte for byte.
// Prompt-cache reuse needs an identical prefix across workspaces (7.13), so
// nothing derived from the workspace, its branch or its host may appear here.
func canonicalSettings() map[string]any {
	return map[string]any{
		"env": map[string]any{"DISABLE_AUTOUPDATER": "1"},
	}
}

func writeCanonicalSettings(configDir string) error {
	return writeJSON(filepath.Join(configDir, "settings.json"), canonicalSettings())
}

// EnforcePromptIsolation restores the platform-owned parts of the config area
// so Claude Code is presented with the same system prompt in every workspace
// (7.13) and with none of the developer's own configuration (7.15). It is
// idempotent and runs on every start, because drift accumulates on the PVC.
func EnforcePromptIsolation(configDir string) error {
	if err := writeCanonicalSettings(configDir); err != nil {
		return err
	}
	for _, name := range personalConfigEntries {
		if err := os.RemoveAll(filepath.Join(configDir, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

// clearCommand is Claude Code's own command for dropping the accumulated
// conversation, issued through the session so the running process acts on it.
const clearCommand = "/clear"

// ReleaseContext drops the session's accumulated context once a request is
// done (7.16). It goes through SendInput, so it inherits the same refusal
// while a developer holds the session writable — clearing someone else's
// context mid-conversation would be worse than carrying it.
func (s *Supervisor) ReleaseContext() error {
	return s.SendInput(clearCommand)
}
