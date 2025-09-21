package job

import (
	"fmt"
	"net/http"

	"github.com/caesium-cloud/caesium/api/rest/service/atom"
	"github.com/caesium-cloud/caesium/api/rest/service/job"
	"github.com/caesium-cloud/caesium/api/rest/service/task"
	"github.com/caesium-cloud/caesium/api/rest/service/trigger"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/labstack/echo/v4"
)

func Post(c echo.Context) error {
	var (
		req  = &PostRequest{}
		tsvc = task.Service(c.Request().Context())
		asvc = atom.Service(c.Request().Context())
	)

	if err := c.Bind(req); err != nil {
		return err
	}

	if req.Trigger == nil {
		return echo.ErrBadRequest.SetInternal(fmt.Errorf("trigger is required"))
	}

	log.Info(
		"creating trigger",
		"type", req.Trigger.Type,
		"config", req.Trigger.Configuration)

	trig, err := trigger.Service(c.Request().Context()).Create(req.Trigger)
	if err != nil {
		log.Error("failed to create trigger", "error", err)
		return echo.ErrInternalServerError.SetInternal(err)
	}

	log.Info("creating job", "alias", req.Alias)

	j, err := job.Service(c.Request().Context()).Create(&job.CreateRequest{
		TriggerID:   trig.ID,
		Alias:       req.Alias,
		Labels:      metadataLabels(req.Metadata),
		Annotations: metadataAnnotations(req.Metadata),
	})
	if err != nil {
		log.Error("failed to create job", "error", err)
		return echo.ErrInternalServerError.SetInternal(err)
	}

	for _, t := range req.Tasks {
		log.Info(
			"creating atom",
			"job_id", j.ID,
			"engine", t.Atom.Engine,
			"image", t.Atom.Image,
			"cmd", t.Atom.Command,
		)

		a, err := asvc.Create(&atom.CreateRequest{
			Engine:  t.Atom.Engine,
			Image:   t.Atom.Image,
			Command: t.Atom.Command,
		})
		if err != nil {
			log.Error("failed to create atom", "error", err)
			return echo.ErrInternalServerError.SetInternal(err)
		}

		log.Info(
			"creating task",
			"job_id", j.ID,
			"atom_id", a.ID,
			"next_id", t.NextID,
		)

		if _, err = tsvc.Create(&task.CreateRequest{
			JobID:  j.ID.String(),
			AtomID: a.ID.String(),
			NextID: t.NextID,
		}); err != nil {
			log.Error("failed to create task", "error", err)
			return echo.ErrInternalServerError.SetInternal(err)
		}
	}

	return c.JSON(http.StatusCreated, j)
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
	Tasks    []struct {
		Atom   *atom.CreateRequest `json:"atom"`
		NextID *string             `json:"next_id"`
	} `json:"tasks"`
}

type MetadataRequest struct {
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}
