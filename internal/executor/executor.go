package executor

import (
	"context"
	"sync"
	"time"

	svc "github.com/caesium-cloud/caesium/api/rest/service/trigger"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/trigger"
	"github.com/caesium-cloud/caesium/internal/trigger/cron"
	"github.com/caesium-cloud/caesium/pkg/log"
)

var (
	exec Executor
)

type Executor struct {
	m sync.Map
}

func Queue(ctx context.Context, t trigger.Trigger) {
	exec.queue(ctx, t)
}

func (e *Executor) queue(ctx context.Context, t trigger.Trigger) {
	triggerCtx, cancel := context.WithCancel(ctx)
	_, loaded := e.m.LoadOrStore(t.ID(), cancel)
	if !loaded {
		go t.Listen(triggerCtx)
	}
}

func Start(ctx context.Context) error {
	var (
		t   = time.NewTicker(time.Minute)
		req = &svc.ListRequest{
			Type: string(models.TriggerTypeCron),
		}
	)

	for {
		select {
		case <-t.C:
			if err := queueTriggers(ctx, req); err != nil {
				log.Error("trigger queue failure", "error", err)
				return err
			}
			continue
		case <-ctx.Done():
			return nil
		}
	}
}

func queueTriggers(ctx context.Context, req *svc.ListRequest) error {
	triggers, err := svc.Service(ctx).List(req)
	if err != nil {
		return err
	}

	log.Info("queueing triggers", "count", len(triggers))

	for _, trig := range triggers {
		if c, err := cron.New(trig); err == nil {
			exec.queue(ctx, c)
		} else {
			return err
		}
	}

	return nil
}
