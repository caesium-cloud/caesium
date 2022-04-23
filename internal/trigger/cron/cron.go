package cron

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	jsvc "github.com/caesium-cloud/caesium/api/rest/service/job"
	"github.com/caesium-cloud/caesium/internal/job"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/trigger"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
	"github.com/robfig/cron"
)

type Cron struct {
	trigger.Trigger
	schedule cron.Schedule
	id       uuid.UUID
}

func New(t *models.Trigger) (*Cron, error) {
	if t.Type != models.TriggerTypeCron {
		return nil, fmt.Errorf("trigger is %v not %v", t.Type, models.TriggerTypeCron)
	}

	m := map[string]interface{}{}

	if err := json.Unmarshal([]byte(t.Configuration), &m); err != nil {
		return nil, err
	}

	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	sched, err := parser.Parse(m["expression"].(string))
	if err != nil {
		return nil, err
	}

	return &Cron{schedule: sched, id: uuid.MustParse(t.ID)}, nil
}

func (c *Cron) Listen(ctx context.Context) {
	log.Info("trigger listening", "id", c.id)

	select {
	case <-time.After(time.Until(c.schedule.Next(time.Now()))):
		if err := c.Fire(ctx); err != nil {
			log.Error("trigger fire failure", "id", c.id, "error", err)
		}
	case <-ctx.Done():
		return
	}
}

func (c *Cron) Fire(ctx context.Context) error {
	return trigger.Fire(ctx, c.id)
}

func (c *Cron) ID() uuid.UUID {
	return c.id
}
