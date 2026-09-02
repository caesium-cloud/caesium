package run

import (
	"fmt"
	"net/http"
	"testing"

	runstorage "github.com/caesium-cloud/caesium/internal/run"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRetryPartitionHTTPErrorMapsBlockedRetryToConflict pins the status the
// CLI branches on when the store refuses a retry nothing in the run could ever
// dispatch: a state conflict with the working alternative named, not a server
// fault.
func TestRetryPartitionHTTPErrorMapsBlockedRetryToConflict(t *testing.T) {
	err := retryPartitionHTTPError(runstorage.ErrPartitionRetryBlocked)
	var httpErr *echo.HTTPError
	require.ErrorAs(t, err, &httpErr)
	assert.Equal(t, http.StatusConflict, httpErr.Code)
	assert.Contains(t, fmt.Sprint(httpErr.Message), "retry the run")
}
