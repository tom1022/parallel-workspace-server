package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/tom1022/gitops-apps/apps/devplatform/supervisor/internal/dbboot"
	"github.com/tom1022/gitops-apps/apps/devplatform/supervisor/internal/evacuation"
	"github.com/tom1022/gitops-apps/apps/devplatform/supervisor/internal/repocfg"
	"github.com/tom1022/gitops-apps/apps/devplatform/supervisor/internal/session"
	"github.com/tom1022/gitops-apps/apps/devplatform/supervisor/internal/testrun"
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

	// modelPollInterval paces the check that each turn ran on the configured
	// model. A turn is minutes long, so the reading only has to be finer than
	// that to catch every one.
	modelPollInterval = 5 * time.Second

	// evacuationPollInterval paces both the turn-completion watch and the
	// settle wait an explicit evacuation request performs.
	evacuationPollInterval = 5 * time.Second

	// evacuationSettleTimeout bounds how long an explicit request waits for an
	// executing turn to finish writing (16.6). Exceeding it fails the request,
	// which keeps the caller from stopping the workspace unevacuated (16.8).
	evacuationSettleTimeout = 10 * time.Minute

	// authProbePrompt is deliberately trivial: 5.7 asks for proof that the
	// credential works over the real route, not for useful output.
	authProbePrompt       = "Reply with the single word: ok"
	authProbeTimeout      = 2 * time.Minute
	authProbePollInterval = 2 * time.Second

	// Long enough for Claude Code's UI to finish drawing before the probe
	// pastes into it; input arriving earlier is dropped without a trace.
	authProbeSettle = 10 * time.Second

	defaultDatabasePort = "5432"

	// databaseWait bounds the branch instance's start. CNPG has already made
	// kubelet hold this container until the branch role's certificate exists,
	// so what is left is the instance answering.
	databaseWait         = 5 * time.Minute
	databasePollInterval = 2 * time.Second

	// eventDatabaseBootstrapFailed tells Hermes Agent the branch never got a
	// usable schema (8.7).
	eventDatabaseBootstrapFailed = "DatabaseBootstrapFailed"

	// testRunPollInterval paces the watch for the completed turns that trigger
	// a test run (9.1).
	testRunPollInterval = 5 * time.Second

	// fixRequestTimeout bounds one self-healing turn. It is generous because
	// the turn it waits on is a real implementation task, not a probe; the
	// loop's bound on wasted work is the attempt limit, not this.
	fixRequestTimeout = 30 * time.Minute

	// testArtifactDir sits beside the working directory rather than inside it,
	// so run artifacts never show up as changes in the branch under test.
	testArtifactDir = "testruns"

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
	if len(os.Args) > 1 && os.Args[1] == "restore" {
		if err := restore(); err != nil {
			log.Fatal(err)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "dbboot" {
		if err := bootstrapDatabase(); err != nil {
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

// restore reconstructs the working directory from the last evacuation after
// the node holding it was lost (16.9). It runs from the init container, right
// after the clone that gives it a tree to replay onto, and is a no-op when
// that tree already holds local work.
func restore() error {
	agent := newEvacuationAgent(workingDirFromEnv())
	if agent.Store.Endpoint == "" {
		// The init container must not block a workspace whose template
		// predates an evacuation destination; the supervisor itself fails
		// loudly on the next turn instead.
		log.Print("no evacuation destination configured, skipping restore")
		return nil
	}
	return agent.Restore(context.Background())
}

// bootstrapDatabase applies the branch database's schema and seed data before
// the session container starts (8.5, 8.6).
//
// A failure is not this process's to escalate: exiting non-zero would put the
// Pod in a restart loop where nothing can serve the reason. It instead marks
// the workspace unusable and notifies Hermes Agent (8.7), and lets the
// workspace come up so that mark is readable.
func bootstrapDatabase() error {
	workingDir := workingDirFromEnv()
	_, defaultConfigDir, _ := session.Paths(env("WORKSPACE_MOUNT", defaultMountRoot))
	configDir := env("CLAUDE_CONFIG_DIR", defaultConfigDir)

	cfg, err := repocfg.Load(workingDir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		return err
	}

	boot := &dbboot.Bootstrap{
		WorkingDir: workingDir,
		Config:     cfg,
		Addr:       net.JoinHostPort(os.Getenv("PGHOST"), env("PGPORT", defaultDatabasePort)),
		Wait:       databaseWait,
		Poll:       databasePollInterval,
		SeedMarker: filepath.Join(configDir, "db-seeded"),
	}
	health := &session.Health{ConfigDir: configDir, Source: "dbboot"}
	if err := boot.Run(context.Background()); err != nil {
		detail := err.Error()
		log.Printf("database bootstrap failed, workspace marked unusable: %v", err)
		health.MarkUnusable(detail)
		if err := session.NotifyHermes(env("HERMES_NOTIFY_URL", ""), session.Event{
			Kind:      eventDatabaseBootstrapFailed,
			Workspace: env("WORKSPACE_NAME", ""),
			Detail:    detail,
		}); err != nil {
			log.Printf("notifying hermes failed: %v", err)
		}
		return nil
	}
	// A restart that gets the schema in place withdraws the previous
	// container's verdict, so a transient database outage does not leave the
	// workspace permanently unusable.
	health.ClearUnusable()
	return nil
}

// workingDirFromEnv resolves the same working directory the init container and
// the supervisor both address, from whichever of the two variables is set.
func workingDirFromEnv() string {
	defaultWorkingDir, _, _ := session.Paths(env("WORKSPACE_MOUNT", defaultMountRoot))
	return env("WORKSPACE_DIR", defaultWorkingDir)
}

func newEvacuationAgent(workingDir string) *evacuation.Agent {
	return &evacuation.Agent{
		WorkingDir:  workingDir,
		WorkspaceId: env("WORKSPACE_ID", os.Getenv("WORKSPACE_NAME")),
		Store: &evacuation.S3{
			Endpoint:  os.Getenv("EVACUATION_ENDPOINT"),
			Bucket:    os.Getenv("EVACUATION_BUCKET"),
			Region:    env("EVACUATION_REGION", "garage"),
			AccessKey: os.Getenv("EVACUATION_ACCESS_KEY"),
			SecretKey: os.Getenv("EVACUATION_SECRET_KEY"),
		},
	}
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
		Tmux:           tm,
		ConfigDir:      configDir,
		OutputLog:      filepath.Join(configDir, "session-output.log"),
		WorkingDir:     workingDir,
		ClaudeMDSource: os.Getenv("BLACKBOARD_CLAUDE_MD"),
	}

	health := &session.Health{ConfigDir: configDir}

	evacuator := &evacuation.Trigger{
		Agent: newEvacuationAgent(workingDir),
		Turn:  turnReader(sup),
		Poll:  evacuationPollInterval,
		Wait:  evacuationSettleTimeout,
	}

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

	healer, err := newHealingLoop(sup, mountRoot, workingDir, workspace, hermesURL)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go watchProcess(ctx, sup, workspace, hermesURL)
	go watchModel(ctx, sup, workspace, hermesURL)
	go serveSSH(ctx, mountRoot, env("WORKSPACE_ID", workspace))
	go probeAuth(ctx, sup, health, healer, workspace, hermesURL)
	go evacuator.WatchTurns(ctx, func(err error) {
		log.Printf("evacuation after turn failed: %v", err)
	})

	mux := http.NewServeMux()
	mux.Handle("/", sup.Handler())
	mux.Handle("GET /health", health.Handler())
	mux.Handle("GET /usage", health.Handler())
	// Requested before the control plane suspends or destroys this workspace.
	// It answers only once the working directory is safely off-node, so a
	// failure here is the caller's signal to leave the workspace running.
	mux.HandleFunc("POST /evacuate", func(w http.ResponseWriter, r *http.Request) {
		snap, err := evacuator.EvacuateWhenSettled(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(snap)
	})
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

// serveSSH runs the optional IDE route (4.7). It is optional in the strict
// sense: a workspace with no certificate authority key mounted keeps serving
// the browser route, which is the one that must always work (4.3), and only
// loses the IDE route.
func serveSSH(ctx context.Context, mountRoot, workspaceID string) {
	endpoint := &session.SSHEndpoint{
		WorkspaceID: workspaceID,
		StateDir:    session.SSHStateDir(mountRoot),
		CAPublicKey: env("SSH_CA_PUBLIC_KEY", session.DefaultCAPublicKey),
	}
	if err := endpoint.Run(ctx); err != nil && ctx.Err() == nil {
		log.Printf("ssh endpoint unavailable: %v", err)
	}
}

// probeAuth runs the one-shot authentication check once the session is up
// (5.7). It runs in the background so a slow or failed probe does not keep the
// control API from serving — a caller has to be able to read /health to learn
// the workspace is unusable.
func probeAuth(ctx context.Context, sup *session.Supervisor, health *session.Health, healer *testrun.Loop, workspace, hermesURL string) {
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
	if healer == nil {
		return
	}
	// Started here rather than beside the other watchers so the probe's own
	// completed turn lands in the baseline: a workspace must not answer its
	// authentication check by running the suite, and one that failed the check
	// must not run it at all.
	healer.Watch(ctx, func(err error) {
		log.Printf("self-healing run failed: %v", err)
	})
}

// newHealingLoop builds the test runner and its self-healing loop from the
// repository's own declaration. A repository that declares no suites gets no
// loop: every run would be a no-op that still reported a verdict.
func newHealingLoop(sup *session.Supervisor, mountRoot, workingDir, workspace, hermesURL string) (*testrun.Loop, error) {
	// ponytail: read once at startup, so a declaration the session adds or
	// edits later takes effect on the next restart. Reload per run if that
	// turns out to matter.
	cfg, err := repocfg.Load(workingDir)
	if err != nil {
		return nil, err
	}
	if len(cfg.UnitTest) == 0 && len(cfg.E2ETest) == 0 {
		log.Print("no test suites declared, self-healing disabled")
		return nil, nil
	}

	return &testrun.Loop{
		Runner: &testrun.Runner{
			WorkingDir:   workingDir,
			Config:       cfg,
			ArtifactRoot: filepath.Join(mountRoot, testArtifactDir),
		},
		Publisher: newReportPublisher(),
		Turn:      turnReader(sup),
		Request: func(_ context.Context, prompt string) error {
			return session.Request(sup, "self-healing", prompt, fixRequestTimeout, testRunPollInterval)
		},
		Notify: func(kind, detail string) {
			if err := session.NotifyHermes(hermesURL, session.Event{
				Kind:      kind,
				Workspace: workspace,
				Detail:    detail,
			}); err != nil {
				log.Printf("notifying hermes failed: %v", err)
			}
		},
		Poll: testRunPollInterval,
	}, nil
}

// newReportPublisher returns nil when no report destination is configured,
// which leaves runs unpublished rather than failing them.
func newReportPublisher() *testrun.Publisher {
	endpoint := os.Getenv("REPORT_ENDPOINT")
	baseURL := os.Getenv("REPORT_BASE_URL")
	if endpoint == "" || baseURL == "" {
		return nil
	}
	return &testrun.Publisher{
		Store: &evacuation.S3{
			Endpoint:  endpoint,
			Bucket:    os.Getenv("REPORT_BUCKET"),
			Region:    env("REPORT_REGION", "garage"),
			AccessKey: os.Getenv("REPORT_ACCESS_KEY"),
			SecretKey: os.Getenv("REPORT_SECRET_KEY"),
		},
		WorkspaceId:   env("WORKSPACE_ID", os.Getenv("WORKSPACE_NAME")),
		PublicBaseURL: baseURL,
	}
}

// turnReader adapts the session's turn state to what the evacuation triggers
// need: whether writes may still be in flight, and a value that changes each
// time a turn completes.
func turnReader(sup *session.Supervisor) func() (bool, string, error) {
	return func() (bool, string, error) {
		state, err := sup.TurnState()
		if err != nil {
			return false, "", err
		}
		busy := state.Kind == session.TurnRunning || state.Kind == session.TurnAwaitingTool
		if state.Kind != session.TurnCompleted {
			return busy, "", nil
		}
		return busy, state.EndedAt, nil
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

// watchModel reports a turn that ran on a model other than the one this
// workspace was configured with (7.4). A workspace with no model pinned has no
// expectation to check, so it runs no watch at all.
func watchModel(ctx context.Context, sup *session.Supervisor, workspace, hermesURL string) {
	expected := os.Getenv("ANTHROPIC_MODEL")
	if expected == "" {
		return
	}
	mon := &session.ModelMonitor{
		Expected:  expected,
		Workspace: workspace,
		Turn:      sup.TurnState,
		Notify: func(e session.Event) {
			log.Print(e.Detail)
			// Best-effort, as everywhere else: Hermes Agent has no inbound
			// endpoint of its own and a lost notification must not stop the
			// workspace.
			if err := session.NotifyHermes(hermesURL, e); err != nil {
				log.Printf("notifying hermes failed: %v", err)
			}
		},
	}
	mon.Watch(ctx, modelPollInterval, func(err error) {
		log.Printf("model check failed: %v", err)
	})
}

func env(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}
