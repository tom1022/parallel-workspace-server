package testrun

import (
	"io/fs"
	"path/filepath"
	"regexp"
	"strings"
)

// Lines that name a failing test. Runners disagree on everything else about
// their output, but each marks the start of a failure report, and the lines
// between two markers are that failure's evidence.
var (
	numberedFailure = regexp.MustCompile(`^\s*\d+\)\s+(\S.*)$`)
	bulletFailure   = regexp.MustCompile(`^\s*[●✕✖×✗]\s+(\S.*)$`)
	goFailure       = regexp.MustCompile(`^\s*--- FAIL:\s+(\S+)`)

	stackFrame     = regexp.MustCompile(`^(at\s+\S|\S+\.\w+:\d+:\d+\s*$)`)
	retryBanner    = regexp.MustCompile(`^Retry #\d+`)
	attachmentHead = regexp.MustCompile(`^attachment #\d+:`)
	imagePath      = regexp.MustCompile(`^\S+\.(?i:png|jpe?g|webp)$`)
	separator      = regexp.MustCompile(`^[─—\-=*_]{3,}$`)
	// Timing suffixes runners append to a test title, e.g. "(312ms)".
	titleDuration = regexp.MustCompile(`\s+\(\d+(\.\d+)?\s*m?s\)$`)
	// A segment that locates the test rather than naming it.
	locationSegment = regexp.MustCompile(`^\[.*\]$|:\d+:\d+$`)

	nonAlnum = regexp.MustCompile(`[^a-z0-9]+`)
)

// Substrings that place the blame outside the code under test. Matched
// case-insensitively against the collected evidence.
var environmentSignals = []string{
	"executable file not found",
	"command not found",
	"econnrefused",
	"enotfound",
	"eaddrinuse",
	"enospc",
	"connection refused",
	"no such host",
	"no space left on device",
	"permission denied",
	"cannot find module",
	"out of memory",
	"browsertype.launch",
	"failed to launch",
}

// maxMessageBytes caps one failure's message. The whole point of collecting
// evidence is to hand it to the session, and a suite that dumps a megabyte of
// output would otherwise take the context window with it. The tail is kept
// because that is where a runner puts what went wrong.
const maxMessageBytes = 4000

// collectFailures turns the raw output of the failed steps into structured
// evidence (9.2) with a category attached to each (9.3).
func (r *Runner) collectFailures(steps []Step) []Failure {
	out := []Failure{}
	for _, s := range steps {
		if !s.Failed {
			continue
		}
		parsed := parseFailures(s.Output)
		if len(parsed) == 0 {
			// The suite never reached a test — a missing runner, a build
			// error. The step itself is the failure, and the output is all
			// the evidence there is.
			f := Failure{TestName: s.Name, Message: tail(strings.TrimSpace(s.Output))}
			f.Category = classify(f, false)
			parsed = []Failure{f}
		}
		out = append(out, parsed...)
	}
	return r.attachScreenshots(merge(out))
}

// parseFailures splits output into one report per marker line and reads the
// evidence out of each.
func parseFailures(output string) []Failure {
	lines := strings.Split(output, "\n")
	var found []Failure
	name, start := "", -1
	flush := func(end int) {
		if start < 0 {
			return
		}
		found = append(found, readBody(name, lines[start:end]))
	}
	for i, line := range lines {
		title, ok := failureTitle(line)
		if !ok {
			continue
		}
		flush(i)
		name, start = title, i+1
	}
	flush(len(lines))
	return found
}

func failureTitle(line string) (string, bool) {
	if m := goFailure.FindStringSubmatch(line); m != nil {
		return m[1], true
	}
	for _, re := range []*regexp.Regexp{numberedFailure, bulletFailure} {
		if m := re.FindStringSubmatch(line); m != nil {
			return testTitle(m[1]), true
		}
	}
	return "", false
}

// testTitle strips the decoration runners wrap a title in, keeping the part
// that identifies the test to a human and to a --grep filter: the browser
// project and the source location go, the suite path stays.
func testTitle(raw string) string {
	raw = strings.TrimRight(raw, " \t─—-=")
	raw = titleDuration.ReplaceAllString(raw, "")
	segments := strings.Split(raw, " › ")
	kept := segments[:0]
	for _, s := range segments {
		if s = strings.TrimSpace(s); s != "" && !locationSegment.MatchString(s) {
			kept = append(kept, s)
		}
	}
	if len(kept) == 0 {
		return strings.TrimSpace(raw)
	}
	return strings.Join(kept, " › ")
}

