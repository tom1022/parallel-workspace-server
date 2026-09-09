package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return m
}

func TestPrepareConfigDirPreAcceptsInteractivePrompts(t *testing.T) {
	dir := t.TempDir()
	configDir := filepath.Join(dir, "config")
	workDir := filepath.Join(dir, "workspace")

	if err := PrepareConfigDir(configDir, workDir); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	state := readJSON(t, filepath.Join(configDir, ".claude.json"))
	if state["hasCompletedOnboarding"] != true {
		t.Error("onboarding must be pre-accepted so no first-run prompt appears (2.8)")
	}
	projects, ok := state["projects"].(map[string]any)
	if !ok {
		t.Fatalf("projects = %v, want a map keyed by working directory", state["projects"])
	}
	project, ok := projects[workDir].(map[string]any)
	if !ok {
		t.Fatalf("no project entry for %q; got %v", workDir, projects)
	}
	if project["hasTrustDialogAccepted"] != true {
		t.Error("the working directory's trust dialog must be pre-accepted (2.8)")
	}
}

func TestPrepareConfigDirIsPrivate(t *testing.T) {
	configDir := filepath.Join(t.TempDir(), "config")
	if err := PrepareConfigDir(configDir, "/workspace"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	info, err := os.Stat(configDir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("config dir mode = %o, want 700", perm)
	}
}

func TestPrepareConfigDirKeepsExistingState(t *testing.T) {
	dir := t.TempDir()
	configDir := filepath.Join(dir, "config")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	prior := `{"numStartups":7,"projects":{"/workspace":{"lastSessionId":"abc"}}}`
	if err := os.WriteFile(filepath.Join(configDir, ".claude.json"), []byte(prior), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := PrepareConfigDir(configDir, "/workspace"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	state := readJSON(t, filepath.Join(configDir, ".claude.json"))
	if state["numStartups"] != float64(7) {
		t.Errorf("numStartups = %v, want the pre-existing 7 to survive a restart", state["numStartups"])
	}
	project := state["projects"].(map[string]any)["/workspace"].(map[string]any)
	if project["lastSessionId"] != "abc" {
		t.Error("existing project state must survive a restart")
	}
	if project["hasTrustDialogAccepted"] != true {
		t.Error("trust must still be applied on top of existing state")
	}
}

func TestClaudeEnvDropsNonSubscriptionAuthRoutes(t *testing.T) {
	base := []string{
		"PATH=/usr/bin",
		"ANTHROPIC_API_KEY=sk-leak",
		"ANTHROPIC_AUTH_TOKEN=leak",
		"ANTHROPIC_BASE_URL=https://proxy.invalid",
		"CLAUDE_CODE_USE_BEDROCK=1",
		"CLAUDE_CODE_USE_VERTEX=1",
	}
	got := ClaudeEnv(base, "/cfg")

	for _, banned := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX"} {
		for _, e := range got {
			if len(e) > len(banned) && e[:len(banned)+1] == banned+"=" {
				t.Errorf("%s must not reach Claude Code (5.5); env has %q", banned, e)
			}
		}
	}
	if !slices.Contains(got, "PATH=/usr/bin") {
		t.Error("unrelated variables must be preserved")
	}
	if !slices.Contains(got, "CLAUDE_CONFIG_DIR=/cfg") {
		t.Error("the per-workspace config dir must be set (5.3)")
	}
	if !slices.Contains(got, "DISABLE_AUTOUPDATER=1") {
		t.Error("the auto-updater must be disabled so the version stays pinned (5.6)")
	}
}

func TestClaudeEnvOverridesInheritedConfigDir(t *testing.T) {
	got := ClaudeEnv([]string{"CLAUDE_CONFIG_DIR=/somewhere/else"}, "/cfg")
	n := 0
	for _, e := range got {
		if len(e) >= 18 && e[:18] == "CLAUDE_CONFIG_DIR=" {
			n++
			if e != "CLAUDE_CONFIG_DIR=/cfg" {
				t.Errorf("config dir = %q, want /cfg", e)
			}
		}
	}
	if n != 1 {
		t.Errorf("CLAUDE_CONFIG_DIR appears %d times, want exactly 1", n)
	}
}
