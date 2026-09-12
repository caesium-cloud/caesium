package run

import (
	"fmt"
	"math"

	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/caesium-cloud/caesium/internal/metrics"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/google/uuid"
)

// TaskResourceOutcome fences observations to the attempt/runtime that produced
// them. A reclaimed worker or a delayed local completion cannot write a new
// attempt's row. Claim fields are additionally checked for distributed tasks.
type TaskResourceOutcome struct {
	atom.ResourceSummary
	ExitCode     *int
	RuntimeID    string
	Attempt      int
	ClaimedBy    string
	ClaimAttempt int
}

// SetTaskResourceOutcome records one final observation on exactly one instance.
// TaskRun is already migrated through models.All; no new table or router entry
// is needed. Empty StatsSource marks an unwritten attempt, making retries of
// this setter idempotent, including its metric increments.
func (s *Store) SetTaskResourceOutcome(runID, taskRef uuid.UUID, outcome TaskResourceOutcome) error {
	if !env.Variables().ResourceStatsEnabled {
		return nil
	}
	if outcome.RuntimeID == "" || outcome.Attempt < 1 {
		return fmt.Errorf("resource outcome requires runtime and attempt identity")
	}
	if outcome.PeakMemoryBytes != nil && *outcome.PeakMemoryBytes < 0 {
		return fmt.Errorf("resource memory observation must be nonnegative")
	}
	if outcome.CPUSeconds != nil && (*outcome.CPUSeconds < 0 || math.IsNaN(*outcome.CPUSeconds) || math.IsInf(*outcome.CPUSeconds, 0)) {
		return fmt.Errorf("resource CPU observation must be finite and nonnegative")
	}
	switch outcome.StatsSource {
	case "sampled", "oom_inferred", "none":
	default:
		return fmt.Errorf("invalid resource stats source %q", outcome.StatsSource)
	}
	row, err := loadTaskRunByIDOrUnique(s.db, runID, taskRef)
	if err != nil {
		return err
	}
	query := s.db.Model(&models.TaskRun{}).Where("id = ? AND status = ? AND runtime_id = ? AND attempt = ? AND stats_source = ?", row.ID, string(TaskStatusRunning), outcome.RuntimeID, outcome.Attempt, "")
	if outcome.ClaimedBy != "" {
		query = query.Where("claimed_by = ? AND claim_attempt = ?", outcome.ClaimedBy, outcome.ClaimAttempt)
	}
	result := query.Updates(map[string]any{
		"peak_memory_bytes": outcome.PeakMemoryBytes,
		"exit_code":         outcome.ExitCode,
		"cpu_seconds":       outcome.CPUSeconds,
		"stats_source":      outcome.StatsSource,
		"oom_killed":        outcome.OOMKilled,
	})
	if result.Error != nil || result.RowsAffected == 0 {
		return result.Error
	}
	var jr models.JobRun
	if err := s.db.Select("job_id", "quarantine").First(&jr, "id = ?", runID).Error; err != nil {
		return err
	}
	// Replay observations remain visible on their task, but cannot influence
	// production health or sizing metrics. Either durable flag is sufficient.
	if row.Quarantine || jr.Quarantine {
		return nil
	}
	labels := []string{jr.JobID.String(), row.TaskID.String(), string(row.Engine)}
	if outcome.OOMKilled {
		metrics.TaskOOMKillsTotal.WithLabelValues(labels...).Inc()
	}
	if outcome.PeakMemoryBytes != nil {
		metrics.TaskMemoryPeakBytes.WithLabelValues(labels...).Observe(float64(*outcome.PeakMemoryBytes))
	}
	if outcome.CPUSeconds != nil {
		metrics.TaskCPUSecondsTotal.WithLabelValues(labels...).Add(*outcome.CPUSeconds)
	}
	return nil
}

// TaskResourceResetColumns clears evidence owned by the runtime that is being
// discarded. Retry and reclaim callers merge these columns into their existing
// guarded UPDATE so observation cleanup cannot race a terminal completion.
// Reclaim preserves scheduling attempt counters; it still creates a new runtime.
func TaskResourceResetColumns() map[string]any {
	return map[string]any{
		"exit_code":         nil,
		"peak_memory_bytes": nil,
		"cpu_seconds":       nil,
		"stats_source":      "",
		"oom_killed":        false,
		"applied_resources": nil,
		"escalation_level":  0,
	}
}
