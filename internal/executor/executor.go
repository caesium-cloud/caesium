package executor

import (
	"context"
	"sync"
	"time"

	tsvc "github.com/caesium-cloud/caesium/api/rest/service/trigger"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/trigger"
	"github.com/caesium-cloud/caesium/internal/trigger/cron"
	"github.com/caesium-cloud/caesium/internal/trigger/http"
	"github.com/caesium-cloud/caesium/pkg/log"
)

type Executor struct {
	m sync.Map
}

func (e *Executor) Queue(ctx context.Context, t trigger.Trigger) {
	triggerCtx, cancel := context.WithCancel(ctx)
	_, loaded := e.m.LoadOrStore(t.ID(), cancel)
	if !loaded {
		go t.Listen(triggerCtx)
	}
}

func Start(ctx context.Context) error {
	var (
		e   Executor
		t   = time.NewTicker(time.Minute)
		req = &tsvc.ListRequest{}
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

func queueTriggers(ctx context.Context, req *tsvc.ListRequest, e Executor) error {
	triggers, err := tsvc.Service(ctx).List(req)
	if err != nil {
		return err
	}

	log.Info("queueing triggers", "count", len(triggers))

	for _, trig := range triggers {
		switch trig.Type {
		case models.TriggerTypeCron:
			if c, err := cron.New(trig); err == nil {
				e.Queue(ctx, c)
			} else {
				return err
			}
		case models.TriggerTypeHTTP:
			if h, err := http.New(trig); err == nil {
				e.Queue(ctx, h)
			}
		}

	}

	return nil
}
