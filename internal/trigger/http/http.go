package http

import (
	"context"
	"fmt"

	jsvc "github.com/caesium-cloud/caesium/api/rest/service/job"
	"github.com/caesium-cloud/caesium/internal/job"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/trigger"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
)

type HTTP struct {
	trigger.Trigger
	id uuid.UUID
}

func New(t *models.Trigger) (*HTTP, error) {
	if t.Type != models.TriggerTypeHTTP {
		return nil, fmt.Errorf("trigger is %v not %v", t.Type, models.TriggerTypeHTTP)
	}

	return &HTTP{id: uuid.MustParse(t.ID)}, nil
}

func (h *HTTP) Listen(ctx context.Context) {
	log.Info(
		"trigger listening",
		"id", h.id,
		"type", models.TriggerTypeHTTP)

	h.Fire(ctx)
}

func (h *HTTP) Fire(ctx context.Context) error {
	log.Info(
		"trigger firing",
		"id", h.id,
		"type", models.TriggerTypeHTTP)

	req := &jsvc.ListRequest{TriggerID: h.id.String()}

	jobs, err := jsvc.Service(ctx).List(req)
	if err != nil {
		return err
	}

	log.Info("running jobs", "count", len(jobs))

	for _, j := range jobs {
		go func() {
			if err = job.New(j).Run(ctx); err != nil {
				log.Error("job run failure", "id", j.ID, "error", err)
			}
		}()
	}

	return nil
}

func (h *HTTP) ID() uuid.UUID {
	return h.id
}
