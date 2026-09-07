// The workspace half of task 8.2: taking the Blackboard's regenerated
// CLAUDE.md into the working directory (design.md "Blackboard Reconciler"
// State Management, Requirement 10.6). The control plane writes the document
// into a ConfigMap and kubelet refreshes the mount whenever it likes; copying
// it in at the start of a turn is what fixes the moment it takes effect, so a
// running turn never sees its own instructions change underneath it.
package session

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// ClaudeMDName is the document's name in both the mount and the checkout.
const ClaudeMDName = "CLAUDE.md"

// SyncClaudeMD copies the mounted document into workingDir. An unset or absent
// source is not an error: the ConfigMap may not have landed yet, and a turn
// must not be blocked on context it can do without.
func SyncClaudeMD(src, workingDir string) error {
	if src == "" {
		return nil
	}
	content, err := os.ReadFile(src)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	// Written beside the destination and renamed over it, so a reader either
	// gets the whole previous document or the whole new one (10.9).
	dst := filepath.Join(workingDir, ClaudeMDName)
	tmp, err := os.CreateTemp(workingDir, ".claude-md-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}
