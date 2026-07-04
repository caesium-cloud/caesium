package notification

import (
	"context"
	"encoding/json"
	"time"

	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/incident"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// aiAgentFailureTypes are the only event types the ai_agent sender opens
// incidents for — the same failure set the leader-gated incident subscriber
// consumes. A policy could legitimately fan a SUCCESS event (run_completed,
// task_succeeded) to an ai_agent channel; opening an incident for a healthy run
// would be bogus, so success events are skipped.
var aiAgentFailureTypes = map[event.Type]struct{}{
	event.TypeTaskFailed:              {},
	event.TypeRunFailed:               {},
	event.TypeRunTimedOut:             {},
	event.TypeSLAMissed:               {},
	event.TypeSchemaViolationRecorded: {},
}

// AIAgentLeaderCheck reports whether this node hosts the cluster leader. The
// ai_agent sender is leader-gated so that — unlike the per-node notification
// subscriber it runs inside — a matched policy opens exactly one incident per
// failure on an N-node cluster (the incident store's atomic conditional insert is
// the backstop; the leader gate avoids the wasted work). A nil check means
// "always act" (single-node / tests).
type AIAgentLeaderCheck func(context.Context) (bool, error)

// AIAgentSender makes the reserved ChannelTypeAIAgent = "ai_agent" a real
// dispatch path (agent-in-the-loop D3). It is a second, policy-driven route into
// the incident manager: the same NotificationPolicy matching that fans a failure
// event out to Slack can fan it to the agent. Rather than re-publishing onto the
// bus (which the leader-gated incident subscriber already consumes — and which
// would risk a match/republish loop), the sender opens/appends the incident
// directly via the shared incident store, converging on the same pipeline as the
// job-owner-facing metadata.remediation opt-in.
type AIAgentSender struct {
	db          *gorm.DB
	store       *incident.Store
	classifier  *incident.Classifier
	leaderCheck AIAgentLeaderCheck
	cooldown    time.Duration
}

// NewAIAgentSender constructs an ai_agent sender over db. leaderCheck may be nil
// (single-node / tests), in which case the sender always acts. cooldown is the
// configured CAESIUM_AGENT_INCIDENT_COOLDOWN so flap suppression applies to this
// dispatch path too, matching the main subscriber.
func NewAIAgentSender(db *gorm.DB, leaderCheck AIAgentLeaderCheck, cooldown time.Duration) *AIAgentSender {
	return &AIAgentSender{
		db:          db,
		store:       incident.NewStore(db),
		classifier:  incident.NewClassifier(),
		leaderCheck: leaderCheck,
		cooldown:    cooldown,
	}
}

// Send routes a matched FAILURE event into the incident manager by opening or
// appending an incident for the failing job/task. Success events (a policy could
// fan run_completed / task_succeeded to an ai_agent channel) are skipped — they
// must never manufacture an incident for a healthy run.
func (s *AIAgentSender) Send(ctx context.Context, ch models.NotificationChannel, payload Payload) error {
	if _, ok := aiAgentFailureTypes[payload.EventType]; !ok {
		return nil
	}
	if !s.isLeader(ctx) {
		return nil
	}

	jobID := payload.JobID
	var runID *uuid.UUID
	if payload.RunID != uuid.Nil {
		r := payload.RunID
		runID = &r
	}
	var taskID *uuid.UUID
	if payload.TaskID != uuid.Nil {
		t := payload.TaskID
		taskID = &t
	}

	// Resolve the persisted TaskRun (Result/ExitCode/SchemaViolations/log/error)
	// and the task name, mirroring the incident subscriber's classification input.
	sig := incident.Signal{EventType: string(payload.EventType), Error: payload.Error}
	var (
		taskName   string
		backfillID *uuid.UUID
		exitCode   *int
	)
	if payload.RunID != uuid.Nil {
		var jr models.JobRun
		if err := s.db.WithContext(ctx).
			Select("id", "job_id", "backfill_id").
			First(&jr, "id = ?", payload.RunID).Error; err == nil {
			if jobID == uuid.Nil {
				jobID = jr.JobID
			}
			backfillID = jr.BackfillID
		}
	}
	if payload.TaskID != uuid.Nil && payload.RunID != uuid.Nil {
		var tr models.TaskRun
		if err := s.db.WithContext(ctx).
			Where("job_run_id = ? AND task_id = ?", payload.RunID, payload.TaskID).
			First(&tr).Error; err == nil {
			sig.Result = tr.Result
			sig.HasSchemaViolations = len(tr.SchemaViolations) > 0
			sig.LogTail = tr.LogText
			if tr.Error != "" {
				sig.Error = tr.Error
			}
			sig.ExitCode = tr.ExitCode
			exitCode = tr.ExitCode
		}
		var task models.Task
		if err := s.db.WithContext(ctx).Select("name").First(&task, "id = ?", payload.TaskID).Error; err == nil {
			taskName = task.Name
		}
	}

	if jobID == uuid.Nil {
		log.Debug("ai_agent: could not resolve job for notification payload",
			"channel", ch.Name, "event_type", string(payload.EventType))
		return nil
	}

	class := s.classifier.Classify(sig)

	inc, outcome, err := s.store.OpenOrAppend(ctx, incident.OpenParams{
		JobID:                  jobID,
		RunID:                  runID,
		TaskID:                 taskID,
		TaskName:               taskName,
		Class:                  class,
		LastError:              sig.Error,
		Evidence:               aiAgentEvidence(ch.Name, class, exitCode),
		BackfillID:             backfillID,
		RemediationTargetRunID: runID,
		Cooldown:               s.cooldown,
	})
	if err != nil {
		return err
	}

	log.Info("ai_agent: routed event into incident manager",
		"channel", ch.Name,
		"incident_id", inc.ID,
		"job_id", jobID,
		"class", class,
		"outcome", outcome,
	)
	return nil
}

// isLeader reports whether this node should act on the event.
func (s *AIAgentSender) isLeader(ctx context.Context) bool {
	if s.leaderCheck == nil {
		return true
	}
	leader, err := s.leaderCheck(ctx)
	if err != nil {
		log.Warn("ai_agent: leader check failed; skipping event", "error", err)
		return false
	}
	return leader
}

// aiAgentEvidence renders a small JSON evidence blob recording the dispatch
// source (the ai_agent channel) and classifier signal for the incident feed.
func aiAgentEvidence(channel string, class incident.FailureClass, exitCode *int) datatypes.JSON {
	m := map[string]any{
		"class":   string(class),
		"source":  "ai_agent_channel",
		"channel": channel,
	}
	if exitCode != nil {
		m["exit_code"] = *exitCode
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	return datatypes.JSON(b)
}
