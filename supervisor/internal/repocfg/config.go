// Package repocfg reads the target repository's own declaration of how it is
// migrated, started and tested. The platform cannot know a repository's
// commands, and hard-coding a toolchain would restrict which repositories can
// be worked on at all, so the commands come from a file the repository owns.
package repocfg

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Path is where the declaration lives, relative to the working directory.
const Path = ".devplatform/workspace.json"

// Config is the declaration. Every field is optional: a repository that
// declares nothing has nothing for the platform to run, which is a valid state
// rather than a misconfiguration.
type Config struct {
	Migrate []string `json:"migrate"`
	Seed    []string `json:"seed"`
	// App is the long-running application the browser E2E suite drives.
	App      []string `json:"app"`
	UnitTest []string `json:"unitTest"`
	E2ETest  []string `json:"e2eTest"`
	// E2ETestFilter is the same suite restricted to named tests, which are
	// appended to it. Without it, re-verifying one fix costs a full suite run.
	E2ETestFilter []string `json:"e2eTestFilter"`
	// ReportDir is where the E2E suite leaves its HTML report, relative to the
	// working directory.
	ReportDir string `json:"reportDir"`
}

// Load reads the declaration. A repository without one is not an error — the
// caller skips whatever it would have described.
func Load(workingDir string) (Config, error) {
	path := filepath.Join(workingDir, Path)
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Config{}, nil
	}
	if err != nil {
		return Config{}, err
	}
	var cfg Config
	if err := json.Unmarshal(b, &cfg); err != nil {
		return Config{}, fmt.Errorf("repocfg: %s: %w", path, err)
	}
	return cfg, nil
}
