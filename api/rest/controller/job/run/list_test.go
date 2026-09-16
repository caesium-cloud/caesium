package run

import (
	"net/http"
	"testing"

	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRunListPageBoundsDefaultsAndValidates pins the parameter-validation
// half of issue #499: an absent limit defaults rather than falling back to
// "unbounded", and a negative/non-integer/oversized value is a 400 instead
// of a silent clamp — mirroring partitionPageBounds' contract.
func TestRunListPageBoundsDefaultsAndValidates(t *testing.T) {
	t.Run("defaults when absent", func(t *testing.T) {
		limit, offset, err := runListPageBounds("", "")
		require.NoError(t, err)
		assert.Equal(t, defaultRunListPageSize, limit)
		assert.Equal(t, 0, offset)
	})

	t.Run("honours an explicit in-range limit and offset", func(t *testing.T) {
		limit, offset, err := runListPageBounds("5", "10")
		require.NoError(t, err)
		assert.Equal(t, 5, limit)
		assert.Equal(t, 10, offset)
	})

	t.Run("honours the documented ceiling", func(t *testing.T) {
		limit, _, err := runListPageBounds("1000", "")
		require.NoError(t, err)
		assert.Equal(t, 1000, limit)
	})

	for name, limitParam := range map[string]string{
		"zero":             "0",
		"negative":         "-1",
		"non-integer":      "abc",
		"over the ceiling": "1001",
	} {
		t.Run("rejects "+name+" limit", func(t *testing.T) {
			_, _, err := runListPageBounds(limitParam, "")
			require.Error(t, err)
			var httpErr *echo.HTTPError
			require.ErrorAs(t, err, &httpErr)
			assert.Equal(t, http.StatusBadRequest, httpErr.Code)
		})
	}

	for name, offsetParam := range map[string]string{
		"negative":    "-1",
		"non-integer": "abc",
	} {
		t.Run("rejects "+name+" offset", func(t *testing.T) {
			_, _, err := runListPageBounds("", offsetParam)
			require.Error(t, err)
			var httpErr *echo.HTTPError
			require.ErrorAs(t, err, &httpErr)
			assert.Equal(t, http.StatusBadRequest, httpErr.Code)
		})
	}
}

// TestNextRunListOffset pins the null-means-done continuation contract.
func TestNextRunListOffset(t *testing.T) {
	assert.Nil(t, nextRunListOffset(0, 0, 0), "no rows returned means no next page")
	assert.Nil(t, nextRunListOffset(0, 5, 5), "exactly exhausting total means no next page")

	next := nextRunListOffset(0, 2, 5)
	require.NotNil(t, next)
	assert.Equal(t, 2, *next)
}
