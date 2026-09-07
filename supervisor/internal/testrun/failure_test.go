package testrun

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tom1022/gitops-apps/apps/devplatform/supervisor/internal/repocfg"
)

const playwrightOutput = `
Running 3 tests using 1 worker

  ✓  1 tests/home.spec.ts:4:1 › home renders (312ms)

  1) [chromium] › tests/checkout.spec.ts:12:5 › checkout completes an order ──────────

    Error: expect(received).toBe(expected)

    Expected: "confirmed"
    Received: "pending"

      at tests/checkout.spec.ts:18:32
      at runTest (node_modules/@playwright/test/lib/worker.js:120:9)

    attachment #1: screenshot (image/png)
    test-results/checkout-completes-an-order/test-failed-1.png

  2) [chromium] › tests/login.spec.ts:7:5 › login rejects a bad password ─────────────

    Retry #1 ──────────────────────────────────────────────────────────────────────

    Error: Timed out 5000ms waiting for expect(locator).toBeVisible()

      at tests/login.spec.ts:9:20

  1 failed
  1 flaky
`

const jestOutput = `
 FAIL  src/cart.test.ts
  Cart
    ✕ applies a coupon (5 ms)

  ● Cart › applies a coupon

    expect(received).toEqual(expected)

      at Object.<anonymous> (src/cart.test.ts:22:18)
`

const goOutput = `
--- FAIL: TestTotalAppliesDiscount (0.00s)
    cart_test.go:31: total = 12, want 10
FAIL
`

func findFailure(t *testing.T, fs []Failure, name string) Failure {
	t.Helper()
	for _, f := range fs {
		if f.TestName == name {
			return f
		}
	}
	t.Fatalf("no failure named %q in %+v", name, fs)
	return Failure{}
}

func TestCollectFailuresParsesPlaywrightEvidence(t *testing.T) {
	r := &Runner{WorkingDir: t.TempDir()}
	got := r.collectFailures([]Step{{Name: StepE2E, Output: playwrightOutput, Failed: true}})

	if len(got) != 2 {
		t.Fatalf("collected %d failures, want 2: %+v", len(got), got)
	}
	f := findFailure(t, got, "checkout completes an order")
	if !strings.Contains(f.Message, `Expected: "confirmed"`) {
		t.Errorf("Message = %q, want the whole assertion, not only its first line", f.Message)
	}
	if !strings.Contains(f.StackTrace, "tests/checkout.spec.ts:18:32") {
		t.Errorf("StackTrace = %q, want the frames below the message", f.StackTrace)
	}
	if strings.Contains(f.Message, "at tests/checkout.spec.ts") {
		t.Errorf("Message = %q, want the frames kept out of it", f.Message)
	}
	if f.ScreenshotKey != "test-results/checkout-completes-an-order/test-failed-1.png" {
		t.Errorf("ScreenshotKey = %q, want the attached screenshot", f.ScreenshotKey)
	}
	if f.Category != ImplementationDefect {
		t.Errorf("Category = %q, want a failed assertion read as a defect", f.Category)
	}
}

func TestCollectFailuresClassifiesARetriedTestAsFlaky(t *testing.T) {
	r := &Runner{WorkingDir: t.TempDir()}
	got := r.collectFailures([]Step{{Name: StepE2E, Output: playwrightOutput, Failed: true}})

	f := findFailure(t, got, "login rejects a bad password")
	if f.Category != FlakyTest {
		t.Errorf("Category = %q, want a test that needed a retry read as flaky", f.Category)
	}
	if strings.Contains(f.Message, "Retry #1") {
		t.Errorf("Message = %q, want the retry banner kept out of the evidence", f.Message)
	}
}

func TestCollectFailuresCountsAJestTestOnceUnderItsSuitePath(t *testing.T) {
	r := &Runner{WorkingDir: t.TempDir()}
	got := r.collectFailures([]Step{{Name: StepUnit, Output: jestOutput, Failed: true}})

	if len(got) != 1 {
		t.Fatalf("collected %d failures, want the listing and the detail merged into 1: %+v", len(got), got)
	}
	f := findFailure(t, got, "Cart › applies a coupon")
	if !strings.Contains(f.Message, "expect(received).toEqual(expected)") {
		t.Errorf("Message = %q, want the detail section's evidence", f.Message)
	}
	if !strings.Contains(f.StackTrace, "src/cart.test.ts:22:18") {
		t.Errorf("StackTrace = %q, want the frame", f.StackTrace)
	}
}

