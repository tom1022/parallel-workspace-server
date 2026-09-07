// Package testrun executes the suites the target repository declares. It is
// driven by task completion and never consults the session or its attached
// clients, so a run is unaffected by whether a developer is connected (9.10).
package testrun

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"github.com/tom1022/gitops-apps/apps/devplatform/supervisor/internal/repocfg"
)

// FailureCategory is what the evidence of a failure is judged to mean (9.3).
type FailureCategory string

const (
	ImplementationDefect FailureCategory = "ImplementationDefect"
	FlakyTest            FailureCategory = "FlakyTest"
	EnvironmentIssue     FailureCategory = "EnvironmentIssue"
)

// Failure is one failing test with the evidence collected for it (9.2).
type Failure struct {
	TestName      string          `json:"testName"`
	Message       string          `json:"message"`
	StackTrace    string          `json:"stackTrace"`
	ScreenshotKey string          `json:"screenshotKey"`
	Category      FailureCategory `json:"category"`
}

// Step names, which are also the basenames of the captured output files.
const (
	StepUnit = "unit"
	StepE2E  = "e2e"
)

// Step is one command the run executed. It carries the raw output because
// attributing a failure to the suite that produced it is what makes the
// evidence classifiable.
type Step struct {
	Name   string
	Argv   []string
	Output string
	Failed bool
}

// Result is the outcome of one run.
type Result struct {
	RunId     string    `json:"runId"`
	Attempt   int       `json:"attempt"`
	Passed    bool      `json:"passed"`
	Failures  []Failure `json:"failures"`
	ReportUrl string    `json:"reportUrl"`

	// Steps is the raw material failures are extracted from, not part of the
	// published result.
	Steps []Step `json:"-"`
}

// Run identifies one execution.
type Run struct {
	Id      string
	Attempt int
	// OnlyTests restricts the E2E suite to the named tests, so verifying a fix
	// costs one test rather than a whole suite (9.5). Empty runs everything.
	OnlyTests []string
}

// Runner executes the declared suites for one working directory.
type Runner struct {
	WorkingDir string
	Config     repocfg.Config
	// ArtifactRoot holds one directory per run id.
	ArtifactRoot string

	// Exec runs a command to completion and returns its combined output. Nil
	// starts a real process; tests substitute it.
	Exec func(ctx context.Context, argv []string) (string, error)
	// Serve starts the application the E2E suite drives and returns the call
	// that stops it. Nil starts a real process.
	Serve func(ctx context.Context, argv []string) (func(), error)
}

// Dir is where a run's captured output lands. It is derived from the run id
// alone, so re-running an id replaces an interrupted run's artifacts rather
// than accumulating beside them.
func (r *Runner) Dir(runId string) string {
	return filepath.Join(r.ArtifactRoot, runId)
}

// Execute runs the unit suite, then the E2E suite with the application up,
// and collects the evidence for whatever failed.
//
// A suite that fails is a failed step, not an error: the error return is
// reserved for the run being unable to happen at all. A command that does not
// exist therefore also lands as a failed step, which is the evidence the
// classifier needs to call it an environment problem rather than a defect.
func (r *Runner) Execute(ctx context.Context, run Run) (Result, error) {
	res, err := r.execute(ctx, run)
	if err != nil {
		return Result{}, err
	}
	res.Failures = r.collectFailures(res.Steps)
	return res, nil
}

func (r *Runner) execute(ctx context.Context, run Run) (Result, error) {
	if run.Id == "" {
		return Result{}, fmt.Errorf("testrun: run id is required")
	}
	dir := r.Dir(run.Id)
	if err := os.RemoveAll(dir); err != nil {
		return Result{}, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Result{}, err
	}

	res := Result{RunId: run.Id, Attempt: run.Attempt, Passed: true, Failures: []Failure{}}

	rerun := len(run.OnlyTests) > 0
	// ponytail: a filtered re-run narrows the E2E suite only — the declaration
	// has no unit-test filter — so it skips the unit suite entirely rather
	// than re-running all of it. Add a unitTestFilter to repocfg.Config if a
	// unit failure ever has to be re-verified on its own.
	if len(r.Config.UnitTest) > 0 && !rerun {
		if err := r.step(ctx, dir, StepUnit, r.Config.UnitTest, &res); err != nil {
			return Result{}, err
		}
	}

	e2e := r.e2eArgv(run.OnlyTests)
	if len(e2e) == 0 {
		return res, nil
	}
	if len(r.Config.App) > 0 {
		// ponytail: started and handed straight to the suite with no readiness
		// probe. A repository whose app is slow to bind should declare no app
		// and let its own E2E config start and await one; add a readiness URL
		// to the declaration if that stops being enough.
		stop, err := r.serve(ctx, r.Config.App)
		if err != nil {
			return Result{}, err
		}
		defer stop()
	}
	if err := r.step(ctx, dir, StepE2E, e2e, &res); err != nil {
		return Result{}, err
	}
	return res, nil
}

// e2eArgv builds the E2E command. A narrowed re-run that the repository gave
// no way to express falls back to the whole suite: running more than asked
// still verifies the fix, whereas running nothing verifies nothing.
func (r *Runner) e2eArgv(only []string) []string {
	if len(only) == 0 || len(r.Config.E2ETestFilter) == 0 {
		return r.Config.E2ETest
	}
	return append(append([]string{}, r.Config.E2ETestFilter...), only...)
}

func (r *Runner) step(ctx context.Context, dir, name string, argv []string, res *Result) error {
	out, err := r.exec(ctx, argv)
	res.Steps = append(res.Steps, Step{Name: name, Argv: argv, Output: out, Failed: err != nil})
	if err != nil {
		res.Passed = false
	}
	return os.WriteFile(filepath.Join(dir, name+".log"), []byte(out), 0o644)
}

func (r *Runner) exec(ctx context.Context, argv []string) (string, error) {
	if r.Exec != nil {
		return r.Exec(ctx, argv)
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = r.WorkingDir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (r *Runner) serve(ctx context.Context, argv []string) (func(), error) {
	if r.Serve != nil {
		return r.Serve(ctx, argv)
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = r.WorkingDir
	// Own process group: a dev server started through a package-manager script
	// is a child of that script, and signalling only the direct child would
	// leave the server holding its port for the next run.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		_ = cmd.Wait()
	}, nil
}
