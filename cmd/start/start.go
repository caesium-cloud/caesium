package start

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"runtime/pprof"
	"strings"
	"syscall"
	"time"

	"github.com/caesium-cloud/caesium/api"
	jsvc "github.com/caesium-cloud/caesium/api/rest/service/job"
	runsvc "github.com/caesium-cloud/caesium/api/rest/service/run"
	"github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/executor"
	"github.com/caesium-cloud/caesium/internal/jobdef"
	"github.com/caesium-cloud/caesium/internal/jobdef/git"
	"github.com/caesium-cloud/caesium/internal/jobdef/runtime"
	"github.com/caesium-cloud/caesium/internal/lineage"
	"github.com/caesium-cloud/caesium/internal/run"
	triggerhttp "github.com/caesium-cloud/caesium/internal/trigger/http"
	"github.com/caesium-cloud/caesium/api/rest/controller/webhook"
	"github.com/caesium-cloud/caesium/internal/worker"
	"github.com/caesium-cloud/caesium/pkg/db"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/spf13/cobra"
)

const (
	usage   = "start"
	short   = "Start a caesium scheduling instance"
	long    = "This command starts a caesium scheduling instance"
	example = "caesium start"
)

var (
	// Cmd is the start command.
	Cmd = &cobra.Command{
		Use:        usage,
		Short:      short,
		Long:       long,
		Aliases:    []string{"s"},
		SuggestFor: []string{"launch", "boot", "up", "run", "begin"},
		Example:    example,
		RunE:       start,
	}
)

var cancel context.CancelFunc

func start(cmd *cobra.Command, args []string) error {
	if err := log.CaptureStderr(); err != nil {
		log.Warn("failed to capture stderr for unified logging", "error", err)
	}

	signalChan := make(chan os.Signal, 1)

	go func() {
		for s := range signalChan {
			switch s {
			case syscall.SIGUSR1:
				log.Info("dumping stack traces due to SIGUSR1 signal")
				if profile := pprof.Lookup("goroutine"); profile != nil {
					if err := profile.WriteTo(os.Stdout, 1); err != nil {
						log.Error("write goroutine profile", "error", err)
					}
				}
			case syscall.SIGINT:
				log.Info("gracefully shutting down due to SIGINT signal")
				shutdown()
				os.Exit(0)
			}
		}
	}()

	signal.Notify(signalChan, syscall.SIGUSR1, syscall.SIGINT)

	var errs = make(chan error)
	ctx, cancelFunc := context.WithCancel(context.Background())
	cancel = cancelFunc

	log.Info("migrating database")
	if err := db.Migrate(); err != nil {
		log.Fatal("database migration failure", "error", err)
	}

	bus := event.New()
	run.Default().SetBus(bus)
	jsvc.Service(ctx).SetBus(bus)
	runsvc.New(ctx).SetBus(bus)

	vars := env.Variables()

	// --- Authentication & Authorization ---
	authSvc, auditor, limiter := initAuth(ctx, vars)

	log.Info(
		"execution configuration",
		"max_parallel_tasks",
		vars.MaxParallelTasks,
		"task_failure_policy",
		vars.TaskFailurePolicy,
		"task_timeout",
		vars.TaskTimeout,
		"execution_mode",
		vars.ExecutionMode,
		"worker_enabled",
		vars.WorkerEnabled,
		"worker_pool_size",
		vars.WorkerPoolSize,
		"worker_poll_interval",
		vars.WorkerPollInterval,
		"worker_lease_ttl",
		vars.WorkerLeaseTTL,
		"node_labels",
		vars.NodeLabels,
	)
	if vars.OpenLineageEnabled {
		transport, err := lineage.BuildTransport(lineage.Config{
			Enabled:       vars.OpenLineageEnabled,
			Transport:     vars.OpenLineageTransport,
			URL:           vars.OpenLineageURL,
			Namespace:     vars.OpenLineageNamespace,
			Headers:       vars.OpenLineageHeaders,
			FilePath:      vars.OpenLineageFilePath,
			Timeout:       vars.OpenLineageTimeout,
			RetryAttempts: vars.OpenLineageRetryAttempts,
		})
		if err != nil {
			log.Error("openlineage transport configuration failure, disabling integration", "error", err)
		} else {
			lineage.RegisterMetrics()
			sub := lineage.NewSubscriber(bus, transport, vars.OpenLineageNamespace, db.Connection())
			sub.SetTransportName(vars.OpenLineageTransport)
			go func() {
				log.Info("launching openlineage subscriber",
					"transport", vars.OpenLineageTransport,
					"namespace", vars.OpenLineageNamespace,
				)
				if err := sub.Start(ctx); err != nil && ctx.Err() == nil {
					log.Error("openlineage subscriber exited", "error", err)
				}
			}()
		}
	}

	importer := jobdef.NewImporter(db.Connection())
	resolver, err := runtime.BuildSecretResolver(vars)
	if err != nil {
		log.Fatal("secret resolver configuration failure", "error", err)
	}
	triggerhttp.SetSecretResolver(resolver)
	webhook.SetAuditLogger(auditor)

	watches, err := runtime.BuildGitWatches(vars, resolver)
	if err != nil {
		log.Fatal("git sync configuration failure", "error", err)
	}

	for _, watch := range watches {
		watch := watch
		go func() {
			log.Info("starting job definition git sync", "url", watch.Source.URL, "ref", watch.Source.Ref, "once", watch.Once, "interval", watch.Interval)
			opts := git.WatchOptions{Source: watch.Source, Interval: watch.Interval, Once: watch.Once}
			if err := git.Watch(ctx, importer, opts); err != nil && ctx.Err() == nil {
				log.Error("job definition git sync exited", "url", watch.Source.URL, "error", err)
				errs <- err
			}
		}()
	}

	go func() {
		log.Info("spinning up api")
		errs <- api.Start(ctx, bus, authSvc, auditor, limiter)
	}()

	go func() {
		log.Info("launching execution routine")
		errs <- executor.Start(ctx)
	}()

	if vars.WorkerEnabled && strings.EqualFold(strings.TrimSpace(vars.ExecutionMode), "distributed") {
		go func() {
			poolSize := vars.WorkerPoolSize
			if poolSize < 1 {
				poolSize = 1
			}

			log.Info(
				"launching distributed worker",
				"node_address",
				vars.NodeAddress,
				"node_labels",
				vars.NodeLabels,
				"pool_size",
				poolSize,
				"poll_interval",
				vars.WorkerPollInterval,
				"lease_ttl",
				vars.WorkerLeaseTTL,
			)

			store := run.Default()
			claimer := worker.NewClaimer(vars.NodeAddress, store, vars.WorkerLeaseTTL, worker.ParseNodeLabels(vars.NodeLabels))
			executorFn := worker.NewRuntimeExecutor(store, vars.TaskTimeout, vars.WorkerLeaseTTL, vars.TaskFailurePolicy)
			wakeups := worker.SubscribeWakeups(ctx, bus)
			w := worker.NewWorker(claimer, worker.NewPool(poolSize), vars.WorkerPollInterval, executorFn).WithWakeups(wakeups)
			errs <- w.Run(ctx)
		}()
	} else {
		log.Info(
			"distributed worker disabled",
			"worker_enabled",
			vars.WorkerEnabled,
			"execution_mode",
			vars.ExecutionMode,
		)
	}

	defer shutdown()

	return <-errs
}

