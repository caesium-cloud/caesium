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
	"github.com/caesium-cloud/caesium/internal/dispatch"
	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/executor"
	"github.com/caesium-cloud/caesium/internal/jobdef"
	"github.com/caesium-cloud/caesium/internal/jobdef/git"
	"github.com/caesium-cloud/caesium/internal/jobdef/runtime"
	"github.com/caesium-cloud/caesium/internal/lineage"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/notification"
	"github.com/caesium-cloud/caesium/internal/run"
	triggerhttp "github.com/caesium-cloud/caesium/internal/trigger/http"
	"github.com/caesium-cloud/caesium/internal/worker"
	"github.com/caesium-cloud/caesium/pkg/db"
	"github.com/caesium-cloud/caesium/pkg/dqlite"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/spf13/cobra"
)

// dqliteDispatchPeerResolver returns a PeerLister that discovers all dqlite
// cluster members (excluding abstract-unix and empty addresses) and converts
// them to node-address strings for the dispatch loop.  The loop itself
// converts these to base HTTP URLs using the API port.
func dqliteDispatchPeerResolver() dispatch.PeerLister {
	return dispatch.PeerListerFunc(func(ctx context.Context) ([]string, error) {
		nodes, err := dqlite.Cluster(ctx)
		if err != nil {
			return nil, err
		}
		peers := make([]string, 0, len(nodes))
		for _, node := range nodes {
			if node.Address == "" || strings.HasPrefix(node.Address, "@") {
				continue
			}
			peers = append(peers, node.Address)
		}
		return peers, nil
	})
}

