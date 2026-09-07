// The changed-file half of the Blackboard entry (10.3). The list is published
// for the Workspace Controller to fetch rather than pushed into Workspace
// status, for the same reason the SSH session count is: the controller stays
// the single writer of that status.
package session

import (
	"fmt"
	"os/exec"
	"sort"
	"strings"
)

// ChangedFiles lists the working directory's paths that differ from HEAD,
// newly created ones included. It reads git's own porcelain output instead of
// walking the tree, so `.gitignore` and rename detection are honoured without
// being reimplemented here.
func ChangedFiles(workingDir string) ([]string, error) {
	cmd := exec.Command("git", "status", "--porcelain=v1", "-z", "--untracked-files=all")
	cmd.Dir = workingDir
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("supervisor: git status in %s: %w", workingDir, err)
	}
	return parseChangedFiles(string(out)), nil
}

// parseChangedFiles reads `git status -z` records: each is "XY <path>", NUL
// terminated, and a rename or copy is followed by a second record holding its
// source path.
func parseChangedFiles(out string) []string {
	records := strings.Split(strings.TrimSuffix(out, "\x00"), "\x00")
	var files []string
	for i := 0; i < len(records); i++ {
		rec := records[i]
		if len(rec) < 4 {
			continue
		}
		status, path := rec[:2], rec[3:]
		if strings.ContainsAny(status, "RC") {
			i++ // the source path, which no longer exists under that name
		}
		files = append(files, path)
	}
	sort.Strings(files)
	return files
}
