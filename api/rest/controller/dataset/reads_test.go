package dataset

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

func TestDatasetPathDecodesEscapedIdentityExactlyOnce(t *testing.T) {
	for _, name := range []string{"warehouse/orders", "warehouse%2Forders", "warehouse/order 1", "warehouse/orders%2Fraw"} {
		t.Run(name, func(t *testing.T) {
			wantNamespace := "tenant/scope%2Fone"
			e := echo.New()
			e.GET("/datasets/:ns/:name/metrics", func(c *echo.Context) error {
				namespace, got := datasetPath(c)
				require.Equal(t, wantNamespace, namespace)
				require.Equal(t, name, got)
				return c.NoContent(http.StatusOK)
			})
			req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/datasets/"+url.PathEscape(wantNamespace)+"/"+url.PathEscape(name)+"/metrics", nil)
			rec := httptest.NewRecorder()
			e.ServeHTTP(rec, req)
			require.Equal(t, http.StatusOK, rec.Code)
		})
	}
}
