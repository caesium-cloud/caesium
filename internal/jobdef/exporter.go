package jobdef

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/container"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/caesium-cloud/caesium/pkg/jsonmap"
	"github.com/caesium-cloud/caesium/pkg/ptr"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Exporter reconstructs the authoring manifest (pkg/jobdef.Definition) for a
// job that is already persisted. It is the inverse of Importer: everything
// Importer.ApplyWithOptions folds into jobs/triggers/tasks/atoms/task_edges/
// callbacks/dataset_declarations is read back out and re-expressed in the
// manifest vocabulary, so `caesium job export` (and the Console's YAML tab) can
// round-trip a live job back to YAML that `caesium job lint` accepts and
// `caesium job apply` re-applies as a no-op.
//
// Two things are deliberately NOT recoverable, because they were never
// persisted:
//
//   - A volume's alternative per-engine sources. Only the source that a step's
//     own engine resolved is stored (container.Spec.ResolvedVolumeMounts), so a
//     volume declared with docker+podman+kubernetes sources comes back carrying
//     only the engines the job's steps actually mount it with. Its optional
//     `accessMode` is not persisted at all.
//   - metadata.serviceAccountName / podAnnotations /
//     automountServiceAccountToken. RuntimeSpecForStep merges the job-level
//     defaults into every kubernetes step's spec before storing it, so they come
//     back on each step. Step-level values override job-level ones, so the
//     re-expressed manifest is semantically identical.
type Exporter struct {
	db *gorm.DB
}

// NewExporter creates an exporter. The provided db connection must be non-nil.
func NewExporter(dbConn *gorm.DB) *Exporter {
	if dbConn == nil {
		panic("jobdef exporter requires a database connection")
	}
	return &Exporter{db: dbConn}
}

// JobRecords is the persisted state a manifest is reconstructed from. It is
// exported so callers (and tests) can assemble one without a database.
type JobRecords struct {
	Job          *models.Job
	Trigger      *models.Trigger
	Tasks        []models.Task
	Atoms        map[uuid.UUID]*models.Atom
	Edges        []models.TaskEdge
	Callbacks    []models.Callback
	Declarations []models.DatasetDeclaration
}

// Export loads jobID's persisted records and rebuilds its manifest.
func (e *Exporter) Export(ctx context.Context, jobID uuid.UUID) (*schema.Definition, error) {
	records, err := e.Load(ctx, jobID)
	if err != nil {
		return nil, err
	}
	return BuildDefinition(records)
}

// ErrJobChangedDuringExport means a concurrent apply committed while the
// manifest was being read, twice in a row. Export is a pure read, so the caller
// can simply ask again; the endpoint surfaces it as 409 rather than serving a
// manifest stitched from two different revisions of the job.
var ErrJobChangedDuringExport = errors.New("job changed while its manifest was being exported")

// exportLoadAttempts is how many times Load re-reads after observing a
// concurrent apply. An apply is a single short transaction, so one retry
// clears the overwhelmingly common case (an export that happened to land on a
// deploy); a job being re-applied in a tight loop is told to retry rather than
// silently served a hybrid.
const exportLoadAttempts = 2

// Load reads every record that participates in a job's manifest. It returns
// gorm.ErrRecordNotFound when the job does not exist.
//
// All reads run inside ONE transaction, and the job row's revision is re-read
// at the end and compared with the value the transaction opened on. Importer
// applies atomically — trigger, tasks, atoms, edges, callbacks and dataset
// declarations all move together — so issuing these as independent statements
// let an apply commit between them and yield a manifest stitched from two
// revisions (say, new tasks against the old trigger). The transaction alone is
// enough on a snapshot-isolating engine; the revision check makes the guarantee
// dialect-independent, because Importer bumps jobs.updated_at on every apply.
func (e *Exporter) Load(ctx context.Context, jobID uuid.UUID) (*JobRecords, error) {
	for attempt := 0; attempt < exportLoadAttempts; attempt++ {
		records, err := e.loadOnce(ctx, jobID)
		if err == nil {
			return records, nil
		}
		if !errors.Is(err, errJobRevisionChanged) {
			return nil, err
		}
	}
	return nil, ErrJobChangedDuringExport
}

