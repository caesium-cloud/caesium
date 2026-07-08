package contract

import (
	"bytes"
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/models"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/caesium-cloud/caesium/pkg/jobdef/schemacompat"
	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

var eventOutputPathPattern = regexp.MustCompile(`^\$\.tasks\[([0-9]+)\]\.output\.([^.\s\[\]]+)$`)

// JobReader reads the merged job world used to derive contract graph edges.
type JobReader interface {
	ListContractJobs(ctx context.Context, incoming []schema.Definition) ([]Job, error)
}

// EvidenceReader reads lineage-observed job-level dataset flow edges.
type EvidenceReader interface {
	ListContractEvidence(ctx context.Context) ([]EvidenceRecord, error)
}

// ProducerSchemaReader reads persisted producer schemas replaced by an incoming batch.
type ProducerSchemaReader interface {
	ListContractProducerSchemas(ctx context.Context, incoming []schema.Definition) ([]ProducerSchemaRecord, error)
}

// Deriver loads authoritative sources and derives a contract graph on demand.
type Deriver struct {
	Jobs            JobReader
	Evidence        EvidenceReader
	ProducerSchemas ProducerSchemaReader
}

// GORMStore reads contract graph inputs from the existing GORM catalog.
type GORMStore struct {
	DB *gorm.DB
}

// NewGORMDeriver returns a Deriver backed by the existing GORM catalog tables.
func NewGORMDeriver(db *gorm.DB) Deriver {
	store := GORMStore{DB: db}
	return Deriver{Jobs: store, Evidence: store, ProducerSchemas: store}
}

// DeriveGraph loads the merged job world, lineage evidence, and returns the
// derived contract graph. Incoming definitions replace persisted jobs with
// matching aliases, mirroring trigger-chain validation.
func (d Deriver) DeriveGraph(ctx context.Context, incoming []schema.Definition) (Graph, error) {
	if d.Jobs == nil {
		return Graph{}, errors.New("contract deriver requires a job reader")
	}
	jobs, err := d.Jobs.ListContractJobs(ctx, incoming)
	if err != nil {
		return Graph{}, err
	}

	var evidence []EvidenceRecord
	if d.Evidence != nil {
		evidence, err = d.Evidence.ListContractEvidence(ctx)
		if err != nil {
			return Graph{}, err
		}
	}

	var previousProducerSchemas []ProducerSchemaRecord
	if d.ProducerSchemas != nil {
		previousProducerSchemas, err = d.ProducerSchemas.ListContractProducerSchemas(ctx, incoming)
		if err != nil {
			return Graph{}, err
		}
	}

	return DeriveGraph(DeriveInput{Jobs: jobs, Evidence: evidence, PreviousProducerSchemas: previousProducerSchemas})
}

// ListContractJobs returns incoming definitions plus persisted jobs not
// replaced by those definitions.
func (s GORMStore) ListContractJobs(ctx context.Context, incoming []schema.Definition) ([]Job, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	jobs := make([]Job, 0, len(incoming))
	incomingAliases := make(map[string]struct{}, len(incoming))
	incomingAliasIndexes := make(map[string]int, len(incoming))
	for idx := range incoming {
		job, err := jobFromDefinition(incoming[idx])
		if err != nil {
			return nil, fmt.Errorf("definition %d: %w", idx, err)
		}
		if _, ok := incomingAliases[job.Alias]; ok {
			return nil, fmt.Errorf("duplicate job alias %q", job.Alias)
		}
		incomingAliases[job.Alias] = struct{}{}
		incomingAliasIndexes[job.Alias] = len(jobs)
		jobs = append(jobs, job)
	}

	if s.DB == nil {
		sortJobs(jobs)
		return jobs, nil
	}

	incomingJobIDs, err := s.existingJobIDsByAlias(ctx, incomingAliases)
	if err != nil {
		return nil, err
	}
	for alias, id := range incomingJobIDs {
		if idx, ok := incomingAliasIndexes[alias]; ok {
			jobs[idx].ID = id
		}
	}

	incomingJobIDSet := make(map[uuid.UUID]struct{}, len(incomingJobIDs))
	for _, id := range incomingJobIDs {
		incomingJobIDSet[id] = struct{}{}
	}

	existing, err := s.persistedContractJobs(ctx, incomingAliases, incomingJobIDSet)
	if err != nil {
		return nil, err
	}
	jobs = append(jobs, existing...)
	sortJobs(jobs)
	return jobs, nil
}

// ListContractProducerSchemas returns resolved persisted producer schemas for
// jobs that the incoming batch is replacing.
func (s GORMStore) ListContractProducerSchemas(ctx context.Context, incoming []schema.Definition) ([]ProducerSchemaRecord, error) {
	if s.DB == nil || len(incoming) == 0 {
		return nil, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	incomingAliases := make(map[string]struct{}, len(incoming))
	for idx := range incoming {
		alias := strings.TrimSpace(incoming[idx].Metadata.Alias)
		if alias == "" {
			return nil, fmt.Errorf("definition %d: metadata.alias is required", idx)
		}
		// Mirror ListContractJobs' duplicate guard so a direct call cannot
		// silently pick an arbitrary definition for a repeated alias.
		if _, exists := incomingAliases[alias]; exists {
			return nil, fmt.Errorf("duplicate job alias %q", alias)
		}
		incomingAliases[alias] = struct{}{}
	}

	incomingJobIDs, err := s.existingJobIDsByAlias(ctx, incomingAliases)
	if err != nil {
		return nil, err
	}
	if len(incomingJobIDs) == 0 {
		return nil, nil
	}

	jobIDs := make([]uuid.UUID, 0, len(incomingJobIDs))
	aliasByJobID := make(map[uuid.UUID]string, len(incomingJobIDs))
	for alias, id := range incomingJobIDs {
		jobIDs = append(jobIDs, id)
		aliasByJobID[id] = alias
	}
	sort.Slice(jobIDs, func(i, j int) bool {
		return aliasByJobID[jobIDs[i]] < aliasByJobID[jobIDs[j]]
	})

	stepsByJobID, err := s.persistedContractSteps(ctx, jobIDs)
	if err != nil {
		return nil, err
	}
	declsByJobID, err := s.persistedDatasetDeclarations(ctx, jobIDs)
	if err != nil {
		return nil, err
	}

	records := make([]ProducerSchemaRecord, 0)
	for _, jobID := range jobIDs {
		alias := aliasByJobID[jobID]
		stepsByName := make(map[string]Step, len(stepsByJobID[jobID]))
		for _, step := range stepsByJobID[jobID] {
			stepsByName[strings.TrimSpace(step.Name)] = step
		}
		for _, decl := range declsByJobID[jobID] {
			if decl.Direction != models.DatasetDirectionProduces {
				continue
			}
			name := strings.TrimSpace(decl.Name)
			if name == "" {
				continue
			}
			stepName := strings.TrimSpace(decl.StepName)
			record := ProducerSchemaRecord{
				JobAlias: alias,
				StepName: stepName,
				Dataset:  datasetRefFromName(name),
			}
			step := stepsByName[stepName]
			producerSchema, schemaKnown, err := persistedProducerSchema(decl, step)
			if err != nil {
				return nil, fmt.Errorf("existing job %s step %s dataset %s schema: %w", alias, stepName, name, err)
			}
			record.Schema = producerSchema
			record.SchemaKnown = schemaKnown
			records = append(records, record)
		}
	}
	sortProducerSchemaRecords(records)
	return records, nil
}

// ListContractEvidence returns distinct job-level lineage evidence edges.
func (s GORMStore) ListContractEvidence(ctx context.Context) ([]EvidenceRecord, error) {
	if s.DB == nil {
		return nil, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	type evidenceRow struct {
		ProducerJobID    uuid.UUID `gorm:"column:producer_job_id"`
		ProducerJobAlias string    `gorm:"column:producer_job_alias"`
		ConsumerJobID    uuid.UUID `gorm:"column:consumer_job_id"`
		ConsumerJobAlias string    `gorm:"column:consumer_job_alias"`
		Namespace        string    `gorm:"column:namespace"`
		Name             string    `gorm:"column:name"`
		LastSeen         sqlTime   `gorm:"column:last_seen"`
	}
	var rows []evidenceRow

	lineageJobDatasetAggregate := func() *gorm.DB {
		return s.DB.WithContext(ctx).
			Table("lineage_datasets AS ld").
			Select(
				"job_runs.job_id AS job_id," +
					" jobs.alias AS job_alias," +
					" ld.namespace AS namespace," +
					" ld.name AS name," +
					" ld.direction AS direction," +
					" MAX(ld.created_at) AS last_seen",
			).
			Joins("JOIN task_runs ON task_runs.id = ld.task_run_id").
			Joins("JOIN job_runs ON job_runs.id = task_runs.job_run_id").
			Joins("JOIN jobs ON jobs.id = job_runs.job_id AND jobs.deleted_at IS NULL").
			Group("job_runs.job_id, jobs.alias, ld.namespace, ld.name, ld.direction")
	}

	err := s.DB.WithContext(ctx).
		Table("(?) AS producer", lineageJobDatasetAggregate()).
		Select(
			"producer.job_id AS producer_job_id,"+
				" producer.job_alias AS producer_job_alias,"+
				" consumer.job_id AS consumer_job_id,"+
				" consumer.job_alias AS consumer_job_alias,"+
				" producer.namespace AS namespace,"+
				" producer.name AS name,"+
				" consumer.last_seen AS last_seen",
		).
		Joins(
			"JOIN (?) AS consumer ON consumer.namespace = producer.namespace"+
				" AND consumer.name = producer.name"+
				" AND producer.direction = ?"+
				" AND consumer.direction = ?"+
				" AND producer.job_id <> consumer.job_id",
			lineageJobDatasetAggregate(),
			"output",
			"input",
		).
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}

	records := make([]EvidenceRecord, 0, len(rows))
	for _, row := range rows {
		records = append(records, EvidenceRecord{
			ProducerJobID:    row.ProducerJobID,
			ProducerJobAlias: strings.TrimSpace(row.ProducerJobAlias),
			ConsumerJobID:    row.ConsumerJobID,
			ConsumerJobAlias: strings.TrimSpace(row.ConsumerJobAlias),
			Dataset: DatasetRef{
				Namespace: strings.TrimSpace(row.Namespace),
				Name:      strings.TrimSpace(row.Name),
			},
			LastSeen: row.LastSeen.Time,
		})
	}
	sortEvidence(records)
	return records, nil
}

type sqlTime struct {
	time.Time
}

func (t *sqlTime) Scan(value any) error {
	switch v := value.(type) {
	case time.Time:
		t.Time = v
		return nil
	case string:
		return t.scanString(v)
	case []byte:
		return t.scanString(string(v))
	case nil:
		t.Time = time.Time{}
		return nil
	default:
		return fmt.Errorf("unsupported timestamp value %T", value)
	}
}

func (t sqlTime) Value() (driver.Value, error) {
	if t.IsZero() {
		return nil, nil
	}
	return t.Time, nil
}

func (t *sqlTime) scanString(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		t.Time = time.Time{}
		return nil
	}
	for _, layout := range []string{
		time.RFC3339Nano,
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05.999999999Z07:00",
		"2006-01-02 15:04:05.999999999 -0700 MST",
		"2006-01-02 15:04:05-07:00",
		"2006-01-02 15:04:05",
	} {
		parsed, err := time.Parse(layout, value)
		if err == nil {
			t.Time = parsed
			return nil
		}
	}
	return fmt.Errorf("parse timestamp %q", value)
}

// DeriveGraph derives a contract graph from already-loaded jobs and lineage
// evidence. It performs no writes and stores no graph state.
func DeriveGraph(input DeriveInput) (Graph, error) {
	builder := newGraphBuilder()

	jobs := append([]Job(nil), input.Jobs...)
	evidence := append([]EvidenceRecord(nil), input.Evidence...)
	sortJobs(jobs)

	seenAliases := make(map[string]struct{}, len(jobs))
	for idx := range jobs {
		jobs[idx].Alias = strings.TrimSpace(jobs[idx].Alias)
		if jobs[idx].Alias == "" {
			return Graph{}, fmt.Errorf("job %d: alias is required", idx)
		}
		if _, exists := seenAliases[jobs[idx].Alias]; exists {
			return Graph{}, fmt.Errorf("duplicate job alias %q", jobs[idx].Alias)
		}
		seenAliases[jobs[idx].Alias] = struct{}{}
		builder.addJob(jobs[idx])
	}

	deriveDeclaredEdges(builder, jobs, input.PreviousProducerSchemas)
	if err := deriveInferredEdges(builder, jobs); err != nil {
		return Graph{}, err
	}
	deriveEvidenceEdges(builder, evidence)

	return builder.graph(), nil
}

func (s GORMStore) existingJobIDsByAlias(ctx context.Context, incomingAliases map[string]struct{}) (map[string]uuid.UUID, error) {
	if len(incomingAliases) == 0 {
		return nil, nil
	}

	aliases := make([]string, 0, len(incomingAliases))
	for alias := range incomingAliases {
		aliases = append(aliases, alias)
	}

	var rows []struct {
		ID    uuid.UUID
		Alias string
	}
	err := s.DB.WithContext(ctx).
		Table("jobs").
		Select("id, alias").
		Where("deleted_at IS NULL").
		Where("alias IN ?", aliases).
		Find(&rows).Error
	if err != nil {
		return nil, err
	}

	ids := make(map[string]uuid.UUID, len(rows))
	for _, row := range rows {
		alias := strings.TrimSpace(row.Alias)
		if alias == "" || row.ID == uuid.Nil {
			continue
		}
		ids[alias] = row.ID
	}
	return ids, nil
}

func (s GORMStore) persistedContractJobs(ctx context.Context, incomingAliases map[string]struct{}, incomingJobIDs map[uuid.UUID]struct{}) ([]Job, error) {
	type jobRow struct {
		ID            uuid.UUID         `gorm:"column:id"`
		Alias         string            `gorm:"column:alias"`
		Labels        datatypes.JSONMap `gorm:"column:labels"`
		TriggerType   string            `gorm:"column:trigger_type"`
		Configuration string            `gorm:"column:configuration"`
	}
	var rows []jobRow

	err := s.DB.WithContext(ctx).
		Table("jobs").
		Select("jobs.id, jobs.alias, jobs.labels, triggers.type AS trigger_type, triggers.configuration").
		Joins("JOIN triggers ON triggers.id = jobs.trigger_id AND triggers.deleted_at IS NULL").
		Where("jobs.deleted_at IS NULL").
		Find(&rows).Error
	if err != nil {
		return nil, err
	}

	eligibleRows := make([]jobRow, 0, len(rows))
	jobIDs := make([]uuid.UUID, 0, len(rows))
	for _, row := range rows {
		alias := strings.TrimSpace(row.Alias)
		if alias == "" {
			continue
		}
		if _, replaced := incomingAliases[alias]; replaced {
			continue
		}
		if _, replaced := incomingJobIDs[row.ID]; replaced {
			continue
		}
		row.Alias = alias
		eligibleRows = append(eligibleRows, row)
		jobIDs = append(jobIDs, row.ID)
	}

	stepsByJobID, err := s.persistedContractSteps(ctx, jobIDs)
	if err != nil {
		return nil, err
	}
	declsByJobID, err := s.persistedDatasetDeclarations(ctx, jobIDs)
	if err != nil {
		return nil, err
	}

	jobs := make([]Job, 0, len(eligibleRows))
	for _, row := range eligibleRows {
		cfg, err := parseJSONMap(row.Configuration)
		if err != nil {
			return nil, fmt.Errorf("existing trigger %s configuration: %w", row.Alias, err)
		}
		steps, err := attachPersistedDatasetDeclarations(stepsByJobID[row.ID], declsByJobID[row.ID])
		if err != nil {
			return nil, fmt.Errorf("existing job %s dataset declarations: %w", row.Alias, err)
		}
		jobs = append(jobs, Job{
			ID:     row.ID,
			Alias:  row.Alias,
			Labels: stringLabels(row.Labels),
			Trigger: Trigger{
				Type:          strings.TrimSpace(row.TriggerType),
				Configuration: cfg,
			},
			Steps: steps,
		})
	}
	return jobs, nil
}

func (s GORMStore) persistedContractSteps(ctx context.Context, jobIDs []uuid.UUID) (map[uuid.UUID][]Step, error) {
	stepsByJobID := make(map[uuid.UUID][]Step, len(jobIDs))
	if len(jobIDs) == 0 {
		return stepsByJobID, nil
	}

	var tasks []models.Task
	if err := s.DB.WithContext(ctx).
		Where("job_id IN ?", jobIDs).
		Order("job_id ASC").
		Order("position ASC").
		Order("created_at ASC").
		Find(&tasks).Error; err != nil {
		return nil, err
	}

	for _, task := range tasks {
		outputSchema, err := parseJSONMapBytes(task.OutputSchema)
		if err != nil {
			return nil, fmt.Errorf("task %s output_schema: %w", task.Name, err)
		}
		stepsByJobID[task.JobID] = append(stepsByJobID[task.JobID], Step{
			Name:         strings.TrimSpace(task.Name),
			OutputSchema: outputSchema,
		})
	}
	return stepsByJobID, nil
}

func (s GORMStore) persistedDatasetDeclarations(ctx context.Context, jobIDs []uuid.UUID) (map[uuid.UUID][]models.DatasetDeclaration, error) {
	declsByJobID := make(map[uuid.UUID][]models.DatasetDeclaration, len(jobIDs))
	if len(jobIDs) == 0 {
		return declsByJobID, nil
	}

	var decls []models.DatasetDeclaration
	if err := s.DB.WithContext(ctx).
		Where("job_id IN ?", jobIDs).
		Order("job_id ASC").
		Order("step_name ASC").
		Order("name ASC").
		Find(&decls).Error; err != nil {
		return nil, err
	}

	for _, decl := range decls {
		declsByJobID[decl.JobID] = append(declsByJobID[decl.JobID], decl)
	}
	return declsByJobID, nil
}

func jobFromDefinition(def schema.Definition) (Job, error) {
	alias := strings.TrimSpace(def.Metadata.Alias)
	if alias == "" {
		return Job{}, errors.New("metadata.alias is required")
	}

	steps := make([]Step, 0, len(def.Steps))
	for _, step := range def.Steps {
		steps = append(steps, Step{
			Name:         strings.TrimSpace(step.Name),
			OutputSchema: cloneAnyMap(step.OutputSchema),
			Produces:     producedDatasetsFromDefinition(step.Datasets),
			Consumes:     consumedDatasetsFromDefinition(step.Datasets),
		})
	}

	return Job{
		Alias:    alias,
		Labels:   cloneStringMap(def.Metadata.Labels),
		Incoming: true,
		Trigger: Trigger{
			Type:          strings.TrimSpace(def.Trigger.Type),
			Configuration: cloneAnyMap(def.Trigger.Configuration),
		},
		Steps: steps,
	}, nil
}

func producedDatasetsFromDefinition(datasets *schema.StepDatasets) []ProducedDataset {
	if datasets == nil || len(datasets.Produces) == 0 {
		return nil
	}
	out := make([]ProducedDataset, 0, len(datasets.Produces))
	for _, produced := range datasets.Produces {
		name := strings.TrimSpace(produced.Name)
		if name == "" {
			continue
		}
		out = append(out, ProducedDataset{
			Name:       name,
			Schema:     cloneAnyMap(produced.Schema),
			SchemaFrom: strings.TrimSpace(produced.SchemaFrom),
			Version:    produced.Version,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Name < out[j].Name
	})
	return out
}

func consumedDatasetsFromDefinition(datasets *schema.StepDatasets) []ConsumedDataset {
	if datasets == nil || len(datasets.Consumes) == 0 {
		return nil
	}
	out := make([]ConsumedDataset, 0, len(datasets.Consumes))
	for _, consumed := range datasets.Consumes {
		name := strings.TrimSpace(consumed.Name)
		if name == "" {
			continue
		}
		out = append(out, ConsumedDataset{
			Name:   name,
			Schema: cloneAnyMap(consumed.Schema),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Name < out[j].Name
	})
	return out
}

func attachPersistedDatasetDeclarations(steps []Step, decls []models.DatasetDeclaration) ([]Step, error) {
	if len(steps) == 0 || len(decls) == 0 {
		return steps, nil
	}

	stepIndexByName := make(map[string]int, len(steps))
	for idx := range steps {
		stepIndexByName[strings.TrimSpace(steps[idx].Name)] = idx
	}

	for _, decl := range decls {
		name := strings.TrimSpace(decl.Name)
		if name == "" {
			continue
		}
		stepName := strings.TrimSpace(decl.StepName)
		idx, ok := stepIndexByName[stepName]
		if !ok {
			continue
		}
		switch decl.Direction {
		case models.DatasetDirectionProduces:
			inlineSchema, err := persistedInlineSchema(decl)
			if err != nil {
				return nil, fmt.Errorf("%s produces %s: %w", stepName, name, err)
			}
			produced := ProducedDataset{
				Name:       name,
				Schema:     inlineSchema,
				SchemaFrom: strings.TrimSpace(decl.SchemaFrom),
				Version:    decl.SchemaVersion,
			}
			steps[idx].Produces = append(steps[idx].Produces, produced)
		case models.DatasetDirectionConsumes:
			inlineSchema, err := persistedInlineSchema(decl)
			if err != nil {
				return nil, fmt.Errorf("%s consumes %s: %w", stepName, name, err)
			}
			steps[idx].Consumes = append(steps[idx].Consumes, ConsumedDataset{
				Name:   name,
				Schema: inlineSchema,
			})
		}
	}

	for idx := range steps {
		sort.Slice(steps[idx].Produces, func(i, j int) bool {
			return steps[idx].Produces[i].Name < steps[idx].Produces[j].Name
		})
		sort.Slice(steps[idx].Consumes, func(i, j int) bool {
			return steps[idx].Consumes[i].Name < steps[idx].Consumes[j].Name
		})
	}
	return steps, nil
}

func persistedProducerSchema(decl models.DatasetDeclaration, step Step) (map[string]any, bool, error) {
	inlineSchema, err := persistedInlineSchema(decl)
	if err != nil {
		return nil, false, err
	}
	if strings.TrimSpace(decl.SchemaJSON) != "" {
		return inlineSchema, true, nil
	}
	if strings.TrimSpace(decl.SchemaFrom) == schema.DatasetSchemaFromOutput && len(step.OutputSchema) > 0 {
		return cloneAnyMap(step.OutputSchema), true, nil
	}
	return nil, false, nil
}

func persistedInlineSchema(decl models.DatasetDeclaration) (map[string]any, error) {
	raw := strings.TrimSpace(decl.SchemaJSON)
	if raw == "" {
		return nil, nil
	}
	out, err := parseJSONMap(raw)
	if err != nil {
		return nil, fmt.Errorf("schema_json: %w", err)
	}
	return out, nil
}

type declaredProducer struct {
	job         Job
	step        Step
	dataset     DatasetRef
	schema      map[string]any
	schemaKnown bool
}

type declaredConsumer struct {
	job         Job
	step        Step
	dataset     DatasetRef
	schema      map[string]any
	schemaKnown bool
}

func deriveDeclaredEdges(builder *graphBuilder, jobs []Job, previousSchemas []ProducerSchemaRecord) {
	previousByKey := make(map[producerSchemaKey]ProducerSchemaRecord, len(previousSchemas))
	for _, record := range previousSchemas {
		key := producerSchemaKeyFor(record.JobAlias, record.StepName, record.Dataset)
		if key.jobAlias == "" || key.datasetName == "" {
			continue
		}
		previousByKey[key] = record
	}

	producersByDataset := map[string][]declaredProducer{}
	consumersByDataset := map[string][]declaredConsumer{}

	for _, job := range jobs {
		for _, step := range job.Steps {
			for _, produced := range step.Produces {
				dataset := datasetRefFromName(produced.Name)
				if dataset.Name == "" {
					continue
				}
				producerSchema, schemaKnown := resolveProducedDatasetSchema(step, produced)
				producersByDataset[dataset.Name] = append(producersByDataset[dataset.Name], declaredProducer{
					job:         job,
					step:        step,
					dataset:     dataset,
					schema:      producerSchema,
					schemaKnown: schemaKnown,
				})
			}
			for _, consumed := range step.Consumes {
				dataset := datasetRefFromName(consumed.Name)
				if dataset.Name == "" {
					continue
				}
				consumerSchema := cloneAnyMap(consumed.Schema)
				consumersByDataset[dataset.Name] = append(consumersByDataset[dataset.Name], declaredConsumer{
					job:         job,
					step:        step,
					dataset:     dataset,
					schema:      consumerSchema,
					schemaKnown: len(consumerSchema) > 0,
				})
			}
		}
	}

	datasetNames := make([]string, 0, len(producersByDataset))
	for name := range producersByDataset {
		if len(consumersByDataset[name]) > 0 {
			datasetNames = append(datasetNames, name)
		}
	}
	sort.Strings(datasetNames)

	for _, datasetName := range datasetNames {
		producers := producersByDataset[datasetName]
		consumers := consumersByDataset[datasetName]
		sortDeclaredProducers(producers)
		sortDeclaredConsumers(consumers)

		for _, producer := range producers {
			builder.addDataset(producer.dataset)
			previous, previousExists := previousByKey[producerSchemaKeyFor(producer.job.Alias, producer.step.Name, producer.dataset)]
			compareFindings, previousSchema := declaredCompareFindings(producer, previous, previousExists)
			for _, consumer := range consumers {
				if producer.job.Alias == consumer.job.Alias {
					continue
				}
				dataset := producer.dataset
				findings := declaredEdgeFindings(compareFindings, producer, consumer)
				builder.addEdge(Edge{
					From:                   jobNodeID(producer.job.Alias),
					To:                     jobNodeID(consumer.job.Alias),
					Class:                  EdgeClassDeclared,
					Verdict:                verdictForFindings(findings, schemacompat.VerdictCompatible),
					Findings:               findings,
					Dataset:                &dataset,
					ProducerSchema:         cloneAnyMap(producer.schema),
					PreviousProducerSchema: cloneAnyMap(previousSchema),
					ConsumerSchema:         cloneAnyMap(consumer.schema),
				})
			}
		}
	}
}

func resolveProducedDatasetSchema(step Step, produced ProducedDataset) (map[string]any, bool) {
	if len(produced.Schema) > 0 {
		return cloneAnyMap(produced.Schema), true
	}
	if strings.TrimSpace(produced.SchemaFrom) == schema.DatasetSchemaFromOutput && len(step.OutputSchema) > 0 {
		return cloneAnyMap(step.OutputSchema), true
	}
	return nil, false
}

func declaredCompareFindings(producer declaredProducer, previous ProducerSchemaRecord, previousExists bool) ([]schemacompat.Finding, map[string]any) {
	var findings []schemacompat.Finding
	var previousSchema map[string]any
	if producer.job.Incoming && previousExists {
		if !previous.SchemaKnown {
			findings = append(findings, schemacompat.Finding{
				Kind:    schemacompat.FindingKindRequirementUnknown,
				Detail:  fmt.Sprintf("persisted producer schema for dataset %q on %s/%s is unavailable; cannot compare old producer schema to the incoming schema", producer.dataset.Name, producer.job.Alias, producer.step.Name),
				Verdict: schemacompat.VerdictUnknown,
			})
			return prefixFindings(findings, declaredProduceFindingPath(producer.dataset)), nil
		}
		previousSchema = cloneAnyMap(previous.Schema)
	} else if producer.schemaKnown {
		previousSchema = cloneAnyMap(producer.schema)
	}

	if producer.job.Incoming && previousExists && previous.SchemaKnown {
		findings = append(findings, schemacompat.Compare(previousSchema, schemaForCompare(producer.schema, producer.schemaKnown))...)
	}
	return prefixFindings(findings, declaredProduceFindingPath(producer.dataset)), previousSchema
}

func declaredEdgeFindings(compareFindings []schemacompat.Finding, producer declaredProducer, consumer declaredConsumer) []schemacompat.Finding {
	findings := make([]schemacompat.Finding, 0, len(compareFindings))
	if !consumer.schemaKnown {
		findings = append(findings, compareFindings...)
		return sortedFindings(findings)
	}

	requirementFindings := schemacompat.Satisfies(schemaForCompare(producer.schema, producer.schemaKnown), consumer.schema)
	findings = append(findings, prefixFindings(requirementFindings, declaredConsumeFindingPath(consumer.dataset))...)
	for _, finding := range compareFindings {
		if finding.Verdict != schemacompat.VerdictBreaking {
			findings = append(findings, finding)
		}
	}
	return sortedFindings(findings)
}

func schemaForCompare(schema map[string]any, known bool) map[string]any {
	if !known {
		return nil
	}
	return schema
}

func datasetRefFromName(name string) DatasetRef {
	return DatasetRef{Name: strings.TrimSpace(name)}
}

func declaredProduceFindingPath(dataset DatasetRef) string {
	return "datasets.produces." + strings.TrimSpace(dataset.Name) + ".schema"
}

func declaredConsumeFindingPath(dataset DatasetRef) string {
	return "datasets.consumes." + strings.TrimSpace(dataset.Name) + ".schema"
}

func prefixFindings(findings []schemacompat.Finding, prefix string) []schemacompat.Finding {
	if len(findings) == 0 {
		return nil
	}
	out := make([]schemacompat.Finding, 0, len(findings))
	prefix = strings.TrimSuffix(strings.TrimSpace(prefix), ".")
	for _, finding := range findings {
		if prefix != "" {
			finding.Path = prefixFindingPath(prefix, finding.Path)
		}
		out = append(out, finding)
	}
	return sortedFindings(out)
}

func prefixFindingPath(prefix, path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return prefix
	}
	return prefix + "." + path
}

func sortedFindings(findings []schemacompat.Finding) []schemacompat.Finding {
	sort.Slice(findings, func(i, j int) bool {
		if findings[i].Path == findings[j].Path {
			if findings[i].Kind == findings[j].Kind {
				return findings[i].Detail < findings[j].Detail
			}
			return findings[i].Kind < findings[j].Kind
		}
		return findings[i].Path < findings[j].Path
	})
	return findings
}

type producerSchemaKey struct {
	jobAlias    string
	stepName    string
	datasetName string
}

func producerSchemaKeyFor(jobAlias, stepName string, dataset DatasetRef) producerSchemaKey {
	return producerSchemaKey{
		jobAlias:    strings.TrimSpace(jobAlias),
		stepName:    strings.TrimSpace(stepName),
		datasetName: strings.TrimSpace(dataset.Name),
	}
}

func sortDeclaredProducers(producers []declaredProducer) {
	sort.Slice(producers, func(i, j int) bool {
		if producers[i].job.Alias == producers[j].job.Alias {
			if producers[i].step.Name == producers[j].step.Name {
				return producers[i].dataset.Name < producers[j].dataset.Name
			}
			return producers[i].step.Name < producers[j].step.Name
		}
		return producers[i].job.Alias < producers[j].job.Alias
	})
}

func sortDeclaredConsumers(consumers []declaredConsumer) {
	sort.Slice(consumers, func(i, j int) bool {
		if consumers[i].job.Alias == consumers[j].job.Alias {
			if consumers[i].step.Name == consumers[j].step.Name {
				return consumers[i].dataset.Name < consumers[j].dataset.Name
			}
			return consumers[i].step.Name < consumers[j].step.Name
		}
		return consumers[i].job.Alias < consumers[j].job.Alias
	})
}

func sortProducerSchemaRecords(records []ProducerSchemaRecord) {
	sort.Slice(records, func(i, j int) bool {
		if records[i].JobAlias == records[j].JobAlias {
			if records[i].StepName == records[j].StepName {
				return records[i].Dataset.Name < records[j].Dataset.Name
			}
			return records[i].StepName < records[j].StepName
		}
		return records[i].JobAlias < records[j].JobAlias
	})
}

func deriveInferredEdges(builder *graphBuilder, jobs []Job) error {
	jobsByAlias := make(map[string]Job, len(jobs))
	aliasByJobID := make(map[string]string, len(jobs))
	knownAliases := make([]string, 0, len(jobs))
	for _, job := range jobs {
		jobsByAlias[job.Alias] = job
		knownAliases = append(knownAliases, job.Alias)
		if job.ID != uuid.Nil {
			aliasByJobID[job.ID.String()] = job.Alias
		}
	}
	sort.Strings(knownAliases)

	pairs := make(map[inferredPair]struct{})
	for _, consumer := range jobs {
		if consumer.Trigger.Type != schema.TriggerEvent && consumer.Trigger.Type != string(models.TriggerTypeEvent) {
			continue
		}

		outputRefs, err := outputParamMappingRefs(consumer.Trigger.Configuration)
		if err != nil {
			return fmt.Errorf("definition %s: %w", consumer.Alias, err)
		}
		if len(outputRefs) == 0 {
			continue
		}

		patterns, err := triggerChainPatterns(consumer.Trigger.Configuration)
		if err != nil {
			return fmt.Errorf("definition %s: %w", consumer.Alias, err)
		}
		for _, pattern := range patterns {
			if !patternCanMatchCaesiumLifecycle(pattern) {
				continue
			}
			sourceAlias, scoped := triggerChainPatternSourceAlias(pattern, aliasByJobID)
			if scoped {
				addInferredPair(pairs, sourceAlias, consumer.Alias)
				continue
			}
			for _, alias := range knownAliases {
				addInferredPair(pairs, alias, consumer.Alias)
			}
		}
	}

	orderedPairs := make([]inferredPair, 0, len(pairs))
	for p := range pairs {
		orderedPairs = append(orderedPairs, p)
	}
	sort.Slice(orderedPairs, func(i, j int) bool {
		if orderedPairs[i].from == orderedPairs[j].from {
			return orderedPairs[i].to < orderedPairs[j].to
		}
		return orderedPairs[i].from < orderedPairs[j].from
	})

	for _, p := range orderedPairs {
		consumer := jobsByAlias[p.to]
		outputRefs, err := outputParamMappingRefs(consumer.Trigger.Configuration)
		if err != nil {
			return fmt.Errorf("definition %s: %w", consumer.Alias, err)
		}
		producer, ok := jobsByAlias[p.from]
		if !ok {
			findings := unknownProducerFindings(p.from, consumer, outputRefs)
			builder.addJob(Job{Alias: p.from})
			builder.addEdge(Edge{
				From:     jobNodeID(p.from),
				To:       jobNodeID(consumer.Alias),
				Class:    EdgeClassInferred,
				Verdict:  schemacompat.VerdictUnknown,
				Findings: findings,
			})
			continue
		}
		findings := inferredFindings(producer, outputRefs)
		builder.addEdge(Edge{
			From:     jobNodeID(producer.Alias),
			To:       jobNodeID(consumer.Alias),
			Class:    EdgeClassInferred,
			Verdict:  verdictForFindings(findings, schemacompat.VerdictCompatible),
			Findings: findings,
		})
	}
	return nil
}

type inferredPair struct {
	from string
	to   string
}

func addInferredPair(pairs map[inferredPair]struct{}, from, to string) {
	from = strings.TrimSpace(from)
	to = strings.TrimSpace(to)
	if from == "" || to == "" || from == to {
		return
	}
	pairs[inferredPair{from: from, to: to}] = struct{}{}
}

func deriveEvidenceEdges(builder *graphBuilder, records []EvidenceRecord) {
	sortEvidence(records)
	for _, record := range records {
		producerAlias := strings.TrimSpace(record.ProducerJobAlias)
		consumerAlias := strings.TrimSpace(record.ConsumerJobAlias)
		if producerAlias == "" || consumerAlias == "" || producerAlias == consumerAlias {
			continue
		}
		dataset := DatasetRef{
			Namespace: strings.TrimSpace(record.Dataset.Namespace),
			Name:      strings.TrimSpace(record.Dataset.Name),
		}
		if dataset.Namespace == "" && dataset.Name == "" {
			continue
		}

		builder.addJob(Job{ID: record.ProducerJobID, Alias: producerAlias})
		builder.addJob(Job{ID: record.ConsumerJobID, Alias: consumerAlias})
		builder.addDataset(dataset)

		lastSeen := record.LastSeen
		builder.addEdge(Edge{
			From:     jobNodeID(producerAlias),
			To:       jobNodeID(consumerAlias),
			Class:    EdgeClassEvidence,
			Verdict:  schemacompat.VerdictUnknown,
			Dataset:  &dataset,
			LastSeen: &lastSeen,
		})
	}
}

type triggerChainPattern struct {
	eventType string
	source    string
	filter    map[string]string
}

func triggerChainPatterns(cfg map[string]any) ([]triggerChainPattern, error) {
	rawEvents, ok := cfg["events"]
	if !ok || rawEvents == nil {
		return nil, nil
	}
	events, ok := rawEvents.([]any)
	if !ok {
		return nil, fmt.Errorf("trigger.configuration.events must be a list")
	}

	patterns := make([]triggerChainPattern, 0, len(events))
	for idx, rawEvent := range events {
		eventMap, ok := rawEvent.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("trigger.configuration.events[%d] must be an object", idx)
		}
		pattern := triggerChainPattern{filter: map[string]string{}}
		pattern.eventType, _ = eventMap["type"].(string)
		pattern.source, _ = eventMap["source"].(string)
		if rawFilter, ok := eventMap["filter"]; ok && rawFilter != nil {
			filter, ok := rawFilter.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("trigger.configuration.events[%d].filter must be an object", idx)
			}
			for key, value := range filter {
				if stringValue, ok := value.(string); ok {
					pattern.filter[key] = stringValue
				}
			}
		}
		patterns = append(patterns, pattern)
	}
	return patterns, nil
}

