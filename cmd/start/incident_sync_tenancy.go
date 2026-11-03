package start

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/caesium-cloud/caesium/internal/incident"
	internaljobdef "github.com/caesium-cloud/caesium/internal/jobdef"
	jobdefgit "github.com/caesium-cloud/caesium/internal/jobdef/git"
	jobdefruntime "github.com/caesium-cloud/caesium/internal/jobdef/runtime"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"gorm.io/gorm"
)

// newSyncTenancyCheck previews, for the Git-PR route of apply_jobdef_patch, the
// trigger tenancy (B9) git sync applies when it imports the merged pull request.
// The scope is built as sync builds it: the source's namespaces normalised as
// runtime.BuildGitWatches does, then git.SourceApplyOptions. With no allowlist
// the scope is nil, and the rule applies only where a policy file enforces it,
// which ValidateTriggerTenancy decides as the apply does.
func newSyncTenancyCheck(db *gorm.DB) incident.SyncTenancyCheck {
	return func(ctx context.Context, def *schema.Definition, sourceNamespaces []string) (bool, error) {
		if def == nil {
			return false, errors.New("no definition to check")
		}
		namespaces, err := jobdefruntime.GitSourceNamespaces(sourceNamespaces)
		if err != nil {
			return false, err
		}
		scope := jobdefgit.SourceApplyOptions(namespaces, nil).TenancyScope()
		// The check may normalise its input, and the route still splices def.
		encoded, err := json.Marshal(def)
		if err != nil {
			return false, err
		}
		var copied schema.Definition
		if err := json.Unmarshal(encoded, &copied); err != nil {
			return false, err
		}
		err = internaljobdef.ValidateTriggerTenancy(ctx, db, []schema.Definition{copied}, scope)
		switch {
		case err == nil:
			return true, nil
		case errors.Is(err, internaljobdef.ErrNamespaceNotAllowed), errors.Is(err, internaljobdef.ErrTriggerTenancy):
			return false, nil
		default:
			return false, err
		}
	}
}
