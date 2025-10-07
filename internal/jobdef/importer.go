package jobdef

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/caesium-cloud/caesium/internal/models"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/caesium-cloud/caesium/pkg/jsonutil"
	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

var ErrDuplicateJob = errors.New("job alias already exists")

// Importer coordinates persistence of job definitions.
type Importer struct {
	db *gorm.DB
}

// NewImporter creates a new importer. The provided db connection must be non-nil.
func NewImporter(dbConn *gorm.DB) *Importer {
	if dbConn == nil {
		panic("jobdef importer requires a database connection")
	}
	return &Importer{db: dbConn}
}

// Apply persists the provided definition and returns the created job record.
func (i *Importer) Apply(ctx context.Context, def *schema.Definition) (*models.Job, error) {
	return i.ApplyWithOptions(ctx, def, nil)
}

// ApplyOptions control optional behaviors for ApplyWithOptions.
type ApplyOptions struct {
	Provenance *Provenance
}

// Provenance captures metadata describing the origin of a job definition.
type Provenance struct {
	SourceID string
	Repo     string
	Ref      string
	Commit   string
	Path     string
}

// ApplyWithOptions persists the provided definition using the supplied options.
func (i *Importer) ApplyWithOptions(ctx context.Context, def *schema.Definition, opts *ApplyOptions) (*models.Job, error) {
	if err := def.Validate(); err != nil {
		return nil, err
	}

	var result *models.Job
	err := i.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		alias := def.Metadata.Alias

		var count int64
		if err := tx.Model(&models.Job{}).Where("alias = ?", alias).Count(&count).Error; err != nil {
			return err
		}
		if count > 0 {
			return fmt.Errorf("%w: %s", ErrDuplicateJob, alias)
		}

		trig, err := i.createTrigger(tx, alias, &def.Trigger, opts)
		if err != nil {
			return err
		}

		jobModel := &models.Job{
			ID:          uuid.New(),
			Alias:       alias,
			TriggerID:   trig.ID,
			Labels:      mapToJSONMap(def.Metadata.Labels),
			Annotations: mapToJSONMap(def.Metadata.Annotations),
		}

		if opts != nil && opts.Provenance != nil {
			prov := opts.Provenance
			jobModel.ProvenanceSourceID = strings.TrimSpace(prov.SourceID)
			jobModel.ProvenanceRepo = strings.TrimSpace(prov.Repo)
			jobModel.ProvenanceRef = strings.TrimSpace(prov.Ref)
			jobModel.ProvenanceCommit = strings.TrimSpace(prov.Commit)
			jobModel.ProvenancePath = strings.TrimSpace(prov.Path)
		}
		if err := tx.Create(jobModel).Error; err != nil {
			return err
		}

		taskByName, err := i.createAtomsAndTasks(tx, jobModel, def.Steps, opts)
		if err != nil {
			return err
		}

		if err := i.createEdges(tx, jobModel, def.Steps, taskByName, opts); err != nil {
			return err
		}

		if err := i.createCallbacks(tx, jobModel.ID, def.Callbacks); err != nil {
			return err
		}

		result = jobModel
		return nil
	})

	if err != nil {
		return nil, err
	}

	return result, nil
}

func (i *Importer) createTrigger(tx *gorm.DB, alias string, trig *schema.Trigger, opts *ApplyOptions) (*models.Trigger, error) {
	cfg, err := jsonutil.MarshalMapString(trig.Configuration)
	if err != nil {
		return nil, err
	}

	model := &models.Trigger{
		ID:            uuid.New(),
		Alias:         alias,
		Type:          models.TriggerType(trig.Type),
		Configuration: cfg,
	}

	if opts != nil && opts.Provenance != nil {
		copyProvenance(&model.ProvenanceSourceID, &model.ProvenanceRepo, &model.ProvenanceRef, &model.ProvenanceCommit, &model.ProvenancePath, opts.Provenance, "trigger")
	}

	if err := tx.Create(model).Error; err != nil {
		return nil, err
	}
	return model, nil
}