func patternCanMatchCaesiumLifecycle(pattern triggerChainPattern) bool {
	source := strings.TrimSpace(pattern.source)
	if source != "" && source != "caesium" {
		return false
	}
	for _, lifecycleType := range []event.Type{event.TypeRunCompleted, event.TypeRunFailed, event.TypeRunTerminal} {
		if matchesTriggerChainEventType(pattern.eventType, string(lifecycleType)) {
			return true
		}
	}
	return false
}

func triggerChainPatternSourceAlias(pattern triggerChainPattern, aliasByJobID map[string]string) (string, bool) {
	sourceAlias := strings.TrimSpace(pattern.filter["job_alias"])
	sourceJobID := strings.TrimSpace(pattern.filter["job_id"])

	if sourceAlias == "" && sourceJobID == "" {
		return "", false
	}
	if sourceJobID == "" {
		return sourceAlias, true
	}

	resolvedAlias := aliasByJobID[sourceJobID]
	if sourceAlias == "" {
		if resolvedAlias != "" {
			return resolvedAlias, true
		}
		return sourceJobID, true
	}
	if resolvedAlias != "" && resolvedAlias != sourceAlias {
		return "", true
	}
	return sourceAlias, true
}

func matchesTriggerChainEventType(patternValue, eventType string) bool {
	patternValue = strings.TrimSpace(patternValue)
	eventType = strings.TrimSpace(eventType)
	if patternValue == "" || eventType == "" {
		return false
	}
	if !strings.ContainsAny(patternValue, "*?[") {
		return patternValue == eventType
	}
	matched, err := path.Match(patternValue, eventType)
	return err == nil && matched
}