// initAuth sets up authentication services based on CAESIUM_AUTH_MODE.
// Returns nil services when auth is disabled so callers can pass them through safely.
func initAuth(ctx context.Context, vars env.Environment) (*auth.Service, *auth.AuditLogger, *auth.RateLimiter) {
	conn := db.Connection()
	authSvc := auth.NewService(conn, auth.WithKeyHashSecret(vars.AuthKeyHashSecret))
	auditor := auth.NewAuditLogger(conn)
	limiter := auth.NewRateLimiter(vars.AuthRateLimitPerMinute, time.Minute)

	switch vars.AuthMode {
	case "none", "":
		log.Warn("authentication is disabled — all endpoints are publicly accessible")
		return authSvc, auditor, limiter

	case "api-key":
		log.Info("authentication enabled", "mode", vars.AuthMode)

		if strings.TrimSpace(vars.AuthKeyHashSecret) == "" {
			log.Fatal(
				"CAESIUM_AUTH_KEY_HASH_SECRET must be set when API-key authentication is enabled. " +
					"Use a long random secret so newly created keys are stored with a keyed hash",
			)
		}
		if len(strings.TrimSpace(vars.AuthKeyHashSecret)) < 32 {
			log.Fatal(
				"CAESIUM_AUTH_KEY_HASH_SECRET must be at least 32 characters — " +
					"use a cryptographically random value (e.g. openssl rand -hex 32)",
			)
		}

		// TLS requirement check.
		if vars.AuthRequireTLS {
			hasTLS := vars.TLSCert != "" && vars.TLSKey != ""
			hasProxy := vars.TrustedProxies != ""
			if !hasTLS && !hasProxy {
				log.Fatal(
					"TLS is required when authentication is enabled. " +
						"Set CAESIUM_TLS_CERT/CAESIUM_TLS_KEY or CAESIUM_TRUSTED_PROXIES, " +
						"or set CAESIUM_AUTH_REQUIRE_TLS=false to disable this check",
				)
			}
		}

		// Bootstrap: generate initial admin key if none exist.
		plaintext, err := authSvc.Bootstrap()
		if err != nil {
			log.Fatal("auth bootstrap failure", "error", err)
		}
		if plaintext != "" {
			// Print to stdout only — not to structured logs.
			fmt.Println("==========================================================")
			fmt.Println("  BOOTSTRAP ADMIN API KEY (shown once, save it now):")
			fmt.Printf("  %s\n", plaintext)
			fmt.Println("==========================================================")
		} else {
			// Verify at least one admin key exists.
			exists, err := authSvc.AdminKeyExists()
			if err != nil {
				log.Fatal("failed to verify admin key existence", "error", err)
			}
			if !exists {
				log.Fatal("no admin API key found — cannot start with authentication enabled")
			}
		}

		authSvc.StartLastUsedFlusher(ctx)
		limiter.StartCleanup(ctx.Done())
		return authSvc, auditor, limiter

	default:
		log.Fatal("unknown CAESIUM_AUTH_MODE value", "mode", vars.AuthMode)
		return nil, nil, nil // unreachable
	}
}

func shutdown() {
	if cancel != nil {
		cancel()
	}
}
