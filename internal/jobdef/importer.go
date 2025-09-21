package jobdef

import (
	"context"
	"errors"
	"fmt"
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

		trig, err := i.createTrigger(tx, alias, &def.Trigger)
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

		entries, taskByName, err := i.createAtomsAndTasks(tx, jobModel, def.Steps)
		if err != nil {
			return err
		}

		if err := i.linkTasks(tx, entries, taskByName); err != nil {
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

func (i *Importer) createTrigger(tx *gorm.DB, alias string, trig *schema.Trigger) (*models.Trigger, error) {
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

	if err := tx.Create(model).Error; err != nil {
		return nil, err
	}
	return model, nil
}

type stepEntry struct {
	def  *schema.Step
	task *models.Task
}

func (i *Importer) createAtomsAndTasks(tx *gorm.DB, job *models.Job, steps []schema.Step) ([]*stepEntry, map[string]*models.Task, error) {
	entries := make([]*stepEntry, 0, len(steps))
	taskByName := make(map[string]*models.Task, len(steps))

	for idx := range steps {
		step := &steps[idx]

		command, err := jsonutil.MarshalSliceString(step.Command)
		if err != nil {
			return nil, nil, fmt.Errorf("step %s: %w", step.Name, err)
		}

		atom := &models.Atom{
			ID:      uuid.New(),
			Engine:  models.AtomEngine(step.Engine),
			Image:   step.Image,
			Command: command,
		}
		if atom.Engine == "" {
			atom.Engine = models.AtomEngine(schema.EngineDocker)
		}

		if err := tx.Create(atom).Error; err != nil {
			return nil, nil, err
		}

		task := &models.Task{
			ID:     uuid.New(),
			JobID:  job.ID,
			AtomID: atom.ID,
		}

		if err := tx.Create(task).Error; err != nil {
			return nil, nil, err
		}

		entries = append(entries, &stepEntry{def: step, task: task})
		taskByName[step.Name] = task
	}

	return entries, taskByName, nil
}

func (i *Importer) linkTasks(tx *gorm.DB, entries []*stepEntry, taskByName map[string]*models.Task) error {
	for idx, entry := range entries {
		step := entry.def

		var nextName string
		if step.Next != "" {
			nextName = step.Next
		} else if idx+1 < len(entries) {
			nextName = entries[idx+1].def.Name
		}

		if nextName == "" {
			continue
		}

		nextTask, ok := taskByName[nextName]
		if !ok {
			return fmt.Errorf("step %s references unknown next step %s", step.Name, nextName)
		}

		if err := tx.Model(&models.Task{}).
			Where("id = ?", entry.task.ID).
			Update("next_id", nextTask.ID).Error; err != nil {
			return err
		}
	}
	return nil
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
