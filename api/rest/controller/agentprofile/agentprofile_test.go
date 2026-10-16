package agentprofile

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"

	svc "github.com/caesium-cloud/caesium/api/rest/service/agentprofile"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

func TestToViewCarriesNamespace(t *testing.T) {
	view := toView(models.AgentProfile{ID: uuid.New(), Namespace: "marketing", Name: "p", Image: "img"})
	require.Equal(t, "marketing", view.Namespace)
	require.Equal(t, models.ClusterNamespace, toView(models.AgentProfile{Namespace: models.ClusterNamespace}).Namespace)
	require.Equal(t, models.DefaultNamespace, toView(models.AgentProfile{}).Namespace, "legacy empty reads as default")

	// namespace is always on the wire (no omitempty).
	raw, err := json.Marshal(toView(models.AgentProfile{Namespace: "finance"}))
	require.NoError(t, err)
	var body map[string]any
	require.NoError(t, json.Unmarshal(raw, &body))
	require.Equal(t, "finance", body["namespace"])
}

func TestServiceErrorMapsNamespaceImmutable(t *testing.T) {
	err := serviceError(fmt.Errorf("%w: %w", svc.ErrInvalidProfile, fmt.Errorf("%w: object is in namespace %q", models.ErrNamespaceImmutable, "finance")))
	var httpErr *echo.HTTPError
	require.True(t, errors.As(err, &httpErr))
	require.Equal(t, http.StatusBadRequest, httpErr.Code)
	require.Equal(t, namespaceImmutableMessage, httpErr.Message)
	require.Contains(t, httpErr.Message, "create-only")
	require.NotContains(t, httpErr.Message, "finance", "the fixed message must not echo stored data")

	err = serviceError(fmt.Errorf("%w: namespace %q must be a DNS label", svc.ErrInvalidProfile, "Bad_NS"))
	require.True(t, errors.As(err, &httpErr))
	require.Equal(t, http.StatusBadRequest, httpErr.Code)
	require.Equal(t, "bad request", httpErr.Message)
}