type outputRef struct {
	paramName string
	jsonPath  string
	stepIndex int
	key       string
}

func outputParamMappingRefs(cfg map[string]any) ([]outputRef, error) {
	mapping, err := paramMapping(cfg)
	if err != nil {
		return nil, err
	}
	if len(mapping) == 0 {
		return nil, nil
	}

	params := make([]string, 0, len(mapping))
	for param := range mapping {
		params = append(params, param)
	}
	sort.Strings(params)

	refs := make([]outputRef, 0, len(mapping))
	for _, param := range params {
		jsonPath := strings.TrimSpace(mapping[param])
		match := eventOutputPathPattern.FindStringSubmatch(jsonPath)
		if len(match) != 3 {
			continue
		}
		stepIndex, err := strconv.Atoi(match[1])
		if err != nil {
			continue
		}
		refs = append(refs, outputRef{
			paramName: param,
			jsonPath:  jsonPath,
			stepIndex: stepIndex,
			key:       match[2],
		})
	}
	return refs, nil
}

func paramMapping(cfg map[string]any) (map[string]string, error) {
	rawMapping, ok := cfg["paramMapping"]
	if !ok || rawMapping == nil {
		return nil, nil
	}

	switch mapping := rawMapping.(type) {
	case map[string]string:
		out := make(map[string]string, len(mapping))
		for key, value := range mapping {
			out[key] = value
		}
		return out, nil
	case map[string]any:
		out := make(map[string]string, len(mapping))
		for key, value := range mapping {
			stringValue, ok := value.(string)
			if !ok {
				return nil, fmt.Errorf("trigger.configuration.paramMapping[%q] must be a string", key)
			}
			out[key] = stringValue
		}
		return out, nil
	default:
		return nil, fmt.Errorf("trigger.configuration.paramMapping must be a map of string keys and values")
	}
}

