package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	stdhttp "net/http"
	"strings"

	jsvc "github.com/caesium-cloud/caesium/api/rest/service/job"
	"github.com/caesium-cloud/caesium/internal/job"
	"github.com/caesium-cloud/caesium/internal/jobdef/secret"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/trigger"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
)

type HTTP struct {
	trigger.Trigger
	id     uuid.UUID
	config Config
}

var ErrInvalidSignature = errors.New("invalid signature")

func New(t *models.Trigger) (*HTTP, error) {
	if t.Type != models.TriggerTypeHTTP {
		return nil, fmt.Errorf("trigger is %v not %v", t.Type, models.TriggerTypeHTTP)
	}

	cfg, err := parseConfig(t.Configuration)
	if err != nil {
		return nil, err
	}

	return &HTTP{id: t.ID, config: cfg}, nil
}

func (h *HTTP) Listen(ctx context.Context) {
	log.Info(
		"trigger listening",
		"id", h.id,
		"type", models.TriggerTypeHTTP)

	if err := h.Fire(ctx); err != nil {
		log.Error("trigger fire failure", "id", h.id, "error", err)
	}
}

func (h *HTTP) Fire(ctx context.Context) error {
	return h.FireWithParams(ctx, nil)
}

func (h *HTTP) FireWithParams(ctx context.Context, params map[string]string) error {
	log.Info(
		"trigger firing",
		"id", h.id,
		"type", models.TriggerTypeHTTP)

	jobs, err := listJobs(ctx, h.id.String())
	if err != nil {
		return err
	}

	log.Info("running jobs", "count", len(jobs))

	mergedParams := h.config.mergedParams(params)

	for _, j := range jobs {
		jobModel := j
		if jobModel == nil {
			log.Warn("skipping nil job", "trigger_id", h.id)
			continue
		}
		if jobModel.Paused {
			log.Info("skipping paused job", "id", j.ID)
			continue
		}
		runtimeParams := cloneParams(mergedParams)
		go func(jobModel *models.Job, params map[string]string) {
			if err := runJob(context.WithoutCancel(ctx), jobModel, params); err != nil {
				log.Error("job run failure", "id", jobModel.ID, "error", err)
			}
		}(jobModel, runtimeParams)
	}

	return nil
}

func (h *HTTP) Path() string {
	return h.config.Path
}

func (h *HTTP) MergeParams(params map[string]string) map[string]string {
	return h.config.mergedParams(params)
}

func (h *HTTP) ExtractWebhookParams(ctx context.Context, req *stdhttp.Request, body []byte) (map[string]string, error) {
	resolvedSecret, err := resolveSecret(ctx, h.config.Secret)
	if err != nil {
		return nil, err
	}
	if !validateSignature(req, body, resolvedSecret, h.config.SignatureScheme, h.config.SignatureHeader) {
		return nil, ErrInvalidSignature
	}
	return extractParams(body, h.config.ParamMapping), nil
}

func (h *HTTP) ID() uuid.UUID {
	return h.id
}

var (
	secretResolver secret.Resolver

	listJobs = func(ctx context.Context, triggerID string) (models.Jobs, error) {
		req := &jsvc.ListRequest{TriggerID: triggerID}
		return jsvc.Service(ctx).List(req)
	}

	runJob = func(ctx context.Context, j *models.Job, params map[string]string) error {
		if j == nil {
			return fmt.Errorf("job is nil")
		}
		return job.New(j, job.WithParams(params)).Run(ctx)
	}
)

func SetSecretResolver(resolver secret.Resolver) {
	secretResolver = resolver
}

func cloneParams(params map[string]string) map[string]string {
	if len(params) == 0 {
		return map[string]string{}
	}
	out := make(map[string]string, len(params))
	for k, v := range params {
		out[k] = v
	}
	return out
}

func parseConfig(raw string) (Config, error) {
	cfg := Config{}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return cfg.withDefaults(), nil
	}
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return Config{}, fmt.Errorf("parse trigger configuration: %w", err)
	}
	return cfg.withDefaults(), nil
}

func resolveSecret(ctx context.Context, value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || !strings.HasPrefix(value, "secret://") {
		return value, nil
	}
	if secretResolver == nil {
		return "", fmt.Errorf("secret resolver is not configured")
	}
	return secretResolver.Resolve(ctx, value)
}
