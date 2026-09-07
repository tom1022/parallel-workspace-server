package testrun

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// MaxHealingAttempts is D-3. The first run is what a completed turn triggers,
// not an attempt at healing, so a session issues at most this many fix
// requests and this many runs beyond the first. The classifier has no accuracy
// floor by design; this bound is what keeps a misclassified failure from
// looping forever (9.6).
const MaxHealingAttempts = 3

// Event kinds reported to Hermes Agent when a healing session ends.
const (
	EventTestsVerified        = "TestsVerified"
	EventSelfHealingExhausted = "SelfHealingExhausted"
)

const (
	// evidenceFile holds the classified failures a fix request points at. The
	// request itself has to be one line — session input is pasted and every
	// newline submits it — so a stack trace cannot travel in the prompt.
	evidenceFile = "failures.json"
	// resultFile is the run's durable record, which is what marks a task
	// verified: notification is best-effort, this is not (9.7).
	resultFile = "result.json"
)

// Loop drives a run and the bounded self-healing that follows it (9.4-9.7).
type Loop struct {
	Runner *Runner
	// Publisher is optional. Publication failing, or not being configured at
	// all, must not stop the loop that produces the fix.
	Publisher *Publisher

	// Turn reports whether a turn is executing and a value that changes each
	// time one completes. Supplied by the caller so this package stays free of
	// the session's vocabulary of turn kinds.
	Turn func() (busy bool, completedAt string, err error)
	// Request delivers a fix request to the session and returns once the turn
	// it starts has completed.
	Request func(ctx context.Context, prompt string) error
	// Notify reports the healing session's verdict to Hermes Agent.
	Notify func(kind, detail string)

	Poll time.Duration
}

// Watch runs a healing session for each completed turn. Nothing here consults
// the attached clients, so the loop runs whether or not a developer is
// connected (9.10).
func (l *Loop) Watch(ctx context.Context, onError func(error)) {
	var seen string
	first := true
	ticker := time.NewTicker(l.Poll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		_, completedAt, err := l.Turn()
		if err != nil {
			onError(err)
			continue
		}
		// The first observation only establishes a baseline: a session
		// resuming with a completed turn already in its transcript must not
		// re-test work that was already verified.
		if first {
			first, seen = false, completedAt
			continue
		}
		if completedAt == "" || completedAt == seen {
			continue
		}
		seen = completedAt

		if _, err := l.Heal(ctx, time.Now().UTC().Format("20060102T150405Z")); err != nil {
			onError(err)
		}
		// The fix requests the session just answered completed turns of their
		// own. Re-baselining is what keeps the loop from reading its own
		// prompts as new implementation work.
		if _, after, err := l.Turn(); err == nil {
			seen = after
		}
	}
}

// Heal runs the declared suites and, while they fail, hands the classified
// evidence to the session and re-runs what failed — at most MaxHealingAttempts
// times. It returns the last run either way; an exhausted loop is a reported
// verdict, not an error.
func (l *Loop) Heal(ctx context.Context, sessionId string) (Result, error) {
	res, err := l.run(ctx, sessionId, 1, nil)
	if err != nil {
		return Result{}, err
	}
	for n := 1; n <= MaxHealingAttempts && !res.Passed; n++ {
		if err := l.Request(ctx, fixPrompt(l.Runner.Dir(runId(sessionId, n)))); err != nil {
			return res, err
		}
		if res, err = l.run(ctx, sessionId, n+1, nextOnly(res)); err != nil {
			return Result{}, err
		}
	}
	l.report(res)
	return res, nil
}

// run executes one attempt under its own run id, so no attempt overwrites the
// report of the one before it (9.8).
func (l *Loop) run(ctx context.Context, sessionId string, attempt int, only []string) (Result, error) {
	id := runId(sessionId, attempt)
	res, err := l.Runner.Execute(ctx, Run{Id: id, Attempt: attempt, OnlyTests: only})
	if err != nil {
		return Result{}, err
	}
	dir := l.Runner.Dir(id)
	writeJSON(filepath.Join(dir, evidenceFile), res.Failures)
	writeJSON(filepath.Join(dir, resultFile), res)
	res.ReportUrl = l.publish(ctx, id, dir)
	return res, nil
}

func (l *Loop) publish(ctx context.Context, id, logDir string) string {
	if l.Publisher == nil {
		return ""
	}
	reportDir := ""
	if l.Runner.Config.ReportDir != "" {
		reportDir = filepath.Join(l.Runner.WorkingDir, l.Runner.Config.ReportDir)
	}
	url, err := l.Publisher.Publish(ctx, id, reportDir, logDir)
	if err != nil {
		log.Printf("publishing the test report failed: %v", err)
		return ""
	}
	return url
}

func (l *Loop) report(res Result) {
	if l.Notify == nil {
		return
	}
	if res.Passed {
		l.Notify(EventTestsVerified, res.ReportUrl)
		return
	}
	l.Notify(EventSelfHealingExhausted, exhaustedDetail(res))
}

// exhaustedDetail names what is still failing and where to read it, which is
// all a chat message can usefully carry (9.6).
func exhaustedDetail(res Result) string {
	names := make([]string, 0, len(res.Failures))
	for _, f := range res.Failures {
		names = append(names, fmt.Sprintf("%s (%s)", f.TestName, f.Category))
	}
	detail := fmt.Sprintf("%d attempts exhausted, still failing: %s", res.Attempt, strings.Join(names, ", "))
	if res.ReportUrl != "" {
		detail += " — " + res.ReportUrl
	}
	return detail
}

func fixPrompt(dir string) string {
	return "The test run failed. Read " + filepath.Join(dir, evidenceFile) +
		" for each failing test, its classified cause and its evidence; fix what it points at and reply when the fix is in place."
}

// nextOnly decides what the next attempt narrows to. A unit failure cannot be
// narrowed — the declaration carries no unit-test filter, so a narrowed run
// skips that suite altogether — and narrowing one anyway would let the attempt
// pass without ever re-running what failed, reporting the task verified on
// evidence it never gathered. Re-running everything costs a suite; getting
// this wrong costs the verification.
func nextOnly(res Result) []string {
	for _, s := range res.Steps {
		if s.Name == StepUnit && s.Failed {
			return nil
		}
	}
	return failedTests(res.Failures)
}

// failedTests is what the next attempt narrows to. Duplicates are dropped
// because a filter is a list of names, not of failures.
func failedTests(failures []Failure) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, f := range failures {
		if f.TestName == "" || seen[f.TestName] {
			continue
		}
		seen[f.TestName] = true
		out = append(out, f.TestName)
	}
	return out
}

func runId(sessionId string, attempt int) string {
	return fmt.Sprintf("%s-%d", sessionId, attempt)
}

// writeJSON is best-effort: these are the durable record and the evidence a
// fix request reads, and neither is worth failing a run that already happened.
func writeJSON(path string, v any) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		log.Printf("encoding %s failed: %v", path, err)
		return
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		log.Printf("writing %s failed: %v", path, err)
	}
}
