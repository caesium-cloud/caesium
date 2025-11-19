package cron

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
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
	location *time.Location
}

func New(t *models.Trigger) (*Cron, error) {
	if t.Type != models.TriggerTypeCron {
		return nil, fmt.Errorf(
			"trigger is %v not %v",
			t.Type,
			models.TriggerTypeCron)
	}

	m := map[string]interface{}{}

	if err := json.Unmarshal([]byte(t.Configuration), &m); err != nil {
		return nil, err
	}

	expr, err := extractExpression(m)
	if err != nil {
		return nil, err
	}

	loc, err := extractLocation(m)
	if err != nil {
		return nil, err
	}

	parser := cron.NewParser(
		cron.Minute |
			cron.Hour |
			cron.Dom |
			cron.Month |
			cron.Dow,
	)

	sched, err := parser.Parse(expr)
	if err != nil {
		return nil, err
	}

	return &Cron{schedule: sched, id: t.ID, location: loc}, nil
}

func (c *Cron) Listen(ctx context.Context) {
	log.Info(
		"trigger listening",
		"id", c.id,
		"type", models.TriggerTypeCron,
	)

	select {
	case <-time.After(time.Until(c.nextTick())):
		if err := c.Fire(ctx); err != nil {
			log.Error("trigger fire failure", "id", c.id, "error", err)
		}
	case <-ctx.Done():
		return
	}
}

func (c *Cron) Fire(ctx context.Context) error {
	log.Info(
		"trigger firing",
		"id", c.id,
		"type", models.TriggerTypeCron,
	)

	req := &jsvc.ListRequest{TriggerID: c.id.String()}

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

func (c *Cron) ID() uuid.UUID {
	return c.id
}

func extractExpression(cfg map[string]interface{}) (string, error) {
	candidates := []string{"expression", "cron", "schedule"}
	for _, key := range candidates {
		if raw, ok := cfg[key]; ok && raw != nil {
			if expr, ok := raw.(string); ok && strings.TrimSpace(expr) != "" {
				return expr, nil
			}
		}
	}
	return "", fmt.Errorf("cron trigger configuration missing expression/cron field")
}

func extractLocation(cfg map[string]interface{}) (*time.Location, error) {
	raw, ok := cfg["timezone"]
	if !ok || raw == nil {
		return nil, nil
	}

	switch tz := raw.(type) {
	case string:
		if strings.TrimSpace(tz) == "" {
			return nil, nil
		}
		loc, err := time.LoadLocation(tz)
		if err != nil {
			return nil, fmt.Errorf("invalid timezone %q: %w", tz, err)
		}
		return loc, nil
	default:
		return nil, fmt.Errorf("timezone must be a string")
	}
}

func (c *Cron) nextTick() time.Time {
	base := time.Now()
	if c.location != nil {
		base = base.In(c.location)
	}
	return c.schedule.Next(base)
}