// readBody separates a failure report into the message, the stack frames and
// the screenshot the runner attached.
func readBody(name string, lines []string) Failure {
	var message, stack []string
	shot := ""
	retried := false
	for _, line := range lines {
		line = strings.TrimSpace(line)
		switch {
		case line == "" && len(message) == 0:
		case separator.MatchString(line):
		case retryBanner.MatchString(line):
			retried = true
		case attachmentHead.MatchString(line):
		case imagePath.MatchString(line):
			shot = line
		case stackFrame.MatchString(line):
			stack = append(stack, line)
		default:
			message = append(message, line)
		}
	}
	f := Failure{
		TestName:      name,
		Message:       tail(strings.TrimSpace(strings.Join(message, "\n"))),
		StackTrace:    strings.Join(stack, "\n"),
		ScreenshotKey: shot,
	}
	f.Category = classify(f, retried)
	return f
}

// classify reads the evidence for who is at fault (9.3).
//
// ponytail: substring matching over unstructured output. design.md sets no
// accuracy bar for this and bounds the cost of getting it wrong at the
// self-healing attempt limit, so the upgrade path — parsing each runner's
// machine-readable reporter — waits until misclassification is shown to be
// burning attempts.
func classify(f Failure, retried bool) FailureCategory {
	evidence := strings.ToLower(f.Message + "\n" + f.StackTrace)
	for _, s := range environmentSignals {
		if strings.Contains(evidence, s) {
			return EnvironmentIssue
		}
	}
	// A test that failed only after the runner re-ran it did not fail
	// reproducibly, which is what distinguishes a weak test from a defect.
	if retried || strings.Contains(evidence, "flaky") {
		return FlakyTest
	}
	return ImplementationDefect
}

// merge folds the several places a runner reports the same test into one
// entry, keeping the richest evidence from each.
//
// ponytail: identity is the trailing name segment, so two tests with the same
// title in different suites collapse into one. Widening the key to the whole
// path would instead double-count the runners that print the listing short and
// the detail fully qualified, which is the more common shape.
func merge(in []Failure) []Failure {
	out := make([]Failure, 0, len(in))
	at := map[string]int{}
	for _, f := range in {
		segments := strings.Split(f.TestName, " › ")
		key := slug(segments[len(segments)-1])
		i, seen := at[key]
		if !seen {
			at[key] = len(out)
			out = append(out, f)
			continue
		}
		out[i] = combine(out[i], f)
	}
	return out
}

func combine(a, b Failure) Failure {
	if len(b.Message) > len(a.Message) {
		a.Message = b.Message
	}
	if len(b.TestName) > len(a.TestName) {
		a.TestName = b.TestName
	}
	if a.StackTrace == "" {
		a.StackTrace = b.StackTrace
	}
	if a.ScreenshotKey == "" {
		a.ScreenshotKey = b.ScreenshotKey
	}
	if a.Category == ImplementationDefect {
		a.Category = b.Category
	}
	return a
}

// attachScreenshots fills in the failures whose report named no attachment by
// looking through the declared report directory, where a browser runner leaves
// the capture under a path derived from the test's name.
func (r *Runner) attachScreenshots(in []Failure) []Failure {
	if r.Config.ReportDir == "" {
		return in
	}
	missing := false
	for _, f := range in {
		missing = missing || f.ScreenshotKey == ""
	}
	if !missing {
		return in
	}
	images := r.imagesUnderReportDir()
	for i, f := range in {
		if f.ScreenshotKey != "" {
			continue
		}
		name := slug(f.TestName)
		for _, img := range images {
			if name != "" && strings.Contains(slug(img), name) {
				in[i].ScreenshotKey = img
				break
			}
		}
	}
	return in
}

// imagesUnderReportDir returns the captures below the declared report
// directory, as working-directory-relative paths so a caller can both read
// them and publish them under a stable key.
func (r *Runner) imagesUnderReportDir() []string {
	root := filepath.Join(r.WorkingDir, r.Config.ReportDir)
	var out []string
	// A missing or unreadable report directory means no screenshots, not a
	// failed collection: the evidence that matters is already in the message.
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !imagePath.MatchString(path) {
			return nil
		}
		if rel, err := filepath.Rel(r.WorkingDir, path); err == nil {
			out = append(out, rel)
		}
		return nil
	})
	return out
}

func slug(s string) string {
	return strings.Trim(nonAlnum.ReplaceAllString(strings.ToLower(s), "-"), "-")
}

func tail(s string) string {
	if len(s) <= maxMessageBytes {
		return s
	}
	return "…" + s[len(s)-maxMessageBytes:]
}
