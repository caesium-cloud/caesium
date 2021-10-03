package job

import (
	"context"
	"fmt"
	"time"

	asvc "github.com/caesium-cloud/caesium/api/rest/service/atom"
	"github.com/caesium-cloud/caesium/api/rest/service/task"
	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/caesium-cloud/caesium/internal/atom/docker"
	"github.com/caesium-cloud/caesium/internal/atom/kubernetes"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
)

// Job
type Job interface {
	Run(ctx context.Context) error
}

type job struct {
	id uuid.UUID
}

func New(j *models.Job) Job {
	return &job{id: j.ID}
}

type atomRunner struct {
	image   string
	command []string
	engine  atom.Engine
	nextID  *uuid.UUID
}

func (j *job) Run(ctx context.Context) error {
	tasks, err := task.Service().List(&task.ListRequest{OrderBy: []string{"next_id"}})
	if err != nil {
		return err
	}

	log.Info("running tasks", "count", len(tasks))

	m := make(map[uuid.UUID]*atomRunner, len(tasks))

	svc := asvc.Service()

	for _, t := range tasks {
		a, err := svc.Get(t.AtomID)
		if err != nil {
			return err
		}

		runner := &atomRunner{
			image:   a.Image,
			command: a.Cmd(),
			nextID:  t.NextID,
		}

		log.Info("evaluating task atom", "engine", a.Engine, "id", a.ID)

		switch a.Engine {
		case models.AtomEngineDocker:
			runner.engine = docker.NewEngine(ctx)
		case models.AtomEngineKubernetes:
			runner.engine = kubernetes.NewEngine(ctx)
		default:
			return fmt.Errorf("unable to run atom with engine: %v", a.Engine)
		}

		m[t.ID] = runner
	}

	taskID := tasks[0].ID

	for {
		r := m[taskID]

		log.Info("running atom", "image", r.image, "cmd", r.command)

		a, err := r.engine.Create(&atom.EngineCreateRequest{
			Name:    taskID.String(),
			Image:   r.image,
			Command: r.command,
		})
		if err != nil {
			return err
		}

		// TODO: stream logs somewhere
		// r.engine.Logs(&atom.EngineLogsRequest{ID: a.ID()})

		// TODO: handle errors, properly cleanup,
		// make timer configurable, many things...
		f := func() error {
			for {
				select {
				case <-ctx.Done():
					return nil
				case <-time.After(5 * time.Second):
					a, err = r.engine.Get(&atom.EngineGetRequest{
						ID: a.ID(),
					})
					if err != nil {
						return err
					}

					if !a.StoppedAt().IsZero() {
						log.Info("atom finished", "id", a.ID(), "result", a.Result())

						return r.engine.Stop(
							&atom.EngineStopRequest{
								ID:    a.ID(),
								Force: true,
							})
					}

					log.Info("atom running", "id", a.ID(), "state", a.State())
				}
			}
		}

		if err = f(); err != nil {
			return err
		}

		if r.nextID == nil {
			break
		} else {
			taskID = *r.nextID
		}
	}

	return nil
}
