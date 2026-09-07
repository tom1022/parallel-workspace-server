package testrun

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tom1022/gitops-apps/apps/devplatform/supervisor/internal/repocfg"
)

type fakeSession struct {
	prompts []string
	err     error
}

func (s *fakeSession) request(_ context.Context, prompt string) error {
	if s.err != nil {
		return s.err
	}
	s.prompts = append(s.prompts, prompt)
	return nil
}

type note struct {
	kind   string
	detail string
}

func newLoop(t *testing.T, cfg repocfg.Config) (*Loop, *fakeProc, *fakeSession, *[]note) {
	t.Helper()
	r, f := newRunner(t, cfg)
	sess := &fakeSession{}
	notes := &[]note{}
	l := &Loop{
		Runner:  r,
		Request: sess.request,
		Notify:  func(kind, detail string) { *notes = append(*notes, note{kind, detail}) },
		Poll:    time.Millisecond,
	}
	return l, f, sess, notes
}

func TestHealStopsAtTheFirstPassingRun(t *testing.T) {
	l, _, sess, notes := newLoop(t, declared)

	res, err := l.Heal(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Passed {
		t.Errorf("res = %+v, want a passing run", res)
	}
	if len(sess.prompts) != 0 {
		t.Errorf("prompts = %v, want none when nothing failed", sess.prompts)
	}
	if len(*notes) != 1 || (*notes)[0].kind != EventTestsVerified {
		t.Errorf("notes = %+v, want one %s", *notes, EventTestsVerified)
	}
}

func TestHealIsBoundedWhenTestsKeepFailing(t *testing.T) {
	// 9.6: a test that never passes — the case a misclassification produces —
	// must still end the loop.
	l, f, sess, notes := newLoop(t, declared)
	f.fail["unit"] = true
	f.fail["e2e"] = true

	done := make(chan struct{})
	var res Result
	var err error
	go func() {
		res, err = l.Heal(context.Background(), "s1")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the healing loop did not terminate on a test that never passes")
	}
	if err != nil {
		t.Fatal(err)
	}

	if res.Passed {
		t.Error("an exhausted loop must not report a passing run")
	}
	if len(sess.prompts) != MaxHealingAttempts {
		t.Errorf("fix requests = %d, want %d", len(sess.prompts), MaxHealingAttempts)
	}
	if res.Attempt != MaxHealingAttempts+1 {
		t.Errorf("final attempt = %d, want %d", res.Attempt, MaxHealingAttempts+1)
	}
	if len(*notes) != 1 || (*notes)[0].kind != EventSelfHealingExhausted {
		t.Errorf("notes = %+v, want one %s", *notes, EventSelfHealingExhausted)
	}
}

func TestHealReRunsOnlyTheFailedTests(t *testing.T) {
	l, f, _, _ := newLoop(t, declared)
	f.fail["e2e"] = true

	if _, err := l.Heal(context.Background(), "s1"); err != nil {
		t.Fatal(err)
	}
	// The first attempt runs both suites; every later one narrows the E2E
	// suite to what failed and skips the unit suite (9.5).
	first := f.argv[:2]
	if len(first) != 2 || first[0][0] != "unit" || first[1][0] != "e2e" {
		t.Fatalf("first attempt = %v, want the whole declaration", first)
	}
	rerun := f.argv[2]
	if rerun[0] != "e2e" || !contains(rerun, "--grep") {
		t.Errorf("re-run = %v, want the filtered E2E command", rerun)
	}
	for _, argv := range f.argv[2:] {
		if argv[0] == "unit" {
			t.Errorf("a narrowed re-run must not re-run the unit suite, got %v", argv)
		}
	}
}

func TestHealLeavesTheClassifiedEvidenceWhereTheRequestPointsIt(t *testing.T) {
	// 9.4: the request carries the classification and the evidence. The
	// session's input is pasted line by line, so the prompt has to stay one
	// line and reference a file rather than inline a stack trace.
	l, f, sess, _ := newLoop(t, declared)
	f.fail["unit"] = true

	if _, err := l.Heal(context.Background(), "s1"); err != nil {
		t.Fatal(err)
	}
	if len(sess.prompts) == 0 {
		t.Fatal("a failing run must ask the session for a fix")
	}
	prompt := sess.prompts[0]
	if strings.Contains(prompt, "\n") {
		t.Errorf("prompt = %q, want a single line", prompt)
	}
	path := filepath.Join(l.Runner.Dir("s1-1"), evidenceFile)
	if !strings.Contains(prompt, path) {
		t.Errorf("prompt = %q, want it to point at %s", prompt, path)
	}
	var failures []Failure
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &failures); err != nil {
		t.Fatal(err)
	}
	if len(failures) == 0 || failures[0].Category == "" {
		t.Errorf("evidence = %+v, want classified failures", failures)
	}
}

