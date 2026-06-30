package models

import (
	"time"

	"github.com/caesium-cloud/caesium/pkg/container"
	"github.com/google/uuid"
	"gorm.io/datatypes"
)

type JobRun struct {
	ID           uuid.UUID      `gorm:"type:uuid;primaryKey" json:"id"`
	JobID        uuid.UUID      `gorm:"type:uuid;index;not null" json:"job_id"`
	Job          Job            `gorm:"constraint:OnDelete:CASCADE" json:"-"`
	BackfillID   *uuid.UUID     `gorm:"type:uuid;index" json:"backfill_id,omitempty"`
	Backfill     *Backfill      `gorm:"constraint:OnDelete:SET NULL" json:"-"`
	TriggerID    uuid.UUID      `gorm:"type:uuid;index" json:"trigger_id"`
	TriggerType  string         `gorm:"type:text" json:"trigger_type"`
	TriggerAlias string         `gorm:"type:text" json:"trigger_alias"`
	Status       string         `gorm:"type:text;index;not null" json:"status"`
	Priority     int            `gorm:"not null;default:2" json:"priority"`
	Error        string         `json:"error,omitempty"`
	Params       datatypes.JSON `gorm:"type:json" json:"params,omitempty"`
	Quarantine   bool           `gorm:"not null;default:false;index" json:"quarantine"`
	// ReplayFingerprint is the scoped, server-derived idempotency fingerprint
	// for quarantined replay creation. It is nullable so ordinary runs do not
	// participate in the unique index.
	ReplayFingerprint *string        `gorm:"type:text;uniqueIndex:idx_job_runs_replay_fingerprint" json:"replay_fingerprint,omitempty"`
	ReplayOverrides   datatypes.JSON `gorm:"type:json" json:"replay_overrides,omitempty"`
	StartedAt         time.Time      `gorm:"not null" json:"started_at"`
	CompletedAt       *time.Time     `json:"completed_at,omitempty"`
	CreatedAt         time.Time      `gorm:"not null" json:"created_at"`
	UpdatedAt         time.Time      `gorm:"not null" json:"updated_at"`
	Tasks             []*TaskRun     `gorm:"foreignKey:JobRunID;constraint:OnDelete:CASCADE" json:"tasks,omitempty"`
	CacheHits         int            `gorm:"-" json:"cache_hits"`
	ExecutedTasks     int            `gorm:"-" json:"executed_tasks"`
	TotalTasks        int            `gorm:"-" json:"total_tasks"`
}

