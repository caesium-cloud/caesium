package replay

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

func TestDecodePostRequestRejectsOversizedBody(t *testing.T) {
	c := replayDecodeContext(`{"set":{"k":"` + strings.Repeat("a", maxReplayRequestBodyBytes) + `"}}`)

	_, err := decodePostRequest(c)
	require.Error(t, err)
	var maxBytesErr *http.MaxBytesError
	require.True(t, errors.As(err, &maxBytesErr), "expected MaxBytesReader overflow, got %v", err)
}

func TestDecodePostRequestRejectsOverCapSet(t *testing.T) {
	set := make(map[string]string, maxReplaySetEntries+1)
	for i := 0; i < maxReplaySetEntries+1; i++ {
		set[fmt.Sprintf("key-%03d", i)] = "value"
	}
	body, err := json.Marshal(PostRequest{Set: set})
	require.NoError(t, err)

	_, err = decodePostRequest(replayDecodeContext(string(body)))
	require.Error(t, err)
	require.Contains(t, err.Error(), "at most")
}

func replayDecodeContext(body string) *echo.Context {
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/v1/jobs/job/runs/run/replay", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	return e.NewContext(req, rec)
}
