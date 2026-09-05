package evacuation

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// maxArchiveBytes bounds what one restore will expand, so a corrupt or
// oversized archive fails instead of filling the workspace volume.
const maxArchiveBytes = 2 << 30

// Restore reconstructs the working directory from the most recent evacuation
// after the node holding it was lost (16.9). The clone from the remote is the
// init script's step; what a clone cannot re-derive is replayed here — the
// local-only commits from the bundle, then the uncommitted changes from the
// archive, in that order.
//
// It is a no-op when the working directory already holds local work of its
// own. The init script runs on every container start, and replaying an older
// snapshot over live work would destroy exactly what evacuation protects.
func (a *Agent) Restore(ctx context.Context) error {
	if a.Store == nil {
		return fmt.Errorf("evacuation: no object store configured")
	}
	if err := a.Store.Validate(); err != nil {
		return err
	}

	local, err := a.hasLocalWork()
	if err != nil {
		return err
	}
	if local {
		return nil
	}

	branch, err := a.git("rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return err
	}

	staging, err := os.MkdirTemp("", "devplatform-restore-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)

	bundlePath := filepath.Join(staging, "bundle.git")
	size, err := a.Store.GetFile(ctx, BundleKey(a.WorkspaceId), bundlePath)
	if err != nil {
		return err
	}
	if size > 0 {
		if _, err := a.git("fetch", bundlePath, branch); err != nil {
			return err
		}
		if _, err := a.git("reset", "--hard", "FETCH_HEAD"); err != nil {
			return err
		}
	}

	archivePath := filepath.Join(staging, "dirty.tar.gz")
	size, err = a.Store.GetFile(ctx, DirtyArchiveKey(a.WorkspaceId), archivePath)
	if err != nil {
		return err
	}
	if size == 0 {
		return nil
	}
	return a.extractArchive(archivePath)
}

// hasLocalWork reports whether the working directory holds anything a restore
// would overwrite: commits the remote does not have, or uncommitted changes.
func (a *Agent) hasLocalWork() (bool, error) {
	status, err := a.git("status", "--porcelain", "-z", "-uall")
	if err != nil {
		return false, err
	}
	if len(dirtyPaths(status)) > 0 {
		return true, nil
	}

	branch, err := a.git("rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return false, err
	}
	// No remote-tracking branch means nothing has been pushed, so every commit
	// on this branch is local — but that is also the state a fresh clone of a
	// remote that lacks the branch leaves behind, which is precisely what a
	// restore is for. Only commits past the tracked remote branch count.
	if _, err := a.git("rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+branch); err != nil {
		return false, nil
	}
	count, err := a.git("rev-list", "--count", "origin/"+branch+".."+branch)
	if err != nil {
		return false, err
	}
	return count != "0", nil
}

// extractArchive writes the uncommitted changes back over the tree. Entry
// names come from an object store and are therefore untrusted: anything that
// would land outside the working directory fails the whole restore rather
// than being skipped, so a tampered archive is visible instead of silent.
func (a *Agent) extractArchive(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("evacuation: reading dirty archive: %w", err)
	}
	defer zr.Close()

	root, err := filepath.Abs(a.WorkingDir)
	if err != nil {
		return err
	}
	tr := tar.NewReader(io.LimitReader(zr, maxArchiveBytes))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("evacuation: reading dirty archive: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		target := filepath.Join(root, filepath.FromSlash(hdr.Name))
		if target != root && !strings.HasPrefix(target, root+string(os.PathSeparator)) {
			return fmt.Errorf("evacuation: dirty archive entry %q escapes the working directory", hdr.Name)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(hdr.Mode).Perm())
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, tr); err != nil {
			out.Close()
			return err
		}
		if err := out.Close(); err != nil {
			return err
		}
	}
}
