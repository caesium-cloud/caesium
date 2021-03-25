package bind

import (
	"net/http"

	"github.com/caesium-cloud/caesium/api/rest/controller/private/cluster"
	"github.com/caesium-cloud/caesium/api/rest/controller/private/db"
	"github.com/labstack/echo/v4"
)

func All(g *echo.Group) {
	Private(g.Group("/private"))
	Public(g)
}

func Public(g *echo.Group) {
	g.GET("/placeholder", func(c echo.Context) error {
		return c.JSON(http.StatusNoContent, nil)
	})
}

func Private(g *echo.Group) {
	Cluster(g.Group("/cluster"))
	DB(g.Group("/db"))
}

func Cluster(g *echo.Group) {
	g.POST("/join", cluster.Join)
	g.DELETE("/remove", cluster.Remove)
	g.GET("/status", cluster.Status)
}

func DB(g *echo.Group) {
	g.POST("/execute", db.Execute)
	g.POST("/query", db.Query)
	g.GET("/backup", db.Backup)
	g.POST("/load", db.Load)
}
