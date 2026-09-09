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

func TestDatasetReadOptionsRejectMalformedQueriesBeforeDatabaseAccess(t *testing.T) {
	for _, path := range []string{
		"/datasets/_/orders?include_hold=maybe",
		"/datasets/_/orders?include_hold=",
		"/datasets/_/orders/metrics?metric=rowCount&limit=bad",
		"/datasets/_/orders/metrics?metric=rowCount&offset=bad",
	} {
		t.Run(path, func(t *testing.T) {
			e := echo.New()
			ctrl := New()
			e.GET("/datasets/:ns/:name", ctrl.Get)
			e.GET("/datasets/:ns/:name/metrics", ctrl.Metrics)
			req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, path, nil)
			rec := httptest.NewRecorder()
			e.ServeHTTP(rec, req)
			require.Equal(t, http.StatusBadRequest, rec.Code)
		})
	}
}
