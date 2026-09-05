package evacuation

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeStore records the last body written to each key, which is what the
// overwrite-in-place contract is judged on.
type fakeStore struct {
	mu      sync.Mutex
	objects map[string][]byte
	puts    []string
}

func newFakeStore(t *testing.T) (*S3, *fakeStore) {
	t.Helper()
	f := &fakeStore{objects: map[string][]byte{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.URL.Path, "/workspace/")
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Method == http.MethodGet {
			body, ok := f.objects[key]
			if !ok {
				http.Error(w, "no such key", http.StatusNotFound)
				return
			}
			_, _ = w.Write(body)
			return
		}
		body, _ := io.ReadAll(r.Body)
		f.objects[key] = body
		f.puts = append(f.puts, key)
	}))
	t.Cleanup(srv.Close)
	return &S3{
		Endpoint:  srv.URL,
		Bucket:    "workspace",
		Region:    "garage",
		AccessKey: "key",
		SecretKey: "secret",
	}, f
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// newRepo builds a working directory cloned from a bare remote, with one
// commit already shared with that remote.
func newRepo(t *testing.T) (workingDir, remote string) {
	t.Helper()
	root := t.TempDir()
	remote = filepath.Join(root, "remote.git")
	seed := filepath.Join(root, "seed")
	workingDir = filepath.Join(root, "work")

	git(t, root, "init", "--bare", "-b", "main", remote)
	git(t, root, "init", "-b", "main", seed)
	write(t, seed, "README.md", "base\n")
	git(t, seed, "add", ".")
	git(t, seed, "commit", "-m", "base")
	git(t, seed, "remote", "add", "origin", remote)
	git(t, seed, "push", "origin", "main")

	git(t, root, "clone", remote, workingDir)
	return workingDir, remote
}

func archiveEntries(t *testing.T, raw []byte) map[string]string {
	t.Helper()
	entries := map[string]string{}
	if len(raw) == 0 {
		return entries
	}
	zr, err := gzip.NewReader(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("archive is not gzip: %v", err)
	}
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("archive is not tar: %v", err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		entries[h.Name] = string(body)
	}
	return entries
}

func TestEvacuateBundlesCommitsMissingFromRemote(t *testing.T) {
	workingDir, remote := newRepo(t)
	write(t, workingDir, "a.txt", "one\n")
	git(t, workingDir, "add", ".")
	git(t, workingDir, "commit", "-m", "local one")
	head := git(t, workingDir, "rev-parse", "HEAD")

	store, fake := newFakeStore(t)
	snap, err := (&Agent{WorkingDir: workingDir, WorkspaceId: "ws-abc", Store: store}).Evacuate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snap.HeadCommit != head {
		t.Errorf("headCommit = %q, want %q", snap.HeadCommit, head)
	}

	bundle := filepath.Join(t.TempDir(), "b.bundle")
	if err := os.WriteFile(bundle, fake.objects[BundleKey("ws-abc")], 0o600); err != nil {
		t.Fatal(err)
	}
	// A fresh clone of the remote lacks the local commit; applying the bundle
	// must supply exactly it.
	restored := filepath.Join(t.TempDir(), "restored")
	git(t, t.TempDir(), "clone", remote, restored)
	git(t, restored, "fetch", bundle, "main")
	if got := git(t, restored, "rev-parse", "FETCH_HEAD"); got != head {
		t.Errorf("bundle did not carry the local commit: FETCH_HEAD = %q, want %q", got, head)
	}
}

func TestEvacuateBundlesFullHistoryWhenRemoteBranchMissing(t *testing.T) {
	workingDir, remote := newRepo(t)
	git(t, workingDir, "checkout", "-b", "feature/x")
	write(t, workingDir, "a.txt", "one\n")
	git(t, workingDir, "add", ".")
	git(t, workingDir, "commit", "-m", "local one")
	head := git(t, workingDir, "rev-parse", "HEAD")

	store, fake := newFakeStore(t)
	snap, err := (&Agent{WorkingDir: workingDir, WorkspaceId: "ws-abc", Store: store}).Evacuate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snap.Branch != "feature/x" {
		t.Errorf("branch = %q", snap.Branch)
	}

	bundle := filepath.Join(t.TempDir(), "b.bundle")
	if err := os.WriteFile(bundle, fake.objects[BundleKey("ws-abc")], 0o600); err != nil {
		t.Fatal(err)
	}
	restored := filepath.Join(t.TempDir(), "restored")
	git(t, t.TempDir(), "clone", remote, restored)
	git(t, restored, "fetch", bundle, "feature/x")
	if got := git(t, restored, "rev-parse", "FETCH_HEAD"); got != head {
		t.Errorf("FETCH_HEAD = %q, want %q", got, head)
	}
}

