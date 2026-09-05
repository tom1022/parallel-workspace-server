package session

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

var testIdentity = CommitIdentity{Name: "Devplatform Agent", Email: "agent@devplatform.invalid"}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func gitFails(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("git %s unexpectedly succeeded:\n%s", strings.Join(args, " "), out)
	}
	return string(out)
}

func initRepo(t *testing.T, dir string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	git(t, dir, "init", "-q", "-b", "work")
}

func commit(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", name)
	git(t, dir, "commit", "-q", "-m", "add "+name)
}

func trailer(t *testing.T, dir string) string {
	t.Helper()
	return git(t, dir, "log", "-1", "--format=%(trailers:key=Devplatform-Origin,valueonly)")
}

func TestAutonomousCommitCarriesPlatformIdentityAndOrigin(t *testing.T) {
	dir := t.TempDir()
	initRepo(t, dir)
	tm := &Tmux{Socket: "devplatform-absent-" + filepath.Base(dir), Session: "workspace"}
	if err := ConfigureCommitPolicy(dir, testIdentity, tm); err != nil {
		t.Fatalf("configure: %v", err)
	}

	commit(t, dir, "a.txt")

	if got := git(t, dir, "log", "-1", "--format=%an <%ae>"); got != "Devplatform Agent <agent@devplatform.invalid>" {
		t.Errorf("author = %q, want the platform identity (20.2)", got)
	}
	if got := trailer(t, dir); got != OriginAutonomous {
		t.Errorf("origin trailer = %q, want %q", got, OriginAutonomous)
	}
}

func TestManualInterventionIsRecordedWhileAWritableClientHoldsTheSession(t *testing.T) {
	s := newTestSupervisor(t)
	dir := s.ConfigDir
	initRepo(t, dir)
	if err := ConfigureCommitPolicy(dir, testIdentity, s.Tmux); err != nil {
		t.Fatalf("configure: %v", err)
	}

	commit(t, dir, "autonomous.txt")
	if got := trailer(t, dir); got != OriginAutonomous {
		t.Fatalf("origin trailer = %q, want %q before any handover", got, OriginAutonomous)
	}

	stop := attachClient(t, s.Tmux, false)
	defer stop()
	waitForClients(t, s.Tmux, 1)

	commit(t, dir, "manual.txt")
	if got := trailer(t, dir); got != OriginManual {
		t.Errorf("origin trailer = %q, want %q while a writable client is attached (20.3)", got, OriginManual)
	}

	// The whole point of the trailer is that history can be partitioned without
	// reading commit messages by eye.
	autonomous := git(t, dir, "log", "--format=%s", "--grep", "^Devplatform-Origin: "+OriginAutonomous)
	if !strings.Contains(autonomous, "autonomous.txt") || strings.Contains(autonomous, "manual.txt") {
		t.Errorf("grep for autonomous commits returned %q", autonomous)
	}
}

func TestReadOnlyClientDoesNotCountAsManualIntervention(t *testing.T) {
	s := newTestSupervisor(t)
	dir := s.ConfigDir
	initRepo(t, dir)
	if err := ConfigureCommitPolicy(dir, testIdentity, s.Tmux); err != nil {
		t.Fatalf("configure: %v", err)
	}

	stop := attachClient(t, s.Tmux, true)
	defer stop()
	waitForClients(t, s.Tmux, 1)

	commit(t, dir, "watched.txt")
	if got := trailer(t, dir); got != OriginAutonomous {
		t.Errorf("origin trailer = %q, want %q for a read-only observer", got, OriginAutonomous)
	}
}

func TestTrailerIsNotDuplicatedOnAmend(t *testing.T) {
	dir := t.TempDir()
	initRepo(t, dir)
	tm := &Tmux{Socket: "devplatform-absent-" + filepath.Base(dir), Session: "workspace"}
	if err := ConfigureCommitPolicy(dir, testIdentity, tm); err != nil {
		t.Fatalf("configure: %v", err)
	}
	commit(t, dir, "a.txt")
	git(t, dir, "commit", "-q", "--amend", "--no-edit")

	body := git(t, dir, "log", "-1", "--format=%B")
	if n := strings.Count(body, "Devplatform-Origin:"); n != 1 {
		t.Errorf("trailer appears %d times after amend, want 1:\n%s", n, body)
	}
}

