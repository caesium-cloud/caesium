package job

import (
	"errors"
	"fmt"
	"net/http"

	authmw "github.com/caesium-cloud/caesium/api/middleware"
	"github.com/caesium-cloud/caesium/api/rest/service/atom"
	"github.com/caesium-cloud/caesium/api/rest/service/job"
	"github.com/caesium-cloud/caesium/api/rest/service/task"
	"github.com/caesium-cloud/caesium/api/rest/service/taskedge"
	"github.com/caesium-cloud/caesium/api/rest/service/trigger"
	"github.com/caesium-cloud/caesium/internal/event"
	internaljobdef "github.com/caesium-cloud/caesium/internal/jobdef"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/db"
	"github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"gorm.io/gorm"
)

func Post(c *echo.Context) error {
	req := &PostRequest{}
	if err := c.Bind(req); err != nil {
		return err
	}

	namespace := ""
	if req.Metadata != nil {
		namespace = req.Metadata.Namespace
	}
	if err := jobdef.ValidateNamespace(namespace); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error()).Wrap(err)
	}
	// With a policy file loaded, the namespace must be declared (B2).
	if err := internaljobdef.ValidateDeclaredNamespace(req.Alias, namespace); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error()).Wrap(err)
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

	// A confined writer (namespace-confined, or a job-scoped key), and every
	// writer once a policy file is loaded, creates under trigger tenancy
	// (B9): the alias and the webhook path are checked, and on PostgreSQL
	// locked, inside the transaction before anything is written.
	namespaces, aliases, err := authmw.WriteConfinement(authmw.GetPrincipal(c), models.RoleOperator)
	if err != nil {
		return echo.NewHTTPError(http.StatusForbidden, "insufficient permissions").Wrap(err)
	}
	scope := internaljobdef.NewTenancyScope(namespaces, aliases)
	run := func(fn func(tx *gorm.DB) error) error { return db.Transaction(ctx, fn) }
	if scope != nil || internaljobdef.TenancyInvariantEnforced() {
		path := internaljobdef.ProposedWebhookPath(req.Trigger.Type, req.Trigger.Configuration)
		keys := []string{internaljobdef.AliasLockKey(req.Alias)}
		if path != "" {
			keys = append(keys, internaljobdef.PathLockKey(path))
		}
		run = func(fn func(tx *gorm.DB) error) error {
			return internaljobdef.WriteTx(ctx, db.Connection(), keys, func(tx *gorm.DB) error {
				if err := internaljobdef.CheckJobCreateTx(tx, req.Alias, namespace, req.Trigger.Type, req.Trigger.Configuration, scope); err != nil {
					return err
				}
				return fn(tx)
			})
		}
	}

	var created *models.Job
	err = run(func(tx *gorm.DB) error {
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
			Namespace:   namespace,
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

		// A confined writer's dependencies may only name tasks of the job it
		// is creating: an edge to another job's task would read that task
		// into this job's run descriptor (PR #668 review r9). The check reads
		// nothing, so the refusal never depends on the named task.
		if scope != nil {
			if err := requireOwnDependencies(createdTasks, explicitEdges); err != nil {
				return err
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
		if errors.Is(err, errForeignDependency) {
			return echo.NewHTTPError(http.StatusBadRequest, errForeignDependency.Error()).Wrap(err)
		}
		if denial, conflict := internaljobdef.WriteRefusal(err, scope); denial != nil {
			return denial
		} else if conflict != "" {
			return echo.NewHTTPError(http.StatusConflict, conflict).Wrap(err)
		}
		log.Error("failed to apply job", "alias", req.Alias, "error", err)
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}
	if err := trigger.NotifyMutation(c.Request().Context()); err != nil {
		log.Warn("event trigger router reload failed after job create", "alias", req.Alias, "error", err)
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

// errForeignDependency refuses a confined writer's create whose
// tasks[].depends_on names a task outside the job being created.
var errForeignDependency = errors.New("tasks[].depends_on may only reference tasks of the job being created")

// requireOwnDependencies returns errForeignDependency unless every edge's
// source is one of the created tasks (task IDs compared as UUIDs; an
// unparsable one is foreign).
func requireOwnDependencies(created []string, edges []edgeSpec) error {
	own := make(map[uuid.UUID]struct{}, len(created))
	for _, id := range created {
		if parsed, err := uuid.Parse(id); err == nil {
			own[parsed] = struct{}{}
		}
	}
	for _, edge := range edges {
		from, err := uuid.Parse(edge.from)
		if err != nil {
			return errForeignDependency
		}
		if _, ok := own[from]; !ok {
			return errForeignDependency
		}
	}
	return nil
}

type MetadataRequest struct {
	Namespace   string            `json:"namespace,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}
