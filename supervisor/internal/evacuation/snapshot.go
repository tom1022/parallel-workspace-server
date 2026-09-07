package evacuation

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Snapshot is the record of one off-node evacuation (design.md
// "EvacuationSnapshot").
type Snapshot struct {
	WorkspaceId     string `json:"workspaceId"`
	Branch          string `json:"branch"`
	HeadCommit      string `json:"headCommit"`
	BundleKey       string `json:"bundleKey"`
	DirtyArchiveKey string `json:"dirtyArchiveKey"`
	CapturedAt      string `json:"capturedAt"`
	SizeBytes       int64  `json:"sizeBytes"`
}

// editorServerDir is what the IDE route's editor server unpacks into the
// working directory while a developer is connected (4.10). It is reinstalled
// by the next connection and is not the branch's work, so it is never
// collected.
const editorServerDir = ".vscode-server"

// Keys are fixed per workspace: an evacuation overwrites the previous one
// rather than accumulating generations (16.7), so restore never has to pick
// between candidates.
func keyPrefix(workspaceId string) string { return "workspace/" + workspaceId + "/latest/" }

func BundleKey(workspaceId string) string       { return keyPrefix(workspaceId) + "bundle.git" }
func DirtyArchiveKey(workspaceId string) string { return keyPrefix(workspaceId) + "dirty.tar.gz" }

// Agent captures a working directory to the object store.
type Agent struct {
	WorkingDir  string
	WorkspaceId string
	Store       *S3
}

// Evacuate writes the two objects that make the working directory
// reconstructible off-node: the commits the remote does not have, as a Git
// bundle, and the uncommitted changes, as a compressed archive (16.7/16.9).
//
// Both objects are written on every run even when empty. An empty object is
// how "there is nothing local here" is recorded; leaving a previous run's
// bundle in place would make a later restore replay work that has since been
// pushed.
func (a *Agent) Evacuate(ctx context.Context) (Snapshot, error) {
	if a.Store == nil {
		return Snapshot{}, fmt.Errorf("evacuation: no object store configured")
	}
	if err := a.Store.Validate(); err != nil {
		return Snapshot{}, err
	}

	branch, err := a.git("rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return Snapshot{}, err
	}
	head, err := a.git("rev-parse", "HEAD")
	if err != nil {
		return Snapshot{}, err
	}

	staging, err := os.MkdirTemp("", "devplatform-evacuation-")
	if err != nil {
		return Snapshot{}, err
	}
	defer os.RemoveAll(staging)

	bundlePath := filepath.Join(staging, "bundle.git")
	if err := a.writeBundle(branch, bundlePath); err != nil {
		return Snapshot{}, err
	}
	archivePath := filepath.Join(staging, "dirty.tar.gz")
	if err := a.writeDirtyArchive(archivePath); err != nil {
		return Snapshot{}, err
	}

	bundleSize, err := a.Store.PutFile(ctx, BundleKey(a.WorkspaceId), bundlePath)
	if err != nil {
		return Snapshot{}, err
	}
	archiveSize, err := a.Store.PutFile(ctx, DirtyArchiveKey(a.WorkspaceId), archivePath)
	if err != nil {
		return Snapshot{}, err
	}

	return Snapshot{
		WorkspaceId:     a.WorkspaceId,
		Branch:          branch,
		HeadCommit:      head,
		BundleKey:       BundleKey(a.WorkspaceId),
		DirtyArchiveKey: DirtyArchiveKey(a.WorkspaceId),
		CapturedAt:      time.Now().UTC().Format(time.RFC3339),
		SizeBytes:       bundleSize + archiveSize,
	}, nil
}

// writeBundle captures the commits the remote does not already have. When the
// branch exists on the remote only the commits past it are bundled, so restore
// applies them onto a plain clone; when it does not, the branch's whole history
// is local-only and all of it has to travel.
func (a *Agent) writeBundle(branch, path string) error {
	revs := branch
	if _, err := a.git("rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+branch); err == nil {
		revs = "origin/" + branch + ".." + branch
	}

	count, err := a.git("rev-list", "--count", revs)
	if err != nil {
		return err
	}
	if count == "0" {
		return os.WriteFile(path, nil, 0o600)
	}
	_, err = a.git("bundle", "create", path, revs)
	return err
}

// writeDirtyArchive tars every file that differs from HEAD, whether staged or
// not, plus untracked files git is not ignoring.
//
// ponytail: deletions are not represented — a file removed but not committed
// comes back on restore. Recording a delete list alongside the archive is the
// upgrade path if that ever matters.
func (a *Agent) writeDirtyArchive(path string) error {
	// -uall lists untracked files individually; the default collapses them to
	// their directory, which is not a path that can be tarred as a file.
	out, err := a.git("status", "--porcelain", "-z", "-uall")
	if err != nil {
		return err
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	zw := gzip.NewWriter(f)
	tw := tar.NewWriter(zw)

	for _, name := range dirtyPaths(out) {
		if err := addFile(tw, a.WorkingDir, name); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return zw.Close()
}

// dirtyPaths parses `git status --porcelain -z`. Each record is two status
// characters, a space, then the path; a rename or copy is followed by a second
// NUL-terminated field holding the path it came from, which is not itself a
// change to capture.
func dirtyPaths(out string) []string {
	fields := strings.Split(out, "\x00")
	var paths []string
	for i := 0; i < len(fields); i++ {
		rec := fields[i]
		if len(rec) < 4 {
			continue
		}
		if rec[0] == 'R' || rec[0] == 'C' {
			i++
		}
		name := rec[3:]
		if strings.HasPrefix(name, editorServerDir+"/") {
			continue
		}
		paths = append(paths, name)
	}
	return paths
}

func addFile(tw *tar.Writer, root, name string) error {
	full := filepath.Join(root, name)
	info, err := os.Lstat(full)
	if err != nil {
		// A path git reports as changed but that is gone from disk is a
		// deletion, which the archive does not carry.
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	hdr, err := tar.FileInfoHeader(info, "")
	if err != nil {
		return err
	}
	hdr.Name = filepath.ToSlash(name)
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	body, err := os.ReadFile(full)
	if err != nil {
		return err
	}
	_, err = tw.Write(body)
	return err
}

func (a *Agent) git(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = a.WorkingDir
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("evacuation: git %s: %w", strings.Join(args, " "), err)
	}
	return strings.TrimRight(string(out), "\n"), nil
}
