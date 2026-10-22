package run

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"slices"
	"strings"

	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// IsolationSelectorKey is the node-selector entry stamped on every task_runs
// row of a namespace whose isolation context is non-default
// (identity-and-access design §10.1: a Kubernetes mapping, any secret rule, or
// a namespace a policy file does not declare). Its value binds the row to the
// namespace's isolation settings that isolated it (RevisionIsolation), so an
// edit elsewhere in the file never changes it.
//
// The pull lane (worker.Claimer) selects a row only when the claiming worker's
// node labels satisfy its node_selector. A worker built before the isolation
// context — which would run the row without the owner's context, enforcing its
// own (possibly missing) policy — has no matching label and so never selects a
// stamped row. A current worker accepts the entry only when it advertises the
// isolation capability, serves a policy file, and (with run owners) serves the
// same settings for the namespace (IsolationClaimValues); it then resolves the
// context from that file. Owner push dispatch ignores node selectors and gates
// on the capability instead.
const IsolationSelectorKey = "caesium.io/isolation"

// isolationSelectorUnloaded is the value stamped while a configured policy
// file is not installed yet: no worker accepts it, and the owner restamps the
// row on its first dispatch tick.
const isolationSelectorUnloaded = "v1-unloaded"

// isolationSelectorValue digests the settings that decide a namespace's
// isolation context.
func isolationSelectorValue(namespace string, settings policy.Namespace, declared bool) string {
	payload, _ := json.Marshal(struct {
		Namespace  string                     `json:"namespace"`
		Declared   bool                       `json:"declared"`
		Kubernetes *policy.KubernetesSettings `json:"kubernetes,omitempty"`
		Secrets    *policy.SecretRules        `json:"secrets,omitempty"`
	}{namespace, declared, settings.Kubernetes, settings.Secrets})
	sum := sha256.Sum256(payload)
	return "v1-" + hex.EncodeToString(sum[:12])
}

// RevisionIsolation reports whether namespace's isolation context is
// non-default under rev, and the selector value binding a row to the settings
// that make it so.
func RevisionIsolation(rev *policy.Revision, namespace string) (bool, string) {
	namespace = models.NamespaceOrDefault(namespace)
	settings, declared := rev.Namespace(namespace)
	if declared && settings.Kubernetes == nil && settings.Secrets == nil {
		return false, ""
	}
	return true, isolationSelectorValue(namespace, settings, declared)
}

// IsolationClaimValues are the selector values a worker serving rev may pull:
// one per declared namespace whose isolation context is non-default. A row of
// a namespace rev does not declare is never pulled (it could only be denied).
func IsolationClaimValues(rev *policy.Revision) []string {
	if rev == nil || rev.Policy == nil {
		return nil
	}
	values := make([]string, 0, len(rev.Policy.Namespaces))
	for name := range rev.Policy.Namespaces {
		if isolated, value := RevisionIsolation(rev, name); isolated {
			values = append(values, value)
		}
	}
	slices.Sort(values)
	return values
}

// NamespaceIsolation is RevisionIsolation under this process's access policy.
// A configured policy file that is not installed yet counts as isolated: the
// rules cannot be read, so the row is kept away from every worker until an
// owner restamps it.
func NamespaceIsolation(namespace string) (bool, string) {
	holder := policy.ProcessHolder()
	if holder == nil {
		return strings.TrimSpace(env.Variables().AuthPolicyFile) != "", isolationSelectorUnloaded
	}
	if !holder.FileBacked() {
		return false, ""
	}
	return RevisionIsolation(holder.Current(), namespace)
}

// WithIsolationSelector returns selector with the isolation entry added when
// namespace requires isolation, and selector unchanged otherwise.
func WithIsolationSelector(selector datatypes.JSONMap, namespace string) datatypes.JSONMap {
	isolated, value := NamespaceIsolation(namespace)
	if !isolated {
		return selector
	}
	out := maps.Clone(selector)
	if out == nil {
		out = datatypes.JSONMap{}
	}
	out[IsolationSelectorKey] = value
	return out
}

// RunIsolationInfo returns the run's persisted namespace and whether it is a
// quarantined replay: what a run owner resolves the isolation context from.
func (s *Store) RunIsolationInfo(ctx context.Context, runID uuid.UUID) (string, bool, error) {
	var jobRun models.JobRun
	if err := s.db.WithContext(ctx).Select("namespace", "quarantine").First(&jobRun, "id = ?", runID).Error; err != nil {
		return "", false, err
	}
	return models.NamespaceOrDefault(jobRun.Namespace), jobRun.Quarantine, nil
}

