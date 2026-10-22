package authz_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/caesium-cloud/caesium/internal/authz"
	"github.com/stretchr/testify/require"
)

func TestRedactorNilWhenUnrestricted(t *testing.T) {
	r := authz.NewRedactor(context.Background())
	require.Nil(t, r, "an unrestricted request has nothing to redact")
	require.True(t, r.Visible("finance"), "a nil redactor shows everything")
	r.Done() // no-op, must not panic
}

func TestRedactorExternalIDsStableWithinAndUnlinkableAcross(t *testing.T) {
	ctx := authz.WithAllowedNamespaces(context.Background(), []string{"marketing"})
	first, second := authz.NewRedactor(ctx), authz.NewRedactor(ctx)
	require.NotNil(t, first)

	id := first.ExternalID("job\x00payroll")
	require.Equal(t, id, first.ExternalID("job\x00payroll"), "stable within one response")
	require.NotEqual(t, id, first.ExternalID("job\x00payroll2"))
	require.NotEqual(t, id, second.ExternalID("job\x00payroll"), "unlinkable across responses")
	require.True(t, strings.HasPrefix(id, "external:"))
	require.NotContains(t, id, "payroll")

	sum := sha256.Sum256([]byte("job\x00payroll"))
	require.NotContains(t, id, hex.EncodeToString(sum[:])[:32], "never a plain hash of the identity")
}

func TestRedactorVisible(t *testing.T) {
	ctx := authz.WithAllowedNamespaces(context.Background(), []string{"default", "marketing"})
	r := authz.NewRedactor(ctx)
	require.True(t, r.Visible("marketing"))
	require.True(t, r.Visible(""), "a legacy empty namespace is default")
	require.True(t, r.Visible("marketing", "default"))
	require.False(t, r.Visible("marketing", "finance"), "every owning namespace must be allowed")
	require.False(t, r.Visible(), "an unowned row is foreign")

	nothing := authz.NewRedactor(authz.WithAllowedNamespaces(context.Background(), nil))
	require.NotNil(t, nothing)
	require.False(t, nothing.Visible("default"), "restricted to nothing shows nothing")
}

func TestRedactorDoneMarksFiltered(t *testing.T) {
	ctx := authz.WithAllowedNamespaces(context.Background(), []string{"marketing"})
	require.False(t, authz.Filtered(ctx))
	authz.NewRedactor(ctx).Done()
	require.True(t, authz.Filtered(ctx))
}
