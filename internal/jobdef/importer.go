package jobdef

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/caesium-cloud/caesium/pkg/jsonmap"
	"github.com/caesium-cloud/caesium/pkg/jsonutil"
	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

var (
	ErrDuplicateJob       = errors.New("job alias already exists")
	ErrProvenanceConflict = errors.New("job definition provenance conflict")
	ErrJobRunning         = errors.New("job has a running run")
)

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

// Apply persists the provided definition and returns the reconciled job record.
func (i *Importer) Apply(ctx context.Context, def *schema.Definition) (*models.Job, error) {
	return i.ApplyWithOptions(ctx, def, nil)
}

// ApplyOptions control optional behaviors for ApplyWithOptions.
type ApplyOptions struct {
	Provenance *Provenance
	Force      bool
}

// PruneOptions scope the set of jobs that should be retired when pruning.
type PruneOptions struct {
	SourceID string
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
// Existing jobs are reconciled in place so history remains attached to the same job ID.
func (i *Importer) ApplyWithOptions(ctx context.Context, def *schema.Definition, opts *ApplyOptions) (*models.Job, error) {
	if err := def.Validate(); err != nil {
		return nil, err
	}

	var result *models.Job
	err := i.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		existing, err := i.findJobByAliasTx(tx, def.Metadata.Alias)
		if err != nil {
			return err
		}
		if existing != nil {
			if err := i.guardJobMutationTx(tx, existing, opts); err != nil {
				return err
			}
		}

		jobModel, triggerModel, err := i.upsertJobAndTriggerTx(tx, existing, def, opts)
		if err != nil {
			return err
		}
		if triggerModel != nil {
			jobModel.TriggerID = triggerModel.ID
		}

		taskByName, _, retiredAtomIDs, err := i.reconcileTasksTx(tx, jobModel, def.Steps, opts)
		if err != nil {
			return err
		}
		if err := i.reconcileEdgesTx(tx, jobModel, def.Steps, taskByName, opts); err != nil {
			return err
		}
		if err := i.reconcileCallbacksTx(tx, jobModel.ID, def.Callbacks); err != nil {
			return err
		}
		if err := i.softDeleteAtomsTx(tx, retiredAtomIDs); err != nil {
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

// PruneMissing retires active jobs that are absent from the desired alias set.
// When opts.SourceID is set, pruning is scoped to jobs imported by that source.
func (i *Importer) PruneMissing(ctx context.Context, desiredAliases []string, opts *PruneOptions) (int, error) {
	desiredSet := make(map[string]struct{}, len(desiredAliases))
	for _, alias := range desiredAliases {
		alias = strings.TrimSpace(alias)
		if alias == "" {
			continue
		}
		desiredSet[alias] = struct{}{}
	}

	var pruned int
	err := i.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		query := tx.Model(&models.Job{})
		if opts != nil && strings.TrimSpace(opts.SourceID) != "" {
			query = query.Where("provenance_source_id = ?", strings.TrimSpace(opts.SourceID))
		}

		var jobs []models.Job
		if err := query.Find(&jobs).Error; err != nil {
			return err
		}

		toRetire := make([]*models.Job, 0, len(jobs))
		for idx := range jobs {
			jobModel := &jobs[idx]
			if _, ok := desiredSet[jobModel.Alias]; ok {
				continue
			}
			toRetire = append(toRetire, jobModel)
		}
		if len(toRetire) == 0 {
			return nil
		}
		if err := i.ensureJobsNotRunningTx(tx, toRetire); err != nil {
			return err
		}
		if err := i.retireJobsTx(tx, toRetire); err != nil {
			return err
		}
		pruned = len(toRetire)
		return nil
	})
	return pruned, err
}

func (i *Importer) findJobByAliasTx(tx *gorm.DB, alias string) (*models.Job, error) {
	var jobModel models.Job
	err := tx.Unscoped().Where("alias = ?", alias).First(&jobModel).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return nil, nil
	case err != nil:
		return nil, err
	default:
		return &jobModel, nil
	}
}

