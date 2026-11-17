package reproduce

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/caesium-cloud/caesium/internal/authz"
	runstorage "github.com/caesium-cloud/caesium/internal/run"
	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

const (
	miniWhyBaselineNone     = "none"
	miniWhyBaselinePriorRun = "prior_run"
)

// sensitiveKey matches a user-data field name that may hold a credential.
// CamelCase is split so kubernetesSecretNamespace and apiToken both match.
var sensitiveKey = regexp.MustCompile(`(?i)(^|[_-])(password|passwd|secret|token|credential|api[_-]?key|authorization|private[_-]?key)($|[_-])`)

var sha256Digest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// attachMiniWhy adds the redacted hash-input blob and the prior-run field diff.
// A missing or unreadable blob degrades the mini-why; it does not drop the
// descriptor. A baseline query failure is returned — that is a real read error.
func (s *Service) attachMiniWhy(resp *DescriptorResponse, row *taskDescriptorRow, runID uuid.UUID) error {
	why := &MiniWhy{BaselineKind: miniWhyBaselineNone, Summary: "no prior run to compare against"}
	resp.MiniWhy = why

	subject, err := redactHashInputBlob(row.HashInputBlob)
	if err != nil {
		why.Degraded = "hash input blob is unreadable and was withheld"
		why.Summary = why.Degraded
		return nil
	}
	if len(subject) == 0 {
		why.Degraded = "no decomposed input blob recorded for this task"
		why.Summary = why.Degraded
		return nil
	}
	resp.HashInputBlob = subject

	priorRunID, priorRaw, err := s.priorHashInputBlob(row, runID)
	if err != nil {
		return err
	}
	if priorRunID == uuid.Nil {
		return nil
	}
	baseline, err := redactHashInputBlob(priorRaw)
	if err != nil {
		why.BaselineKind = miniWhyBaselinePriorRun
		why.ComparedToRunID = priorRunID.String()
		why.Degraded = "prior run hash input blob is unreadable and was withheld"
		why.Summary = why.Degraded
		return nil
	}
	if len(baseline) == 0 {
		return nil
	}

	diff, err := runstorage.DiffHashInputBlobs(subject, baseline)
	if err != nil {
		why.BaselineKind = miniWhyBaselinePriorRun
		why.ComparedToRunID = priorRunID.String()
		why.Degraded = "could not diff hash input blobs"
		why.Summary = why.Degraded
		return nil
	}
	applyBlobDiff(why, priorRunID, diff)
	return nil
}

type priorBlobRow struct {
	JobRunID      uuid.UUID
	HashInputBlob datatypes.JSON
}

func (s *Service) priorHashInputBlob(row *taskDescriptorRow, runID uuid.UUID) (uuid.UUID, []byte, error) {
	var prior priorBlobRow
	// Compare started_at in SQL. Scanning the subject timestamp into Go and
	// binding it back can change its representation and miss the prior run.
	q := s.db.WithContext(s.ctx).
		Table("task_runs").
		Select("task_runs.job_run_id AS job_run_id, task_runs.hash_input_blob AS hash_input_blob").
		Joins("JOIN job_runs ON job_runs.id = task_runs.job_run_id").
		Where("task_runs.task_id = ? AND job_runs.job_id = ? AND task_runs.job_run_id <> ? AND job_runs.started_at < (SELECT started_at FROM job_runs WHERE id = ?) AND task_runs.hash_input_blob IS NOT NULL AND COALESCE(task_runs.partition_value, '') = ?",
			row.TaskID, row.JobID, runID, runID, row.PartitionValue)
	err := authz.NamespaceFilter(q, "job_runs.namespace").
		Order("job_runs.started_at DESC").
		Take(&prior).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return uuid.Nil, nil, nil
		}
		return uuid.Nil, nil, err
	}
	return prior.JobRunID, prior.HashInputBlob, nil
}

