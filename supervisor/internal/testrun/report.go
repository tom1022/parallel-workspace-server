package testrun

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/tom1022/gitops-apps/apps/devplatform/supervisor/internal/evacuation"
)

// reportRoot is the bucket-relative root every run publishes under. The report
// hostname serves the bucket root, so a key is also the URL path.
const reportRoot = "reports"

// reportEntrypoint is the file a browser is sent to when the suite produced an
// HTML report.
const reportEntrypoint = "index.html"

// Publisher copies a finished run's report and logs into the object store and
// returns where a browser can read them (9.8, 9.9).
type Publisher struct {
	Store       *evacuation.S3
	WorkspaceId string
	// PublicBaseURL is the origin the report hostname resolves to, without a
	// trailing path.
	PublicBaseURL string
}

// Publish uploads reportDir and logDir under a key derived from the run id, so
// runs never land on each other and re-running an id replaces only its own
// artifacts. Either directory may be absent — a suite that died before writing
// a report still has to leave its logs reachable.
func (p *Publisher) Publish(ctx context.Context, runId, reportDir, logDir string) (string, error) {
	switch {
	case runId == "":
		return "", fmt.Errorf("testrun: run id is required")
	case p.WorkspaceId == "":
		return "", fmt.Errorf("testrun: workspace id is not configured")
	case p.PublicBaseURL == "":
		return "", fmt.Errorf("testrun: report base url is not configured")
	case p.Store == nil:
		return "", fmt.Errorf("testrun: report destination is not configured")
	}
	if err := p.Store.Validate(); err != nil {
		return "", err
	}

	prefix := path.Join(reportRoot, p.WorkspaceId, runId) + "/"
	if err := p.upload(ctx, reportDir, prefix); err != nil {
		return "", err
	}
	if err := p.upload(ctx, logDir, prefix+"logs/"); err != nil {
		return "", err
	}

	base := strings.TrimSuffix(p.PublicBaseURL, "/") + "/" + prefix
	if _, err := os.Stat(filepath.Join(reportDir, reportEntrypoint)); reportDir != "" && err == nil {
		return base + reportEntrypoint, nil
	}
	return base, nil
}

// upload walks dir and puts every regular file under it at prefix, preserving
// the relative layout so an HTML report's own links keep resolving.
func (p *Publisher) upload(ctx context.Context, dir, prefix string) error {
	if dir == "" {
		return nil
	}
	if _, err := os.Stat(dir); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return filepath.WalkDir(dir, func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !d.Type().IsRegular() {
			return err
		}
		rel, err := filepath.Rel(dir, name)
		if err != nil {
			return err
		}
		_, err = p.Store.PutFile(ctx, prefix+filepath.ToSlash(rel), name)
		return err
	})
}