func (i *Importer) guardJobMutationTx(tx *gorm.DB, jobModel *models.Job, opts *ApplyOptions) error {
	if jobModel == nil {
		return nil
	}

	if err := i.ensureJobNotRunningTx(tx, jobModel); err != nil {
		return err
	}

	if opts != nil && opts.Force {
		return nil
	}

	incomingSourceID := ""
	if opts != nil && opts.Provenance != nil {
		incomingSourceID = strings.TrimSpace(opts.Provenance.SourceID)
	}
	existingSourceID := strings.TrimSpace(jobModel.ProvenanceSourceID)
	if existingSourceID != incomingSourceID && (existingSourceID != "" || incomingSourceID != "") {
		return fmt.Errorf("%w: alias %s managed by %q, incoming source %q", ErrProvenanceConflict, jobModel.Alias, existingSourceID, incomingSourceID)
	}
	return nil
}

func (i *Importer) ensureJobNotRunningTx(tx *gorm.DB, jobModel *models.Job) error {
	if jobModel == nil {
		return nil
	}

	var running int64
	if err := tx.Model(&models.JobRun{}).
		Where("job_id = ? AND status = ?", jobModel.ID, "running").
		Count(&running).Error; err != nil {
		return err
	}
	if running > 0 {
		return fmt.Errorf("%w: %s", ErrJobRunning, jobModel.Alias)
	}
	return nil
}

func (i *Importer) ensureJobsNotRunningTx(tx *gorm.DB, jobs []*models.Job) error {
	if len(jobs) == 0 {
		return nil
	}

	jobIDs := make([]uuid.UUID, 0, len(jobs))
	aliasByID := make(map[uuid.UUID]string, len(jobs))
	for _, jobModel := range jobs {
		if jobModel == nil {
			continue
		}
		jobIDs = append(jobIDs, jobModel.ID)
		aliasByID[jobModel.ID] = jobModel.Alias
	}
	if len(jobIDs) == 0 {
		return nil
	}

	var running []struct {
		JobID uuid.UUID
	}
	if err := tx.Model(&models.JobRun{}).
		Select("job_id").
		Where("job_id IN ? AND status = ?", jobIDs, "running").
		Group("job_id").
		Find(&running).Error; err != nil {
		return err
	}
	if len(running) == 0 {
		return nil
	}
	alias := aliasByID[running[0].JobID]
	if alias == "" {
		alias = running[0].JobID.String()
	}
	return fmt.Errorf("%w: %s", ErrJobRunning, alias)
}

func (i *Importer) upsertJobAndTriggerTx(tx *gorm.DB, existing *models.Job, def *schema.Definition, opts *ApplyOptions) (*models.Job, *models.Trigger, error) {
	triggerModel, err := i.upsertTriggerTx(tx, existing, def.Metadata.Alias, &def.Trigger, opts)
	if err != nil {
		return nil, nil, err
	}

	cacheConfig, err := marshalOptionalJSON(def.Metadata.Cache)
	if err != nil {
		return nil, nil, fmt.Errorf("metadata.cache: %w", err)
	}

	if existing == nil {
		jobModel := &models.Job{
			ID:               uuid.New(),
			Alias:            def.Metadata.Alias,
			TriggerID:        triggerModel.ID,
			Labels:           jsonmap.FromStringMap(def.Metadata.Labels),
			Annotations:      jsonmap.FromStringMap(def.Metadata.Annotations),
			MaxParallelTasks: def.Metadata.MaxParallelTasks,
			TaskTimeout:      def.Metadata.TaskTimeout,
			RunTimeout:       def.Metadata.RunTimeout,
			SchemaValidation: def.Metadata.SchemaValidation,
			CacheConfig:      cacheConfig,
		}
		applyJobProvenance(jobModel, opts)
		if err := tx.Create(jobModel).Error; err != nil {
			return nil, nil, err
		}
		return jobModel, triggerModel, nil
	}

	existing.Alias = def.Metadata.Alias
	existing.TriggerID = triggerModel.ID
	existing.Labels = jsonmap.FromStringMap(def.Metadata.Labels)
	existing.Annotations = jsonmap.FromStringMap(def.Metadata.Annotations)
	existing.MaxParallelTasks = def.Metadata.MaxParallelTasks
	existing.TaskTimeout = def.Metadata.TaskTimeout
	existing.RunTimeout = def.Metadata.RunTimeout
	existing.SchemaValidation = def.Metadata.SchemaValidation
	existing.CacheConfig = cacheConfig
	applyJobProvenance(existing, opts)

	updates := map[string]any{
		"alias":                existing.Alias,
		"trigger_id":           existing.TriggerID,
		"labels":               existing.Labels,
		"annotations":          existing.Annotations,
		"max_parallel_tasks":   existing.MaxParallelTasks,
		"task_timeout":         existing.TaskTimeout,
		"run_timeout":          existing.RunTimeout,
		"schema_validation":    existing.SchemaValidation,
		"cache_config":         existing.CacheConfig,
		"provenance_source_id": existing.ProvenanceSourceID,
		"provenance_repo":      existing.ProvenanceRepo,
		"provenance_ref":       existing.ProvenanceRef,
		"provenance_commit":    existing.ProvenanceCommit,
		"provenance_path":      existing.ProvenancePath,
		"deleted_at":           nil,
	}
	if err := tx.Unscoped().Model(existing).Updates(updates).Error; err != nil {
		return nil, nil, err
	}

	return existing, triggerModel, nil
}

