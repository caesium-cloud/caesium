package api

import (
	"io/fs"
	"net/http"
	"strings"

	"github.com/caesium-cloud/caesium/ui"
	"github.com/labstack/echo/v4"
)

func RegisterUI(e *echo.Echo) {
	// Get the sub-filesystem for the dist directory
	distFS, err := fs.Sub(ui.DistDir, "dist")
	if err != nil {
		panic(err)
	}

	fileServer := http.FileServer(http.FS(distFS))

	e.GET("/*", func(c echo.Context) error {
		path := c.Request().URL.Path

		// If it's a request for an API or health, don't handle it here
		if strings.HasPrefix(path, "/v1") || strings.HasPrefix(path, "/gql") || path == "/health" {
			return echo.ErrNotFound
		}

		// Check if the file exists in the embedded FS
		f, err := distFS.Open(strings.TrimPrefix(path, "/"))
		if err == nil {
			f.Close()
			fileServer.ServeHTTP(c.Response(), c.Request())
			return nil
		}

		// Fallback to index.html for SPA routing
		c.Request().URL.Path = "/"
		fileServer.ServeHTTP(c.Response(), c.Request())
		return nil
	})
}