func TestHealGivesEachAttemptItsOwnRunId(t *testing.T) {
	// 9.8: one run never overwrites another's report, so the attempts of a
	// healing session cannot share a key.
	l, f, _, _ := newLoop(t, declared)
	f.fail["unit"] = true

	if _, err := l.Heal(context.Background(), "s1"); err != nil {
		t.Fatal(err)
	}
	for n := 1; n <= MaxHealingAttempts+1; n++ {
		dir := l.Runner.Dir(runId("s1", n))
		if _, err := os.Stat(filepath.Join(dir, resultFile)); err != nil {
			t.Errorf("attempt %d left no result: %v", n, err)
		}
	}
}

func TestHealSurvivesAFailedPublication(t *testing.T) {
	// Losing a report must not stop the loop that produces the fix.
	l, _, _, notes := newLoop(t, declared)
	l.Publisher = &Publisher{} // unconfigured: every publish fails

	res, err := l.Heal(context.Background(), "s1")
	if err != nil {
		t.Fatalf("a failed publication must not fail the run: %v", err)
	}
	if !res.Passed {
		t.Error("a failed publication must not fail the run")
	}
	if len(*notes) != 1 || (*notes)[0].kind != EventTestsVerified {
		t.Errorf("notes = %+v, want the run still reported verified", *notes)
	}
}

func TestHealReportsTheFixRequestBeingRefused(t *testing.T) {
	// SendInput refuses while a developer holds the session writable (3.5).
	l, f, sess, notes := newLoop(t, declared)
	f.fail["unit"] = true
	sess.err = errors.New("a writable client holds the session")

	if _, err := l.Heal(context.Background(), "s1"); err == nil {
		t.Error("a refused fix request must surface")
	}
	if len(*notes) != 0 {
		t.Errorf("notes = %+v, want no verdict when the loop never got to run", *notes)
	}
}

func TestWatchHealsOncePerCompletedTurnAndNotOnItsOwn(t *testing.T) {
	// The fix requests the loop issues complete turns of their own. Without
	// re-baselining afterwards the loop would read those as new work and run
	// forever.
	l, f, _, _ := newLoop(t, declared)
	f.fail["unit"] = true

	turns := make(chan string, 8)
	completed := "t1"
	l.Turn = func() (bool, string, error) {
		select {
		case next := <-turns:
			completed = next
		default:
		}
		return false, completed, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go l.Watch(ctx, func(err error) { t.Error(err) })

	// The first observation is only a baseline; a session resuming with a
	// completed turn already in its transcript must not trigger a run.
	time.Sleep(50 * time.Millisecond)
	if attempts(l) != 0 {
		t.Fatalf("the baseline observation triggered %d attempts", attempts(l))
	}

	turns <- "t2"
	deadline := time.After(10 * time.Second)
	for attempts(l) < MaxHealingAttempts+1 {
		select {
		case <-deadline:
			t.Fatal("a completed turn did not trigger a healing session")
		case <-time.After(5 * time.Millisecond):
		}
	}

	// Nothing beyond the exhausted session: the fix requests it just issued
	// completed turns of their own, and those must not start another.
	time.Sleep(100 * time.Millisecond)
	if n := attempts(l); n != MaxHealingAttempts+1 {
		t.Errorf("attempts = %d, want the loop to stop at %d", n, MaxHealingAttempts+1)
	}
}

func contains(argv []string, want string) bool {
	for _, a := range argv {
		if a == want {
			return true
		}
	}
	return false
}

// attempts counts the run directories a healing session has left behind. It
// is read from disk rather than from the fake, which the healing goroutine
// writes to without synchronisation.
func attempts(l *Loop) int {
	entries, err := os.ReadDir(l.Runner.ArtifactRoot)
	if err != nil {
		return 0
	}
	return len(entries)
}

func TestHealDoesNotNarrowAwayAFailingUnitSuite(t *testing.T) {
	// A narrowed run skips the unit suite, so narrowing after a unit failure
	// would pass without re-running what failed and report the task verified.
	l, f, _, notes := newLoop(t, declared)
	f.fail["unit"] = true

	res, err := l.Heal(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	if res.Passed {
		t.Error("a run that never re-ran the failing suite must not pass")
	}
	if len(*notes) != 1 || (*notes)[0].kind != EventSelfHealingExhausted {
		t.Errorf("notes = %+v, want %s", *notes, EventSelfHealingExhausted)
	}
	for _, argv := range f.argv[2:] {
		if argv[0] == "unit" {
			return
		}
	}
	t.Error("later attempts never re-ran the unit suite")
}