func (i *Importer) upsertTriggerTx(tx *gorm.DB, existingJob *models.Job, alias string, trig *schema.Trigger, opts *ApplyOptions) (*models.Trigger, error) {
	cfgMap := cloneAnyMap(trig.Configuration)
	if cfgMap == nil {
		cfgMap = make(map[string]any)
	}
	if len(trig.DefaultParams) > 0 {
		cfgMap["defaultParams"] = trig.DefaultParams
	}
	cfg, err := jsonutil.MarshalMapString(cfgMap)
	if err != nil {
		return nil, err
	}

	var triggerModel *models.Trigger
	if existingJob != nil && existingJob.TriggerID != uuid.Nil {
		var existingTrigger models.Trigger
		if err := tx.Unscoped().First(&existingTrigger, "id = ?", existingJob.TriggerID).Error; err == nil {
			triggerModel = &existingTrigger
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, err
		}
	}

	if triggerModel == nil {
		triggerModel = &models.Trigger{ID: uuid.New()}
	}

	triggerModel.Alias = alias
	triggerModel.Type = models.TriggerType(trig.Type)
	triggerModel.Configuration = cfg
	applyTriggerProvenance(triggerModel, opts)

	if triggerModel.CreatedAt.IsZero() {
		if err := tx.Create(triggerModel).Error; err != nil {
			return nil, err
		}
		return triggerModel, nil
	}

	updates := map[string]any{
		"alias":                triggerModel.Alias,
		"type":                 triggerModel.Type,
		"configuration":        triggerModel.Configuration,
		"provenance_source_id": triggerModel.ProvenanceSourceID,
		"provenance_repo":      triggerModel.ProvenanceRepo,
		"provenance_ref":       triggerModel.ProvenanceRef,
		"provenance_commit":    triggerModel.ProvenanceCommit,
		"provenance_path":      triggerModel.ProvenancePath,
		"deleted_at":           nil,
	}
	if err := tx.Unscoped().Model(triggerModel).Updates(updates).Error; err != nil {
		return nil, err
	}
	return triggerModel, nil
}