func (i *Importer) createAtomsAndTasks(tx *gorm.DB, job *models.Job, steps []schema.Step, opts *ApplyOptions) (map[string]*models.Task, error) {
	taskByName := make(map[string]*models.Task, len(steps))

	for idx := range steps {
		step := &steps[idx]

		command, err := jsonutil.MarshalSliceString(step.Command)
		if err != nil {
			return nil, fmt.Errorf("step %s: %w", step.Name, err)
		}

		atom := &models.Atom{
			ID:      uuid.New(),
			Engine:  models.AtomEngine(step.Engine),
			Image:   step.Image,
			Command: command,
		}

		if opts != nil && opts.Provenance != nil {
			suffix := fmt.Sprintf("step/%s", url.PathEscape(step.Name))
			copyProvenance(&atom.ProvenanceSourceID, &atom.ProvenanceRepo, &atom.ProvenanceRef, &atom.ProvenanceCommit, &atom.ProvenancePath, opts.Provenance, suffix)
		}
		if atom.Engine == "" {
			atom.Engine = models.AtomEngine(schema.EngineDocker)
		}

		if err := tx.Create(atom).Error; err != nil {
			return nil, err
		}

		task := &models.Task{
			ID:     uuid.New(),
			JobID:  job.ID,
			AtomID: atom.ID,
		}

		if err := tx.Create(task).Error; err != nil {
			return nil, err
		}

		taskByName[step.Name] = task
	}

	return taskByName, nil
}

func copyProvenance(sourceID, repo, ref, commit, path *string, prov *Provenance, suffix string) {
	if prov == nil {
		return
	}

	*sourceID = strings.TrimSpace(prov.SourceID)
	*repo = strings.TrimSpace(prov.Repo)
	*ref = strings.TrimSpace(prov.Ref)
	*commit = strings.TrimSpace(prov.Commit)
	basePath := strings.TrimSpace(prov.Path)
	if basePath == "" {
		*path = ""
		return
	}
	suffix = strings.TrimSpace(suffix)
	if suffix == "" {
		*path = basePath
		return
	}
	if strings.HasPrefix(suffix, "#") {
		*path = basePath + suffix
		return
	}
	*path = basePath + "#" + suffix
}

func (i *Importer) createEdges(tx *gorm.DB, job *models.Job, steps []schema.Step, taskByName map[string]*models.Task, opts *ApplyOptions) error {
	successors, err := schema.DeriveStepSuccessors(steps)
	if err != nil {
		return err
	}

	totalEdges := 0
	for _, succs := range successors {
		totalEdges += len(succs)
	}

	edges := make([]*models.TaskEdge, 0, totalEdges)

	for fromName, succs := range successors {
		fromTask, ok := taskByName[fromName]
		if !ok {
			return fmt.Errorf("step %s missing task mapping", fromName)
		}
		for _, toName := range succs {
			toTask, ok := taskByName[toName]
			if !ok {
				return fmt.Errorf("step %s successor %s missing task mapping", fromName, toName)
			}

			edge := &models.TaskEdge{
				ID:         uuid.New(),
				JobID:      job.ID,
				FromTaskID: fromTask.ID,
				ToTaskID:   toTask.ID,
			}

			if opts != nil && opts.Provenance != nil {
				suffix := fmt.Sprintf("edge/%s->%s", url.PathEscape(fromName), url.PathEscape(toName))
				copyProvenance(&edge.ProvenanceSourceID, &edge.ProvenanceRepo, &edge.ProvenanceRef, &edge.ProvenanceCommit, &edge.ProvenancePath, opts.Provenance, suffix)
			}

			edges = append(edges, edge)
		}
	}

	if len(edges) > 0 {
		if err := tx.Create(&edges).Error; err != nil {
			return err
		}
	}

	return i.backfillLegacyNext(tx, successors, taskByName)
}

func (i *Importer) createCallbacks(tx *gorm.DB, jobID uuid.UUID, callbacks []schema.Callback) error {
	for _, cb := range callbacks {
		cfg, err := jsonutil.MarshalMapString(cb.Configuration)
		if err != nil {
			return err
		}

		model := &models.Callback{
			ID:            uuid.New(),
			JobID:         jobID,
			Type:          models.CallbackType(cb.Type),
			Configuration: cfg,
		}

		if err := tx.Create(model).Error; err != nil {
			return err
		}
	}
	return nil
}

func mapToJSONMap(in map[string]string) datatypes.JSONMap {
	if in == nil {
		return datatypes.JSONMap{}
	}
	out := datatypes.JSONMap{}
	for k, v := range in {
		out[k] = v
	}
	return out
}

func (i *Importer) backfillLegacyNext(tx *gorm.DB, successors map[string][]string, taskByName map[string]*models.Task) error {
	for fromName, succs := range successors {
		if len(succs) != 1 {
			continue
		}

		fromTask, ok := taskByName[fromName]
		if !ok {
			return fmt.Errorf("missing task for step %s", fromName)
		}
		toTask, ok := taskByName[succs[0]]
		if !ok {
			return fmt.Errorf("missing task for successor step %s", succs[0])
		}

		if err := tx.Model(&models.Task{}).
			Where("id = ?", fromTask.ID).
			Update("next_id", toTask.ID).Error; err != nil {
			return err
		}
	}
	return nil
}
