package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCanonicalSettingsAreIdenticalAcrossWorkspaces(t *testing.T) {
	a := filepath.Join(t.TempDir(), "config")
	b := filepath.Join(t.TempDir(), "config")
	if err := PrepareConfigDir(a, "/workspace/repo"); err != nil {
		t.Fatal(err)
	}
	if err := PrepareConfigDir(b, "/workspace/repo"); err != nil {
		t.Fatal(err)
	}

	first, err := os.ReadFile(filepath.Join(a, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(filepath.Join(b, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Errorf("settings differ between workspaces, so the prompt prefix cannot be reused (7.13):\n%s\n---\n%s", first, second)
	}
}

// Drift in the platform-owned settings would silently break prompt-cache reuse
// across workspaces, so the file is restored rather than merely seeded.
func TestEnforcePromptIsolationRestoresDriftedSettings(t *testing.T) {
	configDir := filepath.Join(t.TempDir(), "config")
	if err := PrepareConfigDir(configDir, "/workspace/repo"); err != nil {
		t.Fatal(err)
	}
	canonical, err := os.ReadFile(filepath.Join(configDir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(configDir, "settings.json"), []byte(`{"outputStyle":"local-drift"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := EnforcePromptIsolation(configDir); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(filepath.Join(configDir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(canonical) {
		t.Errorf("settings not restored:\n%s", got)
	}
}

func TestEnforcePromptIsolationDropsPersonalInstructions(t *testing.T) {
	configDir := filepath.Join(t.TempDir(), "config")
	if err := os.MkdirAll(filepath.Join(configDir, "plugins"), 0o700); err != nil {
		t.Fatal(err)
	}
	personal := filepath.Join(configDir, "CLAUDE.md")
	if err := os.WriteFile(personal, []byte("# my personal rules"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := EnforcePromptIsolation(configDir); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(personal); !os.IsNotExist(err) {
		t.Error("a user-level CLAUDE.md must not reach Claude Code (7.15)")
	}
	if _, err := os.Stat(filepath.Join(configDir, "plugins")); !os.IsNotExist(err) {
		t.Error("user-level plugins must not reach Claude Code (7.15)")
	}
}

func TestCanonicalSettingsCarryNoWorkspaceIdentity(t *testing.T) {
	configDir := filepath.Join(t.TempDir(), "config")
	if err := PrepareConfigDir(configDir, "/workspace/repo"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(configDir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"WORKSPACE_NAME", "branch", "fickledev.com"} {
		if strings.Contains(string(b), forbidden) {
			t.Errorf("settings mention %q; workspace-specific data must stay out of the prompt prefix (7.14)", forbidden)
		}
	}
}

func TestReleaseContextClearsAccumulatedContext(t *testing.T) {
	sup := newTestSupervisor(t)

	if err := sup.ReleaseContext(); err != nil {
		t.Fatalf("release: %v", err)
	}

	typed := filepath.Join(sup.ConfigDir, "typed.txt")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(typed); err == nil && strings.Contains(string(b), "/clear") {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	b, _ := os.ReadFile(typed)
	t.Errorf("expected the context-clearing command to reach the session, got %q", b)
}

func TestReleaseContextRefusesWhenSessionIsNotRunning(t *testing.T) {
	sup := &Supervisor{Tmux: &Tmux{Socket: "devplatform-test-absent", Session: "nope"}}
	if err := sup.ReleaseContext(); err == nil {
		t.Error("expected an error when there is no session to clear")
	}
}