func (i *Importer) reconcileTasksTx(tx *gorm.DB, jobModel *models.Job, steps []schema.Step, opts *ApplyOptions) (map[string]*models.Task, []uuid.UUID, []uuid.UUID, error) {
	var existingTasks []models.Task
	if err := tx.Unscoped().Where("job_id = ?", jobModel.ID).Order("position asc").Order("created_at asc").Find(&existingTasks).Error; err != nil {
		return nil, nil, nil, err
	}

	existingByName := make(map[string]*models.Task, len(existingTasks))
	for idx := range existingTasks {
		taskModel := &existingTasks[idx]
		existingByName[taskModel.Name] = taskModel
	}

	taskByName := make(map[string]*models.Task, len(steps))
	seenNames := make(map[string]struct{}, len(steps))

	for idx := range steps {
		step := &steps[idx]
		taskModel := existingByName[step.Name]

		command, err := jsonutil.MarshalSliceString(step.Command)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("step %s: %w", step.Name, err)
		}
		specJSON, err := json.Marshal(step.Spec)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("step %s: %w", step.Name, err)
		}

		var atomModel *models.Atom
		if taskModel != nil && taskModel.AtomID != uuid.Nil {
			var existingAtom models.Atom
			if err := tx.Unscoped().First(&existingAtom, "id = ?", taskModel.AtomID).Error; err == nil {
				atomModel = &existingAtom
			} else if !errors.Is(err, gorm.ErrRecordNotFound) {
				return nil, nil, nil, err
			}
		}
		if atomModel == nil {
			atomModel = &models.Atom{ID: uuid.New()}
		}
		atomModel.Engine = models.AtomEngine(step.Engine)
		if atomModel.Engine == "" {
			atomModel.Engine = models.AtomEngine(schema.EngineDocker)
		}
		atomModel.Image = step.Image
		atomModel.Command = command
		atomModel.Spec = datatypes.JSON(specJSON)
		applyAtomProvenance(atomModel, step.Name, opts)

		if atomModel.CreatedAt.IsZero() {
			if err := tx.Create(atomModel).Error; err != nil {
				return nil, nil, nil, err
			}
		} else {
			atomUpdates := map[string]any{
				"engine":               atomModel.Engine,
				"image":                atomModel.Image,
				"command":              atomModel.Command,
				"spec":                 atomModel.Spec,
				"provenance_source_id": atomModel.ProvenanceSourceID,
				"provenance_repo":      atomModel.ProvenanceRepo,
				"provenance_ref":       atomModel.ProvenanceRef,
				"provenance_commit":    atomModel.ProvenanceCommit,
				"provenance_path":      atomModel.ProvenancePath,
				"deleted_at":           nil,
			}
			if err := tx.Unscoped().Model(atomModel).Updates(atomUpdates).Error; err != nil {
				return nil, nil, nil, err
			}
		}

		if taskModel == nil {
			taskModel = &models.Task{ID: uuid.New(), JobID: jobModel.ID}
		}
		if err := populateTaskFromStep(taskModel, atomModel.ID, step); err != nil {
			return nil, nil, nil, err
		}
		taskModel.Position = idx

		if taskModel.UpdatedAt.IsZero() {
			if err := tx.Create(taskModel).Error; err != nil {
				return nil, nil, nil, err
			}
		} else {
			taskUpdates := map[string]any{
				"atom_id":       taskModel.AtomID,
				"name":          taskModel.Name,
				"type":          taskModel.Type,
				"node_selector": taskModel.NodeSelector,
				"retries":       taskModel.Retries,
				"retry_delay":   taskModel.RetryDelay,
				"retry_backoff": taskModel.RetryBackoff,
				"trigger_rule":  taskModel.TriggerRule,
				"cache_config":  taskModel.CacheConfig,
				"output_schema": taskModel.OutputSchema,
				"input_schema":  taskModel.InputSchema,
				"position":      taskModel.Position,
				"deleted_at":    nil,
			}
			if err := tx.Unscoped().Model(taskModel).Updates(taskUpdates).Error; err != nil {
				return nil, nil, nil, err
			}
		}

		taskByName[step.Name] = taskModel
		seenNames[step.Name] = struct{}{}
	}

	var retiredTaskIDs []uuid.UUID
	var retiredAtomIDs []uuid.UUID
	for idx := range existingTasks {
		taskModel := &existingTasks[idx]
		if _, ok := seenNames[taskModel.Name]; ok {
			continue
		}
		if err := tx.Delete(taskModel).Error; err != nil {
			return nil, nil, nil, err
		}
		retiredTaskIDs = append(retiredTaskIDs, taskModel.ID)
		if taskModel.AtomID != uuid.Nil {
			retiredAtomIDs = append(retiredAtomIDs, taskModel.AtomID)
		}
	}

	return taskByName, retiredTaskIDs, retiredAtomIDs, nil
}

func (i *Importer) reconcileEdgesTx(tx *gorm.DB, jobModel *models.Job, steps []schema.Step, taskByName map[string]*models.Task, opts *ApplyOptions) error {
	successors, err := schema.DeriveStepSuccessors(steps)
	if err != nil {
		return err
	}

	if err := tx.Unscoped().Where("job_id = ?", jobModel.ID).Delete(&models.TaskEdge{}).Error; err != nil {
		return err
	}

	totalEdges := 0
	for _, succs := range successors {
		totalEdges += len(succs)
	}
	edges := make([]*models.TaskEdge, 0, totalEdges)
	sequenceBase := time.Now().UTC()
	edgeIdx := 0

	for fromName, succs := range successors {
		fromTask := taskByName[fromName]
		if fromTask == nil {
			return fmt.Errorf("step %s missing task mapping", fromName)
		}
		for _, toName := range succs {
			toTask := taskByName[toName]
			if toTask == nil {
				return fmt.Errorf("step %s successor %s missing task mapping", fromName, toName)
			}

			edge := &models.TaskEdge{
				ID:         uuid.New(),
				JobID:      jobModel.ID,
				FromTaskID: fromTask.ID,
				ToTaskID:   toTask.ID,
				CreatedAt:  sequenceBase.Add(time.Duration(edgeIdx) * time.Microsecond),
			}
			if opts != nil && opts.Provenance != nil {
				suffix := fmt.Sprintf("edge/%s->%s", url.PathEscape(fromName), url.PathEscape(toName))
				copyProvenance(&edge.ProvenanceSourceID, &edge.ProvenanceRepo, &edge.ProvenanceRef, &edge.ProvenanceCommit, &edge.ProvenancePath, opts.Provenance, suffix)
			}
			edges = append(edges, edge)
			edgeIdx++
		}
	}

	if len(edges) == 0 {
		return nil
	}
	return tx.Create(&edges).Error
}

