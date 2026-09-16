package run

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

// TestPostRunReturnsBadRequestForInvalidID verifies that an invalid UUID
// is rejected with 400 before any DB lookup occurs.
func TestPostRunReturnsBadRequestForInvalidID(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/", strings.NewReader(""))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetPathValues(echo.PathValues{{Name: "id", Value: "not-a-uuid"}})

	err := Post(c)
	require.Error(t, err)

	he, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusBadRequest, he.Code)
}

func TestPostRunRejectsSchedulerParamsBeforeStartingRun(t *testing.T) {
	for _, key := range []string{"_trigger_depth", "_derived_from_dataset", "_consumed_watermarks", "_consumed_watermarks_start", "_future_scheduler_field", "logical_date"} {
		t.Run(key, func(t *testing.T) {
			body, err := json.Marshal(PostRequest{Params: map[string]string{key: "private-value"}})
			require.NoError(t, err)
			e := echo.New()
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", strings.NewReader(string(body)))
			req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
			c := e.NewContext(req, httptest.NewRecorder())
			c.SetPathValues(echo.PathValues{{Name: "id", Value: uuid.NewString()}})

			err = Post(c)
			var httpErr *echo.HTTPError
			require.ErrorAs(t, err, &httpErr)
			require.Equal(t, http.StatusBadRequest, httpErr.Code)
			require.Contains(t, err.Error(), key)
			require.NotContains(t, err.Error(), "private-value")
		})
	}
}
