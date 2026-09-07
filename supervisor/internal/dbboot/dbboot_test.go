package dbboot

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tom1022/gitops-apps/apps/devplatform/supervisor/internal/repocfg"
)

type recorder struct {
	argv [][]string
	fail map[string]error
	out  map[string]string
}

func (r *recorder) exec(_ context.Context, argv []string) (string, error) {
	r.argv = append(r.argv, argv)
	key := strings.Join(argv, " ")
	return r.out[key], r.fail[key]
}

func (r *recorder) ran(argv ...string) bool {
	want := strings.Join(argv, " ")
	for _, got := range r.argv {
		if strings.Join(got, " ") == want {
			return true
		}
	}
	return false
}

func newBootstrap(t *testing.T, cfg repocfg.Config, rec *recorder) *Bootstrap {
	t.Helper()
	return &Bootstrap{
		WorkingDir: t.TempDir(),
		Config:     cfg,
		Addr:       "db:5432",
		Wait:       time.Second,
		Poll:       time.Millisecond,
		SeedMarker: filepath.Join(t.TempDir(), "db-seeded"),
		Exec:       rec.exec,
		Dial:       func(context.Context, string) error { return nil },
	}
}

// 8.5: the schema is applied only once the database answers, so a migration
// never fails merely because the instance had not finished starting.
func TestRun_WaitsForTheDatabaseBeforeMigrating(t *testing.T) {
	rec := &recorder{}
	b := newBootstrap(t, repocfg.Config{Migrate: []string{"npm", "run", "migrate"}}, rec)

	attempts := 0
	dialed := false
	b.Dial = func(context.Context, string) error {
		attempts++
		if attempts < 3 {
			return errors.New("connection refused")
		}
		dialed = true
		return nil
	}
	b.Exec = func(ctx context.Context, argv []string) (string, error) {
		if !dialed {
			t.Error("migration ran before the database was reachable")
		}
		return rec.exec(ctx, argv)
	}

	if err := b.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if attempts != 3 {
		t.Errorf("dial attempts = %d, want the loop to retry until it connects", attempts)
	}
	if !rec.ran("npm", "run", "migrate") {
		t.Errorf("migration never ran, commands = %v", rec.argv)
	}
}

func TestRun_FailsWhenTheDatabaseNeverBecomesReachable(t *testing.T) {
	rec := &recorder{}
	b := newBootstrap(t, repocfg.Config{Migrate: []string{"migrate"}}, rec)
	b.Wait = 5 * time.Millisecond
	b.Dial = func(context.Context, string) error { return errors.New("connection refused") }

	err := b.Run(context.Background())
	if err == nil {
		t.Fatal("Run succeeded although the database never answered")
	}
	if len(rec.argv) != 0 {
		t.Errorf("commands ran against an unreachable database: %v", rec.argv)
	}
}

// 8.6: seed data is applied after the migration, and only when the repository
// declares any.
func TestRun_SeedsAfterMigrating(t *testing.T) {
	rec := &recorder{}
	b := newBootstrap(t, repocfg.Config{
		Migrate: []string{"migrate"},
		Seed:    []string{"seed"},
	}, rec)

	if err := b.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rec.argv) != 2 {
		t.Fatalf("commands = %v, want migrate then seed", rec.argv)
	}
	if rec.argv[0][0] != "migrate" || rec.argv[1][0] != "seed" {
		t.Errorf("commands = %v, want migrate before seed", rec.argv)
	}
}

func TestRun_SkipsSeedWhenTheRepositoryDeclaresNone(t *testing.T) {
	rec := &recorder{}
	b := newBootstrap(t, repocfg.Config{Migrate: []string{"migrate"}}, rec)

	if err := b.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rec.argv) != 1 {
		t.Errorf("commands = %v, want the migration alone", rec.argv)
	}
	if _, err := os.Stat(b.SeedMarker); err == nil {
		t.Error("a seed marker was written although nothing was seeded")
	}
}

// Seeding is for an empty database only: a restarted Pod must not re-insert
// the fixtures on top of whatever the branch's work has since produced.
func TestRun_SeedsOnlyOnce(t *testing.T) {
	cfg := repocfg.Config{Migrate: []string{"migrate"}, Seed: []string{"seed"}}

	rec := &recorder{}
	b := newBootstrap(t, cfg, rec)
	if err := b.Run(context.Background()); err != nil {
		t.Fatalf("first Run: %v", err)
	}

	again := &recorder{}
	b.Exec = again.exec
	if err := b.Run(context.Background()); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if again.ran("seed") {
		t.Errorf("the seed ran a second time, commands = %v", again.argv)
	}
	if !again.ran("migrate") {
		t.Errorf("the migration was skipped on the second run, commands = %v", again.argv)
	}
}

// 8.7: a failed migration is the caller's signal to mark the workspace
// unusable, so the failure has to surface with the tool's own output in it.
func TestRun_ReportsMigrationFailureWithOutput(t *testing.T) {
	rec := &recorder{
		fail: map[string]error{"migrate": errors.New("exit status 1")},
		out:  map[string]string{"migrate": "relation \"users\" already exists"},
	}
	b := newBootstrap(t, repocfg.Config{
		Migrate: []string{"migrate"},
		Seed:    []string{"seed"},
	}, rec)

	err := b.Run(context.Background())
	if err == nil {
		t.Fatal("Run succeeded although the migration failed")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("error = %q, want the migration tool's output in it", err)
	}
	if rec.ran("seed") {
		t.Error("seeding ran after a failed migration")
	}
}

func TestRun_ReportsSeedFailure(t *testing.T) {
	rec := &recorder{
		fail: map[string]error{"seed": errors.New("exit status 1")},
		out:  map[string]string{"seed": "duplicate key value"},
	}
	b := newBootstrap(t, repocfg.Config{
		Migrate: []string{"migrate"},
		Seed:    []string{"seed"},
	}, rec)

	err := b.Run(context.Background())
	if err == nil {
		t.Fatal("Run succeeded although seeding failed")
	}
	if !strings.Contains(err.Error(), "duplicate key") {
		t.Errorf("error = %q, want the seed tool's output in it", err)
	}
	if _, statErr := os.Stat(b.SeedMarker); statErr == nil {
		t.Error("a failed seed was recorded as done, so it would never be retried")
	}
}

// A repository that declares neither is not misconfigured; it simply has no
// database step, and must not be held up waiting for one.
func TestRun_IsANoOpWhenNothingIsDeclared(t *testing.T) {
	rec := &recorder{}
	b := newBootstrap(t, repocfg.Config{}, rec)
	b.Dial = func(context.Context, string) error {
		t.Error("waited for a database no declared command uses")
		return nil
	}

	if err := b.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rec.argv) != 0 {
		t.Errorf("commands = %v, want none", rec.argv)
	}
}
