package start

import (
	"context"
	"errors"
	"fmt"

	contractenforce "github.com/caesium-cloud/caesium/internal/contract"
	"github.com/caesium-cloud/caesium/internal/freshness"
	internaljobdef "github.com/caesium-cloud/caesium/internal/jobdef"
	"github.com/caesium-cloud/caesium/internal/models"
	"gorm.io/gorm"
)

// An agent's jobdef patch is validated and applied against the whole job
// graph, but its error lands on the incident's action timeline, which a
// principal confined to the incident's namespace reads (identity-and-access
// A8). These helpers keep another namespace's job aliases, datasets and
// contract findings out of that error.

// errPatchConflictsElsewhere replaces a validation error that names a job in
// another namespace.
var errPatchConflictsElsewhere = errors.New("the proposed job definition conflicts with a job in another namespace")

// patchJobsInNamespace reports which of aliases name a job stored in
// namespace (exact alias match).
func patchJobsInNamespace(ctx context.Context, db *gorm.DB, namespace string, aliases []string) (map[string]bool, error) {
	inside := make(map[string]bool, len(aliases))
	if len(aliases) == 0 {
		return inside, nil
	}
	var jobs []models.Job
	if err := db.WithContext(ctx).Select("alias", "namespace").Where("alias IN ?", aliases).Find(&jobs).Error; err != nil {
		return nil, err
	}
	for i := range jobs {
		if models.NamespaceOrDefault(jobs[i].Namespace) == namespace {
			inside[jobs[i].Alias] = true
		}
	}
	return inside, nil
}

// redactPatchError returns err unchanged when it names no job outside
// namespace, and a redacted error otherwise. self is the patched job's alias,
// which is the incident's own job even while the patch is not yet stored.
func redactPatchError(ctx context.Context, db *gorm.DB, namespace, self string, err error) error {
	if err == nil {
		return nil
	}
	if refErr, ok := errors.AsType[*internaljobdef.ObjectRefNamespaceError](err); ok {
		if refErr.ObjectNamespace == models.ClusterNamespace || refErr.ObjectNamespace == namespace {
			return err
		}
		return errors.New(refErr.Redacted())
	}
	if msg := internaljobdef.RedactedTenancyMessage(err); msg != "" {
		return errors.New(msg)
	}
	var named []string
	switch {
	case asGraph(err, &named):
	case asCycle(err, &named):
	case asChainJob(err, &named):
	default:
		return err
	}
	inside, lookupErr := patchJobsInNamespace(ctx, db, namespace, named)
	if lookupErr != nil {
		return errPatchConflictsElsewhere
	}
	for _, alias := range named {
		if alias != self && !inside[alias] {
			return errPatchConflictsElsewhere
		}
	}
	return err
}

func asGraph(err error, named *[]string) bool {
	graphErr, ok := errors.AsType[*freshness.GraphError](err)
	if ok {
		*named = graphErr.Jobs
		if len(*named) == 0 {
			// A graph error naming no job (a cycle without a path) still
			// concerns the global graph: name nothing.
			*named = []string{""}
		}
	}
	return ok
}

func asCycle(err error, named *[]string) bool {
	cycle, ok := errors.AsType[*internaljobdef.TriggerChainCycleError](err)
	if ok {
		*named = cycle.Path
	}
	return ok
}

func asChainJob(err error, named *[]string) bool {
	jobErr, ok := errors.AsType[*internaljobdef.TriggerChainJobError](err)
	if ok {
		*named = []string{jobErr.Alias}
	}
	return ok
}

// patchContractAudience is the importer's ContractAudience for an incident
// patch: a finding is reported only when its producer and consumer are both
// jobs of the incident's namespace (or the patched job itself).
func patchContractAudience(ctx context.Context, namespace, self string) func(tx *gorm.DB) func(contractenforce.ContractFinding) bool {
	return func(tx *gorm.DB) func(contractenforce.ContractFinding) bool {
		return func(finding contractenforce.ContractFinding) bool {
			inside, err := patchJobsInNamespace(ctx, tx, namespace, []string{finding.Producer, finding.Consumer})
			if err != nil {
				return false
			}
			for _, alias := range []string{finding.Producer, finding.Consumer} {
				if alias == "" || (alias != self && !inside[alias]) {
					return false
				}
			}
			return true
		}
	}
}

// patchErrorf wraps a redacted validation or apply error.
func patchErrorf(ctx context.Context, db *gorm.DB, namespace, self, prefix string, err error) error {
	return fmt.Errorf("%s: %w", prefix, redactPatchError(ctx, db, namespace, self, err))
}
