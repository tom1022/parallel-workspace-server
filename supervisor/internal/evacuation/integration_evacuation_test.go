package evacuation

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestIntegration_SettledEvacuationRebuildsTheWorkingDirectoryAfterNodeLoss
// joins the two halves that the per-stage tests exercise separately: the
// capture the control plane asks for before it stops a workspace (16.6/16.8),
// and the reconstruction on a replacement node once the original node's disk
// is gone for good (16.9). Nothing but the object store survives in between.
func TestIntegration_SettledEvacuationRebuildsTheWorkingDirectoryAfterNodeLoss(t *testing.T) {
	workingDir, remote := newRepo(t)
	write(t, workingDir, "feature.go", "package feature\n")
	git(t, workingDir, "add", ".")
	git(t, workingDir, "commit", "-m", "unpushed work")
	head := git(t, workingDir, "rev-parse", "HEAD")
	write(t, workingDir, "README.md", "half-finished\n")
	write(t, workingDir, "notes/scratch.txt", "not yet tracked\n")

	store, _ := newFakeStore(t)
	polls := 0
	trig := &Trigger{
		Agent: &Agent{WorkingDir: workingDir, WorkspaceId: "ws-node-loss", Store: store},
		// The turn is still writing on the first polls, so the capture below
		// is the settled tree rather than a half-written one.
		Turn: func() (bool, string, error) { polls++; return polls < 3, "", nil },
		Poll: time.Millisecond,
		Wait: 2 * time.Second,
	}

	snap, err := trig.EvacuateWhenSettled(context.Background())
	if err != nil {
		t.Fatalf("EvacuateWhenSettled: %v", err)
	}
	if snap.HeadCommit != head {
		t.Errorf("snapshot head = %s, want %s", snap.HeadCommit, head)
	}
	if snap.BundleKey != BundleKey("ws-node-loss") || snap.DirtyArchiveKey != DirtyArchiveKey("ws-node-loss") {
		t.Errorf("snapshot keys = %s / %s, want the keys restore reads", snap.BundleKey, snap.DirtyArchiveKey)
	}

	if err := os.RemoveAll(workingDir); err != nil {
		t.Fatalf("simulate node loss: %v", err)
	}

	replacement := freshClone(t, remote)
	if err := (&Agent{WorkingDir: replacement, WorkspaceId: "ws-node-loss", Store: store}).Restore(context.Background()); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	if got := git(t, replacement, "rev-parse", "HEAD"); got != head {
		t.Errorf("HEAD = %s, want the evacuated commit %s", got, head)
	}
	for name, want := range map[string]string{
		"feature.go":        "package feature\n",
		"README.md":         "half-finished\n",
		"notes/scratch.txt": "not yet tracked\n",
	} {
		body, err := os.ReadFile(filepath.Join(replacement, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if string(body) != want {
			t.Errorf("%s = %q, want %q", name, body, want)
		}
	}
}

// An unreachable destination has to surface as a failure, because that is the
// only thing standing between the control plane and a stop that loses the
// uncommitted work (16.8).
func TestIntegration_SettledEvacuationFailsWhileTheDestinationIsUnavailable(t *testing.T) {
	workingDir, _ := newRepo(t)
	write(t, workingDir, "README.md", "half-finished\n")

	trig := &Trigger{
		Agent: &Agent{
			WorkingDir:  workingDir,
			WorkspaceId: "ws-store-down",
			Store:       &S3{Endpoint: "http://127.0.0.1:1", Bucket: "workspace", Region: "garage", AccessKey: "k", SecretKey: "s"},
		},
		Turn: func() (bool, string, error) { return false, "", nil },
		Poll: time.Millisecond,
		Wait: 200 * time.Millisecond,
	}

	if _, err := trig.EvacuateWhenSettled(context.Background()); err == nil {
		t.Fatal("expected an error while the evacuation destination is unavailable")
	}
}