func inferredFindings(producer Job, refs []outputRef) []schemacompat.Finding {
	findings := make([]schemacompat.Finding, 0)
	for _, ref := range refs {
		if ref.stepIndex >= 0 && ref.stepIndex < len(producer.Steps) {
			step := producer.Steps[ref.stepIndex]
			if outputSchemaHasProperty(step.OutputSchema, ref.key) {
				continue
			}
			findings = append(findings, schemacompat.Finding{
				Kind:    schemacompat.FindingKindRequirementUnsatisfied,
				Path:    paramMappingFindingPath(ref.paramName),
				Key:     ref.key,
				Detail:  fmt.Sprintf("paramMapping %q references %s output key %q from producer %s step %q, but that key is missing from the step outputSchema", ref.paramName, ref.jsonPath, ref.key, producer.Alias, step.Name),
				Verdict: schemacompat.VerdictBreaking,
			})
			continue
		}

		foundInUnion := producerOutputSchemaUnionHasProperty(producer, ref.key)
		kind := schemacompat.FindingKindRequirementUnknown
		detail := fmt.Sprintf("paramMapping %q references %s, but tasks[%d] cannot be resolved to a producer step in %s; key %q was found in the producer outputSchema union", ref.paramName, ref.jsonPath, ref.stepIndex, producer.Alias, ref.key)
		if !foundInUnion {
			kind = schemacompat.FindingKindRequirementUnsatisfied
			detail = fmt.Sprintf("paramMapping %q references %s, but tasks[%d] cannot be resolved to a producer step in %s and key %q is missing from every producer step outputSchema", ref.paramName, ref.jsonPath, ref.stepIndex, producer.Alias, ref.key)
		}
		findings = append(findings, schemacompat.Finding{
			Kind:    kind,
			Path:    paramMappingFindingPath(ref.paramName),
			Key:     ref.key,
			Detail:  detail,
			Verdict: schemacompat.VerdictUnknown,
		})
	}

	sort.Slice(findings, func(i, j int) bool {
		if findings[i].Path == findings[j].Path {
			return findings[i].Detail < findings[j].Detail
		}
		return findings[i].Path < findings[j].Path
	})
	return findings
}