type TaskRun struct {
	ID             uuid.UUID  `gorm:"type:uuid;primaryKey" json:"id"`
	JobRunID       uuid.UUID  `gorm:"type:uuid;index:idx_taskrun_jobrun_task;index;index:idx_taskrun_terminal_seq,priority:1;not null" json:"job_run_id"`
	JobRun         JobRun     `gorm:"constraint:OnDelete:CASCADE" json:"-"`
	TaskID         uuid.UUID  `gorm:"type:uuid;index:idx_taskrun_jobrun_task;index;not null" json:"task_id"`
	Task           Task       `gorm:"constraint:OnDelete:CASCADE" json:"-"`
	AtomID         uuid.UUID  `gorm:"type:uuid;index;not null" json:"atom_id"`
	Engine         AtomEngine `gorm:"type:text;not null" json:"engine"`
	Image          string     `gorm:"not null" json:"image"`
	Command        string     `gorm:"not null" json:"command"`
	Status         string     `gorm:"type:text;index;index:idx_taskrun_claim_priority,priority:1;not null" json:"status"`
	ClaimedBy      string     `gorm:"type:text;index;index:idx_taskrun_claim_priority,priority:5;not null;default:''" json:"claimed_by"`
	ClaimExpiresAt *time.Time `gorm:"index" json:"claim_expires_at,omitempty"`
	ClaimAttempt   int        `gorm:"not null;default:0" json:"claim_attempt"`
	// RateLimitRetryAfter keeps over-limit tasks pending without letting worker
	// claims or owner dispatch pick them back up before the current window rolls.
	RateLimitRetryAfter *time.Time        `gorm:"index" json:"rate_limit_retry_after,omitempty"`
	Attempt             int               `gorm:"not null;default:1" json:"attempt"`
	MaxAttempts         int               `gorm:"not null;default:1" json:"max_attempts"`
	Priority            int               `gorm:"not null;default:2;index:idx_taskrun_claim_priority,priority:3,sort:desc" json:"priority"`
	NodeSelector        datatypes.JSONMap `gorm:"type:json" json:"node_selector,omitempty"`
	Hash                string            `gorm:"type:text;index" json:"-"`
	// EffectiveHash is the identity this task presents to its DOWNSTREAM
	// consumers when a value-verified short-circuit was proven (design Component
	// 5 / D2). Nullable: empty means "use Hash" — the common case. When this
	// task re-executed because its OWN identity changed (Hash != a prior run's)
	// but it produced byte-identical output to a prior successful run, this is
	// set to that prior run's identity hash. Downstream PredecessorHashes reads
	// COALESCE(effective_hash, hash), so a downstream task whose only changed
	// input was this step sees an UNCHANGED predecessor and cache-hits — proven,
	// not heuristic. Hash itself is left untouched so this task's own receipt /
	// `caesium why` still reflect its true identity. See
	// cache.EquivalentPriorHash for the proof and its default-to-rerun guards.
	EffectiveHash    string         `gorm:"type:text" json:"-"`
	Result           string         `json:"result,omitempty"`
	Output           datatypes.JSON `gorm:"type:json" json:"output,omitempty"`
	BranchSelections datatypes.JSON `gorm:"type:json" json:"branch_selections,omitempty"`
	Quarantine       bool           `gorm:"not null;default:false;index" json:"quarantine"`
	CacheHit         bool           `gorm:"not null;default:false" json:"cache_hit"`
	CacheEnabled     bool           `gorm:"not null;default:false" json:"-"`
	CacheTTL         time.Duration  `gorm:"not null;default:0" json:"-"`
	CacheVersion     int            `gorm:"not null;default:0" json:"-"`
	// ReplaySafe snapshots the effective job/step replaySafe mark when this
	// task run is materialized. Replay authorization reads this baseline value,
	// not the mutable live job definition.
	ReplaySafe bool `gorm:"not null;default:false" json:"replay_safe"`
	// CachePinDigests snapshots whether image-digest pinning is in effect for
	// this task. Like CacheEnabled/CacheTTL/CacheVersion it is scheduler-set on
	// the row so distributed workers behave identically to local execution
	// without reloading the job definition.
	CachePinDigests bool `gorm:"not null;default:false" json:"-"`
	// CacheDigestTTL snapshots how long a resolved tag->digest mapping may be
	// reused before re-resolution (0 = re-resolve every check). Scheduler-set so
	// distributed workers apply the same freshness window as local execution.
	CacheDigestTTL time.Duration `gorm:"not null;default:0" json:"-"`
	// ResolvedImageDigest records the content digest (sha256:...) the image tag
	// resolved to when pinning is on. Nullable: empty/unset when pinning is off
	// or the digest could not be resolved (in which case the cache key falls
	// back to the literal tag).
	ResolvedImageDigest string `gorm:"type:text" json:"resolved_image_digest,omitempty"`
	// HashInputBlob is the canonical, secret-redacted, field-by-field JSON
	// decomposition of the HashInput that produced Hash. Nullable: written only
	// when caching is enabled and the hash was computed, left null otherwise.
	// It lets `caesium why` report *which* input changed between two runs
	// instead of only "the hashes differ". Env values are redacted in the blob;
	// see cache.HashInput.CanonicalJSON.
	HashInputBlob    datatypes.JSON `gorm:"type:json" json:"-"`
	CacheOriginRunID *uuid.UUID     `gorm:"type:uuid;index" json:"cache_origin_run_id,omitempty"`
	CacheCreatedAt   *time.Time     `json:"cache_created_at,omitempty"`
	CacheExpiresAt   *time.Time     `gorm:"index" json:"cache_expires_at,omitempty"`
	// OutputSchema snapshots the task's declared runtime output schema onto the task run.
	OutputSchema datatypes.JSON `gorm:"type:json" json:"-"`
	// SchemaValidation snapshots the job's schema validation mode onto the task run.
	SchemaValidation string `gorm:"type:text;not null;default:''" json:"-"`
	// SchemaViolations stores any output schema violations detected at runtime.
	SchemaViolations        datatypes.JSON `gorm:"type:json" json:"schema_violations,omitempty"`
	ExecutionDescriptor     datatypes.JSON `gorm:"type:json" json:"-"`
	LogText                 string         `gorm:"type:text" json:"-"`
	LogTruncated            bool           `gorm:"not null;default:false" json:"-"`
	Error                   string         `json:"error,omitempty"`
	RuntimeID               string         `json:"runtime_id,omitempty"`
	OutstandingPredecessors int            `gorm:"not null;index:idx_taskrun_claim_priority,priority:2" json:"outstanding_predecessors"`
	// OwnerGeneration is set to the RunLease.Generation of the owning node when
	// run-owner mode is active.  Every coordination write by the owner
	// includes AND (owner_generation = ? OR owner_generation = 0) in its WHERE
	// clause — the OR = 0 keeps legacy rows (and flag-off rows) mutable by any
	// node so the migration path stays gradual.  Defaults to 0.
	OwnerGeneration int64 `gorm:"not null;default:0" json:"owner_generation,omitempty"`
	// TerminalSequence is the per-run monotonic, dense sequence number stamped on
	// a task_runs row when it reaches a terminal status under run-owner mode.  It
	// shares a number space with run_checkpoints.sequence_high so failure
	// recovery can replay "terminal rows since the last checkpoint" in a
	// deterministic order (NOT wall-clock, which is skew-prone).  0 means
	// "never stamped" (non-owner mode, or not yet terminal).  The composite index
	// (job_run_id, terminal_sequence) makes the post-checkpoint tail scan cheap.
	TerminalSequence int64      `gorm:"not null;default:0;index:idx_taskrun_terminal_seq,priority:2" json:"terminal_sequence,omitempty"`
	StartedAt        *time.Time `json:"started_at,omitempty"`
	CompletedAt      *time.Time `json:"completed_at,omitempty"`
	CreatedAt        time.Time  `gorm:"not null;index:idx_taskrun_claim_priority,priority:4,sort:asc" json:"created_at"`
	UpdatedAt        time.Time  `gorm:"not null" json:"updated_at"`
}

