package trigger

import (
	"context"

	jsvc "github.com/caesium-cloud/caesium/api/rest/service/job"
	"github.com/caesium-cloud/caesium/internal/job"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
)

// Trigger
type Trigger interface {
	Listen(context.Context)
	Fire(context.Context) error
	ID() uuid.UUID
}

func Fire(ctx context.Context, id uuid.UUID) error {
	log.Info("firing trigger", "id", id)

	req := &jsvc.ListRequest{TriggerID: id.String()}

	jobs, err := jsvc.Service(ctx).List(req)
	if err != nil {
		return err
	}

	log.Info("running jobs", "count", len(jobs))

	for _, j := range jobs {
		go func(j *models.Job) {
			if err = job.New(j).Run(ctx); err != nil {
				log.Error("job run failure", "id", j.ID, "error", err)
			}
		}(j)
	}

	return nil
}