func unknownProducerFindings(producerAlias string, consumer Job, refs []outputRef) []schemacompat.Finding {
	findings := make([]schemacompat.Finding, 0, len(refs))
	for _, ref := range refs {
		findings = append(findings, schemacompat.Finding{
			Kind:    schemacompat.FindingKindRequirementUnknown,
			Path:    paramMappingFindingPath(ref.paramName),
			Key:     ref.key,
			Detail:  fmt.Sprintf("paramMapping %q on %s references %s from producer %s, but that producer is not present in the merged job set; cannot prove the output key %q exists", ref.paramName, consumer.Alias, ref.jsonPath, producerAlias, ref.key),
			Verdict: schemacompat.VerdictUnknown,
		})
	}
	return findings
}

func paramMappingFindingPath(paramName string) string {
	return "trigger.configuration.paramMapping." + paramName
}

func outputSchemaHasProperty(outputSchema map[string]any, key string) bool {
	key = strings.TrimSpace(key)
	if key == "" || len(outputSchema) == 0 {
		return false
	}
	props, ok := outputSchema["properties"].(map[string]any)
	if !ok {
		return false
	}
	_, ok = props[key]
	return ok
}

func producerOutputSchemaUnionHasProperty(producer Job, key string) bool {
	for _, step := range producer.Steps {
		if outputSchemaHasProperty(step.OutputSchema, key) {
			return true
		}
	}
	return false
}