// newPushRepo returns a working clone whose origin is a bare repo, with the
// commit policy already applied.
func newPushRepo(t *testing.T) (work string) {
	t.Helper()
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	work = filepath.Join(root, "work")
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	git(t, root, "init", "-q", "--bare", "-b", "work", remote)
	if err := os.Mkdir(work, 0o700); err != nil {
		t.Fatal(err)
	}
	initRepo(t, work)
	git(t, work, "remote", "add", "origin", remote)
	tm := &Tmux{Socket: "devplatform-absent-" + filepath.Base(root), Session: "workspace"}
	if err := ConfigureCommitPolicy(work, testIdentity, tm); err != nil {
		t.Fatalf("configure: %v", err)
	}
	return work
}

func TestPushRejectsForcedNonFastForward(t *testing.T) {
	work := newPushRepo(t)
	commit(t, work, "a.txt")
	git(t, work, "push", "-q", "origin", "work")

	commit(t, work, "b.txt")
	git(t, work, "push", "-q", "origin", "work")

	// Discard the pushed commit and build a divergent history.
	git(t, work, "reset", "-q", "--hard", "HEAD~1")
	commit(t, work, "c.txt")

	// --force is what git would otherwise honour, so the refusal has to be
	// attributable to the hook rather than to git's own default check.
	out := gitFails(t, work, "push", "--force", "origin", "work")
	if !strings.Contains(out, "devplatform: refusing non-fast-forward") {
		t.Errorf("force push output = %q, want the policy's refusal (20.4)", out)
	}
}

func TestPushAllowsFastForward(t *testing.T) {
	work := newPushRepo(t)
	commit(t, work, "a.txt")
	git(t, work, "push", "-q", "origin", "work")
	commit(t, work, "b.txt")
	git(t, work, "push", "-q", "origin", "work")
}

func TestPushRejectsBranchDeletion(t *testing.T) {
	work := newPushRepo(t)
	commit(t, work, "a.txt")
	git(t, work, "push", "-q", "origin", "work")

	out := gitFails(t, work, "push", "origin", "--delete", "work")
	if !strings.Contains(out, "refusing to delete") {
		t.Errorf("delete push output = %q, want the policy's refusal", out)
	}
}

func TestConfigureCommitPolicyRefusesToClobberForeignHooks(t *testing.T) {
	dir := t.TempDir()
	initRepo(t, dir)
	hook := filepath.Join(dir, ".git", "hooks", "pre-push")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	tm := &Tmux{Socket: "devplatform-absent-" + filepath.Base(dir), Session: "workspace"}
	if err := ConfigureCommitPolicy(dir, testIdentity, tm); err == nil {
		t.Error("configure must fail loudly rather than silently leave force pushes unguarded")
	}
}

func TestConfigureCommitPolicyIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	initRepo(t, dir)
	tm := &Tmux{Socket: "devplatform-absent-" + filepath.Base(dir), Session: "workspace"}
	for i := range 2 {
		if err := ConfigureCommitPolicy(dir, testIdentity, tm); err != nil {
			t.Fatalf("configure attempt %d: %v", i, err)
		}
	}
}

func TestConfigureCommitPolicyHonoursCoreHooksPath(t *testing.T) {
	dir := t.TempDir()
	initRepo(t, dir)
	hooks := filepath.Join(dir, "custom-hooks")
	if err := os.Mkdir(hooks, 0o700); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "config", "core.hooksPath", hooks)
	tm := &Tmux{Socket: "devplatform-absent-" + filepath.Base(dir), Session: "workspace"}
	if err := ConfigureCommitPolicy(dir, testIdentity, tm); err != nil {
		t.Fatalf("configure: %v", err)
	}
	if _, err := os.Stat(filepath.Join(hooks, "pre-push")); err != nil {
		t.Errorf("hook not installed where git will look for it: %v", err)
	}
}
