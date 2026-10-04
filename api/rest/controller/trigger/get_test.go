package trigger

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

func TestGetMalformedUUIDReturnsBadRequestBeforeService(t *testing.T) {
	e := echo.New()
	e.GET("/v1/triggers/:id", Get)

	req := httptest.NewRequest(http.MethodGet, "/v1/triggers/not-a-uuid", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
}