func verdictForFindings(findings []schemacompat.Finding, defaultVerdict schemacompat.Verdict) schemacompat.Verdict {
	verdict := defaultVerdict
	for _, finding := range findings {
		switch finding.Verdict {
		case schemacompat.VerdictBreaking:
			return schemacompat.VerdictBreaking
		case schemacompat.VerdictUnknown:
			verdict = schemacompat.VerdictUnknown
		}
	}
	return verdict
}

type graphBuilder struct {
	nodes map[string]Node
	edges map[string]Edge
}

func newGraphBuilder() *graphBuilder {
	return &graphBuilder{
		nodes: map[string]Node{},
		edges: map[string]Edge{},
	}
}

func (b *graphBuilder) addJob(job Job) {
	alias := strings.TrimSpace(job.Alias)
	if alias == "" {
		return
	}
	id := jobNodeID(alias)
	node := Node{
		ID:     id,
		Kind:   NodeKindJob,
		Alias:  alias,
		Labels: cloneStringMap(job.Labels),
	}
	if existing, ok := b.nodes[id]; ok {
		if len(existing.Labels) == 0 && len(node.Labels) > 0 {
			existing.Labels = node.Labels
			b.nodes[id] = existing
		}
		return
	}
	b.nodes[id] = node
}

func (b *graphBuilder) addDataset(dataset DatasetRef) {
	dataset.Namespace = strings.TrimSpace(dataset.Namespace)
	dataset.Name = strings.TrimSpace(dataset.Name)
	if dataset.Namespace == "" && dataset.Name == "" {
		return
	}
	id := datasetNodeID(dataset)
	if _, ok := b.nodes[id]; ok {
		return
	}
	ref := dataset
	b.nodes[id] = Node{
		ID:      id,
		Kind:    NodeKindDataset,
		Dataset: &ref,
	}
}