const TaskExecutionDescriptorSchemaVersion = 1

// TaskExecutionDescriptor is the immutable, per-TaskRun runtime envelope used
// by quarantined replay. Secret values are never stored; secret refs are
// recorded with provider identity metadata. Large object/reference capture is
// out of scope for descriptor schema v1 and must be added explicitly in a later
// schema version before replay can depend on those refs.
type TaskExecutionDescriptor struct {
	SchemaVersion int       `json:"schemaVersion"`
	CapturedAt    time.Time `json:"capturedAt"`

	Baseline TaskExecutionBaseline `json:"baseline"`
	DAG      TaskExecutionDAG      `json:"dag"`
	Run      TaskExecutionRun      `json:"run"`
	Runtime  TaskExecutionRuntime  `json:"runtime"`
	Timing   TaskExecutionTiming   `json:"timing"`
	Cache    TaskExecutionCache    `json:"cache"`
	Schema   TaskExecutionSchema   `json:"schema"`
	Job      TaskExecutionJob      `json:"job"`

	ContainerSpec  container.Spec            `json:"containerSpec"`
	KubernetesSpec *container.KubernetesSpec `json:"kubernetesSpec,omitempty"`
	SecretRefs     []TaskExecutionSecretRef  `json:"secretRefs,omitempty"`
}

type TaskExecutionBaseline struct {
	JobID               uuid.UUID `json:"jobId"`
	JobAlias            string    `json:"jobAlias"`
	TaskID              uuid.UUID `json:"taskId"`
	TaskName            string    `json:"taskName"`
	AtomID              uuid.UUID `json:"atomId"`
	BaselineRunID       uuid.UUID `json:"baselineRunId"`
	TriggerID           uuid.UUID `json:"triggerId,omitempty"`
	TriggerType         string    `json:"triggerType,omitempty"`
	TriggerAlias        string    `json:"triggerAlias,omitempty"`
	ReplaySafe          bool      `json:"replaySafe"`
	Quarantine          bool      `json:"quarantine"`
	ComputedHash        string    `json:"computedHash,omitempty"`
	EffectiveHash       string    `json:"effectiveHash,omitempty"`
	HashInputBlobStored bool      `json:"hashInputBlobStored,omitempty"`
}

type TaskExecutionDAG struct {
	Predecessors               []TaskExecutionEdgeRef          `json:"predecessors,omitempty"`
	Successors                 []TaskExecutionEdgeRef          `json:"successors,omitempty"`
	TriggerRule                string                          `json:"triggerRule,omitempty"`
	BranchBehavior             string                          `json:"branchBehavior,omitempty"`
	EdgeMode                   string                          `json:"edgeMode,omitempty"`
	TaskPosition               int                             `json:"taskPosition"`
	OutstandingPredecessors    int                             `json:"outstandingPredecessors"`
	PredecessorOutputs         map[uuid.UUID]map[string]string `json:"predecessorOutputs,omitempty"`
	PredecessorEffectiveHashes map[uuid.UUID]string            `json:"predecessorEffectiveHashes,omitempty"`
}

