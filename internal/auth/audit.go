package auth

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/caesium-cloud/caesium/internal/metrics"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// AuditOutcome enumerates the possible outcomes for an audit log entry.
const (
	OutcomeSuccess = "success"
	OutcomeDenied  = "denied"
	OutcomeError   = "error"
)

// AuditAction enumerates well-known auditable actions.
const (
	ActionAuthAttempt        = "auth.attempt"
	ActionAuthDenied         = "auth.denied"
	ActionAuthLogin          = "auth.login"
	ActionAuthLogout         = "auth.logout"
	ActionAuthLoginDenied    = "auth.login_denied"
	ActionAuthSessionRevoked = "auth.session_revoked"
	ActionUserProvisioned    = "user.provisioned"
	ActionKeyCreate          = "api_key.create"
	ActionKeyRevoke          = "api_key.revoke"
	ActionKeyRotate          = "api_key.rotate"
	ActionJobCreate          = "job.create"
	ActionJobDelete          = "job.delete"
	ActionJobPause           = "job.pause"
	ActionJobUnpause         = "job.unpause"
	ActionRunTrigger         = "run.trigger"
	ActionRunRetry           = "run.retry"
	ActionRunQueueRead       = "run_queue.read"
	ActionRunQueueCancel     = "run_queue.cancel"
	ActionBackfill           = "run.backfill"
	ActionJobdefApply        = "jobdef.apply"
	ActionJobdefGitSync      = "jobdef.git_sync"
	ActionCachePrune         = "cache.prune"
	ActionCacheDelete        = "cache.delete"
	ActionLogLevel           = "log.set_level"
	ActionDBQuery            = "database.query"
	// ActionClusterMemberRemove records a stale dqlite member removed through
	// DELETE /v1/system/nodes/:id.
	ActionClusterMemberRemove = "cluster.member_remove"
	ActionWebhookDenied       = "webhook.denied"

	ActionNotificationChannelCreate = "notification_channel.create"
	ActionNotificationChannelUpdate = "notification_channel.update"
	ActionNotificationChannelDelete = "notification_channel.delete"
	ActionNotificationPolicyCreate  = "notification_policy.create"
	ActionNotificationPolicyUpdate  = "notification_policy.update"
	ActionNotificationPolicyDelete  = "notification_policy.delete"
)

// ActorGitSync is the audit actor of the git sync watcher; it records each
// sync of a namespace-restricted source as ActionJobdefGitSync.
const ActorGitSync = "system:git-sync"

// Access policy reloads (spec §6.1, §8.4). The poller audits as
// ActorPolicyLoader (policy.PollActor); an API reload audits as the caller.
const (
	ActionPolicyReloaded     = "policy.reloaded"
	ActionPolicyReloadFailed = "policy.reload_failed"
	ActorPolicyLoader        = "system:policy-loader"
)

// User and session administration (spec §8.4, §11.2). ActionSessionsRevoked
// carries metadata {"reason", "count"}; it is written only when sessions were
// actually revoked.
const (
	ActionUserDisable     = "user.disable"
	ActionUserEnable      = "user.enable"
	ActionSessionsRevoked = "auth.sessions_revoked"

	SessionsRevokedReasonUserDisabled = "user_disabled"
	SessionsRevokedReasonUserEnabled  = "user_enabled"
	SessionsRevokedReasonAdmin        = "admin"
)

// AuditLogger writes structured audit log entries to the database.
type AuditLogger struct {
	db *gorm.DB
}

// NewAuditLogger creates a new audit logger.
func NewAuditLogger(db *gorm.DB) *AuditLogger {
	return &AuditLogger{db: db}
}

// AuditEntry holds the fields for a single audit log write.
type AuditEntry struct {
	Actor        string
	Action       string
	ResourceType string
	ResourceID   string
	SourceIP     string
	Outcome      string
	Metadata     map[string]any
	// Namespace is AuditLog.Namespace: the namespace of the object the action
	// was about ("*" for cluster-wide or multi-namespace, "" when none).
	Namespace string
}

// Log writes an audit entry to the database.
func (a *AuditLogger) Log(entry AuditEntry) error {
	var metaJSON []byte
	if entry.Metadata != nil {
		var err error
		metaJSON, err = json.Marshal(entry.Metadata)
		if err != nil {
			return fmt.Errorf("marshal audit metadata: %w", err)
		}
	}

	record := &models.AuditLog{
		ID:           uuid.New(),
		Timestamp:    time.Now().UTC(),
		Actor:        entry.Actor,
		Action:       entry.Action,
		ResourceType: entry.ResourceType,
		ResourceID:   entry.ResourceID,
		SourceIP:     entry.SourceIP,
		Outcome:      entry.Outcome,
		Metadata:     metaJSON,
		Namespace:    entry.Namespace,
	}

	if err := a.db.Create(record).Error; err != nil {
		return fmt.Errorf("write audit log: %w", err)
	}
	metrics.AuditLogEntriesTotal.WithLabelValues(entry.Action, entry.Outcome).Inc()
	return nil
}

// AuditQueryRequest holds filters for querying the audit log.
type AuditQueryRequest struct {
	Since  *time.Time
	Until  *time.Time
	Actor  string
	Action string
	// Namespace, when non-empty, returns only rows whose namespace equals it
	// exactly. Empty applies no namespace filter (every row, including
	// identity rows and rows written before the column existed).
	Namespace string
	Limit     int
	Offset    int
}

// Query returns audit log entries matching the given filters.
func (a *AuditLogger) Query(req *AuditQueryRequest) ([]models.AuditLog, error) {
	q := a.db.Order("timestamp DESC")

	if req.Since != nil {
		q = q.Where("timestamp >= ?", *req.Since)
	}
	if req.Until != nil {
		q = q.Where("timestamp <= ?", *req.Until)
	}
	if req.Actor != "" {
		q = q.Where("actor = ?", req.Actor)
	}
	if req.Action != "" {
		q = q.Where("action = ?", req.Action)
	}
	if req.Namespace != "" {
		q = q.Where("namespace = ?", req.Namespace)
	}

	limit := req.Limit
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	q = q.Limit(limit)

	if req.Offset > 0 {
		q = q.Offset(req.Offset)
	}

	var entries []models.AuditLog
	if err := q.Find(&entries).Error; err != nil {
		return nil, fmt.Errorf("query audit log: %w", err)
	}
	return entries, nil
}
