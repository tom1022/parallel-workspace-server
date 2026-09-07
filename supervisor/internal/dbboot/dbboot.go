// Package dbboot applies the target repository's schema and seed data to the
// branch-dedicated database before the session is handed the branch (8.5-8.7).
package dbboot

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/tom1022/gitops-apps/apps/devplatform/supervisor/internal/repocfg"
)

// outputTail bounds how much of a failing tool's output travels into the error,
// which is forwarded to a chat relay.
const outputTail = 4000

// Bootstrap applies one working directory's declared database steps.
type Bootstrap struct {
	WorkingDir string
	Config     repocfg.Config

	// Addr is the database endpoint, dialled until it answers. The declared
	// commands are what actually connect; this only decides when they may.
	Addr string
	Wait time.Duration
	Poll time.Duration

	// SeedMarker records that seeding has happened. It lives outside the
	// working directory so a `git add -A` in the session cannot commit it.
	SeedMarker string

	// Exec runs a command to completion and returns its combined output. Nil
	// starts a real process; tests substitute it.
	Exec func(ctx context.Context, argv []string) (string, error)
	// Dial reports whether the database answers. Nil opens a real connection.
	Dial func(ctx context.Context, addr string) error
}

// Run waits for the database, applies the migration, then seeds. A repository
// that declares neither step is left alone rather than held up waiting for a
// database it does not use.
func (b *Bootstrap) Run(ctx context.Context) error {
	if len(b.Config.Migrate) == 0 && len(b.Config.Seed) == 0 {
		return nil
	}
	if err := b.await(ctx); err != nil {
		return err
	}
	if len(b.Config.Migrate) > 0 {
		if err := b.run(ctx, "migration", b.Config.Migrate); err != nil {
			return err
		}
	}
	return b.seed(ctx)
}

// seed applies the fixtures once. The branch database is created empty and
// destroyed with the workspace, so a marker on the workspace volume answers
// "is it still empty" without a SQL connection of our own.
//
// ponytail: the marker records that seeding ran, not that the database is
// still pristine. Work that empties the database mid-branch will not be
// re-seeded; re-provision the branch if that ever matters.
func (b *Bootstrap) seed(ctx context.Context) error {
	if len(b.Config.Seed) == 0 {
		return nil
	}
	if b.SeedMarker == "" {
		return errors.New("dbboot: no seed marker path, seeding would repeat on every restart")
	}
	if _, err := os.Stat(b.SeedMarker); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := b.run(ctx, "seed", b.Config.Seed); err != nil {
		return err
	}
	return os.WriteFile(b.SeedMarker, nil, 0o600)
}

func (b *Bootstrap) run(ctx context.Context, step string, argv []string) error {
	out, err := b.exec(ctx, argv)
	if err == nil {
		return nil
	}
	return fmt.Errorf("dbboot: %s failed (%v): %s", step, err, tail(out))
}

// await blocks until the database accepts a connection. CNPG has already
// gated the Pod on the branch role's certificate existing, so what is left to
// wait for is the instance itself answering.
func (b *Bootstrap) await(ctx context.Context) error {
	poll := b.Poll
	if poll <= 0 {
		poll = time.Second
	}
	deadline := time.Now().Add(b.Wait)
	var last error
	for {
		last = b.dial(ctx, b.Addr)
		if last == nil {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("dbboot: %s did not answer within %s: %w", b.Addr, b.Wait, last)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}

func (b *Bootstrap) dial(ctx context.Context, addr string) error {
	if b.Dial != nil {
		return b.Dial(ctx, addr)
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	return conn.Close()
}

func (b *Bootstrap) exec(ctx context.Context, argv []string) (string, error) {
	if b.Exec != nil {
		return b.Exec(ctx, argv)
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = b.WorkingDir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func tail(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= outputTail {
		return s
	}
	return "..." + s[len(s)-outputTail:]
}
