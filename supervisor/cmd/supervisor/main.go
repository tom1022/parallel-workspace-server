package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/tom1022/gitops-apps/apps/devplatform/supervisor/internal/session"
)

const (
	defaultMountRoot = "/workspace"
	defaultAuthFile  = "/run/devplatform/claude-auth/credentials.json"
	defaultAddr      = ":8787"
	tmuxSocket       = "devplatform"
	tmuxSession      = "workspace"

	// scrollback retained across detach/reattach so a returning client sees
	// what happened while it was away (2.4).
	historyLimit = 50000

	crashPollInterval = 2 * time.Second

	// authProbePrompt is deliberately trivial: 5.7 asks for proof that the
	// credential works over the real route, not for useful output.
	authProbePrompt       = "Reply with the single word: ok"
	authProbeTimeout      = 2 * time.Minute
	authProbePollInterval = 2 * time.Second

	// Long enough for Claude Code's UI to finish drawing before the probe
	// pastes into it; input arriving earlier is dropped without a trace.
	authProbeSettle = 10 * time.Second

	// The identity every workspace commits under, so autonomous work stays
	// distinguishable from a developer's own commits in the history (20.2).
	defaultCommitAuthorName  = "devplatform"
	defaultCommitAuthorEmail = "devplatform@fickledev.com"
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("supervisor: ")

	if len(os.Args) > 1 && os.Args[1] == "attach" {
		if err := attach(os.Args[2:]); err != nil {
			log.Fatal(err)
		}
		return
	}
	if err := serve(); err != nil {
		log.Fatal(err)
	}
}

// attach connects a developer to the session. Read-only is the default (3.3);
// a writable client is only handed out when handover is asked for explicitly
// (3.4).
func attach(args []string) error {
	fs := flag.NewFlagSet("attach", flag.ExitOnError)
	takeover := fs.Bool("takeover", false, "attach with write access, taking over from the agent")
	if err := fs.Parse(args); err != nil {
		return err
	}
	tm := &session.Tmux{Socket: tmuxSocket, Session: tmuxSession}
	return tm.Attach(!*takeover)
}

func serve() error {
	mountRoot := env("WORKSPACE_MOUNT", defaultMountRoot)
	defaultWorkingDir, defaultConfigDir, defaultCacheDir := session.Paths(mountRoot)
	workingDir := env("WORKSPACE_DIR", defaultWorkingDir)
	configDir := env("CLAUDE_CONFIG_DIR", defaultConfigDir)
	cacheDir := env("WORKSPACE_CACHE_DIR", defaultCacheDir)
	authFile := env("CLAUDE_AUTH_FILE", defaultAuthFile)
	workspace := env("WORKSPACE_NAME", "")
	hermesURL := env("HERMES_NOTIFY_URL", "")
	addr := env("SUPERVISOR_ADDR", defaultAddr)

	if err := os.MkdirAll(configDir, 0o700); err != nil {
		return err
	}
	if err := session.EnsureCacheDirs(cacheDir); err != nil {
		return err
	}
	// Taken before the session is touched: a second supervisor on this working
	// directory must fail here rather than race into starting a second Claude
	// Code process against the same checkout (16.3).
	release, err := session.AcquireWorkingDirLock(filepath.Join(configDir, "session.lock"))
	if err != nil {
		return err
	}
	defer release()

	if err := session.PrepareConfigDir(configDir, workingDir, authFile); err != nil {
		return err
	}

	tm := &session.Tmux{Socket: tmuxSocket, Session: tmuxSession}
	commitIdentity := session.CommitIdentity{
		Name:  env("COMMIT_AUTHOR_NAME", defaultCommitAuthorName),
		Email: env("COMMIT_AUTHOR_EMAIL", defaultCommitAuthorEmail),
	}
	if err := session.ConfigureCommitPolicy(workingDir, commitIdentity, tm); err != nil {
		return err
	}

	sup := &session.Supervisor{
		Tmux:      tm,
		ConfigDir: configDir,
		OutputLog: filepath.Join(configDir, "session-output.log"),
	}

	health := &session.Health{ConfigDir: configDir}

	claudeCmd := []string{env("CLAUDE_COMMAND", "claude")}
	err = tm.Start(session.StartConfig{
		WorkingDir:   workingDir,
		Command:      claudeCmd,
		Env:          session.CacheEnv(session.ClaudeEnv(os.Environ(), configDir), cacheDir),
		OutputLog:    sup.OutputLog,
		HistoryLimit: historyLimit,
	})
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go watchProcess(ctx, sup, workspace, hermesURL)
	go probeAuth(sup, health, workspace, hermesURL)

	mux := http.NewServeMux()
	mux.Handle("/", sup.Handler())
	mux.Handle("GET /health", health.Handler())
	mux.Handle("GET /usage", health.Handler())
	mux.HandleFunc("POST /release-context", func(w http.ResponseWriter, r *http.Request) {
		if err := sup.ReleaseContext(); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	srv := &http.Server{Addr: addr, Handler: mux}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Printf("serving session %q on %s", tmuxSession, addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// probeAuth runs the one-shot authentication check once the session is up
// (5.7). It runs in the background so a slow or failed probe does not keep the
// control API from serving — a caller has to be able to read /health to learn
// the workspace is unusable.
func probeAuth(sup *session.Supervisor, health *session.Health, workspace, hermesURL string) {
	err := session.AuthProbe(sup, health, session.ProbeConfig{
		Prompt:    authProbePrompt,
		Timeout:   authProbeTimeout,
		Poll:      authProbePollInterval,
		Settle:    authProbeSettle,
		Workspace: workspace,
		HermesURL: hermesURL,
	})
	if err != nil {
		log.Printf("auth probe failed, workspace marked unusable: %v", err)
		return
	}
	// The probe's own turn is the first thing in the context window and has
	// nothing to do with the work that follows (7.16).
	if err := sup.ReleaseContext(); err != nil {
		log.Printf("releasing probe context failed: %v", err)
	}
}

func watchProcess(ctx context.Context, sup *session.Supervisor, workspace, hermesURL string) {
	ticker := time.NewTicker(crashPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			crashed := false
			err := session.CheckProcess(sup.Tmux, workspace, func(e session.Event) {
				crashed = true
				log.Printf("claude code exited: %s", e.Detail)
				sup.SetFailure(e.Detail)
				// Best-effort: Hermes Agent exposes no inbound endpoint of its
				// own, so the durable signal is the turn state set above.
				if err := session.NotifyHermes(hermesURL, e); err != nil {
					log.Printf("notifying hermes failed: %v", err)
				}
			})
			if err != nil {
				log.Printf("process check failed: %v", err)
				continue
			}
			if !crashed {
				sup.ClearFailure()
			}
		}
	}
}

func env(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}
