package start

import (
	"crypto/tls"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/pprof"
	"strings"
	"syscall"

	"github.com/caesium-cloud/caesium/api"
	"github.com/caesium-cloud/caesium/db/cluster"
	"github.com/caesium-cloud/caesium/db/store"
	"github.com/caesium-cloud/caesium/db/tcp"
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
		SuggestFor: []string{"boot", "up", "run", "begin"},
		Example:    example,
		RunE:       start,
	}
)

func start(cmd *cobra.Command, args []string) error {
	signalChan := make(chan os.Signal)
	go func() {
		for s := range signalChan {
			switch s {
			case syscall.SIGUSR1:
				log.Info("dumping stack traces due to SIGUSR1 signal")
				pprof.Lookup("goroutine").WriteTo(os.Stdout, 1)
			case syscall.SIGINT:
				log.Info("gracefully shutting down due to SIGINT signal")
				shutdown()
				os.Exit(0)
			}
		}
	}()
	signal.Notify(signalChan, syscall.SIGUSR1)
	signal.Notify(signalChan, syscall.SIGINT)

	errs := make(chan error)

	go func() {
		log.Info("clusterizing caesium")
		errs <- clusterize()
	}()

	go func() {
		log.Info("spinning up api")
		errs <- api.Start()
	}()

	defer shutdown()

	return <-errs
}

func shutdown() {
	api.Shutdown()
	store.GlobalStore().Close(true)
}

func clusterize() error {
	dbPath := env.Variables().DBPath

	// Create internode network layer.
	tn := tcp.NewTransport()
	if err := tn.Open(env.Variables().RaftAddr); err != nil {
		log.Fatal("failed to open inter-node network", "error", err)
	}

	// Create and open the store.
	dbPath, err := filepath.Abs(dbPath)
	if err != nil {
		log.Fatal("failed to determine absolute data path", "error", err)
	}
	dbConf := store.NewDBConfig(env.Variables().DSN, !env.Variables().OnDisk)

	store.NewGlobal(tn, &store.StoreConfig{
		DBConf: dbConf,
		Dir:    dbPath,
		ID:     idOrRaftAddr(),
	})
	s := store.GlobalStore()

	// Set optional parameters on store.
	s.SetRequestCompression(
		env.Variables().CompressionBatch,
		env.Variables().CompressionSize,
	)
	s.RaftLogLevel = env.Variables().RaftLogLevel
	s.ShutdownOnRemove = env.Variables().RaftShutdownOrRemove
	s.SnapshotThreshold = env.Variables().RaftSnapThreshold
	s.SnapshotInterval = env.Variables().RaftSnapInterval
	s.LeaderLeaseTimeout = env.Variables().RaftLeaderLeaseTimeout
	s.HeartbeatTimeout = env.Variables().RaftHeartbeatTimeout
	s.ElectionTimeout = env.Variables().RaftElectionTimeout
	s.ApplyTimeout = env.Variables().RaftApplyTimeout

	// Any prexisting node state?
	var enableBootstrap bool
	isNew := store.IsNewNode(dbPath)
	if isNew {
		log.Info("node bootstrapping", "path", dbPath)
		enableBootstrap = true // New node, so we may be bootstrapping
	} else {
		log.Info("preexisting node state detected", "path", dbPath)
	}

	// Determine join addresses
	var joins []string
	joins, err = determineJoinAddresses()
	if err != nil {
		log.Fatal("failed to determine join addresses", "error", err)
	}

	// Supplying join addresses means bootstrapping a new cluster won't
	// be required.
	if len(joins) > 0 {
		enableBootstrap = false
		log.Info("join addresses specified, node is not bootstrapping")
	} else {
		log.Info("no join addresses set")
	}

	// Join address supplied, but we don't need them!
	if !isNew && len(joins) > 0 {
		log.Info("node is already member of cluster, ignoring join addresses")
	}

	// Now, open store.
	if err := s.Open(enableBootstrap); err != nil {
		log.Fatal("failed to open raft store", "error", err)
	}

	// Prepare metadata for join command.
	apiAdv := env.Variables().HttpAddr
	if env.Variables().HttpAdvAddr != "" {
		apiAdv = env.Variables().HttpAdvAddr
	}
	apiProto := "http"

	meta := map[string]string{
		"api_addr":  apiAdv,
		"api_proto": apiProto,
	}

	// Execute any requested join operation.
	if len(joins) > 0 && isNew {
		advAddr := env.Variables().RaftAddr
		if env.Variables().RaftAdvAddr != "" {
			advAddr = env.Variables().RaftAdvAddr
		}

		tlsConfig := tls.Config{InsecureSkipVerify: true}

		if j, err := cluster.Join(&cluster.JoinRequest{
			SourceIP:    env.Variables().JoinSrcIP,
			JoinAddress: joins,
			ID:          s.ID(),
			Address:     advAddr,
			Voter:       !env.Variables().RaftNonVoter,
			Metadata:    meta,
			TLSConfig:   &tlsConfig,
		}); err != nil {
			log.Fatal("failed to join cluster", "addresses", joins, "error", err)
		} else {
			log.Info("successfully joined cluster", "address", j)
		}

	}

	// Wait until the store is in full consensus.
	if err := waitForConsensus(s); err != nil {
		log.Fatal("failed to wait for consensus", "error", err)
	}

	// This may be a standalone server. In that case set its own metadata.
	if err := s.SetMetadata(meta); err != nil && err != store.ErrNotLeader {
		// Non-leader errors are OK, since metadata will then be set through
		// consensus as a result of a join. All other errors indicate a problem.
		log.Fatal("failed to set store metadata", "error", err)
	}

	log.Info("node is ready")

	// Block until signalled.
	terminate := make(chan os.Signal, 1)
	signal.Notify(terminate, os.Interrupt)
	<-terminate
	if err := s.Close(true); err != nil {
		log.Info("failed to close store", "error", err)
	}

	log.Info("caesium server stopped")

	return err
}

func determineJoinAddresses() ([]string, error) {
	var addrs []string
	if env.Variables().JoinAddr != "" {
		// Explicit join addresses are first priority.
		addrs = strings.Split(env.Variables().JoinAddr, ",")
	}

	return addrs, nil
}

func waitForConsensus(s *store.Store) error {
	if _, err := s.WaitForLeader(env.Variables().RaftOpenTimeout); err != nil {
		if env.Variables().RaftLeaderWait {
			return fmt.Errorf("leader did not appear within timeout: %s", err.Error())
		}
		log.Info("ignoring error while waiting for leader")
	}
	if env.Variables().RaftOpenTimeout != 0 {
		if err := s.WaitForApplied(env.Variables().RaftOpenTimeout); err != nil {
			return fmt.Errorf("log was not fully applied within timeout: %s", err.Error())
		}
	} else {
		log.Info("not waiting for logs to be applied")
	}
	return nil
}

func idOrRaftAddr() string {
	if env.Variables().NodeID != "" {
		return env.Variables().NodeID
	}
	if env.Variables().RaftAdvAddr == "" {
		return env.Variables().RaftAddr
	}
	return env.Variables().RaftAdvAddr
}
