package cron

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	jsvc "github.com/caesium-cloud/caesium/api/rest/service/job"
	"github.com/caesium-cloud/caesium/internal/job"
	"github.com/caesium-cloud/caesium/internal/metrics"
	"github.com/caesium-cloud/caesium/internal/models"
	runstore "github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/internal/trigger"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
	"github.com/robfig/cron"
)

type Cron struct {
	trigger.Trigger
	schedule      cron.Schedule
	id            uuid.UUID
	location      *time.Location
	defaultParams map[string]string
	catchup       bool
	catchupOnce   sync.Once
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

	defaultParams, err := extractDefaultParams(m)
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

	catchup, err := extractCatchup(m)
	if err != nil {
		return nil, err
	}

	return &Cron{schedule: sched, id: t.ID, location: loc, defaultParams: defaultParams, catchup: catchup}, nil
}

func (c *Cron) Listen(ctx context.Context) {
	log.Info(
		"trigger listening",
		"id", c.id,
		"type", models.TriggerTypeCron,
	)

	next := c.nextTick()
	if next.IsZero() {
		log.Warn("trigger has no future occurrence, skipping", "id", c.id)
		<-ctx.Done()
		return
	}

	select {
	case <-time.After(time.Until(next)):
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

	// On first fire after startup, queue any missed runs for catchup-enabled jobs.
	if c.catchup {
		c.catchupOnce.Do(func() {
			c.fireCatchup(ctx, jobs)
		})
	}

	for _, j := range jobs {
		if j.Paused {
			log.Info("skipping paused job", "id", j.ID)
			continue
		}
		metrics.TriggerFiresTotal.WithLabelValues(j.ID.String(), string(models.TriggerTypeCron)).Inc()
		params := c.defaultParams
		go func() {
			if err = job.New(j, job.WithParams(params)).Run(ctx); err != nil {
				log.Error("job run failure", "id", j.ID, "error", err)
			}
		}()
	}

	return nil
}

// fireCatchup enumerates missed fire times since the last successful run for
// each job and queues them as runs with logical_date and is_catchup params.
func (c *Cron) fireCatchup(ctx context.Context, jobs models.Jobs) {
	rStore := runstore.Default()
	now := time.Now().UTC()

	for _, j := range jobs {
		if j.Paused {
			continue
		}

		latest, err := rStore.Latest(j.ID)
		if err != nil || latest == nil {
			// No prior runs — nothing to catch up.
			continue
		}

		since := latest.StartedAt
		missed := job.EnumerateLogicalDates(c.schedule, since, now)
		if len(missed) == 0 {
			continue
		}

		log.Info("catchup: queuing missed runs",
			"job_id", j.ID,
			"count", len(missed),
			"since", since,
		)

		for _, d := range missed {
			logicalDate := d.UTC().Format(time.RFC3339)
			params := map[string]string{
				"logical_date": logicalDate,
				"is_catchup":   "true",
			}
			for k, v := range c.defaultParams {
				if _, exists := params[k]; !exists {
					params[k] = v
				}
			}

			metrics.TriggerFiresTotal.WithLabelValues(j.ID.String(), string(models.TriggerTypeCron)).Inc()
			capturedJob := j
			capturedParams := params
			capturedLD := logicalDate
			go func() {
				if err := job.New(capturedJob, job.WithParams(capturedParams)).Run(ctx); err != nil {
					log.Error("catchup run failure", "job_id", capturedJob.ID, "logical_date", capturedLD, "error", err)
				}
			}()
		}
	}
}

func (c *Cron) ID() uuid.UUID {
	return c.id
}

// ParseSchedule parses the cron schedule from a trigger's Configuration JSON
// string. It is exported so that callers (e.g. the backfill API handler) can
// enumerate fire times without instantiating a full Cron trigger.
func ParseSchedule(configuration string) (cron.Schedule, error) {
	m := map[string]interface{}{}
	if err := json.Unmarshal([]byte(configuration), &m); err != nil {
		return nil, fmt.Errorf("cron: invalid trigger configuration: %w", err)
	}

	expr, err := extractExpression(m)
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

	return parser.Parse(expr)
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

func extractCatchup(cfg map[string]interface{}) (bool, error) {
	raw, ok := cfg["catchup"]
	if !ok || raw == nil {
		return false, nil
	}
	switch v := raw.(type) {
	case bool:
		return v, nil
	default:
		return false, fmt.Errorf("catchup must be a boolean")
	}
}

func extractDefaultParams(cfg map[string]interface{}) (map[string]string, error) {
	raw, ok := cfg["defaultParams"]
	if !ok || raw == nil {
		return nil, nil
	}

	switch v := raw.(type) {
	case map[string]interface{}:
		out := make(map[string]string, len(v))
		for key, val := range v {
			switch s := val.(type) {
			case string:
				out[key] = s
			default:
				out[key] = fmt.Sprintf("%v", val)
			}
		}
		return out, nil
	default:
		return nil, fmt.Errorf("defaultParams must be a map of string keys and string values")
	}
}

func (c *Cron) nextTick() time.Time {
	base := time.Now()
	if c.location != nil {
		base = base.In(c.location)
	}
	return c.schedule.Next(base)
}
