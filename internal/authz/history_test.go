package authz

import (
	"context"
	"testing"

	"github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/stretchr/testify/require"
)

func TestWithHistoryScope(t *testing.T) {
	ctx := context.Background()

	// Auth disabled and an unrestricted principal: the context is unchanged.
	require.Equal(t, ctx, WithHistoryScope(ctx, nil))
	clusterViewer := &auth.Principal{Grants: policy.Grants{policy.ClusterNamespace: models.RoleViewer}}
	require.Equal(t, ctx, WithHistoryScope(ctx, clusterViewer))
	require.False(t, Restricted(WithHistoryScope(ctx, clusterViewer)))

	// A confined principal reads history in its viewer namespaces only.
	confined := &auth.Principal{Grants: policy.Grants{"marketing": models.RoleRunner, "finance": models.RoleViewer}}
	scoped := WithHistoryScope(ctx, confined)
	require.True(t, Restricted(scoped))
	allowed, restricted := AllowedNamespaces(scoped)
	require.True(t, restricted)
	require.Equal(t, []string{"finance", "marketing"}, allowed)
	require.True(t, NamespaceAllowed(scoped, "marketing"))
	require.False(t, NamespaceAllowed(scoped, "default"))
	require.False(t, NamespaceAllowed(scoped, ""), "a legacy empty namespace is default")

	// A principal with no grants reads nothing.
	none := WithHistoryScope(ctx, &auth.Principal{Grants: policy.Grants{}})
	allowed, restricted = AllowedNamespaces(none)
	require.True(t, restricted)
	require.Empty(t, allowed)

	// An existing restriction (a collection route's middleware scope) is
	// never replaced: the collection guard tracks it by identity.
	collection := WithAllowedNamespaces(ctx, []string{"default"})
	require.Equal(t, collection, WithHistoryScope(collection, confined))
	require.True(t, NamespaceAllowed(collection, ""))
	require.True(t, NamespaceAllowed(ctx, "anything"), "unrestricted allows every namespace")
}