func (i *Importer) reconcileCallbacksTx(tx *gorm.DB, jobID uuid.UUID, callbacks []schema.Callback) error {
	var existing []models.Callback
	if err := tx.Unscoped().Where("job_id = ?", jobID).Order("position asc").Order("created_at asc").Find(&existing).Error; err != nil {
		return err
	}

	pools := make(map[string][]*models.Callback, len(existing))
	for idx := range existing {
		cb := &existing[idx]
		key := callbackSignature(cb.Type, cb.Configuration)
		pools[key] = append(pools[key], cb)
	}

	claimed := make(map[uuid.UUID]struct{}, len(existing))
	for idx := range callbacks {
		cfg, err := jsonutil.MarshalMapString(callbacks[idx].Configuration)
		if err != nil {
			return err
		}

		cbType := models.CallbackType(callbacks[idx].Type)
		key := callbackSignature(cbType, cfg)

		var callbackModel *models.Callback
		for _, candidate := range pools[key] {
			if _, ok := claimed[candidate.ID]; ok {
				continue
			}
			callbackModel = candidate
			break
		}

		if callbackModel != nil {
			claimed[callbackModel.ID] = struct{}{}
			callbackModel.Position = idx
			if err := tx.Unscoped().Model(callbackModel).Updates(map[string]any{
				"position":   callbackModel.Position,
				"deleted_at": nil,
			}).Error; err != nil {
				return err
			}
			continue
		}

		callbackModel = &models.Callback{
			ID:            uuid.New(),
			JobID:         jobID,
			Type:          cbType,
			Configuration: cfg,
			Position:      idx,
		}
		if err := tx.Create(callbackModel).Error; err != nil {
			return err
		}
	}

	for idx := range existing {
		if _, ok := claimed[existing[idx].ID]; ok {
			continue
		}
		if err := tx.Delete(&existing[idx]).Error; err != nil {
			return err
		}
	}
	return nil
}

func callbackSignature(cbType models.CallbackType, cfg string) string {
	return string(cbType) + "\x00" + cfg
}

func (i *Importer) softDeleteAtomsTx(tx *gorm.DB, atomIDs []uuid.UUID) error {
	if len(atomIDs) == 0 {
		return nil
	}
	return tx.Where("id IN ?", atomIDs).Delete(&models.Atom{}).Error
}

func (i *Importer) retireJobsTx(tx *gorm.DB, jobs []*models.Job) error {
	if len(jobs) == 0 {
		return nil
	}

	jobIDs := make([]uuid.UUID, 0, len(jobs))
	triggerIDs := make([]uuid.UUID, 0, len(jobs))
	for _, jobModel := range jobs {
		if jobModel == nil {
			continue
		}
		jobIDs = append(jobIDs, jobModel.ID)
		if jobModel.TriggerID != uuid.Nil {
			triggerIDs = append(triggerIDs, jobModel.TriggerID)
		}
	}
	if len(jobIDs) == 0 {
		return nil
	}

	if err := tx.Unscoped().Where("job_id IN ?", jobIDs).Delete(&models.TaskEdge{}).Error; err != nil {
		return err
	}

	if err := tx.Where("job_id IN ?", jobIDs).Delete(&models.Callback{}).Error; err != nil {
		return err
	}

	var tasks []models.Task
	if err := tx.Where("job_id IN ?", jobIDs).Find(&tasks).Error; err != nil {
		return err
	}

	atomIDs := make([]uuid.UUID, 0, len(tasks))
	for idx := range tasks {
		if tasks[idx].AtomID != uuid.Nil {
			atomIDs = append(atomIDs, tasks[idx].AtomID)
		}
	}

	if err := tx.Where("job_id IN ?", jobIDs).Delete(&models.Task{}).Error; err != nil {
		return err
	}
	if err := i.softDeleteAtomsTx(tx, atomIDs); err != nil {
		return err
	}
	if len(triggerIDs) > 0 {
		if err := tx.Delete(&models.Trigger{}, triggerIDs).Error; err != nil {
			return err
		}
	}
	return tx.Delete(&models.Job{}, jobIDs).Error
}

