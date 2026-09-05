package session

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestPathsKeepTheCheckoutSeparateFromPlatformState(t *testing.T) {
	workingDir, configDir, cacheDir := Paths("/workspace")

	if workingDir == "/workspace" {
		t.Error("the checkout must not be the volume root, or platform state lands inside the git tree")
	}
	for name, dir := range map[string]string{"config": configDir, "cache": cacheDir} {
		if strings.HasPrefix(dir, workingDir+"/") || dir == workingDir {
			t.Errorf("%s dir %q is inside the checkout %q", name, dir, workingDir)
		}
		if !strings.HasPrefix(dir, "/workspace/") {
			t.Errorf("%s dir %q must stay on the same persistent volume (15.12)", name, dir)
		}
	}
}

func TestPathsAreIdenticalForEveryWorkspace(t *testing.T) {
	a1, a2, a3 := Paths("/workspace")
	b1, b2, b3 := Paths("/workspace")
	if a1 != b1 || a2 != b2 || a3 != b3 {
		t.Error("layout must not vary between workspaces; the checkout path reaches the system prompt (7.14)")
	}
	for _, p := range []string{a1, a2, a3} {
		if strings.Contains(p, "branch") || strings.Contains(p, "workspace-") {
			t.Errorf("path %q looks workspace-specific", p)
		}
	}
}

// The defect this guards: with the checkout at the volume root, the config
// area, session log and package cache all appear as untracked files, so any
// `git add -A` in the session would commit the credential.
func TestPlatformStateDoesNotDirtyTheCheckout(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	workingDir, configDir, cacheDir := Paths(root)

	for _, dir := range []string{workingDir, configDir, cacheDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range [][]string{
		{"init", "-q"},
		{"-c", "user.email=t@example.invalid", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "root"},
	} {
		cmd := exec.Command(git, c...)
		cmd.Dir = workingDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", c, err, out)
		}
	}

	if err := PrepareConfigDir(configDir, workingDir, ""); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "session-output.log"), []byte("output"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, "package.tgz"), []byte("cached"), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(git, "status", "--porcelain")
	cmd.Dir = workingDir
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 0 {
		t.Errorf("checkout is dirty after platform state was written:\n%s", out)
	}
}

func TestCacheEnvRedirectsPackageManagersToThePersistentVolume(t *testing.T) {
	got := CacheEnv([]string{"PATH=/usr/bin"}, "/workspace/.cache")

	for _, want := range []string{
		"XDG_CACHE_HOME=/workspace/.cache",
		"npm_config_cache=/workspace/.cache/npm",
		"PIP_CACHE_DIR=/workspace/.cache/pip",
		"GOMODCACHE=/workspace/.cache/go/mod",
	} {
		if !slices.Contains(got, want) {
			t.Errorf("missing %q; a suspend/resume would re-download (15.12)", want)
		}
	}
	if !slices.Contains(got, "PATH=/usr/bin") {
		t.Error("unrelated variables must be preserved")
	}
}

func TestCacheEnvOverridesInheritedValues(t *testing.T) {
	got := CacheEnv([]string{"npm_config_cache=/tmp/ephemeral"}, "/workspace/.cache")
	n := 0
	for _, e := range got {
		if strings.HasPrefix(e, "npm_config_cache=") {
			n++
			if e != "npm_config_cache=/workspace/.cache/npm" {
				t.Errorf("npm cache = %q, want the persistent path", e)
			}
		}
	}
	if n != 1 {
		t.Errorf("npm_config_cache appears %d times, want 1", n)
	}
}

func TestEnsureCacheDirsCreatesThemUnderThePersistentRoot(t *testing.T) {
	cacheDir := filepath.Join(t.TempDir(), ".cache")
	if err := EnsureCacheDirs(cacheDir); err != nil {
		t.Fatal(err)
	}
	for _, sub := range []string{"npm", "pip", "go/mod"} {
		if _, err := os.Stat(filepath.Join(cacheDir, sub)); err != nil {
			t.Errorf("cache subdir %s not created: %v", sub, err)
		}
	}
}
