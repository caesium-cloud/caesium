package oidc

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNormalizeRequiresCookieSecret(t *testing.T) {
	_, err := Config{
		IssuerURL:     "https://idp.example.com",
		ClientID:      "caesium",
		ClientSecret:  "oidc-client-secret",
		PublicBaseURL: "https://app.example.com",
	}.normalize()

	require.ErrorContains(t, err, "oidc cookie secret is required")
}
