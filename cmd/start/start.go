package start

import (
	"context"
	"crypto/tls"
	"fmt"
	"os"
	"os/signal"
	"runtime/pprof"
	"strings"
	"syscall"
	"time"

	"github.com/caesium-cloud/caesium/api"
	authmw "github.com/caesium-cloud/caesium/api/middleware"
	jsvc "github.com/caesium-cloud/caesium/api/rest/service/job"
	runsvc "github.com/caesium-cloud/caesium/api/rest/service/run"
	triggersvc "github.com/caesium-cloud/caesium/api/rest/service/trigger"
	"github.com/caesium-cloud/caesium/internal/auth"
	authldap "github.com/caesium-cloud/caesium/internal/auth/ldap"
	authoidc "github.com/caesium-cloud/caesium/internal/auth/oidc"
	authsaml "github.com/caesium-cloud/caesium/internal/auth/saml"
	"github.com/caesium-cloud/caesium/internal/dispatch"
	dispatchpki "github.com/caesium-cloud/caesium/internal/dispatch/pki"
	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/executor"
	"github.com/caesium-cloud/caesium/internal/freshness"
	"github.com/caesium-cloud/caesium/internal/incident"
	"github.com/caesium-cloud/caesium/internal/jobdef"
	"github.com/caesium-cloud/caesium/internal/jobdef/git"
	"github.com/caesium-cloud/caesium/internal/jobdef/runtime"
	"github.com/caesium-cloud/caesium/internal/lineage"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/notification"
	"github.com/caesium-cloud/caesium/internal/ratelimit"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/internal/runqueue"
	triggerevent "github.com/caesium-cloud/caesium/internal/trigger/event"
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

func start(cmd *cobra.Command, args []string) error {
	// The server logs to stdout (main() defaulted the CLI to stderr). Restore
	// stdout BEFORE CaptureStderr redirects fd 2 through a pipe back into the
	// logger — routing logs to stderr here would feed that pipe in a loop.
	log.ToStdout()

	if err := log.CaptureStderr(); err != nil {
		log.Warn("failed to capture stderr for unified logging", "error", err)
	}

	ctx, cancelFunc := context.WithCancel(context.Background())
	vars := env.Variables()
	var internalSrv *dispatch.InternalServer
	shutdownCoordinator := newShutdownCoordinator(shutdownConfig{
		cancel:      cancelFunc,
		gracePeriod: vars.ShutdownGracePeriod,
		internalShutdown: func(ctx context.Context) error {
			if internalSrv == nil {
				return nil
			}
			return internalSrv.Shutdown(ctx)
		},
	})
	deactivateShutdown := activateShutdownCoordinator(shutdownCoordinator)
	defer deactivateShutdown()
	defer func() {
		if err := shutdown(); err != nil {
			log.Error("graceful shutdown failed", "error", err)
		}
	}()

	runAsync := shutdownCoordinator.runAsync
	errs := make(chan error, 1)
	reportErr := func(err error) {
		if err == nil || ctx.Err() != nil {
			return
		}
		select {
		case errs <- err:
		default:
			log.Error("background routine exited after an error was already reported", "error", err)
		}
	}

	signalCtx, stopSignals := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()

	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, syscall.SIGUSR1)
	defer signal.Stop(signalChan)
	runAsync(func() {
		for {
			select {
			case <-ctx.Done():
				return
			case s := <-signalChan:
				if s != syscall.SIGUSR1 {
					continue
				}
				log.Info("dumping stack traces due to SIGUSR1 signal")
				if profile := pprof.Lookup("goroutine"); profile != nil {
					if err := profile.WriteTo(os.Stdout, 1); err != nil {
						log.Error("write goroutine profile", "error", err)
					}
				}
			}
		}
	})

	log.Info("migrating database")
	if err := db.Migrate(); err != nil {
		log.Fatal("database migration failure", "error", err)
	}
	event.StartIngestRetentionPruner(ctx, event.NewIngestStore(db.Connection()), vars.EventRetention)
	event.StartWebhookEventRetentionPruner(ctx, event.NewWebhookEventStore(db.Connection()), vars.WebhookEventRetention)
	if vars.RateLimitPrunerEnabled {
		runAsync(func() {
			log.Info("launching rate limit token pruner", "interval", vars.RateLimitPruneInterval)
			ratelimit.NewPruner(db.Connection()).Run(ctx, vars.RateLimitPruneInterval)
		})
	}

	bus := event.New()
	runStore := run.Default()
	runStore.SetBus(bus)
	jsvc.Service(ctx).SetBus(bus)
	runsvc.New(ctx).SetBus(bus)
	if vars.RunQueueEnabled || vars.RunQueueDequeuerEnabled {
		dequeuer := runqueue.NewDequeuer(runqueue.Config{
			DB:                  db.Connection(),
			Store:               runStore,
			NodeID:              vars.NodeAddress,
			Interval:            vars.RunQueueDequeueInterval,
			StaleClaimThreshold: vars.RunQueueClaimStaleAfter,
			LeaderCheck:         dqlite.IsLocalLeader,
		})
		runAsync(func() {
			log.Info("launching run queue dequeuer", "interval", vars.RunQueueDequeueInterval, "stale_claim_after", vars.RunQueueClaimStaleAfter, "max_depth", vars.RunQueueMaxDepth)
			dequeuer.Run(ctx)
		})
	}
	if vars.FreshnessEnabled {
		conn := db.Connection()
		capturer := freshness.NewCapturer(bus, conn)
		evaluator := freshness.NewEvaluator(freshness.Config{
			DB:                    conn,
			Bus:                   bus,
			RunStore:              runStore,
			Interval:              vars.FreshnessEvalInterval,
			MaxDerivationsPerTick: vars.FreshnessMaxDerivationsPerTick,
			MaxTriggerDepth:       vars.MaxTriggerDepth,
			LeaderCheck:           dqlite.IsLocalLeader,
		})
		runAsync(func() {
			log.Info("launching freshness capturer")
			if err := capturer.Start(ctx); err != nil && ctx.Err() == nil {
				log.Error("freshness capturer exited", "error", err)
			}
		})
		runAsync(func() {
			log.Info("launching freshness evaluator", "interval", vars.FreshnessEvalInterval, "max_derivations_per_tick", vars.FreshnessMaxDerivationsPerTick)
			evaluator.Run(ctx)
		})
	}

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
			runAsync(func() {
				log.Info("launching distributed wakeup fanout", "mode", vars.WakeupFanoutMode)
				if err := distributedWakeups.Start(ctx, bus); err != nil && ctx.Err() == nil {
					log.Error("distributed wakeup fanout exited", "error", err)
				}
			})
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
	var ownerInternalServerTLS *tls.Config
	if vars.RunOwnerEnabled {
		mtls := dispatch.MTLSConfig{
			CAFile:   vars.InternalMTLSCA,
			CertFile: vars.InternalMTLSCert,
			KeyFile:  vars.InternalMTLSKey,
		}
		configuredFields := 0
		for _, value := range []string{mtls.CAFile, mtls.CertFile, mtls.KeyFile} {
			if strings.TrimSpace(value) != "" {
				configuredFields++
			}
		}
		if configuredFields > 0 && !mtls.Configured() {
			return fmt.Errorf("run-owner mode: set all explicit mTLS material paths (CAESIUM_INTERNAL_MTLS_CA/CERT/KEY) or leave all three unset for automatic provisioning")
		}

		var holder *dispatchpki.MaterialHolder
		if mtls.Configured() {
			var err error
			holder, err = dispatchpki.NewStaticMaterialHolder(mtls.CAFile, mtls.CertFile, mtls.KeyFile)
			if err != nil {
				return fmt.Errorf("run-owner mode: load explicit internal mTLS material: %w", err)
			}
		} else {
			mtlsToken := strings.TrimSpace(vars.InternalMTLSToken)
			if mtlsToken == "" {
				mtlsToken = strings.TrimSpace(vars.InternalWakeupToken)
			}
			if mtlsToken == "" {
				return fmt.Errorf("run-owner mode requires either explicit mTLS material (CAESIUM_INTERNAL_MTLS_CA/CERT/KEY) or a shared token (CAESIUM_INTERNAL_WAKEUP_TOKEN) for automatic provisioning")
			}
			if len(mtlsToken) < 32 {
				log.Warn("CAESIUM_INTERNAL_WAKEUP_TOKEN/CAESIUM_INTERNAL_MTLS_TOKEN is shorter than 32 bytes; use a high-entropy random token for internal mTLS auto-provisioning")
			}
			provisioner, err := dispatchpki.NewProvisioner(dispatchpki.Config{
				Store:             dispatchpki.NewStore(db.Connection()),
				NodeID:            vars.NodeAddress,
				Token:             mtlsToken,
				CATTL:             vars.InternalMTLSCATTL,
				LeafTTL:           vars.InternalMTLSLeafTTL,
				LeafRenewBefore:   vars.InternalMTLSLeafRenewBefore,
				CARenewBefore:     vars.InternalMTLSCARenewBefore,
				EnrollmentTimeout: vars.InternalMTLSEnrollmentTimeout,
				LeaderCheck:       dqlite.IsLocalLeader,
			})
			if err != nil {
				return fmt.Errorf("run-owner mode: configure internal mTLS auto-provisioning: %w", err)
			}
			log.Info("provisioning internal mTLS material", "node_address", vars.NodeAddress)
			if err := provisioner.Bootstrap(ctx); err != nil {
				return fmt.Errorf("run-owner mode: auto-provision internal mTLS: %w", err)
			}
			holder = provisioner.Holder()
			runAsync(func() {
				log.Info("launching internal mTLS auto-provisioner")
				if err := provisioner.Run(ctx); err != nil && ctx.Err() == nil {
					reportErr(err)
				}
			})
		}

		clientTLS, err := dispatch.ClientTLSConfigFromHolder(holder)
		if err != nil {
			return fmt.Errorf("run-owner mode: build internal client TLS: %w", err)
		}
		dispatch.ConfigureInternalMTLS(clientTLS)
		ownerInternalServerTLS, err = dispatch.ServerTLSConfigFromHolder(holder)
		if err != nil {
			return fmt.Errorf("run-owner mode: build internal server TLS: %w", err)
		}

		// Warn if the bearer token is missing — dispatch endpoints reject every
		// request without it, making run-owner mode silently inert.
		dispatch.WarnIfNoToken(vars.InternalWakeupToken)

		leaseStore := run.NewLeaseStore(db.Connection())
		runStore.WithLeaseStore(leaseStore)

		token := strings.TrimSpace(vars.InternalWakeupToken)
		ownerDispatchHandler = dispatch.NewHandler(runStore, leaseStore, vars.NodeAddress, token)

		// B3: when the in-memory advancement flag is on, build the OwnerManager
		// and route both completion application (handler) and dispatch (loop)
		// through it.  Off by default — completions take the proven SQL path.
		var ownerManager *run.OwnerManager
		if vars.RunOwnerInMemory {
			ownerManager = run.NewOwnerManager(runStore, run.CheckpointConfig{
				Events:   vars.RunCheckpointEvents,
				Interval: vars.RunCheckpointInterval,
			})
			ownerDispatchHandler = ownerDispatchHandler.WithOwnerManager(ownerManager)
		}

		log.Info(
			"run-owner mode enabled",
			"node_address", vars.NodeAddress,
			"in_memory", vars.RunOwnerInMemory,
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
			NodeID:       vars.NodeAddress,
			APIPort:      vars.Port,
			InternalPort: vars.InternalPort,
			Token:        token,
			Interval:     vars.RunOwnerDispatchInterval,
			BatchSize:    vars.RunOwnerDispatchBatch,
			Deadline:     vars.RunOwnerDispatchDeadline,
			LeaseTTL:     vars.RunLeaseTTL,
			LeaseStore:   leaseStore,
			Store:        runStore,
			RateLimitDB:  db.Connection(),
			RateLimiter:  ratelimit.NewLimiter(db.Connection()),
			Peers:        dqliteDispatchPeerResolver(),
			OwnerManager: ownerManager,
		})
		runAsync(func() {
			log.Info("launching owner dispatch loop",
				"node_address", vars.NodeAddress,
				"interval", vars.RunOwnerDispatchInterval,
			)
			dispatchLoop.Run(ctx)
		})
	}

	// --- Authentication & Authorization ---
	authSvc, auditor, limiter, sessions, sso := initAuth(ctx, vars, runAsync)
	ssoProviders := initSSOProviders(ctx, vars)

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
			runAsync(func() {
				log.Info("launching openlineage subscriber",
					"transport", vars.OpenLineageTransport,
					"namespace", vars.OpenLineageNamespace,
				)
				if err := sub.Start(ctx); err != nil && ctx.Err() == nil {
					log.Error("openlineage subscriber exited", "error", err)
				}
			})
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
		// ai_agent dispatch channel (agent-in-the-loop D3): a policy-driven second
		// path into the incident manager. Only registered when the remediation
		// feature is enabled, so an ai_agent channel configured without the master
		// gate never silently opens incidents. Leader-gated (like the incident
		// subscriber) so an N-node cluster opens one incident per matched event.
		if vars.AgentRemediationEnabled {
			notifSub.RegisterSender(models.ChannelTypeAIAgent, notification.NewAIAgentSender(conn, dqlite.IsLocalLeader, vars.AgentIncidentCooldown))
		}
		runAsync(func() {
			log.Info("launching notification subscriber")
			if err := notifSub.Start(ctx); err != nil && ctx.Err() == nil {
				log.Error("notification subscriber exited", "error", err)
			}
		})

		watcher := notification.NewWatcher(conn, bus, event.NewStore(conn), vars.NotificationWatcherInterval)
		runAsync(func() {
			log.Info("launching notification watcher (timeout/SLA)")
			if err := watcher.Start(ctx); err != nil && ctx.Err() == nil {
				log.Error("notification watcher exited", "error", err)
			}
		})
	}

	// --- Agent-in-the-Loop Remediation (Phase 0 incident substrate) ---
	//
	// The whole feature is gated behind CAESIUM_AGENT_REMEDIATION_ENABLED; a
	// deployment that never enables it starts neither the incident subscriber nor
	// the timer sweeper and pays nothing. Both are leader-gated
	// (dqlite.IsLocalLeader, like the run-queue dequeuer) so an N-node cluster
	// opens exactly one incident per failure and fires each durable timer once.
	if vars.AgentRemediationEnabled {
		incConn := db.Connection()

		// The action executor backs the deterministic Phase-0 remediation and the
		// durable snooze_retry timer with the admit-aware retry entry point. It is
		// shared by the subscriber (deterministic rules on incident open) and the
		// timer sweeper (RegisterTimerHandlers), so the snooze_retry handler fires
		// instead of being claimed-and-skipped.
		incExecutor := incident.NewExecutor(incident.NewStore(incConn), newIncidentActionOps(run.NewStore(incConn)))

		incidentSub := incident.NewSubscriber(bus, incConn, dqlite.IsLocalLeader, vars.AgentIncidentCooldown)
		incidentSub.SetRemediator(incExecutor, incident.DefaultRuleSet())
		runAsync(func() {
			log.Info("launching incident subscriber", "cooldown", vars.AgentIncidentCooldown)
			if err := incidentSub.Start(ctx); err != nil && ctx.Err() == nil {
				log.Error("incident subscriber exited", "error", err)
			}
		})

		timerSweeper := incident.NewTimerSupervisor(incConn, dqlite.IsLocalLeader, 0)
		incExecutor.RegisterTimerHandlers(timerSweeper)
		runAsync(func() {
			log.Info("launching incident timer sweeper")
			timerSweeper.Run(ctx)
		})

		// --- Agent session supervisor (Stream C: agent runtime) ---
		//
		// The supervisor drives a profile image through the existing atom.Engine
		// as an AgentSession (deliberately NOT a JobRun), minting a scoped,
		// short-lived credential per session and enforcing the concurrent-session
		// caps against the shared store (leader-node placement in v1). It is
		// registered process-wide; the incident manager / action executor (Streams
		// B, E) obtain it to dispatch a triage session once a playbook admits one.
		sessionSupervisor := incident.NewSupervisor(incConn, authSvc, incident.DefaultEngineFactory, incident.SupervisorConfig{
			APIBaseURL:               strings.TrimSpace(vars.APIExternalURL),
			SessionTimeout:           vars.AgentSessionTimeout,
			MaxConcurrentSessions:    vars.AgentMaxConcurrentSessions,
			PerJobConcurrentSessions: 1,
		})
		incident.SetSessionSupervisor(sessionSupervisor)
		log.Info("agent session supervisor ready",
			"max_concurrent_sessions", vars.AgentMaxConcurrentSessions,
			"session_timeout", vars.AgentSessionTimeout,
			"default_profile", vars.AgentDefaultProfile,
		)
	}

	importer := jobdef.NewImporter(db.Connection())
	resolver, err := runtime.BuildSecretResolver(vars)
	if err != nil {
		log.Fatal("secret resolver configuration failure", "error", err)
	}
	triggerhttp.SetSecretResolver(resolver)
	eventRouter := triggerevent.ConfigureDefaultRouter(db.Connection())
	if err := eventRouter.Reload(ctx); err != nil {
		log.Fatal("event trigger router initial load failure", "error", err)
	}
	lifecycleEvents, err := eventRouter.SubscribeLifecycleBridge(ctx, bus)
	if err != nil {
		log.Fatal("event trigger lifecycle bridge subscription failure", "error", err)
	}
	runAsync(func() {
		log.Info("launching event trigger lifecycle bridge")
		if err := eventRouter.RunLifecycleBridge(ctx, lifecycleEvents); err != nil && ctx.Err() == nil {
			log.Error("event trigger lifecycle bridge exited", "error", err)
		}
	})
	runAsync(func() {
		log.Info("launching event bus dispatcher")
		if err := event.NewBusDispatcher(event.NewStore(db.Connection()), bus).Start(ctx); err != nil && ctx.Err() == nil {
			log.Error("event bus dispatcher exited", "error", err)
		}
	})
	reloadEventRouter := func(reloadCtx context.Context) error {
		return eventRouter.Reload(reloadCtx)
	}
	triggersvc.SetMutationHook(reloadEventRouter)
	jobdef.SetTriggerMutationHook(reloadEventRouter)

	watches, err := runtime.BuildGitWatches(vars, resolver)
	if err != nil {
		log.Fatal("git sync configuration failure", "error", err)
	}

	for _, watch := range watches {
		watch := watch
		runAsync(func() {
			log.Info("starting job definition git sync", "url", watch.Source.URL, "ref", watch.Source.Ref, "once", watch.Once, "interval", watch.Interval)
			opts := git.WatchOptions{Source: watch.Source, Interval: watch.Interval, Once: watch.Once}
			if err := git.Watch(ctx, importer, opts); err != nil && ctx.Err() == nil {
				log.Error("job definition git sync exited", "url", watch.Source.URL, "error", err)
				reportErr(err)
			}
		})
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

		claimer := worker.NewClaimer(vars.NodeAddress, runStore, vars.WorkerLeaseTTL, worker.ParseNodeLabels(vars.NodeLabels)).
			WithRateLimiter(ratelimit.NewLimiter(db.Connection()))
		executorFn := worker.NewRuntimeExecutor(runStore, vars.TaskTimeout, vars.TaskFailurePolicy, resolver)
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

	// Run-owner coordination endpoints live on a dedicated mutually
	// authenticated TLS listener, separate from the public API.  Start it after
	// the worker submitter is wired into the handler (above) so a dispatch can
	// never arrive at a handler with no worker attached.
	if ownerDispatchHandler != nil && ownerInternalServerTLS != nil {
		internalAddr := fmt.Sprintf(":%d", vars.InternalPort)
		internalSrv = dispatch.NewInternalServer(ownerDispatchHandler, internalAddr, ownerInternalServerTLS)
		runAsync(func() {
			log.Info("spinning up internal mTLS listener", "addr", internalAddr)
			if err := internalSrv.Run(ctx); err != nil && ctx.Err() == nil {
				reportErr(err)
			}
		})
	}

	runAsync(func() {
		log.Info("spinning up api")
		if err := api.Start(ctx, bus, authSvc, auditor, limiter, sessions, sso, ssoProviders, internalWakeupHandler); err != nil && ctx.Err() == nil {
			reportErr(err)
		}
	})

	runAsync(func() {
		log.Info("launching execution routine")
		if err := executor.Start(ctx); err != nil && ctx.Err() == nil {
			reportErr(err)
		}
	})

	if distributedWorker != nil {
		runAsync(func() {
			if err := distributedWorker.Run(ctx); err != nil && ctx.Err() == nil {
				reportErr(err)
			}
		})
	}

	select {
	case err := <-errs:
		_ = shutdown()
		return err
	case <-signalCtx.Done():
		log.Info("gracefully shutting down due to OS signal")
		return shutdown()
	}
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

func initSSOProviders(ctx context.Context, vars env.Environment) api.SSOProviders {
	var providers api.SSOProviders

	if vars.AuthOIDCEnabled {
		provider, err := authoidc.New(ctx, authoidc.ConfigFromEnv(vars))
		if err != nil {
			log.Fatal("failed to initialize OIDC provider", "error", err)
		}
		providers.OIDC = provider
	}
	if vars.AuthSAMLEnabled {
		providers.SAML = initSAMLProvider(ctx, vars)
	}
	if vars.AuthLDAPEnabled {
		provider, err := authldap.New(authldap.ConfigFromEnv(vars))
		if err != nil {
			log.Fatal("failed to initialize LDAP provider", "error", err)
		}
		providers.LDAP = provider
	}

	return providers
}

func initSAMLProvider(ctx context.Context, vars env.Environment) auth.RedirectAuthenticator {
	cfg := authsaml.ConfigFromEnv(vars)
	cfg.ReplayCache = authsaml.NewReplayStore(db.Connection())
	provider, err := authsaml.New(ctx, cfg)
	if err != nil {
		log.Fatal("failed to initialize SAML provider", "error", err)
	}
	return provider
}

// initAuth sets up authentication services based on CAESIUM_AUTH_MODE.
// Returns nil services when auth is disabled so callers can pass them through safely.
func initAuth(ctx context.Context, vars env.Environment, runAsync func(func())) (*auth.Service, *auth.AuditLogger, *auth.RateLimiter, *auth.SessionStore, *auth.SSOService) {
	conn := db.Connection()
	authSvc := auth.NewService(conn, auth.WithKeyHashSecret(vars.AuthKeyHashSecret))
	auditor := auth.NewAuditLogger(conn)
	limiter := auth.NewRateLimiter(vars.AuthRateLimitPerMinute, time.Minute)
	var sessions *auth.SessionStore
	var sso *auth.SSOService

	switch vars.AuthMode {
	case "none", "":
		if vars.SSOEnabled() {
			log.Fatal("SSO providers require authentication to be enabled (set CAESIUM_AUTH_MODE=api-key)")
		}
		log.Warn("authentication is disabled — all endpoints are publicly accessible")
		return authSvc, auditor, limiter, nil, nil

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
			hasProxy, err := hasTrustedProxyTLSPath(vars.TrustedProxies)
			if err != nil {
				log.Fatal("invalid CAESIUM_TRUSTED_PROXIES", "error", err)
			}
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

		sessions = auth.NewSessionStore(conn,
			auth.WithSessionHashSecret(vars.AuthKeyHashSecret),
			auth.WithSessionTTLs(vars.AuthSessionIdleTTL, vars.AuthSessionAbsoluteTTL),
		)
		if vars.SSOEnabled() {
			mapper, err := auth.NewRoleMapper(vars.AuthRoleMapping, vars.AuthDefaultRole)
			if err != nil {
				log.Fatal("invalid CAESIUM_AUTH_ROLE_MAPPING", "error", err)
			}
			sso = auth.NewSSOService(auth.NewUserStore(conn), sessions, mapper, auth.WithSSOAuditLogger(auditor))
			runAsync(func() {
				sessions.RunLastSeenFlusher(ctx)
			})
			runAsync(func() {
				sessions.RunReaper(ctx)
			})
		}

		runAsync(func() {
			authSvc.RunLastUsedFlusher(ctx)
		})
		runAsync(func() {
			limiter.RunCleanup(ctx.Done())
		})
		return authSvc, auditor, limiter, sessions, sso

	default:
		log.Fatal("unknown CAESIUM_AUTH_MODE value", "mode", vars.AuthMode)
		return nil, nil, nil, nil, nil // unreachable
	}
}

func hasTrustedProxyTLSPath(raw string) (bool, error) {
	if strings.TrimSpace(raw) == "" {
		return false, nil
	}
	ranges, err := authmw.ParseTrustedProxyRangesStrict(raw)
	if err != nil {
		return false, err
	}
	return len(ranges) > 0, nil
}
