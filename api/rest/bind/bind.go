package bind

import (
	"github.com/caesium-cloud/caesium/api/rest/controller/atom"
	"github.com/caesium-cloud/caesium/api/rest/controller/event"
	"github.com/caesium-cloud/caesium/api/rest/controller/job"
	"github.com/caesium-cloud/caesium/api/rest/controller/job/run"
	jobdef "github.com/caesium-cloud/caesium/api/rest/controller/jobdef"
	"github.com/caesium-cloud/caesium/api/rest/controller/node"
	"github.com/caesium-cloud/caesium/api/rest/controller/stats"
	"github.com/caesium-cloud/caesium/api/rest/controller/trigger"
	internal_event "github.com/caesium-cloud/caesium/internal/event"
	"github.com/labstack/echo/v5"
)

func All(g *echo.Group, bus internal_event.Bus) {
	Public(g, bus)
}

func Public(g *echo.Group, bus internal_event.Bus) {
	// events
	{
		ctrl := event.New(bus)
		g.GET("/events", ctrl.Stream)
	}

	// atoms
	{
		g.GET("/atoms", atom.List)
		g.GET("/atoms/:id", atom.Get)
		g.POST("/atoms", atom.Post)
		g.DELETE("/atoms/:id", atom.Delete)
	}

	// jobs
	{
		g.GET("/jobs", job.List)
		g.GET("/jobs/:id", job.Get)
		g.GET("/jobs/:id/tasks", job.Tasks)
		g.GET("/jobs/:id/dag", job.DAG)
		g.GET("/jobs/:id/runs", run.List)
		g.GET("/jobs/:id/runs/:run_id", run.Get)
		g.GET("/jobs/:id/runs/:run_id/logs", run.Logs)
		g.POST("/jobs/:id/runs/:run_id/callbacks/retry", run.RetryCallbacks)
		g.POST("/jobs/:id/run", run.Post)
		g.POST("/jobs", job.Post)
		g.DELETE("/jobs/:id", job.Delete)
	}

	// job definitions
	{
		g.POST("/jobdefs/apply", jobdef.Apply)
	}

	// triggers
	{
		g.GET("/triggers", trigger.List)
		g.GET("/triggers/:id", trigger.Get)
		g.PUT("/triggers/:id", trigger.Put)
	}

	// stats
	{
		g.GET("/stats", stats.Get)
	}

	// nodes
	{
		g.GET("/nodes/:address/workers", node.Workers)
	}
}
