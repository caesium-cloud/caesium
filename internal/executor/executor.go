package executor

import (
	"context"
	"sync"
	"time"

	"github.com/caesium-cloud/caesium/api/rest/service/trigger"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/trigger/cron"
	"github.com/caesium-cloud/caesium/pkg/log"
)

type Executor struct {
	m sync.Map
}

func (e *Executor) Queue(ctx context.Context, c *cron.Cron) {
	triggerCtx, cancel := context.WithCancel(ctx)
	_, loaded := e.m.LoadOrStore(c.ID(), cancel)
	if !loaded {
		go c.Listen(triggerCtx)
	}
}

func Start(ctx context.Context) error {
	var (
		e   Executor
		t   = time.NewTicker(time.Minute)
		req = &trigger.ListRequest{
			Type: string(models.TriggerTypeCron),
		}
	)

	for {
		select {
		case <-t.C:
			if err := queueTriggers(ctx, req, e); err != nil {
				log.Error("trigger queue failure", "error", err)
				return err
			}
			continue
		case <-ctx.Done():
			return nil
		}
	}
}

func queueTriggers(ctx context.Context, req *trigger.ListRequest, e Executor) error {
	triggers, err := trigger.Service(ctx).List(req)
	if err != nil {
		return err
	}

	log.Info("queueing triggers", "count", len(triggers))

	for _, trig := range triggers {
		if c, err := cron.New(trig); err == nil {
			e.Queue(ctx, c)
		} else {
			return err
		}
	}

	return nil
}
