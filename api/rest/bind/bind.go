package bind

import (
	"github.com/caesium-cloud/caesium/api/rest/controller/atom"
	"github.com/caesium-cloud/caesium/api/rest/controller/job"
	"github.com/caesium-cloud/caesium/api/rest/controller/private/db"
	"github.com/labstack/echo/v4"
)

func All(g *echo.Group) {
	Public(g)
	Private(g.Group("/private"))
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
	DB(g.Group("/db"))
}

func DB(g *echo.Group) {
	g.POST("/migrate", db.Migrate)
}
