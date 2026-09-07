package testrun

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/tom1022/gitops-apps/apps/devplatform/supervisor/internal/evacuation"
)

// capture records every object a publish uploads, keyed by the path the store
// received it under.
type capture struct {
	mu      sync.Mutex
	objects map[string]string
	types   map[string]string
}

func newCapture() (*capture, *httptest.Server) {
	c := &capture{objects: map[string]string{}, types: map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		defer c.mu.Unlock()
		c.objects[strings.TrimPrefix(r.URL.Path, "/reports-bucket/")] = string(body)
		c.types[strings.TrimPrefix(r.URL.Path, "/reports-bucket/")] = r.Header.Get("Content-Type")
	}))
	return c, srv
}

func (c *capture) keys() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.objects))
	for k := range c.objects {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func testPublisher(endpoint string) *Publisher {
	return &Publisher{
		Store: &evacuation.S3{
			Endpoint:  endpoint,
			Bucket:    "reports-bucket",
			Region:    "garage",
			AccessKey: "AKIAIOSFODNN7EXAMPLE",
			SecretKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY",
		},
		WorkspaceId:   "ws-abc",
		PublicBaseURL: "https://reports.example.test",
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPublishUploadsReportTreeAndLogs(t *testing.T) {
	store, srv := newCapture()
	defer srv.Close()

	reportDir := filepath.Join(t.TempDir(), "playwright-report")
	writeFile(t, filepath.Join(reportDir, "index.html"), "<html>report</html>")
	writeFile(t, filepath.Join(reportDir, "data", "trace.zip"), "trace bytes")
	writeFile(t, filepath.Join(reportDir, "data", "shot.png"), "png bytes")

	logDir := t.TempDir()
	writeFile(t, filepath.Join(logDir, "unit.log"), "unit output")
	writeFile(t, filepath.Join(logDir, "e2e.log"), "e2e output")

	url, err := testPublisher(srv.URL).Publish(context.Background(), "run-1", reportDir, logDir)
	if err != nil {
		t.Fatal(err)
	}

	want := []string{
		"reports/ws-abc/run-1/data/shot.png",
		"reports/ws-abc/run-1/data/trace.zip",
		"reports/ws-abc/run-1/index.html",
		"reports/ws-abc/run-1/logs/e2e.log",
		"reports/ws-abc/run-1/logs/unit.log",
	}
	got := store.keys()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("keys\n got: %v\nwant: %v", got, want)
	}
	if body := store.objects["reports/ws-abc/run-1/data/trace.zip"]; body != "trace bytes" {
		t.Errorf("trace body = %q", body)
	}
	if body := store.objects["reports/ws-abc/run-1/logs/e2e.log"]; body != "e2e output" {
		t.Errorf("e2e log body = %q", body)
	}
	if want := "https://reports.example.test/reports/ws-abc/run-1/index.html"; url != want {
		t.Errorf("url = %q, want %q", url, want)
	}
}

// Without text/html the browser downloads the report instead of rendering it,
// which is the whole of 9.9.
func TestPublishSetsBrowsableContentType(t *testing.T) {
	store, srv := newCapture()
	defer srv.Close()

	reportDir := filepath.Join(t.TempDir(), "report")
	writeFile(t, filepath.Join(reportDir, "index.html"), "<html>report</html>")

	if _, err := testPublisher(srv.URL).Publish(context.Background(), "run-1", reportDir, ""); err != nil {
		t.Fatal(err)
	}
	if got := store.types["reports/ws-abc/run-1/index.html"]; !strings.HasPrefix(got, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", got)
	}
}

func TestPublishKeepsRunsApart(t *testing.T) {
	store, srv := newCapture()
	defer srv.Close()

	logDir := t.TempDir()
	writeFile(t, filepath.Join(logDir, "e2e.log"), "first")

	pub := testPublisher(srv.URL)
	if _, err := pub.Publish(context.Background(), "run-1", "", logDir); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(logDir, "e2e.log"), "second")
	if _, err := pub.Publish(context.Background(), "run-2", "", logDir); err != nil {
		t.Fatal(err)
	}

	if store.objects["reports/ws-abc/run-1/logs/e2e.log"] != "first" {
		t.Errorf("run-1 log was overwritten: %q", store.objects["reports/ws-abc/run-1/logs/e2e.log"])
	}
	if store.objects["reports/ws-abc/run-2/logs/e2e.log"] != "second" {
		t.Errorf("run-2 log = %q", store.objects["reports/ws-abc/run-2/logs/e2e.log"])
	}
}

// A repository that declares no report directory still has to leave the run's
// logs somewhere reachable.
func TestPublishWithoutReportDirFallsBackToRunPrefix(t *testing.T) {
	store, srv := newCapture()
	defer srv.Close()

	logDir := t.TempDir()
	writeFile(t, filepath.Join(logDir, "unit.log"), "unit output")

	url, err := testPublisher(srv.URL).Publish(context.Background(), "run-9", "", logDir)
	if err != nil {
		t.Fatal(err)
	}
	if got := store.keys(); strings.Join(got, ",") != "reports/ws-abc/run-9/logs/unit.log" {
		t.Errorf("keys = %v", got)
	}
	if want := "https://reports.example.test/reports/ws-abc/run-9/"; url != want {
		t.Errorf("url = %q, want %q", url, want)
	}
}

// A declared report directory the suite never produced is the normal shape of
// a run that died before writing one; it must not fail the publish.
func TestPublishToleratesMissingDirectories(t *testing.T) {
	_, srv := newCapture()
	defer srv.Close()

	url, err := testPublisher(srv.URL).Publish(context.Background(), "run-3",
		filepath.Join(t.TempDir(), "absent"), filepath.Join(t.TempDir(), "absent"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "https://reports.example.test/reports/ws-abc/run-3/"; url != want {
		t.Errorf("url = %q, want %q", url, want)
	}
}

func TestPublishRejectsIncompleteConfiguration(t *testing.T) {
	_, srv := newCapture()
	defer srv.Close()

	pub := testPublisher(srv.URL)
	pub.WorkspaceId = ""
	if _, err := pub.Publish(context.Background(), "run-1", "", t.TempDir()); err == nil {
		t.Error("publish with no workspace id succeeded")
	}

	pub = testPublisher(srv.URL)
	pub.PublicBaseURL = ""
	if _, err := pub.Publish(context.Background(), "run-1", "", t.TempDir()); err == nil {
		t.Error("publish with no report base url succeeded")
	}

	pub = testPublisher(srv.URL)
	pub.Store.Endpoint = ""
	if _, err := pub.Publish(context.Background(), "run-1", "", t.TempDir()); err == nil {
		t.Error("publish with unconfigured store succeeded")
	}

	pub = testPublisher(srv.URL)
	if _, err := pub.Publish(context.Background(), "", "", t.TempDir()); err == nil {
		t.Error("publish with no run id succeeded")
	}
}
