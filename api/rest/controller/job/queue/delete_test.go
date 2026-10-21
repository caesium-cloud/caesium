package queue

import (
	"errors"
	"testing"

	"github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/caesium-cloud/caesium/internal/authz"
	"github.com/caesium-cloud/caesium/internal/models"
	runstorage "github.com/caesium-cloud/caesium/internal/run"
	"github.com/stretchr/testify/require"
)

// TestAuthorizeQueuedRunCancel pins who may cancel a queued request recorded
// in a namespace (identity-and-access D1): operator in THAT namespace, not
// only in the job's current one. A row the caller cannot read is not found; a
// row it can read but not operate on is a denial; a nil principal and an
// operator at "*" are unchanged.
func TestAuthorizeQueuedRunCancel(t *testing.T) {
	marketing, finance := "marketing", "finance"
	principal := func(grants policy.Grants) *auth.Principal {
		return &auth.Principal{Kind: auth.PrincipalAPIKey, Grants: grants}
	}
	const (
		allow    = "allow"
		notFound = "not_found"
		denied   = "denied"
	)
	for _, tc := range []struct {
		name      string
		principal *auth.Principal
		namespace *string
		want      string
	}{
		{"auth disabled", nil, &marketing, allow},
		{"operator at *", principal(policy.Grants{"*": models.RoleOperator}), &marketing, allow},
		{"admin at *, legacy row", principal(policy.Grants{"*": models.RoleAdmin}), nil, allow},
		{"operator in the row's namespace", principal(policy.Grants{marketing: models.RoleOperator, finance: models.RoleOperator}), &marketing, allow},
		{"operator only in the job's new namespace", principal(policy.Grants{finance: models.RoleOperator}), &marketing, notFound},
		{"viewer where the row lives, operator where the job is", principal(policy.Grants{marketing: models.RoleViewer, finance: models.RoleOperator}), &marketing, denied},
		{"viewer at *, operator where the job is", principal(policy.Grants{"*": models.RoleViewer, finance: models.RoleOperator}), &marketing, denied},
		{"viewer at *, operator where the row lives", principal(policy.Grants{"*": models.RoleViewer, marketing: models.RoleOperator}), &marketing, allow},
		{"confined caller, legacy row", principal(policy.Grants{finance: models.RoleOperator}), nil, notFound},
		{"viewer at *, legacy row", principal(policy.Grants{"*": models.RoleViewer, finance: models.RoleOperator}), nil, allow},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := authorizeQueuedRunCancel(tc.principal, tc.namespace)
			switch tc.want {
			case allow:
				require.NoError(t, err)
			case notFound:
				require.ErrorIs(t, err, runstorage.ErrQueuedRunNotFound)
			case denied:
				denial, ok := errors.AsType[*authz.Denial](err)
				require.True(t, ok, "want a *authz.Denial, got %v", err)
				require.Equal(t, models.RoleOperator, denial.Required)
				require.Equal(t, []string{marketing}, denial.Namespaces)
			}
		})
	}
}
