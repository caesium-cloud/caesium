package job

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	asvc "github.com/caesium-cloud/caesium/api/rest/service/atom"
	"github.com/caesium-cloud/caesium/api/rest/service/task"
	"github.com/caesium-cloud/caesium/api/rest/service/taskedge"
	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/caesium-cloud/caesium/internal/atom/docker"
	"github.com/caesium-cloud/caesium/internal/atom/kubernetes"
	"github.com/caesium-cloud/caesium/internal/atom/podman"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/container"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
	"gorm.io/gorm"
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
	spec    container.Spec
	engine  atom.Engine
}

func (j *job) Run(ctx context.Context) error {
	store := run.Default()

	resolveRun := func() (*run.JobRun, error) {
		if id, ok := run.FromContext(ctx); ok {
			existing, err := store.Get(id)
			if err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return store.Start(j.id)
				}
				return nil, err
			}
			return existing, nil
		}

		running, err := store.FindRunning(j.id)
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, err
		}

		if running != nil {
			if err := store.ResetInFlightTasks(running.ID); err != nil {
				return nil, err
			}
			return store.Get(running.ID)
		}

		return store.Start(j.id)
	}

	snapshot, err := resolveRun()
	if err != nil {
		return err
	}

	runID := snapshot.ID
	ctx = run.WithContext(ctx, runID)

	var runErr error
	defer func() {
		if err := store.Complete(runID, runErr); err != nil {
			log.Error("run completion persistence failure", "run_id", runID, "error", err)
		}
	}()

	tasks, err := task.Service(ctx).List(&task.ListRequest{
		JobID:   j.id.String(),
		OrderBy: []string{"created_at"},
	})
	if err != nil {
		runErr = err
		return err
	}

	if len(tasks) == 0 {
		runErr = fmt.Errorf("job %s has no tasks", j.id)
		return runErr
	}

	log.Info("running job tasks", "job_id", j.id, "count", len(tasks))

	svc := asvc.Service(ctx)

	taskOrder := make(map[uuid.UUID]int, len(tasks))
	atomsByTask := make(map[uuid.UUID]*models.Atom, len(tasks))
	runners := make(map[uuid.UUID]*atomRunner, len(tasks))

	for idx, t := range tasks {
		taskOrder[t.ID] = idx

		modelAtom, err := svc.Get(t.AtomID)
		if err != nil {
			runErr = err
			return err
		}

		atomsByTask[t.ID] = modelAtom

		runner := &atomRunner{
			image:   modelAtom.Image,
			command: modelAtom.Cmd(),
			spec:    modelAtom.ContainerSpec(),
		}

		log.Info("evaluating task atom", "job_id", j.id, "task_id", t.ID, "engine", modelAtom.Engine, "atom_id", modelAtom.ID)

		switch modelAtom.Engine {
		case models.AtomEngineDocker:
			runner.engine = docker.NewEngine(ctx)
		case models.AtomEngineKubernetes:
			runner.engine = kubernetes.NewEngine(ctx)
		case models.AtomEnginePodman:
			runner.engine = podman.NewEngine(ctx)
		default:
			runErr = fmt.Errorf("unable to run atom with engine: %v", modelAtom.Engine)
			return runErr
		}

		runners[t.ID] = runner
	}

	edges, err := taskedge.Service(ctx).List(&taskedge.ListRequest{
		JobID:   j.id.String(),
		OrderBy: []string{"created_at"},
	})
	if err != nil {
		runErr = err
		return err
	}

	adjacency := make(map[uuid.UUID][]uuid.UUID, len(tasks))
	indegree := make(map[uuid.UUID]int, len(tasks))
	edgeSet := make(map[uuid.UUID]map[uuid.UUID]struct{}, len(tasks))

	for _, t := range tasks {
		adjacency[t.ID] = []uuid.UUID{}
		indegree[t.ID] = 0
	}

	addEdge := func(from, to uuid.UUID) {
		if _, ok := adjacency[from]; !ok {
			return
		}
		if _, ok := adjacency[to]; !ok {
			return
		}
		targets, ok := edgeSet[from]
		if !ok {
			targets = make(map[uuid.UUID]struct{})
			edgeSet[from] = targets
		}
		if _, exists := targets[to]; exists {
			return
		}
		adjacency[from] = append(adjacency[from], to)
		indegree[to]++
		targets[to] = struct{}{}
	}

	addedEdges := 0
	for _, edge := range edges {
		addEdge(edge.FromTaskID, edge.ToTaskID)
		addedEdges++
	}

	if addedEdges == 0 {
		for _, t := range tasks {
			if t.NextID == nil {
				continue
			}
			addEdge(t.ID, *t.NextID)
			addedEdges++
		}

		if addedEdges == 0 && len(tasks) > 1 {
			for idx := 0; idx < len(tasks)-1; idx++ {
				addEdge(tasks[idx].ID, tasks[idx+1].ID)
			}
		}
	}

	for _, t := range tasks {
		atomModel := atomsByTask[t.ID]
		if err := store.RegisterTask(runID, t, atomModel, indegree[t.ID]); err != nil {
			runErr = err
			return err
		}
	}

	currentRun, err := store.Get(runID)
	if err != nil {
		runErr = err
		return err
	}

	queue := make([]uuid.UUID, 0, len(tasks))
	inQueue := make(map[uuid.UUID]bool, len(tasks))
	processed := make(map[uuid.UUID]bool, len(tasks))
	totalCompleted := 0

	for _, taskState := range currentRun.Tasks {
		indegree[taskState.ID] = taskState.OutstandingPredecessors
		switch taskState.Status {
		case run.TaskStatusSucceeded:
			processed[taskState.ID] = true
			totalCompleted++
		case run.TaskStatusFailed:
			runErr = fmt.Errorf("task %s previously failed", taskState.ID)
			return runErr
		}
	}

	push := func(id uuid.UUID) {
		if processed[id] || inQueue[id] {
			return
		}
		queue = append(queue, id)
		inQueue[id] = true
		sort.Slice(queue, func(i, j int) bool {
			return taskOrder[queue[i]] < taskOrder[queue[j]]
		})
	}

	for _, taskState := range currentRun.Tasks {
		if processed[taskState.ID] {
			continue
		}
		if indegree[taskState.ID] == 0 {
			push(taskState.ID)
		}
	}

	if len(queue) == 0 && totalCompleted < len(tasks) {
		runErr = fmt.Errorf("job %s has no runnable tasks (verify DAG configuration)", j.id)
		return runErr
	}

	executed := totalCompleted

	for len(queue) > 0 {
		taskID := queue[0]
		queue = queue[1:]
		delete(inQueue, taskID)

		if processed[taskID] {
			continue
		}

		runner := runners[taskID]

		log.Info("running atom", "job_id", j.id, "task_id", taskID, "image", runner.image, "cmd", runner.command)

		a, err := runner.engine.Create(&atom.EngineCreateRequest{
			Name:    taskID.String(),
			Image:   runner.image,
			Command: runner.command,
			Spec:    runner.spec,
		})
		if err != nil {
			if persistErr := store.FailTask(runID, taskID, err); persistErr != nil {
				log.Error("failed to persist task failure", "run_id", runID, "task_id", taskID, "error", persistErr)
			}
			runErr = err
			return err
		}

		if err := store.StartTask(runID, taskID, a.ID()); err != nil {
			runErr = err
			return err
		}

		monitor := func() error {
			for {
				select {
				case <-ctx.Done():
					return nil
				case <-time.After(5 * time.Second):
					var fetchErr error
					a, fetchErr = runner.engine.Get(&atom.EngineGetRequest{ID: a.ID()})
					if fetchErr != nil {
						return fetchErr
					}

					if !a.StoppedAt().IsZero() {
						log.Info("atom finished", "job_id", j.id, "task_id", taskID, "atom_id", a.ID(), "result", a.Result())

						return runner.engine.Stop(&atom.EngineStopRequest{
							ID:    a.ID(),
							Force: true,
						})
					}

					log.Info("atom running", "job_id", j.id, "task_id", taskID, "atom_id", a.ID(), "state", a.State())
				}
			}
		}

		if err = monitor(); err != nil {
			if persistErr := store.FailTask(runID, taskID, err); persistErr != nil {
				log.Error("failed to persist task failure", "run_id", runID, "task_id", taskID, "error", persistErr)
			}
			runErr = err
			return err
		}

		if err := store.CompleteTask(runID, taskID, string(a.Result())); err != nil {
			runErr = err
			return err
		}

		processed[taskID] = true
		executed++

		for _, successor := range adjacency[taskID] {
			if _, ok := indegree[successor]; !ok {
				continue
			}
			if indegree[successor] > 0 {
				indegree[successor]--
			}
			if indegree[successor] == 0 {
				push(successor)
			}
		}
	}

	if executed != len(tasks) {
		runErr = fmt.Errorf("job %s executed %d of %d tasks; remaining tasks may be waiting on unresolved dependencies", j.id, executed, len(tasks))
		return runErr
	}

	return nil
}
