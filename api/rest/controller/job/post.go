package job

import (
	"fmt"
	"net/http"

	"github.com/caesium-cloud/caesium/api/rest/service/atom"
	"github.com/caesium-cloud/caesium/api/rest/service/job"
	"github.com/caesium-cloud/caesium/api/rest/service/task"
	"github.com/caesium-cloud/caesium/api/rest/service/taskedge"
	"github.com/caesium-cloud/caesium/api/rest/service/trigger"
	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/db"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/labstack/echo/v5"
	"gorm.io/gorm"
)

func Post(c *echo.Context) error {
	req := &PostRequest{}
	if err := c.Bind(req); err != nil {
		return err
	}

	if req.Trigger == nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(fmt.Errorf("trigger is required"))
	}

	// Apply the whole job — trigger, job, atoms, tasks, edges — in one
	// transaction so it commits atomically in a single dqlite/Raft round-trip
	// (instead of ~6-8 autocommit writes) and never leaves a partial job behind
	// if a later step fails. Defer bus dispatch of the TypeJobCreated event
	// (emitted by job.Create) so the BusDispatcher delivers it only after the
	// transaction commits — never for a rolled-back or retried attempt.
	ctx := event.WithDeferredBusDispatch(c.Request().Context())

	var created *models.Job
	err := db.Transaction(ctx, func(tx *gorm.DB) error {
		var (
			tsvc          = task.Service(ctx).WithDatabase(tx)
			asvc          = atom.Service(ctx).WithDatabase(tx)
			createdTasks  []string
			explicitEdges []edgeSpec
		)

		trig, err := trigger.Service(ctx).WithDatabase(tx).Create(req.Trigger)
		if err != nil {
			return err
		}

		j, err := job.Service(ctx).WithDatabase(tx).Create(&job.CreateRequest{
			TriggerID:   trig.ID,
			Alias:       req.Alias,
			Labels:      metadataLabels(req.Metadata),
			Annotations: metadataAnnotations(req.Metadata),
		})
		if err != nil {
			return err
		}
		for _, t := range req.Tasks {
			a, err := asvc.Create(&atom.CreateRequest{
				Engine:  t.Atom.Engine,
				Image:   t.Atom.Image,
				Command: t.Atom.Command,
				Spec:    t.Atom.Spec,
			})
			if err != nil {
				return err
			}

			taskModel, err := tsvc.Create(&task.CreateRequest{
				JobID:        j.ID.String(),
				AtomID:       a.ID.String(),
				NodeSelector: t.NodeSelector,
			})
			if err != nil {
				return err
			}

			createdTasks = append(createdTasks, taskModel.ID.String())
			for _, dep := range t.DependsOn {
				explicitEdges = append(explicitEdges, edgeSpec{
					from: dep,
					to:   taskModel.ID.String(),
				})
			}
		}

		edgeSvc := taskedge.Service(ctx).WithDatabase(tx)
		switch {
		case len(explicitEdges) > 0:
			for _, edge := range explicitEdges {
				if _, err := edgeSvc.Create(&taskedge.CreateRequest{
					JobID:      j.ID.String(),
					FromTaskID: edge.from,
					ToTaskID:   edge.to,
				}); err != nil {
					return err
				}
			}
		case len(createdTasks) > 1:
			for idx := 0; idx < len(createdTasks)-1; idx++ {
				if _, err := edgeSvc.Create(&taskedge.CreateRequest{
					JobID:      j.ID.String(),
					FromTaskID: createdTasks[idx],
					ToTaskID:   createdTasks[idx+1],
				}); err != nil {
					return err
				}
			}
		}

		created = j
		return nil
	})
	if err != nil {
		log.Error("failed to apply job", "alias", req.Alias, "error", err)
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}

	return c.JSON(http.StatusCreated, created)
}

func metadataLabels(md *MetadataRequest) map[string]string {
	if md == nil {
		return nil
	}
	return md.Labels
}

func metadataAnnotations(md *MetadataRequest) map[string]string {
	if md == nil {
		return nil
	}
	return md.Annotations
}

type PostRequest struct {
	Alias    string                 `json:"alias"`
	Metadata *MetadataRequest       `json:"metadata,omitempty"`
	Trigger  *trigger.CreateRequest `json:"trigger"`
	Tasks    []TaskRequest          `json:"tasks"`
}

type TaskRequest struct {
	Atom         *atom.CreateRequest `json:"atom"`
	DependsOn    []string            `json:"depends_on,omitempty"`
	NodeSelector map[string]string   `json:"node_selector,omitempty"`
}

type edgeSpec struct {
	from string
	to   string
}

type MetadataRequest struct {
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}