func (b *graphBuilder) addEdge(edge Edge) {
	if strings.TrimSpace(edge.From) == "" || strings.TrimSpace(edge.To) == "" || edge.Class == "" {
		return
	}
	edge.From = strings.TrimSpace(edge.From)
	edge.To = strings.TrimSpace(edge.To)
	edge.ID = edgeID(edge)

	if existing, ok := b.edges[edge.ID]; ok {
		b.edges[edge.ID] = mergeEdges(existing, edge)
		return
	}
	b.edges[edge.ID] = edge
}

func (b *graphBuilder) graph() Graph {
	nodes := make([]Node, 0, len(b.nodes))
	for _, node := range b.nodes {
		nodes = append(nodes, node)
	}
	sort.Slice(nodes, func(i, j int) bool {
		if nodes[i].Kind == nodes[j].Kind {
			return nodes[i].ID < nodes[j].ID
		}
		return nodes[i].Kind < nodes[j].Kind
	})

	edges := make([]Edge, 0, len(b.edges))
	for _, edge := range b.edges {
		edges = append(edges, edge)
	}
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].Class == edges[j].Class {
			if edges[i].From == edges[j].From {
				if edges[i].To == edges[j].To {
					return datasetKey(edges[i].Dataset) < datasetKey(edges[j].Dataset)
				}
				return edges[i].To < edges[j].To
			}
			return edges[i].From < edges[j].From
		}
		return edges[i].Class < edges[j].Class
	})

	return Graph{Nodes: nodes, Edges: edges}
}