func applyBlobDiff(why *MiniWhy, priorRunID uuid.UUID, diff *runstorage.BlobDiff) {
	why.BaselineKind = miniWhyBaselinePriorRun
	why.ComparedToRunID = priorRunID.String()
	if diff == nil {
		why.Degraded = "could not diff hash input blobs"
		why.Summary = why.Degraded
		return
	}
	why.HashEqual = diff.HashEqual
	if diff.Degraded != "" {
		why.Degraded = diff.Degraded
		why.Summary = withNotes(diff.Degraded, diff.Notes)
		return
	}
	for _, change := range diff.Changes {
		if string(change.Kind) == "excluded" {
			continue
		}
		field := miniWhyFieldFrom(change)
		why.Fields = append(why.Fields, field)
	}
	why.Summary = withNotes(summarizeMiniWhy(why), diff.Notes)
}

func miniWhyFieldFrom(change runstorage.FieldChange) MiniWhyField {
	field := MiniWhyField{
		Field:  change.Field,
		Change: miniWhyChange(change),
		Before: change.Before,
		After:  change.After,
	}
	if change.Redacted || hideMiniWhyValues(field.Field, field.Before, field.After) {
		field.Redacted = true
		field.Before = ""
		field.After = ""
	}
	return field
}

func miniWhyChange(change runstorage.FieldChange) string {
	switch {
	case change.Added:
		return "added"
	case change.Removed:
		return "removed"
	default:
		return "changed"
	}
}

// hideMiniWhyValues reports whether before/after must be dropped. secret://
// references are not credential material and stay visible. Everything else on
// a sensitive field, and every redacted env digest, is withheld.
func hideMiniWhyValues(field, before, after string) bool {
	if !sensitiveField(field) {
		return false
	}
	return !publicSecretRef(before, after)
}

func publicSecretRef(before, after string) bool {
	if before != "" && !strings.HasPrefix(before, "secret://") {
		return false
	}
	if after != "" && !strings.HasPrefix(after, "secret://") {
		return false
	}
	return before != "" || after != ""
}

func sensitiveField(field string) bool {
	last := field
	if i := strings.LastIndex(field, "."); i >= 0 {
		last = field[i+1:]
	}
	return sensitiveKey.MatchString(camelBoundaries(last))
}

// camelBoundaries inserts a word break only at a lowercase-to-uppercase
// boundary (dbPassword -> db_Password). Splitting every capital letter turns
// PASSWORD into P_A_S_S_W_O_R_D, which no longer matches sensitiveKey, so an
// uppercase credential would be copied into the descriptor in plaintext.
func camelBoundaries(s string) string {
	var b strings.Builder
	for i, r := range s {
		if i > 0 && r >= 'A' && r <= 'Z' && s[i-1] >= 'a' && s[i-1] <= 'z' {
			b.WriteByte('_')
		}
		b.WriteRune(r)
	}
	return b.String()
}

func summarizeMiniWhy(why *MiniWhy) string {
	if len(why.Fields) == 0 {
		return "no input field differs from the prior run"
	}
	parts := make([]string, 0, len(why.Fields))
	for _, field := range why.Fields {
		parts = append(parts, summarizeMiniWhyField(field))
	}
	return strings.Join(parts, "; ")
}

func summarizeMiniWhyField(field MiniWhyField) string {
	if field.Redacted {
		switch field.Change {
		case "added":
			return field.Field + " added (redacted)"
		case "removed":
			return field.Field + " removed (redacted)"
		default:
			return field.Field + " changed (redacted)"
		}
	}
	switch field.Change {
	case "added":
		if field.After == "" {
			return field.Field + " added"
		}
		return fmt.Sprintf("%s added (%s)", field.Field, field.After)
	case "removed":
		if field.Before == "" {
			return field.Field + " removed"
		}
		return fmt.Sprintf("%s removed (%s)", field.Field, field.Before)
	default:
		if field.Before == "" && field.After == "" {
			return field.Field + " changed"
		}
		return fmt.Sprintf("%s changed %s→%s", field.Field, emptyDash(field.Before), emptyDash(field.After))
	}
}

