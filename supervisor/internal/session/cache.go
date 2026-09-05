package session

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Layout below the PVC mount root. The checkout is a subdirectory rather than
// the mount root itself: the config area, the session output log and the
// package cache all have to live on the same volume (5.3, 15.12), and at the
// root they would sit inside the git working tree as untracked files — where a
// `git add -A` in the session would commit the credential.
const (
	checkoutDirName = "repo"
	configDirName   = ".claude-config"
	cacheDirName    = ".cache"
)

// cacheVars maps each package manager's cache location to its subdirectory.
// The set is deliberately the runtimes the base image ships; anything else
// falls back to XDG_CACHE_HOME.
var cacheVars = map[string]string{
	"npm_config_cache":  "npm",
	"YARN_CACHE_FOLDER": "yarn",
	"PIP_CACHE_DIR":     "pip",
	"GOMODCACHE":        "go/mod",
	"GOCACHE":           "go/build",
}

// Paths derives the workspace layout from the PVC mount root. It takes no
// workspace identity on purpose: the checkout path reaches Claude Code's
// system prompt, so it must be byte-identical across workspaces for the prompt
// cache to be reused (7.13, 7.14).
func Paths(mountRoot string) (workingDir, configDir, cacheDir string) {
	return filepath.Join(mountRoot, checkoutDirName),
		filepath.Join(mountRoot, configDirName),
		filepath.Join(mountRoot, cacheDirName)
}

// CacheEnv points every package manager at cacheDir, which lives on the
// workspace volume, so a suspend/resume cycle reuses what the previous run
// downloaded instead of fetching it again (15.12).
func CacheEnv(base []string, cacheDir string) []string {
	names := make([]string, 0, len(cacheVars)+1)
	for name := range cacheVars {
		names = append(names, name)
	}
	names = append(names, "XDG_CACHE_HOME")

	out := make([]string, 0, len(base)+len(names))
	for _, e := range base {
		name, _, ok := strings.Cut(e, "=")
		if ok && !slices.Contains(names, name) {
			out = append(out, e)
		}
	}
	out = append(out, "XDG_CACHE_HOME="+cacheDir)
	for name, sub := range cacheVars {
		out = append(out, name+"="+filepath.Join(cacheDir, sub))
	}
	slices.Sort(out)
	return out
}

// EnsureCacheDirs creates the cache subdirectories. Package managers create
// their own, but pre-creating them keeps the failure mode at startup rather
// than mid-install on a read-only or full volume.
func EnsureCacheDirs(cacheDir string) error {
	for _, sub := range cacheVars {
		if err := os.MkdirAll(filepath.Join(cacheDir, sub), 0o755); err != nil {
			return err
		}
	}
	return nil
}