func mergeEdges(existing, incoming Edge) Edge {
	if incoming.Verdict == schemacompat.VerdictBreaking || existing.Verdict == "" {
		existing.Verdict = incoming.Verdict
	} else if incoming.Verdict == schemacompat.VerdictUnknown && existing.Verdict != schemacompat.VerdictBreaking {
		existing.Verdict = schemacompat.VerdictUnknown
	}

	existing.Findings = append(existing.Findings, incoming.Findings...)
	existing.Findings = sortedFindings(existing.Findings)
	if len(existing.ProducerSchema) == 0 && len(incoming.ProducerSchema) > 0 {
		existing.ProducerSchema = cloneAnyMap(incoming.ProducerSchema)
	}
	if len(existing.PreviousProducerSchema) == 0 && len(incoming.PreviousProducerSchema) > 0 {
		existing.PreviousProducerSchema = cloneAnyMap(incoming.PreviousProducerSchema)
	}
	if len(existing.ConsumerSchema) == 0 && len(incoming.ConsumerSchema) > 0 {
		existing.ConsumerSchema = cloneAnyMap(incoming.ConsumerSchema)
	}
	if incoming.LastSeen != nil && (existing.LastSeen == nil || incoming.LastSeen.After(*existing.LastSeen)) {
		lastSeen := *incoming.LastSeen
		existing.LastSeen = &lastSeen
	}
	return existing
}

// JobNodeID is the canonical graph node identifier for a job alias. Exported
// so API layers filtering findings by job stay in sync with the graph's ID
// convention instead of duplicating the format.
func JobNodeID(alias string) string {
	return "job:" + strings.TrimSpace(alias)
}

func jobNodeID(alias string) string {
	return JobNodeID(alias)
}

func datasetNodeID(dataset DatasetRef) string {
	return "dataset:" + datasetKey(&dataset)
}

func datasetKey(dataset *DatasetRef) string {
	if dataset == nil {
		return ""
	}
	return strings.TrimSpace(dataset.Namespace) + "/" + strings.TrimSpace(dataset.Name)
}

func edgeID(edge Edge) string {
	key := string(edge.Class) + ":" + edge.From + "->" + edge.To
	if edge.Dataset != nil {
		key += ":" + datasetKey(edge.Dataset)
	}
	return "edge:" + key
}

func parseJSONMap(raw string) (map[string]any, error) {
	return parseJSONMapBytes([]byte(raw))
}

func parseJSONMapBytes(raw []byte) (map[string]any, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil, nil
	}
	var out map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

func stringLabels(labels datatypes.JSONMap) map[string]string {
	if len(labels) == 0 {
		return nil
	}
	out := make(map[string]string, len(labels))
	for key, value := range labels {
		stringValue, ok := value.(string)
		if !ok {
			continue
		}
		out[key] = stringValue
	}
	return out
}

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func cloneAnyMap(in map[string]any) map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]any, len(in))
	for key, value := range in {
		switch typed := value.(type) {
		case map[string]any:
			out[key] = cloneAnyMap(typed)
		case []any:
			out[key] = cloneAnySlice(typed)
		default:
			out[key] = value
		}
	}
	return out
}

func cloneAnySlice(in []any) []any {
	if len(in) == 0 {
		return nil
	}
	out := make([]any, len(in))
	for idx, value := range in {
		switch typed := value.(type) {
		case map[string]any:
			out[idx] = cloneAnyMap(typed)
		case []any:
			out[idx] = cloneAnySlice(typed)
		default:
			out[idx] = value
		}
	}
	return out
}

func sortJobs(jobs []Job) {
	sort.Slice(jobs, func(i, j int) bool {
		return jobs[i].Alias < jobs[j].Alias
	})
}

func sortEvidence(records []EvidenceRecord) {
	sort.Slice(records, func(i, j int) bool {
		left := records[i]
		right := records[j]
		if left.ProducerJobAlias == right.ProducerJobAlias {
			if left.ConsumerJobAlias == right.ConsumerJobAlias {
				if left.Dataset.Namespace == right.Dataset.Namespace {
					if left.Dataset.Name == right.Dataset.Name {
						return left.LastSeen.Before(right.LastSeen)
					}
					return left.Dataset.Name < right.Dataset.Name
				}
				return left.Dataset.Namespace < right.Dataset.Namespace
			}
			return left.ConsumerJobAlias < right.ConsumerJobAlias
		}
		return left.ProducerJobAlias < right.ProducerJobAlias
	})
}
