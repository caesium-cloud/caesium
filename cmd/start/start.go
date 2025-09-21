package start

import (
	"context"
	"os"
	"os/signal"
	"runtime/pprof"
	"syscall"

	"github.com/caesium-cloud/caesium/api"
	"github.com/caesium-cloud/caesium/internal/executor"
	"github.com/caesium-cloud/caesium/pkg/db"
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

func start(cmd *cobra.Command, args []string) error {
	signalChan := make(chan os.Signal, 1)

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

	signal.Notify(signalChan, syscall.SIGUSR1, syscall.SIGINT)

	var (
		errs = make(chan error)
		ctx  = context.Background()
	)

	log.Info("migrating database")
	if err := db.Migrate(); err != nil {
		log.Fatal("database migration failure", "error", err)
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
	api.Shutdown()
}
