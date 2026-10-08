package protocol

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

// RunWithSignalContext cancels role work on the first termination signal and
// restores default handling while that work winds down, allowing a force stop.
func RunWithSignalContext(name string, fn func(context.Context, *Emitter) error) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	stopOnCancel := context.AfterFunc(ctx, stop)
	Run(name, func(e *Emitter) error {
		err := fn(ctx, e)
		if err == nil {
			err = ctx.Err()
		}
		// Run may call os.Exit, which skips cleanup deferred by its caller.
		stopOnCancel()
		stop()
		return err
	})
}
