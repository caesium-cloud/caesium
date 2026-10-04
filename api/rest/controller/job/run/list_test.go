package run

import (
	"net/http"
	"testing"

	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPageBounds(t *testing.T) {
	tests := []struct {
		name       string
		limit      string
		offset     string
		wantLimit  int
		wantOffset int
		wantError  string
	}{
		{name: "defaults", wantLimit: defaultPageSize},
		{name: "whitespace defaults", limit: " \t ", offset: "  ", wantLimit: defaultPageSize},
		{name: "one", limit: "1", wantLimit: 1},
		{name: "ceiling", limit: "1000", wantLimit: 1000},
		{name: "trimmed values", limit: " 7 ", offset: " 9 ", wantLimit: 7, wantOffset: 9},
		{name: "zero", limit: "0", wantError: "limit must be an integer between 1 and 1000"},
		{name: "negative limit", limit: "-1", wantError: "limit must be an integer between 1 and 1000"},
		{name: "oversized", limit: "1001", wantError: "limit must be an integer between 1 and 1000"},
		{name: "invalid limit", limit: "abc", wantError: "limit must be an integer between 1 and 1000"},
		{name: "negative offset", offset: "-1", wantError: "offset must be a non-negative integer"},
		{name: "invalid offset", offset: "abc", wantError: "offset must be a non-negative integer"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			limit, offset, err := pageBounds(tt.limit, tt.offset)
			if tt.wantError != "" {
				require.Error(t, err)
				var httpErr *echo.HTTPError
				require.ErrorAs(t, err, &httpErr)
				assert.Equal(t, http.StatusBadRequest, httpErr.Code)
				assert.Equal(t, tt.wantError, httpErr.Message)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantLimit, limit)
			assert.Equal(t, tt.wantOffset, offset)
		})
	}
}
