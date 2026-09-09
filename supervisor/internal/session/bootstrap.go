package session

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// nonSubscriptionAuthVars route Claude Code at a third-party provider or a raw
// API key instead of the subscription plan. 5.5 requires none of them to be in
// effect, and stripping them here means an operator mistake in the Pod spec
// cannot silently move billing off the plan.
var nonSubscriptionAuthVars = []string{
	"ANTHROPIC_API_KEY",
	"ANTHROPIC_AUTH_TOKEN",
	"ANTHROPIC_BASE_URL",
	"ANTHROPIC_BEDROCK_BASE_URL",
	"ANTHROPIC_VERTEX_BASE_URL",
	"AWS_BEARER_TOKEN_BEDROCK",
	"CLAUDE_CODE_USE_BEDROCK",
	"CLAUDE_CODE_USE_VERTEX",
}

// PrepareConfigDir makes the per-workspace config area usable without any
// interactive first-run confirmation (2.8). The long-lived credential itself
// arrives as the CLAUDE_CODE_OAUTH_TOKEN env var (5.1) rather than a file
// here: Claude Code reads that directly, so there is nothing to install.
func PrepareConfigDir(configDir, workingDir string) error {
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		return err
	}
	// MkdirAll leaves an existing directory's mode alone.
	if err := os.Chmod(configDir, 0o700); err != nil {
		return err
	}
	if err := seedGlobalState(configDir, workingDir); err != nil {
		return err
	}
	return seedSettings(configDir)
}

// seedGlobalState marks the first-run dialogs answered. It merges into any
// existing file so a container restart does not discard the session history
// and counters Claude Code accumulates there.
func seedGlobalState(configDir, workingDir string) error {
	path := filepath.Join(configDir, ".claude.json")
	state := map[string]any{}
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &state); err != nil {
			state = map[string]any{}
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	state["hasCompletedOnboarding"] = true
	state["hasSeenTasksHint"] = true
	state["autoUpdates"] = false

	projects, _ := state["projects"].(map[string]any)
	if projects == nil {
		projects = map[string]any{}
	}
	project, _ := projects[workingDir].(map[string]any)
	if project == nil {
		project = map[string]any{}
	}
	project["hasTrustDialogAccepted"] = true
	project["hasClaudeMdExternalIncludesApproved"] = true
	project["hasClaudeMdExternalIncludesWarningShown"] = true
	projects[workingDir] = project
	state["projects"] = projects

	return writeJSON(path, state)
}

// seedSettings restores the platform-owned settings and clears personal
// configuration. It overwrites rather than preserving what is already there:
// the file is part of the prompt prefix that has to stay identical across
// workspaces (7.13), and the config area outlives the container.
func seedSettings(configDir string) error {
	return EnforcePromptIsolation(configDir)
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

// ClaudeEnv derives the environment Claude Code is started with: the container
// environment minus every non-subscription auth route (5.5), plus the
// per-workspace config area (5.3) and the pinned-version switch (5.6).
func ClaudeEnv(base []string, configDir string) []string {
	out := make([]string, 0, len(base)+2)
	for _, e := range base {
		name, _, ok := strings.Cut(e, "=")
		if !ok {
			continue
		}
		if name == "CLAUDE_CONFIG_DIR" || name == "DISABLE_AUTOUPDATER" {
			continue
		}
		if slices.Contains(nonSubscriptionAuthVars, name) {
			continue
		}
		out = append(out, e)
	}
	return append(out, "CLAUDE_CONFIG_DIR="+configDir, "DISABLE_AUTOUPDATER=1")
}
