package http

import (
	"context"
	"fmt"

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
	log.Info("trigger listening", "id", h.id)

	select {
	// case <-time.After(time.Until(h.schedule.Next(time.Now()))):
	// 	if err := c.Fire(ctx); err != nil {
	// 		log.Error("trigger fire failure", "id", c.id, "error", err)
	// 	}
	case <-ctx.Done():
		return
	}
}

func (h *HTTP) Fire(ctx context.Context) error {
	return trigger.Fire(ctx, h.id)
}

func (h *HTTP) ID() uuid.UUID {
	return h.id
}