func populateTaskFromStep(taskModel *models.Task, atomID uuid.UUID, step *schema.Step) error {
	triggerRule := strings.TrimSpace(step.TriggerRule)
	if triggerRule == "" {
		triggerRule = schema.TriggerRuleAllSuccess
	}

	stepType := strings.TrimSpace(step.Type)
	if stepType == "" {
		stepType = schema.StepTypeTask
	}

	cacheConfig, err := marshalOptionalJSON(step.Cache)
	if err != nil {
		return fmt.Errorf("step %s: cache: %w", step.Name, err)
	}
	outputSchema, err := marshalOptionalJSON(step.OutputSchema)
	if err != nil {
		return fmt.Errorf("step %s: outputSchema: %w", step.Name, err)
	}
	inputSchema, err := marshalOptionalJSON(step.InputSchema)
	if err != nil {
		return fmt.Errorf("step %s: inputSchema: %w", step.Name, err)
	}

	taskModel.AtomID = atomID
	taskModel.Name = step.Name
	taskModel.Type = stepType
	taskModel.NodeSelector = jsonmap.FromStringMap(step.NodeSelector)
	taskModel.Retries = step.Retries
	taskModel.RetryDelay = step.RetryDelay
	taskModel.RetryBackoff = step.RetryBackoff
	taskModel.TriggerRule = triggerRule
	taskModel.CacheConfig = cacheConfig
	taskModel.OutputSchema = outputSchema
	taskModel.InputSchema = inputSchema
	return nil
}

func marshalOptionalJSON(value any) (datatypes.JSON, error) {
	if value == nil {
		return nil, nil
	}
	b, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return datatypes.JSON(b), nil
}

func applyJobProvenance(jobModel *models.Job, opts *ApplyOptions) {
	if jobModel == nil {
		return
	}
	if opts != nil && opts.Provenance != nil {
		prov := opts.Provenance
		jobModel.ProvenanceSourceID = strings.TrimSpace(prov.SourceID)
		jobModel.ProvenanceRepo = strings.TrimSpace(prov.Repo)
		jobModel.ProvenanceRef = strings.TrimSpace(prov.Ref)
		jobModel.ProvenanceCommit = strings.TrimSpace(prov.Commit)
		jobModel.ProvenancePath = strings.TrimSpace(prov.Path)
		return
	}
	jobModel.ProvenanceSourceID = ""
	jobModel.ProvenanceRepo = ""
	jobModel.ProvenanceRef = ""
	jobModel.ProvenanceCommit = ""
	jobModel.ProvenancePath = ""
}

func applyTriggerProvenance(triggerModel *models.Trigger, opts *ApplyOptions) {
	if triggerModel == nil {
		return
	}
	if opts != nil && opts.Provenance != nil {
		copyProvenance(&triggerModel.ProvenanceSourceID, &triggerModel.ProvenanceRepo, &triggerModel.ProvenanceRef, &triggerModel.ProvenanceCommit, &triggerModel.ProvenancePath, opts.Provenance, "trigger")
		return
	}
	triggerModel.ProvenanceSourceID = ""
	triggerModel.ProvenanceRepo = ""
	triggerModel.ProvenanceRef = ""
	triggerModel.ProvenanceCommit = ""
	triggerModel.ProvenancePath = ""
}

func applyAtomProvenance(atomModel *models.Atom, stepName string, opts *ApplyOptions) {
	if atomModel == nil {
		return
	}
	if opts != nil && opts.Provenance != nil {
		suffix := fmt.Sprintf("step/%s", url.PathEscape(stepName))
		copyProvenance(&atomModel.ProvenanceSourceID, &atomModel.ProvenanceRepo, &atomModel.ProvenanceRef, &atomModel.ProvenanceCommit, &atomModel.ProvenancePath, opts.Provenance, suffix)
		return
	}
	atomModel.ProvenanceSourceID = ""
	atomModel.ProvenanceRepo = ""
	atomModel.ProvenanceRef = ""
	atomModel.ProvenanceCommit = ""
	atomModel.ProvenancePath = ""
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

func cloneAnyMap(in map[string]any) map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