func TestCollectFailuresParsesGoTestOutput(t *testing.T) {
	r := &Runner{WorkingDir: t.TempDir()}
	got := r.collectFailures([]Step{{Name: StepUnit, Output: goOutput, Failed: true}})

	f := findFailure(t, got, "TestTotalAppliesDiscount")
	if !strings.Contains(f.Message, "total = 12, want 10") {
		t.Errorf("Message = %q, want the reported difference", f.Message)
	}
}

func TestCollectFailuresSynthesizesOneWhenNoTestIsNamed(t *testing.T) {
	r := &Runner{WorkingDir: t.TempDir()}
	out := `exec: "npx": executable file not found in $PATH`
	got := r.collectFailures([]Step{{Name: StepE2E, Output: out, Failed: true}})

	if len(got) != 1 {
		t.Fatalf("collected %d failures, want the suite itself reported as 1: %+v", len(got), got)
	}
	if got[0].TestName != StepE2E {
		t.Errorf("TestName = %q, want the suite name when no test was reached", got[0].TestName)
	}
	if got[0].Category != EnvironmentIssue {
		t.Errorf("Category = %q, want a missing executable read as an environment issue", got[0].Category)
	}
}

func TestCollectFailuresClassifiesARefusedConnectionAsAnEnvironmentIssue(t *testing.T) {
	r := &Runner{WorkingDir: t.TempDir()}
	out := `
  1) [chromium] › tests/api.spec.ts:3:1 › orders list loads ──────────

    Error: connect ECONNREFUSED 127.0.0.1:5432

      at tests/api.spec.ts:5:10
`
	got := r.collectFailures([]Step{{Name: StepE2E, Output: out, Failed: true}})
	f := findFailure(t, got, "orders list loads")
	if f.Category != EnvironmentIssue {
		t.Errorf("Category = %q, want a refused connection read as an environment issue", f.Category)
	}
}

func TestCollectFailuresIgnoresStepsThatPassed(t *testing.T) {
	r := &Runner{WorkingDir: t.TempDir()}
	got := r.collectFailures([]Step{{Name: StepUnit, Output: playwrightOutput, Failed: false}})

	if len(got) != 0 {
		t.Errorf("collected %+v from a passing step, want nothing", got)
	}
	if got == nil {
		t.Error("Failures must be an empty slice, not nil")
	}
}

func TestCollectFailuresFindsAScreenshotUnderTheReportDir(t *testing.T) {
	dir := t.TempDir()
	shot := filepath.Join(dir, "playwright-report", "data", "tests-login-login-rejects-a-bad-password-chromium", "test-failed-1.png")
	if err := os.MkdirAll(filepath.Dir(shot), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(shot, []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := &Runner{WorkingDir: dir, Config: repocfg.Config{ReportDir: "playwright-report"}}

	got := r.collectFailures([]Step{{Name: StepE2E, Output: playwrightOutput, Failed: true}})
	f := findFailure(t, got, "login rejects a bad password")
	want := filepath.Join("playwright-report", "data", "tests-login-login-rejects-a-bad-password-chromium", "test-failed-1.png")
	if f.ScreenshotKey != want {
		t.Errorf("ScreenshotKey = %q, want %q", f.ScreenshotKey, want)
	}
}

func TestExecuteFillsFailuresFromTheFailedSuite(t *testing.T) {
	r := &Runner{
		WorkingDir:   t.TempDir(),
		ArtifactRoot: t.TempDir(),
		Config:       repocfg.Config{E2ETest: []string{"e2e"}},
		Exec: func(context.Context, []string) (string, error) {
			return playwrightOutput, errors.New("exit status 1")
		},
	}

	res, err := r.Execute(context.Background(), Run{Id: "run-1"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Passed {
		t.Fatal("a failing suite must not report a passing run")
	}
	if len(res.Failures) != 2 {
		t.Fatalf("Failures = %+v, want the run's failures collected", res.Failures)
	}
}