func withNotes(summary string, notes []string) string {
	if len(notes) == 0 {
		return summary
	}
	return summary + " (" + strings.Join(notes, "; ") + ")"
}

func emptyDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// redactHashInputBlob returns the persisted blob with env literals and
// sensitive-named values reduced to digests. A secret:// reference is kept.
// Unreadable JSON is an error so the caller can withhold the raw bytes.
func redactHashInputBlob(raw []byte) (json.RawMessage, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}
	var blob map[string]json.RawMessage
	if err := json.Unmarshal(raw, &blob); err != nil {
		return nil, err
	}
	if env, ok := blob["env"]; ok {
		redacted, err := redactEnvRaw(env)
		if err != nil {
			return nil, err
		}
		blob["env"] = redacted
	}
	for _, key := range []string{"runParams", "partitionAttributes"} {
		body, ok := blob[key]
		if !ok {
			continue
		}
		redacted, err := redactStringMapRaw(body)
		if err != nil {
			return nil, err
		}
		blob[key] = redacted
	}
	if body, ok := blob["predecessorOutputs"]; ok {
		redacted, err := redactPredecessorOutputsRaw(body)
		if err != nil {
			return nil, err
		}
		blob["predecessorOutputs"] = redacted
	}
	out, err := json.Marshal(blob)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func redactEnvRaw(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return raw, nil
	}
	var env map[string]json.RawMessage
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, err
	}
	out := make(map[string]any, len(env))
	for key, entry := range env {
		redacted, err := redactEnvEntry(entry)
		if err != nil {
			return nil, err
		}
		out[key] = redacted
	}
	return json.Marshal(out)
}

type envEntry struct {
	Secret   string          `json:"secret,omitempty"`
	Redacted *redactedDigest `json:"redacted,omitempty"`
}

type redactedDigest struct {
	Digest   string `json:"digest"`
	Redacted bool   `json:"redacted"`
}

func redactEnvEntry(raw json.RawMessage) (any, error) {
	var entry envEntry
	if err := json.Unmarshal(raw, &entry); err == nil && envEntrySafe(entry) {
		if entry.Secret != "" {
			return map[string]any{"secret": entry.Secret}, nil
		}
		return map[string]any{"redacted": map[string]any{
			"digest":   entry.Redacted.Digest,
			"redacted": true,
		}}, nil
	}
	// A bare JSON string, a non-URI stuffed into "secret", or any other shape
	// is hashed so a plaintext credential cannot leave the process.
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return redactedEnvValue(asString), nil
	}
	return redactedEnvValue(string(raw)), nil
}

func envEntrySafe(entry envEntry) bool {
	if entry.Secret != "" && entry.Redacted != nil {
		return false
	}
	if entry.Secret != "" {
		return strings.HasPrefix(entry.Secret, "secret://")
	}
	return entry.Redacted != nil && entry.Redacted.Redacted && sha256Digest.MatchString(entry.Redacted.Digest)
}

func redactedEnvValue(value string) map[string]any {
	return map[string]any{"redacted": map[string]any{
		"digest":   digestValue(value),
		"redacted": true,
	}}
}

func redactStringMapRaw(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return raw, nil
	}
	var values map[string]any
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, err
	}
	for key, value := range values {
		text, ok := value.(string)
		if !ok {
			if sensitiveField(key) {
				encoded, err := json.Marshal(value)
				if err != nil {
					return nil, err
				}
				values[key] = digestValue(string(encoded))
			}
			continue
		}
		if sensitiveField(key) && !strings.HasPrefix(text, "secret://") {
			values[key] = digestValue(text)
		}
	}
	return json.Marshal(values)
}

func redactPredecessorOutputsRaw(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return raw, nil
	}
	var steps map[string]json.RawMessage
	if err := json.Unmarshal(raw, &steps); err != nil {
		return nil, err
	}
	for step, body := range steps {
		redacted, err := redactStringMapRaw(body)
		if err != nil {
			return nil, err
		}
		steps[step] = redacted
	}
	return json.Marshal(steps)
}

func digestValue(value string) string {
	sum := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(sum[:])
}