// errJobRevisionChanged is loadOnce's internal signal that the job was applied
// underneath the read. It never escapes Load.
var errJobRevisionChanged = errors.New("job revision changed during export read")

func (e *Exporter) loadOnce(ctx context.Context, jobID uuid.UUID) (*JobRecords, error) {
	var records *JobRecords
	err := withImporterBusyRetry(ctx, func() error {
		return e.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			loaded, err := loadJobRecordsTx(tx, jobID)
			if err != nil {
				return err
			}
			// Re-read only the revision columns. A change here means an apply
			// committed mid-read, so the rows above may straddle two revisions.
			var current models.Job
			if err := tx.Select("updated_at").Where("id = ?", jobID).First(&current).Error; err != nil {
				return err
			}
			if !current.UpdatedAt.Equal(loaded.Job.UpdatedAt) {
				return errJobRevisionChanged
			}
			records = loaded
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return records, nil
}

func loadJobRecordsTx(db *gorm.DB, jobID uuid.UUID) (*JobRecords, error) {
	var jobModel models.Job
	if err := db.Where("id = ?", jobID).First(&jobModel).Error; err != nil {
		return nil, err
	}

	records := &JobRecords{Job: &jobModel}

	if jobModel.TriggerID != uuid.Nil {
		var trig models.Trigger
		if err := db.Where("id = ?", jobModel.TriggerID).First(&trig).Error; err != nil {
			return nil, fmt.Errorf("job %s trigger: %w", jobID, err)
		}
		records.Trigger = &trig
	}

	if err := db.Where("job_id = ?", jobID).
		Order("position asc").
		Order("created_at asc").
		Find(&records.Tasks).Error; err != nil {
		return nil, err
	}

	atomIDs := make([]uuid.UUID, 0, len(records.Tasks))
	for i := range records.Tasks {
		if records.Tasks[i].AtomID != uuid.Nil {
			atomIDs = append(atomIDs, records.Tasks[i].AtomID)
		}
	}
	records.Atoms = make(map[uuid.UUID]*models.Atom, len(atomIDs))
	if len(atomIDs) > 0 {
		var atoms []models.Atom
		// Retired atoms are soft-deleted but a live task still points at its
		// own atom, so the unscoped read only matters for a job whose atom rows
		// were retired out from under it; falling back to Unscoped keeps export
		// working rather than 500ing on a partially-retired job.
		if err := db.Unscoped().Where("id IN ?", atomIDs).Find(&atoms).Error; err != nil {
			return nil, err
		}
		for i := range atoms {
			records.Atoms[atoms[i].ID] = &atoms[i]
		}
	}

	if err := db.Where("job_id = ?", jobID).
		Order("created_at asc").
		Find(&records.Edges).Error; err != nil {
		return nil, err
	}

	if err := db.Where("job_id = ?", jobID).
		Order("position asc").
		Order("created_at asc").
		Find(&records.Callbacks).Error; err != nil {
		return nil, err
	}

	if err := db.Where("job_id = ?", jobID).
		Order("direction asc").
		Order("name asc").
		Find(&records.Declarations).Error; err != nil {
		return nil, err
	}

	return records, nil
}

// BuildDefinition re-expresses persisted records as an authoring manifest.
//
// It does NOT call Definition.Validate: several validations are gated on server
// feature flags (metadata.onUpstreamHold needs CAESIUM_DATA_ASSERTIONS_ENABLED,
// the freshness trigger needs its own gate), and a job that applied cleanly
// while a gate was on must still be exportable after it is turned off.
func BuildDefinition(records *JobRecords) (*schema.Definition, error) {
	if records == nil || records.Job == nil {
		return nil, fmt.Errorf("job records are required")
	}

	metadata, err := buildMetadata(records)
	if err != nil {
		return nil, err
	}

	trigger, err := buildTrigger(records.Trigger)
	if err != nil {
		return nil, err
	}

	callbacks, err := buildCallbacks(records.Callbacks)
	if err != nil {
		return nil, err
	}

	specs, err := stepRuntimeSpecs(records)
	if err != nil {
		return nil, err
	}

	volumes := buildVolumes(records, specs)
	volumeNames := make(map[string]struct{}, len(volumes))
	for i := range volumes {
		volumeNames[volumes[i].Name] = struct{}{}
	}

	steps, err := buildSteps(records, specs, volumeNames)
	if err != nil {
		return nil, err
	}

	return &schema.Definition{
		APIVersion: schema.APIVersionV1,
		Kind:       schema.KindJob,
		Metadata:   metadata,
		Trigger:    trigger,
		Callbacks:  callbacks,
		Volumes:    volumes,
		Steps:      steps,
	}, nil
}

func buildMetadata(records *JobRecords) (schema.Metadata, error) {
	jobModel := records.Job

	metadata := schema.Metadata{
		Alias:            jobModel.Alias,
		Labels:           nonEmptyStringMap(jsonmap.ToStringMap(jobModel.Labels)),
		Annotations:      nonEmptyStringMap(jsonmap.ToStringMap(jobModel.Annotations)),
		MaxParallelTasks: jobModel.MaxParallelTasks,
		TaskTimeout:      jobModel.TaskTimeout,
		RunTimeout:       jobModel.RunTimeout,
		Priority:         jobModel.Priority,
		SchemaValidation: jobModel.SchemaValidation,
		OnUpstreamHold:   jobModel.OnUpstreamHold,
		ReplaySafe:       jobModel.ReplaySafe,
	}

	if err := unmarshalOptional(jobModel.Concurrency, &metadata.Concurrency, "metadata.concurrency"); err != nil {
		return schema.Metadata{}, err
	}
	if err := unmarshalOptional(jobModel.RateLimits, &metadata.RateLimits, "metadata.rateLimits"); err != nil {
		return schema.Metadata{}, err
	}
	if err := unmarshalOptional(jobModel.SLA, &metadata.SLA, "metadata.sla"); err != nil {
		return schema.Metadata{}, err
	}
	if err := unmarshalOptional(jobModel.CacheConfig, &metadata.Cache, "metadata.cache"); err != nil {
		return schema.Metadata{}, err
	}
	if err := unmarshalOptional(jobModel.Remediation, &metadata.Remediation, "metadata.remediation"); err != nil {
		return schema.Metadata{}, err
	}

	datasets, err := buildMetadataDatasets(records.Declarations)
	if err != nil {
		return schema.Metadata{}, err
	}
	metadata.Datasets = datasets

	return metadata, nil
}

// buildMetadataDatasets rebuilds metadata.datasets from the source-direction
// declarations. skipWhenFresh is only re-emitted when it is explicitly false:
// the registry stores the DEFAULTED value on every row, so re-emitting a true
// would add a field the author never wrote.
func buildMetadataDatasets(decls []models.DatasetDeclaration) (*schema.MetadataDatasets, error) {
	sources := make([]schema.SourceDataset, 0)
	skipWhenFresh := true

	for i := range decls {
		decl := &decls[i]
		if decl.SkipWhenFresh != nil && !*decl.SkipWhenFresh {
			skipWhenFresh = false
		}
		if decl.Direction != models.DatasetDirectionSource {
			continue
		}
		source := schema.SourceDataset{
			Name:          decl.Name,
			ExpectedEvery: decl.ExpectedEvery,
			External:      decl.External,
		}
		if len(decl.ArrivalBinding) > 0 {
			var arrival schema.Arrival
			if err := json.Unmarshal(decl.ArrivalBinding, &arrival); err != nil {
				return nil, fmt.Errorf("metadata.datasets.sources[%q].arrival: %w", decl.Name, err)
			}
			source.Arrival = &arrival
		}
		sources = append(sources, source)
	}

	if len(sources) == 0 && skipWhenFresh {
		return nil, nil
	}

	out := &schema.MetadataDatasets{}
	if len(sources) > 0 {
		out.Sources = sources
	}
	if !skipWhenFresh {
		out.SkipWhenFresh = ptr.Of(false)
	}
	return out, nil
}

// buildTrigger unfolds the `defaultParams` key the importer folds into the
// stored trigger configuration back into its own manifest field.
func buildTrigger(trig *models.Trigger) (schema.Trigger, error) {
	if trig == nil {
		return schema.Trigger{}, nil
	}

	cfg, err := decodeJSONObject(trig.Configuration, "trigger.configuration")
	if err != nil {
		return schema.Trigger{}, err
	}
	if cfg == nil {
		cfg = map[string]any{}
	}

	out := schema.Trigger{
		Type:          string(trig.Type),
		Configuration: cfg,
	}
	if raw, ok := cfg["defaultParams"]; ok {
		delete(cfg, "defaultParams")
		if params := toStringMap(raw); len(params) > 0 {
			out.DefaultParams = params
		}
	}
	return out, nil
}

func buildCallbacks(callbacks []models.Callback) ([]schema.Callback, error) {
	if len(callbacks) == 0 {
		return nil, nil
	}
	out := make([]schema.Callback, 0, len(callbacks))
	for i := range callbacks {
		cb := &callbacks[i]
		cfg, err := decodeJSONObject(cb.Configuration, fmt.Sprintf("callbacks[%d].configuration", i))
		if err != nil {
			return nil, err
		}
		if cfg == nil {
			cfg = map[string]any{}
		}
		out = append(out, schema.Callback{Type: string(cb.Type), Configuration: cfg})
	}
	return out, nil
}

// stepRuntimeSpecs decodes each task's atom spec once, keyed by task index.
func stepRuntimeSpecs(records *JobRecords) ([]container.Spec, error) {
	specs := make([]container.Spec, len(records.Tasks))
	for i := range records.Tasks {
		atom := records.Atoms[records.Tasks[i].AtomID]
		if atom == nil || len(atom.Spec) == 0 {
			continue
		}
		var spec container.Spec
		if err := json.Unmarshal(atom.Spec, &spec); err != nil {
			return nil, fmt.Errorf("steps[%d].spec: %w", i, err)
		}
		specs[i] = spec
	}
	return specs, nil
}

// buildVolumes reconstructs the job-level `volumes:` block from the resolved
// mounts stored on each step's atom spec. Volumes appear in first-mounted
// order; a volume mounted by steps on more than one engine comes back with a
// per-engine `sources` map, otherwise with a single `source`.
func buildVolumes(records *JobRecords, specs []container.Spec) []schema.Volume {
	order := make([]string, 0)
	byName := make(map[string]map[string]schema.VolumeSource)

	for i := range records.Tasks {
		engine := stepEngine(records, i)
		for _, mount := range specs[i].ResolvedVolumeMounts {
			name := strings.TrimSpace(mount.Name)
			if name == "" {
				continue
			}
			source, ok := volumeSourceFromResolvedMount(mount)
			if !ok {
				continue
			}
			sources, seen := byName[name]
			if !seen {
				sources = make(map[string]schema.VolumeSource)
				byName[name] = sources
				order = append(order, name)
			}
			if _, exists := sources[engine]; !exists {
				sources[engine] = source
			}
		}
	}

	if len(order) == 0 {
		return nil
	}

	volumes := make([]schema.Volume, 0, len(order))
	for _, name := range order {
		sources := byName[name]
		volume := schema.Volume{Name: name}
		if len(sources) == 1 {
			for _, source := range sources {
				volume.Source = ptr.Of(source)
			}
		} else {
			volume.Sources = sources
		}
		volumes = append(volumes, volume)
	}
	return volumes
}

func volumeSourceFromResolvedMount(mount container.VolumeMount) (schema.VolumeSource, bool) {
	source := strings.TrimSpace(mount.Source)
	switch mount.Type {
	case container.VolumeMountTypeBind:
		if source == "" {
			return schema.VolumeSource{}, false
		}
		return schema.VolumeSource{Bind: source}, true
	case container.VolumeMountTypeVolume:
		if source == "" {
			return schema.VolumeSource{}, false
		}
		return schema.VolumeSource{Volume: source}, true
	case container.VolumeMountTypeTmpfs:
		tmpfs := &schema.TmpfsSource{}
		if mount.Tmpfs != nil {
			tmpfs.SizeBytes = mount.Tmpfs.SizeBytes
			tmpfs.Mode = ptr.Clone(mount.Tmpfs.Mode)
		}
		return schema.VolumeSource{Tmpfs: tmpfs}, true
	case container.VolumeMountTypePVC:
		if source == "" {
			return schema.VolumeSource{}, false
		}
		return schema.VolumeSource{PVC: source}, true
	case container.VolumeMountTypeClaimTemplate:
		if mount.ClaimTemplate == nil {
			return schema.VolumeSource{}, false
		}
		return schema.VolumeSource{ClaimTemplate: &schema.ClaimTemplate{
			StorageClass: mount.ClaimTemplate.StorageClass,
			Size:         mount.ClaimTemplate.Size,
			AccessMode:   mount.ClaimTemplate.AccessMode,
			Labels:       nonEmptyStringMap(mount.ClaimTemplate.Labels),
			Annotations:  nonEmptyStringMap(mount.ClaimTemplate.Annotations),
		}}, true
	case container.VolumeMountTypeVolumeSource:
		if len(mount.VolumeSource) == 0 {
			return schema.VolumeSource{}, false
		}
		return schema.VolumeSource{VolumeSource: mount.VolumeSource}, true
	default:
		return schema.VolumeSource{}, false
	}
}

func buildSteps(records *JobRecords, specs []container.Spec, volumeNames map[string]struct{}) ([]schema.Step, error) {
	successors := successorNames(records)
	datasets, err := stepDatasets(records.Declarations)
	if err != nil {
		return nil, err
	}

	steps := make([]schema.Step, 0, len(records.Tasks))
	for i := range records.Tasks {
		task := &records.Tasks[i]
		atom := records.Atoms[task.AtomID]

		step := schema.Step{
			Name:         stepName(task),
			Engine:       stepEngine(records, i),
			Next:         successors[task.ID],
			Retries:      task.Retries,
			RetryDelay:   task.RetryDelay,
			RetryBackoff: task.RetryBackoff,
			NodeSelector: nonEmptyStringMap(jsonmap.ToStringMap(task.NodeSelector)),
			Datasets:     datasets[task.Name],
		}
		if atom != nil {
			step.Image = atom.Image
			step.Command = atom.Cmd()
		}
		// type and triggerRule are only re-emitted when they differ from the
		// value the parser defaults them to, so an exported manifest does not
		// gain fields the author never wrote.
		if task.Type != "" && task.Type != schema.StepTypeTask {
			step.Type = task.Type
		}
		if task.TriggerRule != "" && task.TriggerRule != schema.TriggerRuleAllSuccess {
			step.TriggerRule = task.TriggerRule
		}
		// A job-level replaySafe already covers every step, so the per-step mark
		// is only re-emitted when the job-level one is off.
		if task.ReplaySafe && !records.Job.ReplaySafe {
			step.ReplaySafe = true
		}
		if resource := strings.TrimSpace(task.RateLimitResource); resource != "" {
			step.RateLimit = &schema.StepRateLimit{Resource: resource, Units: task.RateLimitUnits}
		}
		if err := unmarshalOptional(task.FanOutConfig, &step.FanOut, fmt.Sprintf("steps[%d].fanOut", i)); err != nil {
			return nil, err
		}
		if err := unmarshalOptional(task.CacheConfig, &step.Cache, fmt.Sprintf("steps[%d].cache", i)); err != nil {
			return nil, err
		}
		if err := unmarshalOptional(task.OutputSchema, &step.OutputSchema, fmt.Sprintf("steps[%d].outputSchema", i)); err != nil {
			return nil, err
		}
		if err := unmarshalOptional(task.InputSchema, &step.InputSchema, fmt.Sprintf("steps[%d].inputSchema", i)); err != nil {
			return nil, err
		}

		spec := specs[i]
		step.VolumeMounts = volumeMountsFromSpec(spec, volumeNames)
		applyKubernetesSpec(&step, spec.Kubernetes)
		// ResolvedVolumeMounts and Kubernetes are DERIVED runtime state, not
		// authoring input: they are re-expressed above as volumeMounts and the
		// workload-identity fields, and must not leak back into the manifest.
		spec.ResolvedVolumeMounts = nil
		spec.Kubernetes = nil
		step.Spec = spec

		steps = append(steps, step)
	}
	return steps, nil
}

// applyKubernetesSpec re-expresses the stored KubernetesSpec as the step's
// workload-identity fields. It is skipped for non-kubernetes steps: those
// fields are kubernetes-only in the schema, so emitting them from a spec a JSON
// apply had put on a docker step would produce a manifest lint rejects.
func applyKubernetesSpec(step *schema.Step, k8s *container.KubernetesSpec) {
	if k8s == nil || step.Engine != schema.EngineKubernetes {
		return
	}
	step.ServiceAccountName = k8s.ServiceAccountName
	step.PodAnnotations = nonEmptyStringMap(k8s.PodAnnotations)
	step.AutomountServiceAccountToken = ptr.Clone(k8s.AutomountServiceAccountToken)
	if queue := strings.TrimSpace(k8s.QueueName); queue != "" {
		step.Kueue = &schema.Kueue{QueueName: queue}
	}
}

// volumeMountsFromSpec re-expresses a step's resolved mounts as manifest
// volumeMounts. A mount whose volume buildVolumes could not reconstruct is
// dropped rather than emitted: a volumeMounts entry naming a volume the
// `volumes:` block does not declare is what lint rejects as an unknown volume.
func volumeMountsFromSpec(spec container.Spec, volumeNames map[string]struct{}) []schema.VolumeMount {
	if len(spec.ResolvedVolumeMounts) == 0 {
		return nil
	}
	mounts := make([]schema.VolumeMount, 0, len(spec.ResolvedVolumeMounts))
	for _, resolved := range spec.ResolvedVolumeMounts {
		name := strings.TrimSpace(resolved.Name)
		target := strings.TrimSpace(resolved.Target)
		if name == "" || target == "" {
			continue
		}
		if _, declared := volumeNames[name]; !declared {
			continue
		}
		mounts = append(mounts, schema.VolumeMount{
			Volume:   name,
			Path:     target,
			ReadOnly: resolved.ReadOnly,
			SubPath:  resolved.SubPath,
		})
	}
	if len(mounts) == 0 {
		return nil
	}
	return mounts
}

// successorNames turns the persisted task_edges rows back into per-step `next`
// lists, ordered by the successor's position so the output is stable.
func successorNames(records *JobRecords) map[uuid.UUID][]string {
	nameByID := make(map[uuid.UUID]string, len(records.Tasks))
	orderByID := make(map[uuid.UUID]int, len(records.Tasks))
	for i := range records.Tasks {
		task := &records.Tasks[i]
		nameByID[task.ID] = stepName(task)
		orderByID[task.ID] = i
	}

	out := make(map[uuid.UUID][]uuid.UUID, len(records.Tasks))
	seen := make(map[[2]uuid.UUID]struct{}, len(records.Edges))
	for i := range records.Edges {
		edge := &records.Edges[i]
		if _, ok := nameByID[edge.FromTaskID]; !ok {
			continue
		}
		if _, ok := nameByID[edge.ToTaskID]; !ok {
			continue
		}
		key := [2]uuid.UUID{edge.FromTaskID, edge.ToTaskID}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out[edge.FromTaskID] = append(out[edge.FromTaskID], edge.ToTaskID)
	}

	names := make(map[uuid.UUID][]string, len(out))
	for from, tos := range out {
		sort.SliceStable(tos, func(a, b int) bool { return orderByID[tos[a]] < orderByID[tos[b]] })
		list := make([]string, 0, len(tos))
		for _, to := range tos {
			list = append(list, nameByID[to])
		}
		names[from] = list
	}
	return names
}

// stepDatasets rebuilds each step's `datasets:` block from the declared
// registry rows, which the importer rewrites from the manifest on every apply.
func stepDatasets(decls []models.DatasetDeclaration) (map[string]*schema.StepDatasets, error) {
	out := make(map[string]*schema.StepDatasets)

	ensure := func(step string) *schema.StepDatasets {
		if existing, ok := out[step]; ok {
			return existing
		}
		created := &schema.StepDatasets{}
		out[step] = created
		return created
	}

	for i := range decls {
		decl := &decls[i]
		if strings.TrimSpace(decl.StepName) == "" {
			continue
		}
		switch decl.Direction {
		case models.DatasetDirectionProduces:
			produced := schema.ProducedDataset{
				Name:         decl.Name,
				SchemaFrom:   decl.SchemaFrom,
				Version:      decl.SchemaVersion,
				Freshness:    decl.Freshness,
				MaxStaleness: decl.MaxStaleness,
				OnViolation:  decl.OnViolation,
				Release:      decl.Release,
			}
			if decl.SchemaJSON != "" {
				if err := json.Unmarshal([]byte(decl.SchemaJSON), &produced.Schema); err != nil {
					return nil, fmt.Errorf("datasets.produces[%q].schema: %w", decl.Name, err)
				}
			}
			if key := strings.TrimSpace(decl.WatermarkKey); key != "" {
				produced.Watermark = &schema.Watermark{Key: key}
			}
			if decl.AssertionsJSON != "" {
				var assertions schema.DatasetAssertions
				if err := json.Unmarshal([]byte(decl.AssertionsJSON), &assertions); err != nil {
					return nil, fmt.Errorf("datasets.produces[%q].assertions: %w", decl.Name, err)
				}
				produced.Assertions = &assertions
			}
			block := ensure(decl.StepName)
			block.Produces = append(block.Produces, produced)
		case models.DatasetDirectionConsumes:
			consumed := schema.ConsumedDataset{Name: decl.Name}
			if decl.SchemaJSON != "" {
				if err := json.Unmarshal([]byte(decl.SchemaJSON), &consumed.Schema); err != nil {
					return nil, fmt.Errorf("datasets.consumes[%q].schema: %w", decl.Name, err)
				}
			}
			block := ensure(decl.StepName)
			block.Consumes = append(block.Consumes, consumed)
		}
	}

	return out, nil
}

// stepName falls back to the task id for jobs created through POST /v1/jobs,
// which persists tasks without a name. A manifest step must be named.
func stepName(task *models.Task) string {
	if name := strings.TrimSpace(task.Name); name != "" {
		return name
	}
	return task.ID.String()
}

func stepEngine(records *JobRecords, idx int) string {
	if atom := records.Atoms[records.Tasks[idx].AtomID]; atom != nil && atom.Engine != "" {
		return string(atom.Engine)
	}
	return schema.EngineDocker
}

// unmarshalOptional decodes a nullable JSON column into target, leaving target
// untouched when the column is empty or literal null.
func unmarshalOptional(raw []byte, target any, field string) error {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	if err := json.Unmarshal([]byte(trimmed), target); err != nil {
		return fmt.Errorf("%s: %w", field, err)
	}
	return nil
}

func decodeJSONObject(raw, field string) (map[string]any, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(trimmed), &out); err != nil {
		return nil, fmt.Errorf("%s: %w", field, err)
	}
	return out, nil
}

func toStringMap(value any) map[string]string {
	raw, ok := value.(map[string]any)
	if !ok || len(raw) == 0 {
		return nil
	}
	out := make(map[string]string, len(raw))
	for key, entry := range raw {
		if entry == nil {
			continue
		}
		if str, ok := entry.(string); ok {
			out[key] = str
			continue
		}
		out[key] = fmt.Sprint(entry)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func nonEmptyStringMap(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	return values
}
