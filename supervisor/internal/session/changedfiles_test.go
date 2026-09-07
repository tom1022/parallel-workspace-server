package session

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func changedFilesRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	initRepo(t, dir)
	git(t, dir, "config", "user.name", "test")
	git(t, dir, "config", "user.email", "test@example.invalid")
	commit(t, dir, "tracked.txt")
	return dir
}

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// 10.3: the changed-file list is what the Blackboard reports for a branch, so
// it has to cover both what the agent edited and what it newly created.
func TestChangedFiles_ReportsModifiedAndUntracked(t *testing.T) {
	dir := changedFilesRepo(t)
	write(t, dir, "tracked.txt", "edited")
	write(t, dir, "pkg/new.go", "package pkg")

	got, err := ChangedFiles(dir)
	if err != nil {
		t.Fatalf("ChangedFiles: %v", err)
	}
	want := []string{"pkg/new.go", "tracked.txt"}
	if !slices.Equal(got, want) {
		t.Fatalf("ChangedFiles = %v, want %v", got, want)
	}
}

func TestChangedFiles_CleanTreeReportsNothing(t *testing.T) {
	dir := changedFilesRepo(t)

	got, err := ChangedFiles(dir)
	if err != nil {
		t.Fatalf("ChangedFiles: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("ChangedFiles = %v, want none on a clean tree", got)
	}
}

// A rename is two paths in one porcelain record; reporting the source as a
// changed file of its own would name a path that no longer exists.
func TestChangedFiles_RenameReportsDestinationOnly(t *testing.T) {
	dir := changedFilesRepo(t)
	git(t, dir, "mv", "tracked.txt", "renamed.txt")

	got, err := ChangedFiles(dir)
	if err != nil {
		t.Fatalf("ChangedFiles: %v", err)
	}
	if !slices.Equal(got, []string{"renamed.txt"}) {
		t.Fatalf("ChangedFiles = %v, want [renamed.txt]", got)
	}
}

func TestHandler_ChangedFiles(t *testing.T) {
	dir := changedFilesRepo(t)
	write(t, dir, "tracked.txt", "edited")

	srv := httptest.NewServer((&Supervisor{WorkingDir: dir}).Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/changed-files")
	if err != nil {
		t.Fatalf("GET /changed-files: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %s, want 200", resp.Status)
	}
	var body struct {
		Files []string `json:"files"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !slices.Equal(body.Files, []string{"tracked.txt"}) {
		t.Fatalf("files = %v, want [tracked.txt]", body.Files)
	}
}