func TestEvacuateArchivesUncommittedChangesOnly(t *testing.T) {
	workingDir, _ := newRepo(t)
	write(t, workingDir, ".gitignore", "ignored.txt\n")
	git(t, workingDir, "add", ".gitignore")
	git(t, workingDir, "commit", "-m", "ignore rules")

	write(t, workingDir, "README.md", "modified\n")
	write(t, workingDir, "nested/new.txt", "untracked\n")
	write(t, workingDir, "ignored.txt", "junk\n")
	write(t, workingDir, "staged.txt", "staged\n")
	git(t, workingDir, "add", "staged.txt")

	store, fake := newFakeStore(t)
	if _, err := (&Agent{WorkingDir: workingDir, WorkspaceId: "ws-abc", Store: store}).Evacuate(context.Background()); err != nil {
		t.Fatal(err)
	}

	entries := archiveEntries(t, fake.objects[DirtyArchiveKey("ws-abc")])
	want := map[string]string{
		"README.md":      "modified\n",
		"nested/new.txt": "untracked\n",
		"staged.txt":     "staged\n",
	}
	for name, content := range want {
		if entries[name] != content {
			t.Errorf("archive[%q] = %q, want %q", name, entries[name], content)
		}
	}
	if _, ok := entries["ignored.txt"]; ok {
		t.Error("ignored file was archived")
	}
	if len(entries) != len(want) {
		t.Errorf("archive holds %d entries, want %d: %v", len(entries), len(want), entries)
	}
}

func TestEvacuateSkipsDeletedFilesWithoutFailing(t *testing.T) {
	workingDir, _ := newRepo(t)
	if err := os.Remove(filepath.Join(workingDir, "README.md")); err != nil {
		t.Fatal(err)
	}
	store, fake := newFakeStore(t)
	if _, err := (&Agent{WorkingDir: workingDir, WorkspaceId: "ws-abc", Store: store}).Evacuate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if entries := archiveEntries(t, fake.objects[DirtyArchiveKey("ws-abc")]); len(entries) != 0 {
		t.Errorf("archive should be empty, got %v", entries)
	}
}

// A clean tree still overwrites both keys: leaving a previous evacuation's
// bundle in place would make a later restore replay work that has since been
// pushed.
func TestEvacuateOverwritesBothKeysWhenNothingIsLocal(t *testing.T) {
	workingDir, _ := newRepo(t)
	store, fake := newFakeStore(t)
	agent := &Agent{WorkingDir: workingDir, WorkspaceId: "ws-abc", Store: store}
	if _, err := agent.Evacuate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(fake.objects[BundleKey("ws-abc")]) != 0 {
		t.Error("expected an empty bundle object for a branch matching its remote")
	}

	if _, err := agent.Evacuate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(fake.puts) != 4 {
		t.Errorf("puts = %v, want two runs over the same two keys", fake.puts)
	}
	if len(fake.objects) != 2 {
		t.Errorf("objects = %v, want exactly two fixed keys", fake.objects)
	}
}

func TestEvacuateReportsSnapshotFields(t *testing.T) {
	workingDir, _ := newRepo(t)
	write(t, workingDir, "a.txt", "one\n")
	git(t, workingDir, "add", ".")
	git(t, workingDir, "commit", "-m", "local one")
	write(t, workingDir, "b.txt", "dirty\n")

	store, _ := newFakeStore(t)
	snap, err := (&Agent{WorkingDir: workingDir, WorkspaceId: "ws-abc", Store: store}).Evacuate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snap.WorkspaceId != "ws-abc" || snap.Branch != "main" {
		t.Errorf("snapshot identity = %+v", snap)
	}
	if snap.BundleKey != "workspace/ws-abc/latest/bundle.git" {
		t.Errorf("bundleKey = %q", snap.BundleKey)
	}
	if snap.DirtyArchiveKey != "workspace/ws-abc/latest/dirty.tar.gz" {
		t.Errorf("dirtyArchiveKey = %q", snap.DirtyArchiveKey)
	}
	if snap.SizeBytes <= 0 {
		t.Errorf("sizeBytes = %d", snap.SizeBytes)
	}
	if snap.CapturedAt == "" {
		t.Error("capturedAt is empty")
	}
}

func TestEvacuateFailsWhenStoreIsUnreachable(t *testing.T) {
	workingDir, _ := newRepo(t)
	store := &S3{Endpoint: "http://127.0.0.1:1", Bucket: "workspace", Region: "garage", AccessKey: "k", SecretKey: "s"}
	if _, err := (&Agent{WorkingDir: workingDir, WorkspaceId: "ws-abc", Store: store}).Evacuate(context.Background()); err == nil {
		t.Fatal("expected an error when the destination is unavailable")
	}
}

func TestEvacuateRejectsUnconfiguredStore(t *testing.T) {
	workingDir, _ := newRepo(t)
	if _, err := (&Agent{WorkingDir: workingDir, WorkspaceId: "ws-abc", Store: &S3{}}).Evacuate(context.Background()); err == nil {
		t.Fatal("expected an error when the destination is not configured")
	}
}
