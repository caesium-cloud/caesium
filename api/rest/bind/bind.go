package bind

import (
	"github.com/caesium-cloud/caesium/api/rest/controller/atom"
	"github.com/caesium-cloud/caesium/api/rest/controller/job"
	"github.com/caesium-cloud/caesium/api/rest/controller/private/cluster"
	"github.com/caesium-cloud/caesium/api/rest/controller/private/db"
	"github.com/labstack/echo/v4"
)

func All(g *echo.Group) {
	Private(g.Group("/private"))
	Public(g)
}

func Public(g *echo.Group) {
	// atoms
	{
		g.GET("/atoms", atom.List)
		g.GET("/atoms/{id}", atom.Get)
		g.POST("/atoms", atom.Post)
		g.DELETE("/atoms/{id}", atom.Delete)
	}

	// jobs
	{
		g.GET("/jobs", job.List)
		g.GET("/jobs/{id}", job.Get)
		g.POST("/jobs", job.Post)
		g.DELETE("/jobs/{id}", job.Delete)
	}
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
	g.POST("/migrate", db.Migrate)
}