type TaskExecutionEdgeRef struct {
	TaskID   uuid.UUID `json:"taskId"`
	TaskName string    `json:"taskName,omitempty"`
}

type TaskExecutionRun struct {
	Params map[string]string `json:"params,omitempty"`
}

type TaskExecutionRuntime struct {
	Engine              AtomEngine        `json:"engine"`
	Image               string            `json:"image"`
	ResolvedImageDigest string            `json:"resolvedImageDigest,omitempty"`
	Command             []string          `json:"command,omitempty"`
	CommandRaw          string            `json:"commandRaw,omitempty"`
	WorkDir             string            `json:"workdir,omitempty"`
	TaskType            string            `json:"taskType,omitempty"`
	NodeSelector        map[string]string `json:"nodeSelector,omitempty"`
	RetryCount          int               `json:"retryCount"`
	RetryDelay          time.Duration     `json:"retryDelay"`
	RetryBackoff        bool              `json:"retryBackoff"`
}

type TaskExecutionTiming struct {
	TaskTimeout time.Duration `json:"taskTimeout"`
	RunTimeout  time.Duration `json:"runTimeout"`
}

type TaskExecutionCache struct {
	Enabled             bool          `json:"enabled"`
	TTL                 time.Duration `json:"ttl"`
	Version             int           `json:"version"`
	PinDigests          bool          `json:"pinDigests"`
	DigestTTL           time.Duration `json:"digestTTL"`
	ComputedHash        string        `json:"computedHash,omitempty"`
	EffectiveHash       string        `json:"effectiveHash,omitempty"`
	HashInputBlobStored bool          `json:"hashInputBlobStored,omitempty"`
}

type TaskExecutionSchema struct {
	InputSchema  datatypes.JSON `json:"inputSchema,omitempty"`
	OutputSchema datatypes.JSON `json:"outputSchema,omitempty"`
	// ValidationMode fully determines violation behavior in descriptor schema v1.
	ValidationMode string `json:"validationMode,omitempty"`
}

type TaskExecutionJob struct {
	MaxParallelTasks int               `json:"maxParallelTasks"`
	Labels           map[string]string `json:"labels,omitempty"`
	Annotations      map[string]string `json:"annotations,omitempty"`
	SLA              datatypes.JSON    `json:"sla,omitempty"`
	CacheDefaults    datatypes.JSON    `json:"cacheDefaults,omitempty"`
	TriggerConfig    datatypes.JSONMap `json:"triggerConfig,omitempty"`
}

type TaskExecutionSecretRef struct {
	Ref                string            `json:"ref"`
	EnvKey             string            `json:"envKey,omitempty"`
	Provider           string            `json:"provider,omitempty"`
	Identity           datatypes.JSONMap `json:"identity,omitempty"`
	Verifiable         bool              `json:"verifiable"`
	UnverifiableReason string            `json:"unverifiableReason,omitempty"`
	IdentityCapturedAt *time.Time        `json:"identityCapturedAt,omitempty"`
}

// TaskCache stores cached task results keyed by identity hash.
type TaskCache struct {
	Hash             string         `gorm:"primaryKey;type:text"`
	JobID            uuid.UUID      `gorm:"type:uuid;not null;index:idx_task_cache_job"`
	TaskName         string         `gorm:"type:text;not null"`
	Result           string         `gorm:"type:text;not null"`
	Output           datatypes.JSON `gorm:"type:json"`
	BranchSelections datatypes.JSON `gorm:"type:json"`
	RunID            uuid.UUID      `gorm:"type:uuid;not null"`
	TaskRunID        uuid.UUID      `gorm:"type:uuid;not null"`
	// ResolvedImageDigest records the content digest folded into Hash when the
	// originating task ran with digest pinning on. Nullable: empty when pinning
	// was off. Stored so a cache hit can attest which image content it covers.
	ResolvedImageDigest string `gorm:"type:text"`
	// HashInputBlob is the canonical, secret-redacted decomposition of the
	// HashInput that produced Hash, mirrored from the originating TaskRun so a
	// cache *hit* can also be explained field-by-field. Nullable.
	HashInputBlob datatypes.JSON `gorm:"type:json"`
	CreatedAt     time.Time
	ExpiresAt     *time.Time `gorm:"index:idx_task_cache_expires"`
}
