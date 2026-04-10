package bind

import (
	authmw "github.com/caesium-cloud/caesium/api/middleware"
	"github.com/caesium-cloud/caesium/api/rest/controller/atom"
	authctrl "github.com/caesium-cloud/caesium/api/rest/controller/auth"
	"github.com/caesium-cloud/caesium/api/rest/controller/backfill"
	"github.com/caesium-cloud/caesium/api/rest/controller/database"
	"github.com/caesium-cloud/caesium/api/rest/controller/event"
	"github.com/caesium-cloud/caesium/api/rest/controller/job"
	jobcache "github.com/caesium-cloud/caesium/api/rest/controller/job/cache"
	"github.com/caesium-cloud/caesium/api/rest/controller/job/run"
	jobdef "github.com/caesium-cloud/caesium/api/rest/controller/jobdef"
	"github.com/caesium-cloud/caesium/api/rest/controller/logs"
	"github.com/caesium-cloud/caesium/api/rest/controller/node"
	"github.com/caesium-cloud/caesium/api/rest/controller/stats"
	"github.com/caesium-cloud/caesium/api/rest/controller/trigger"
	"github.com/caesium-cloud/caesium/api/rest/controller/webhook"
	"github.com/caesium-cloud/caesium/internal/auth"
	internal_event "github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/labstack/echo/v5"
)

var webhookHandler = webhook.Receive

// All binds all routes to the given group, optionally with auth middleware.
func All(g *echo.Group, bus internal_event.Bus, authSvc *auth.Service, auditor *auth.AuditLogger, limiter *auth.RateLimiter) {
	bindWebhooks(g)

	protected := g.Group("")
	if env.Variables().AuthMode == "api-key" {
		protected.Use(authmw.Auth(authSvc, auditor, limiter))
		if authSvc != nil {
			bindAuth(protected, authctrl.New(authSvc, auditor))
		}
	}

	Protected(protected, bus)
}

func Protected(g *echo.Group, bus internal_event.Bus) {
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
		g.POST("/jobs/:id/runs/:run_id/retry", run.Retry)
		g.POST("/jobs/:id/run", run.Post)
		g.POST("/jobs", job.Post)
		g.PUT("/jobs/:id/pause", job.Pause)
		g.PUT("/jobs/:id/unpause", job.Unpause)
		g.DELETE("/jobs/:id", job.Delete)

		// job cache management
		g.GET("/jobs/:id/cache", jobcache.List)
		g.DELETE("/jobs/:id/cache", jobcache.DeleteJob)
		g.DELETE("/jobs/:id/cache/:task_name", jobcache.DeleteTask)

		// backfills
		g.POST("/jobs/:id/backfill", backfill.Post)
		g.GET("/jobs/:id/backfills", backfill.List)
		g.GET("/jobs/:id/backfills/:backfill_id", backfill.Get)
		g.PUT("/jobs/:id/backfills/:backfill_id/cancel", backfill.Cancel)
	}

	// global cache management
	{
		g.POST("/cache/prune", jobcache.Prune)
	}

	// job definitions
	{
		g.POST("/jobdefs/apply", jobdef.Apply)
	}

	// triggers
	{
		g.GET("/triggers", trigger.List)
		g.POST("/triggers", trigger.Post)
		g.GET("/triggers/:id", trigger.Get)
		g.PATCH("/triggers/:id", trigger.Patch)
		g.POST("/triggers/:id/fire", trigger.Fire)
	}

	// stats
	{
		g.GET("/stats", stats.Get)
	}

	// server logs
	if env.Variables().LogConsoleEnabled {
		g.GET("/logs/stream", logs.Stream)
		g.GET("/logs/level", logs.GetLevel)
		g.PUT("/logs/level", logs.SetLevel)
	}

	// database
	if env.Variables().DatabaseConsoleEnabled {
		g.GET("/database/schema", database.Schema)
		g.POST("/database/query", database.Query)
	}

	// nodes
	{
		g.GET("/nodes/:address/workers", node.Workers)
	}
}

func bindWebhooks(g *echo.Group) {
	g.POST("/hooks/*", webhookHandler)
}

func bindAuth(g *echo.Group, controller *authctrl.Controller) {
	g.GET("/auth/keys", controller.ListKeys)
	g.POST("/auth/keys", controller.CreateKey)
	g.POST("/auth/keys/:id/revoke", controller.RevokeKey)
	g.POST("/auth/keys/:id/rotate", controller.RotateKey)
	g.GET("/auth/audit", controller.QueryAudit)
}
