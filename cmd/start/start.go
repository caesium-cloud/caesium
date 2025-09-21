package start

import (
	"context"
	"os"
	"os/signal"
	"runtime/pprof"
	"syscall"

	"github.com/caesium-cloud/caesium/api"
	"github.com/caesium-cloud/caesium/internal/executor"
	"github.com/caesium-cloud/caesium/internal/jobdef"
	"github.com/caesium-cloud/caesium/internal/jobdef/git"
	"github.com/caesium-cloud/caesium/internal/jobdef/runtime"
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

	vars := env.Variables()
	importer := jobdef.NewImporter(db.Connection())
	resolver, err := runtime.BuildSecretResolver(vars)
	if err != nil {
		log.Fatal("secret resolver configuration failure", "error", err)
	}

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
		errs <- api.Start(ctx)
	}()

	go func() {
		log.Info("launching execution routine")
		errs <- executor.Start(ctx)
	}()

	defer shutdown()

	return <-errs
}

func shutdown() {
	if cancel != nil {
		cancel()
	}
	if err := api.Shutdown(); err != nil {
		log.Error("api shutdown failure", "error", err)
	}
}
