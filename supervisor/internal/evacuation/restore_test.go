package evacuation

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"testing"
)

// freshClone stands in for what the init script does on a replacement node:
// the remote is all that is left, so the working directory it produces holds
// none of the local-only work.
func freshClone(t *testing.T, remote string) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "work")
	git(t, root, "clone", remote, dir)
	return dir
}

func TestRestoreReplaysLocalCommitsAndDirtyFilesAfterNodeLoss(t *testing.T) {
	workingDir, remote := newRepo(t)
	write(t, workingDir, "committed.txt", "local commit\n")
	git(t, workingDir, "add", ".")
	git(t, workingDir, "commit", "-m", "local only")
	head := git(t, workingDir, "rev-parse", "HEAD")
	write(t, workingDir, "README.md", "modified\n")
	write(t, workingDir, "scratch/new.txt", "untracked\n")

	store, _ := newFakeStore(t)
	agent := &Agent{WorkingDir: workingDir, WorkspaceId: "ws-1", Store: store}
	if _, err := agent.Evacuate(context.Background()); err != nil {
		t.Fatal(err)
	}

	restored := freshClone(t, remote)
	if err := (&Agent{WorkingDir: restored, WorkspaceId: "ws-1", Store: store}).Restore(context.Background()); err != nil {
		t.Fatal(err)
	}

	if got := git(t, restored, "rev-parse", "HEAD"); got != head {
		t.Errorf("HEAD = %s, want the evacuated commit %s", got, head)
	}
	for name, want := range map[string]string{
		"committed.txt":   "local commit\n",
		"README.md":       "modified\n",
		"scratch/new.txt": "untracked\n",
	} {
		body, err := os.ReadFile(filepath.Join(restored, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if string(body) != want {
			t.Errorf("%s = %q, want %q", name, body, want)
		}
	}
}

// A workspace evacuated with nothing local writes two zero-length objects.
// Restore has to read those as "nothing to replay" rather than feeding an
// empty file to git.
func TestRestoreSkipsEmptyObjects(t *testing.T) {
	workingDir, remote := newRepo(t)
	store, _ := newFakeStore(t)
	if _, err := (&Agent{WorkingDir: workingDir, WorkspaceId: "ws-1", Store: store}).Evacuate(context.Background()); err != nil {
		t.Fatal(err)
	}

	restored := freshClone(t, remote)
	before := git(t, restored, "rev-parse", "HEAD")
	if err := (&Agent{WorkingDir: restored, WorkspaceId: "ws-1", Store: store}).Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := git(t, restored, "rev-parse", "HEAD"); got != before {
		t.Errorf("HEAD moved to %s", got)
	}
}

// Restore runs from the init script, which runs on every container start.
// Replaying an older snapshot over a working directory that already holds
// local work would destroy exactly what evacuation exists to protect.
func TestRestoreLeavesAWorkingDirectoryHoldingLocalWorkAlone(t *testing.T) {
	workingDir, remote := newRepo(t)
	write(t, workingDir, "committed.txt", "evacuated\n")
	git(t, workingDir, "add", ".")
	git(t, workingDir, "commit", "-m", "local only")

	store, _ := newFakeStore(t)
	if _, err := (&Agent{WorkingDir: workingDir, WorkspaceId: "ws-1", Store: store}).Evacuate(context.Background()); err != nil {
		t.Fatal(err)
	}

	live := freshClone(t, remote)
	write(t, live, "newer.txt", "work done since\n")
	git(t, live, "add", ".")
	git(t, live, "commit", "-m", "newer")
	head := git(t, live, "rev-parse", "HEAD")

	if err := (&Agent{WorkingDir: live, WorkspaceId: "ws-1", Store: store}).Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := git(t, live, "rev-parse", "HEAD"); got != head {
		t.Errorf("HEAD = %s, want the untouched local head %s", got, head)
	}
	if _, err := os.Stat(filepath.Join(live, "committed.txt")); !os.IsNotExist(err) {
		t.Error("the snapshot was replayed over a working directory that already held local work")
	}
}

// The archive is fetched from an object store, so a path in it is untrusted
// input; an entry escaping the working directory must not be written.
func TestRestoreRejectsArchivePathsEscapingTheWorkingDirectory(t *testing.T) {
	_, remote := newRepo(t)
	restored := freshClone(t, remote)

	store, fake := newFakeStore(t)
	fake.objects[BundleKey("ws-1")] = nil
	fake.objects[DirtyArchiveKey("ws-1")] = tarGz(t, map[string]string{"../escaped.txt": "nope\n"})

	err := (&Agent{WorkingDir: restored, WorkspaceId: "ws-1", Store: store}).Restore(context.Background())
	if err == nil {
		t.Fatal("expected the traversing entry to be refused")
	}
	if _, statErr := os.Stat(filepath.Join(filepath.Dir(restored), "escaped.txt")); !os.IsNotExist(statErr) {
		t.Error("an entry was written outside the working directory")
	}
}

func tarGz(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for name, body := range entries {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
