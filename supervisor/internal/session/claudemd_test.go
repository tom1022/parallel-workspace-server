package session

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestSyncClaudeMD_CopiesTheMountedDocument(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "mounted", ClaudeMDName)
	if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("# common\n\nbranch: x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	workingDir := filepath.Join(dir, "repo")
	if err := os.MkdirAll(workingDir, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := SyncClaudeMD(src, workingDir); err != nil {
		t.Fatalf("SyncClaudeMD: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(workingDir, ClaudeMDName))
	if err != nil {
		t.Fatalf("read copied document: %v", err)
	}
	if string(got) != "# common\n\nbranch: x\n" {
		t.Errorf("copied document = %q, want the mounted content", got)
	}
}

// A workspace whose ConfigMap has not landed yet must still take input: the
// document is context, not a precondition for a turn.
func TestSyncClaudeMD_MissingSourceIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	if err := SyncClaudeMD(filepath.Join(dir, "absent", ClaudeMDName), dir); err != nil {
		t.Errorf("SyncClaudeMD with no source = %v, want nil", err)
	}
	if err := SyncClaudeMD("", dir); err != nil {
		t.Errorf("SyncClaudeMD with no configured source = %v, want nil", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ClaudeMDName)); !os.IsNotExist(err) {
		t.Error("a missing source wrote a document anyway")
	}
}

// Regenerating overwrites, so a stale document never survives a turn boundary.
func TestSyncClaudeMD_ReplacesAnEarlierDocument(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, ClaudeMDName+".src")
	if err := os.WriteFile(src, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ClaudeMDName), []byte("old and much longer"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := SyncClaudeMD(src, dir); err != nil {
		t.Fatalf("SyncClaudeMD: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, ClaudeMDName))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new" {
		t.Errorf("document = %q, want it replaced wholesale", got)
	}
}

// 10.3 lists what the agent is changing. The platform owns the checkout's
// CLAUDE.md, so its presence there is not the branch's work and must not reach
// the other branches as such.
func TestParseChangedFiles_ExcludesThePlatformDocument(t *testing.T) {
	got := parseChangedFiles("?? CLAUDE.md\x00 M internal/app.go\x00 M docs/CLAUDE.md\x00")
	if !slices.Equal(got, []string{"docs/CLAUDE.md", "internal/app.go"}) {
		t.Errorf("parseChangedFiles = %v, want the checkout root's CLAUDE.md dropped and nothing else", got)
	}
}
