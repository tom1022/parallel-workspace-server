package repocfg

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func write(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(Path)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, Path), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestLoadMissingDeclarationIsNotAnError(t *testing.T) {
	cfg, err := Load(t.TempDir())
	if err != nil {
		t.Fatalf("a repository without a declaration must load cleanly: %v", err)
	}
	if !reflect.DeepEqual(cfg, Config{}) {
		t.Errorf("cfg = %+v, want the zero config", cfg)
	}
}

func TestLoadReadsDeclaredCommands(t *testing.T) {
	dir := write(t, `{
	  "migrate": ["npm", "run", "db:migrate"],
	  "seed": ["npm", "run", "db:seed"],
	  "app": ["npm", "run", "start"],
	  "unitTest": ["npm", "test"],
	  "e2eTest": ["npx", "playwright", "test"],
	  "e2eTestFilter": ["npx", "playwright", "test", "--grep"],
	  "reportDir": "playwright-report"
	}`)

	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		Migrate:       []string{"npm", "run", "db:migrate"},
		Seed:          []string{"npm", "run", "db:seed"},
		App:           []string{"npm", "run", "start"},
		UnitTest:      []string{"npm", "test"},
		E2ETest:       []string{"npx", "playwright", "test"},
		E2ETestFilter: []string{"npx", "playwright", "test", "--grep"},
		ReportDir:     "playwright-report",
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("cfg = %+v, want %+v", cfg, want)
	}
}

func TestLoadAcceptsAPartialDeclaration(t *testing.T) {
	cfg, err := Load(write(t, `{"unitTest": ["go", "test", "./..."]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.E2ETest) != 0 || len(cfg.Migrate) != 0 {
		t.Errorf("undeclared fields must stay empty: %+v", cfg)
	}
}

func TestLoadRejectsMalformedJSON(t *testing.T) {
	if _, err := Load(write(t, `{"unitTest": [`)); err == nil {
		t.Fatal("a malformed declaration must surface as an error, not as an empty config")
	}
}