const (
	usage   = "start"
	short   = "Start a caesium scheduling instance"
	long    = "This command starts a caesium scheduling instance"
	example = "caesium start"

	dqliteWakeupPeerCacheTTL = 5 * time.Second
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
	runStore := run.Default()
	runStore.SetBus(bus)
	jsvc.Service(ctx).SetBus(bus)
	runsvc.New(ctx).SetBus(bus)

	vars := env.Variables()
	distributedMode := strings.EqualFold(strings.TrimSpace(vars.ExecutionMode), "distributed")
	wakeupSignaler := worker.NewWakeupSignaler()
	var distributedWakeups *worker.DistributedWakeups
	var internalWakeupHandler api.InternalWakeupHandler
	if distributedMode {
		if strings.TrimSpace(vars.InternalWakeupToken) != "" {
			distributedWakeups = worker.NewDistributedWakeups(worker.DistributedWakeupConfig{
				Token:      vars.InternalWakeupToken,
				FanoutMode: vars.WakeupFanoutMode,
				Signaler:   wakeupSignaler,
				Resolver: worker.NewCachedWakeupPeerResolver(
					dqliteWakeupPeerResolver(vars.NodeAddress, vars.Port),
					dqliteWakeupPeerCacheTTL,
				),
			})
			internalWakeupHandler = func(ctx context.Context, id string, ttl int) {
				distributedWakeups.HandleRemote(ctx, worker.WakeupMessage{ID: id, TTL: ttl})
			}
			go func() {
				log.Info("launching distributed wakeup fanout", "mode", vars.WakeupFanoutMode)
				if err := distributedWakeups.Start(ctx, bus); err != nil && ctx.Err() == nil {
					log.Error("distributed wakeup fanout exited", "error", err)
				}
			}()
		} else {
			log.Warn("distributed wakeups disabled; set CAESIUM_INTERNAL_WAKEUP_TOKEN to enable cross-node wakeups")
		}
	}

	// --- Phase 2: Run-Owner Coordination ---
	//
	// When CAESIUM_RUN_OWNER_ENABLED=true, every new run is assigned an owner
	// node that holds its DAG coordination state and dispatches work to workers
	// via /internal/dispatch.  Workers push results back via /internal/complete.
	//
	// When the flag is false (default), this block is a no-op and the system
	// behaves byte-identically to Phase 1.
	var ownerDispatchHandler *dispatch.Handler
	if vars.RunOwnerEnabled {
		// Warn if mTLS is not configured — Phase B will make this a hard error.
		// We check for the Phase B env vars to future-proof the warning.
		dispatch.WarnIfNoMTLS()

		// Warn if the bearer token is missing — dispatch endpoints reject every
		// request without it, making run-owner mode silently inert.
		dispatch.WarnIfNoToken(vars.InternalWakeupToken)

		leaseStore := run.NewLeaseStore(db.Connection())
		runStore.WithLeaseStore(leaseStore)

		token := strings.TrimSpace(vars.InternalWakeupToken)
		ownerDispatchHandler = dispatch.NewHandler(runStore, leaseStore, vars.NodeAddress, token)

		log.Info(
			"run-owner mode enabled (Phase 2A substrate)",
			"node_address", vars.NodeAddress,
			"run_lease_ttl", vars.RunLeaseTTL,
			"dispatch_interval", vars.RunOwnerDispatchInterval,
			"dispatch_batch", vars.RunOwnerDispatchBatch,
			"dispatch_deadline", vars.RunOwnerDispatchDeadline,
		)

		// --- Phase A2: Executor-side dispatch loop ---
		//
		// Polls owned runs for pending tasks and pushes them to workers via
		// PostDispatch.  If PostDispatch returns false (worker rejected or network
		// error), the task is left unclaimed and ClaimNext recovery picks it up.
		dispatchLoop := dispatch.NewDispatchLoop(dispatch.DispatchLoopConfig{
			NodeID:     vars.NodeAddress,
			APIPort:    vars.Port,
			Token:      token,
			Interval:   vars.RunOwnerDispatchInterval,
			BatchSize:  vars.RunOwnerDispatchBatch,
			Deadline:   vars.RunOwnerDispatchDeadline,
			LeaseStore: leaseStore,
			Store:      runStore,
			Peers:      dqliteDispatchPeerResolver(),
		})
		go func() {
			log.Info("launching owner dispatch loop",
				"node_address", vars.NodeAddress,
				"interval", vars.RunOwnerDispatchInterval,
			)
			dispatchLoop.Run(ctx)
		}()
	}

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
		"worker_reclaim_interval",
		vars.WorkerReclaimInterval,
		"worker_lease_ttl",
		vars.WorkerLeaseTTL,
		"database_voters",
		vars.DatabaseVoters,
		"database_standbys",
		vars.DatabaseStandbys,
		"wakeup_fanout_mode",
		vars.WakeupFanoutMode,
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

	// --- Notification Subscriber & Watcher ---
	{
		notification.RegisterMetrics()
		conn := db.Connection()
		notifSub := notification.NewSubscriber(bus, conn)
		notifSub.RegisterSender(models.ChannelTypeWebhook, notification.NewWebhookSender())
		notifSub.RegisterSender(models.ChannelTypeSlack, notification.NewSlackSender())
		notifSub.RegisterSender(models.ChannelTypeEmail, notification.NewEmailSender())
		notifSub.RegisterSender(models.ChannelTypePagerDuty, notification.NewPagerDutySender())
		go func() {
			log.Info("launching notification subscriber")
			if err := notifSub.Start(ctx); err != nil && ctx.Err() == nil {
				log.Error("notification subscriber exited", "error", err)
			}
		}()

		watcher := notification.NewWatcher(conn, bus, event.NewStore(conn), vars.NotificationWatcherInterval)
		go func() {
			log.Info("launching notification watcher (timeout/SLA)")
			if err := watcher.Start(ctx); err != nil && ctx.Err() == nil {
				log.Error("notification watcher exited", "error", err)
			}
		}()
	}

	go func() {
		log.Info("launching event bus dispatcher")
		if err := event.NewBusDispatcher(event.NewStore(db.Connection()), bus).Start(ctx); err != nil && ctx.Err() == nil {
			log.Error("event bus dispatcher exited", "error", err)
		}
	}()

	importer := jobdef.NewImporter(db.Connection())
	resolver, err := runtime.BuildSecretResolver(vars)
	if err != nil {
		log.Fatal("secret resolver configuration failure", "error", err)
	}
	triggerhttp.SetSecretResolver(resolver)

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

	// Construct the distributed worker synchronously (cheap, no I/O) so that,
	// when run-owner mode is on, its dispatched-task submit seam can be wired
	// into the dispatch handler BEFORE the API starts serving /internal/dispatch.
	// Wiring it before api.Start avoids a data race on the handler's submitter
	// field and guarantees a dispatch can never arrive at a handler with no
	// worker attached.
	var distributedWorker *worker.Worker
	if vars.WorkerEnabled && distributedMode {
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
			"reclaim_interval",
			vars.WorkerReclaimInterval,
			"lease_ttl",
			vars.WorkerLeaseTTL,
		)

		claimer := worker.NewClaimer(vars.NodeAddress, runStore, vars.WorkerLeaseTTL, worker.ParseNodeLabels(vars.NodeLabels))
		executorFn := worker.NewRuntimeExecutor(runStore, vars.TaskTimeout, vars.TaskFailurePolicy)
		wakeups := worker.SubscribeWakeups(ctx, bus, wakeupSignaler.C())
		distributedWorker = worker.NewWorker(claimer, worker.NewPool(poolSize), vars.WorkerPollInterval, executorFn).
			WithReclaimInterval(vars.WorkerReclaimInterval).
			WithWakeups(wakeups).
			WithLeaseRenewal(runStore, vars.WorkerLeaseTTL, vars.WorkerLeaseRenewInterval)

		// Phase 2: piggyback run-lease renewal on the same worker goroutine
		// when owner mode is enabled, and enable the inbound dispatch path so
		// dispatched tasks flow onto this worker's shared pool.
		if vars.RunOwnerEnabled && runStore.LeaseStore() != nil {
			distributedWorker = distributedWorker.
				WithRunLeaseRenewal(runStore.LeaseStore(), vars.RunLeaseTTL, vars.NodeAddress).
				WithInboundDispatch(strings.TrimSpace(vars.InternalWakeupToken))

			// Hand the dispatch handler the worker's submit seam so accepted
			// dispatches flow onto the worker's pool (the dispatch→execute step).
			if ownerDispatchHandler != nil {
				ownerDispatchHandler = ownerDispatchHandler.WithWorkerSubmitter(distributedWorker)
			}
		}

		if usesInternalDqlite(vars.DatabaseType) {
			distributedWorker = distributedWorker.WithReclaimGate(worker.ReclaimGateFunc(func(ctx context.Context) (bool, error) {
				return dqlite.IsLocalLeader(ctx)
			}))
		}
	} else {
		log.Info(
			"distributed worker disabled",
			"worker_enabled",
			vars.WorkerEnabled,
			"execution_mode",
			vars.ExecutionMode,
		)
	}

	go func() {
		log.Info("spinning up api")
		errs <- api.Start(ctx, bus, authSvc, auditor, limiter, internalWakeupHandler, ownerDispatchHandler)
	}()

	go func() {
		log.Info("launching execution routine")
		errs <- executor.Start(ctx)
	}()

	if distributedWorker != nil {
		go func() {
			errs <- distributedWorker.Run(ctx)
		}()
	}

	defer shutdown()

	return <-errs
}

func usesInternalDqlite(databaseType string) bool {
	switch strings.ToLower(strings.TrimSpace(databaseType)) {
	case "", "internal", "dqlite":
		return true
	default:
		return false
	}
}

func dqliteWakeupPeerResolver(localNodeAddress string, apiPort int) worker.WakeupPeerResolver {
	return worker.WakeupPeerResolverFunc(func(ctx context.Context) ([]string, error) {
		nodes, err := dqlite.Cluster(ctx)
		if err != nil {
			return nil, err
		}

		peers := make([]string, 0, len(nodes))
		for _, node := range nodes {
			if node.Address == "" || node.Address == localNodeAddress {
				continue
			}
			wakeupURL, err := worker.WakeupURLForNodeAddress(node.Address, apiPort)
			if err != nil {
				log.Warn("skipping dqlite peer for wakeup fanout", "address", node.Address, "error", err)
				continue
			}
			peers = append(peers, wakeupURL)
		}
		return peers, nil
	})
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
