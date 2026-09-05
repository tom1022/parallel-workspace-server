package session

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Origin trailer values. Both origins are stated positively rather than one
// being the absence of the other, so a commit made outside the platform stays
// distinguishable from an autonomous one.
const (
	originTrailerKey = "Devplatform-Origin"
	OriginAutonomous = "autonomous"
	OriginManual     = "manual"
)

// hookMarker identifies hooks this package owns, so a re-run updates its own
// files but never overwrites one the target repository brought along.
const hookMarker = "# devplatform-managed"

// CommitIdentity is the author recorded on work the platform performs (20.2).
type CommitIdentity struct {
	Name  string
	Email string
}

// ConfigureCommitPolicy makes the working directory record how each commit came
// about and refuse history-destroying pushes (20.2, 20.3, 20.4).
func ConfigureCommitPolicy(workingDir string, id CommitIdentity, tm *Tmux) error {
	if err := gitConfig(workingDir, "user.name", id.Name); err != nil {
		return err
	}
	if err := gitConfig(workingDir, "user.email", id.Email); err != nil {
		return err
	}
	hooks, err := hooksDir(workingDir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(hooks, 0o700); err != nil {
		return err
	}
	if err := writeHook(filepath.Join(hooks, "prepare-commit-msg"), prepareCommitMsgHook(tm)); err != nil {
		return err
	}
	return writeHook(filepath.Join(hooks, "pre-push"), prePushHook)
}

func gitConfig(workingDir, key, value string) error {
	cmd := exec.Command("git", "config", "--local", key, value)
	cmd.Dir = workingDir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("supervisor: git config %s: %w: %s", key, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// hooksDir asks git where hooks actually live. A repository that sets
// core.hooksPath (husky and friends do) would otherwise leave the hooks written
// to .git/hooks silently inert, and with them the force-push guard.
func hooksDir(workingDir string) (string, error) {
	cmd := exec.Command("git", "rev-parse", "--git-path", "hooks")
	cmd.Dir = workingDir
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("supervisor: locate hooks dir: %w", err)
	}
	path := strings.TrimSpace(string(out))
	if filepath.IsAbs(path) {
		return path, nil
	}
	return filepath.Join(workingDir, path), nil
}

func writeHook(path, body string) error {
	existing, err := os.ReadFile(path)
	switch {
	case err == nil && !strings.Contains(string(existing), hookMarker):
		return fmt.Errorf("supervisor: %s already exists and is not ours; the commit policy would be unenforced", path)
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return err
	}
	return os.WriteFile(path, []byte(body), 0o755)
}

// prepareCommitMsgHook stamps every commit with its origin. The writable-client
// check is the same signal the handover path uses (3.4), so the record cannot
// disagree with who actually held the session at commit time.
func prepareCommitMsgHook(tm *Tmux) string {
	return fmt.Sprintf(`#!/bin/sh
%s
set -eu
origin=%s
if tmux -L %s list-clients -t %s -F '#{client_readonly}' 2>/dev/null | grep -qx 0; then
	origin=%s
fi
git interpret-trailers --in-place --if-exists doNothing --trailer "%s=$origin" "$1"
`, hookMarker, OriginAutonomous, shellQuote(tm.Socket), shellQuote(tm.Session), OriginManual, originTrailerKey)
}

// prePushHook refuses any push that would drop commits the remote already has
// (20.4) or delete the branch outright.
//
// ponytail: a --no-verify push walks past this. The remote's own branch
// protection is the enforceable half; add it there if the workspace ever gets a
// credential that could rewrite a shared branch.
const prePushHook = `#!/bin/sh
` + hookMarker + `
set -eu
while read -r local_ref local_sha remote_ref remote_sha; do
	case "$local_sha" in
	*[!0]*) ;;
	*)
		echo "devplatform: refusing to delete $remote_ref" >&2
		exit 1
		;;
	esac
	# An all-zero remote sha means the branch is new there, so nothing can be lost.
	case "$remote_sha" in
	*[!0]*) ;;
	*) continue ;;
	esac
	if ! git merge-base --is-ancestor "$remote_sha" "$local_sha"; then
		echo "devplatform: refusing non-fast-forward push to $remote_ref" >&2
		exit 1
	fi
done
`