// unfinishedRows loads the id and node selector of runID's pending and running
// rows.
func (s *Store) unfinishedRows(ctx context.Context, runID uuid.UUID) ([]models.TaskRun, error) {
	var rows []models.TaskRun
	err := s.db.WithContext(ctx).
		Select("id", "node_selector").
		Where("job_run_id = ? AND status IN ?", runID, []string{string(TaskStatusPending), string(TaskStatusRunning)}).
		Find(&rows).Error
	return rows, err
}

// RunRequiresIsolation reports whether any unfinished row of runID carries the
// isolation selector. A node that serves no policy file uses it to refuse to
// dispatch a run it took over from a policy-serving owner: it could neither
// resolve the context nor tell a worker to honour one.
func (s *Store) RunRequiresIsolation(ctx context.Context, runID uuid.UUID) (bool, error) {
	rows, err := s.unfinishedRows(ctx, runID)
	if err != nil {
		return false, err
	}
	for _, row := range rows {
		if _, ok := row.NodeSelector[IsolationSelectorKey]; ok {
			return true, nil
		}
	}
	return false, nil
}

// StampIsolationRequired sets the isolation selector entry to value on every
// unfinished row of runID that does not carry it yet. A run owner calls it once
// per policy revision under which the run's namespace is isolated (a reload can
// isolate a namespace after its rows were registered, or change the settings a
// pulling worker must serve), so a row left behind by a dead owner can only be
// pulled by a worker able to honour it. It returns the number of rows stamped.
func (s *Store) StampIsolationRequired(ctx context.Context, runID uuid.UUID, value string) (int, error) {
	rows, err := s.unfinishedRows(ctx, runID)
	if err != nil {
		return 0, err
	}
	stamped := 0
	for _, row := range rows {
		if current, ok := row.NodeSelector[IsolationSelectorKey]; ok && current == value {
			continue
		}
		selector := maps.Clone(row.NodeSelector)
		if selector == nil {
			selector = datatypes.JSONMap{}
		}
		selector[IsolationSelectorKey] = value
		if err := s.db.WithContext(ctx).Model(&models.TaskRun{}).Where("id = ?", row.ID).Update("node_selector", selector).Error; err != nil {
			return stamped, err
		}
		stamped++
	}
	return stamped, nil
}

// isolationPolicyServed reports whether rows can be isolated at all in this
// process: a policy file is served (or configured but not installed yet).
func isolationPolicyServed() bool {
	holder := policy.ProcessHolder()
	if holder == nil {
		return strings.TrimSpace(env.Variables().AuthPolicyFile) != ""
	}
	return holder.FileBacked()
}

// stampIsolationOnResetTx re-applies the isolation selector, under the
// current policy and inside the retry's own transaction, to a row a retry
// returns to the schedulable pool. A row that was terminal when its owner last
// stamped the run (a reload can isolate a namespace after it failed) would
// otherwise come back pullable by a worker that cannot honour the namespace's
// isolation context. Without a policy file it does nothing.
func stampIsolationOnResetTx(tx *gorm.DB, rowID uuid.UUID) error {
	if !isolationPolicyServed() {
		return nil
	}
	var row struct {
		NodeSelector datatypes.JSONMap `gorm:"column:node_selector"`
		Namespace    string            `gorm:"column:namespace"`
	}
	if err := tx.Table("task_runs").
		Select("task_runs.node_selector AS node_selector, job_runs.namespace AS namespace").
		Joins("JOIN job_runs ON job_runs.id = task_runs.job_run_id").
		Where("task_runs.id = ?", rowID).
		Take(&row).Error; err != nil {
		return err
	}
	isolated, value := NamespaceIsolation(row.Namespace)
	if !isolated {
		return nil
	}
	if current, ok := row.NodeSelector[IsolationSelectorKey]; ok && current == value {
		return nil
	}
	selector := maps.Clone(row.NodeSelector)
	if selector == nil {
		selector = datatypes.JSONMap{}
	}
	selector[IsolationSelectorKey] = value
	return tx.Model(&models.TaskRun{}).Where("id = ?", rowID).Update("node_selector", selector).Error
}
