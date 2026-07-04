package bind

import (
	authmw "github.com/caesium-cloud/caesium/api/middleware"
	agentctrl "github.com/caesium-cloud/caesium/api/rest/controller/agent"
	agentprofilectrl "github.com/caesium-cloud/caesium/api/rest/controller/agentprofile"
	"github.com/caesium-cloud/caesium/api/rest/controller/atom"
	authctrl "github.com/caesium-cloud/caesium/api/rest/controller/auth"
	"github.com/caesium-cloud/caesium/api/rest/controller/backfill"
	blamectrl "github.com/caesium-cloud/caesium/api/rest/controller/blame"
	"github.com/caesium-cloud/caesium/api/rest/controller/database"
	datasetctrl "github.com/caesium-cloud/caesium/api/rest/controller/dataset"
	"github.com/caesium-cloud/caesium/api/rest/controller/event"
	incidentctrl "github.com/caesium-cloud/caesium/api/rest/controller/incident"
	"github.com/caesium-cloud/caesium/api/rest/controller/job"
	jobcache "github.com/caesium-cloud/caesium/api/rest/controller/job/cache"
	jobqueue "github.com/caesium-cloud/caesium/api/rest/controller/job/queue"
	"github.com/caesium-cloud/caesium/api/rest/controller/job/run"
	jobdef "github.com/caesium-cloud/caesium/api/rest/controller/jobdef"
	lineagectrl "github.com/caesium-cloud/caesium/api/rest/controller/lineage"
	"github.com/caesium-cloud/caesium/api/rest/controller/logs"
	"github.com/caesium-cloud/caesium/api/rest/controller/node"
	notifctrl "github.com/caesium-cloud/caesium/api/rest/controller/notification"
	receiptctrl "github.com/caesium-cloud/caesium/api/rest/controller/receipt"
	replayctrl "github.com/caesium-cloud/caesium/api/rest/controller/replay"
	rundiffctrl "github.com/caesium-cloud/caesium/api/rest/controller/rundiff"
	"github.com/caesium-cloud/caesium/api/rest/controller/stats"
	"github.com/caesium-cloud/caesium/api/rest/controller/system"
	"github.com/caesium-cloud/caesium/api/rest/controller/topology"
	"github.com/caesium-cloud/caesium/api/rest/controller/trigger"
	"github.com/caesium-cloud/caesium/api/rest/controller/webhook"
	whyctrl "github.com/caesium-cloud/caesium/api/rest/controller/why"
	"github.com/caesium-cloud/caesium/internal/auth"
	internal_event "github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/labstack/echo/v5"
)

// All binds all routes to the given group, optionally with auth middleware.
func All(g *echo.Group, bus internal_event.Bus, authSvc *auth.Service, auditor *auth.AuditLogger, limiter *auth.RateLimiter, sessions *auth.SessionStore) {
	bindWebhooks(g, auditor)

	protected := g.Group("")
	vars := env.Variables()
	if vars.AuthMode == "api-key" || vars.SSOEnabled() {
		protected.Use(authmw.Auth(authmw.AuthDeps{
			Service:    authSvc,
			Auditor:    auditor,
			Limiter:    limiter,
			Sessions:   sessions,
			CookieName: vars.AuthSessionCookieName,
		}))
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
		g.POST("/events", ctrl.Ingest)
		g.GET("/events/ingested", ctrl.ListIngested)
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
		g.GET("/jobs/:id/queue", jobqueue.List)
		g.GET("/jobs/:id/runs", run.List)
		g.GET("/jobs/:id/runs/diff", rundiffctrl.Get)
		g.GET("/jobs/:id/runs/:run_id", run.Get)
		g.GET("/jobs/:id/runs/:run_id/logs", run.Logs)
		// causal explainer (data-plane-memory A3): why a task ran/hit cache/re-ran
		g.GET("/jobs/:id/runs/:run_id/why", whyctrl.Get)
		g.POST("/jobs/:id/runs/:run_id/callbacks/retry", run.RetryCallbacks)
		g.POST("/jobs/:id/runs/:run_id/replay", replayctrl.Post)
		g.POST("/jobs/:id/runs/:run_id/retry", run.Retry)
		g.POST("/jobs/:id/run", run.Post)
		g.POST("/jobs", job.Post)
		g.PUT("/jobs/:id/pause", job.Pause)
		g.PUT("/jobs/:id/unpause", job.Unpause)
		g.DELETE("/jobs/:id", job.Delete)

		// historical DAG topology (data-plane-memory B2)
		g.GET("/jobs/:id/topology", topology.Get)
		g.GET("/jobs/:id/topology/history", topology.History)

		// DAG element attribution (data-plane-memory C2)
		g.GET("/jobs/:id/blame", blamectrl.Get)

		// reproducibility receipt + verify (data-plane-memory A4)
		g.GET("/jobs/:id/runs/:run_id/receipt", receiptctrl.Get)
		g.POST("/jobs/:id/runs/:run_id/receipt/verify", receiptctrl.Verify)

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
		g.POST("/jobdefs/lint", jobdef.Lint)
		g.POST("/jobdefs/diff", jobdef.Diff)
	}

	// datasets
	{
		dc := datasetctrl.New()
		g.GET("/datasets", dc.List)
		g.GET("/datasets/:ns/:name", dc.Get)
		g.GET("/datasets/:ns/:name/derivations", dc.Derivations)
		g.POST("/datasets/:ns/:name/advance", dc.Advance)
	}

	// triggers
	{
		g.GET("/triggers", trigger.List)
		g.POST("/triggers", trigger.Post)
		g.GET("/triggers/:id/events", trigger.Events)
		g.GET("/triggers/:id", trigger.Get)
		g.PATCH("/triggers/:id", trigger.Patch)
		g.POST("/triggers/:id/fire", trigger.Fire)
	}

	// stats
	{
		g.GET("/stats", stats.Get)
		g.GET("/stats/summary", stats.Summary)
	}

	// system
	{
		g.GET("/system/nodes", system.Nodes)
		g.GET("/system/features", system.Features)
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

	// notification channels
	{
		g.GET("/notifications/channels", notifctrl.ListChannels)
		g.GET("/notifications/channels/:id", notifctrl.GetChannel)
		g.POST("/notifications/channels", notifctrl.CreateChannel)
		g.PATCH("/notifications/channels/:id", notifctrl.UpdateChannel)
		g.DELETE("/notifications/channels/:id", notifctrl.DeleteChannel)
	}

	// notification policies
	{
		g.GET("/notifications/policies", notifctrl.ListPolicies)
		g.GET("/notifications/policies/:id", notifctrl.GetPolicy)
		g.POST("/notifications/policies", notifctrl.CreatePolicy)
		g.PATCH("/notifications/policies/:id", notifctrl.UpdatePolicy)
		g.DELETE("/notifications/policies/:id", notifctrl.DeletePolicy)
	}

	// agent profiles (agent-in-the-loop-remediation E2): the AgentProfile
	// server-side resource metadata.remediation.profile references.
	{
		g.GET("/agentprofiles", agentprofilectrl.List)
		g.GET("/agentprofiles/:id", agentprofilectrl.Get)
		g.POST("/agentprofiles", agentprofilectrl.Create)
		g.PATCH("/agentprofiles/:id", agentprofilectrl.Update)
		g.DELETE("/agentprofiles/:id", agentprofilectrl.Delete)
	}

	// nodes
	{
		g.GET("/nodes/:address/workers", node.Workers)
	}

	// lineage impact (data-plane-memory C2)
	{
		g.GET("/lineage/impact", lineagectrl.Impact)
	}

	// incidents — operator read API + tier-3 approval decisions
	// (agent-in-the-loop D1/D2). Bound ONLY when the remediation feature is
	// enabled; pkg/env validate() guarantees an active auth mode in that case, so
	// the approval routes are never reachable unauthenticated. Agent session
	// tokens are additionally rejected on the approval routes in authorizeScope.
	if env.Variables().AgentRemediationEnabled {
		ic := incidentctrl.New(bus)
		g.GET("/incidents", ic.List)
		g.GET("/incidents/:id", ic.Get)
		g.POST("/incidents/:id/approvals/:approval_id/approve", ic.Approve)
		g.POST("/incidents/:id/approvals/:approval_id/reject", ic.Reject)
	}

	// agent tool surface (agent-in-the-loop-remediation Stream C). All routes are
	// gated by the auth middleware's agent-scope switch, which restricts an
	// agent-session token to exactly its own incident's /v1/agent/* routes.
	{
		g.GET("/agent/incidents/:id/bundle", agentctrl.Bundle)
		g.GET("/agent/incidents/:id/context/*", agentctrl.Context)
		g.POST("/agent/incidents/:id/actions", agentctrl.Actions)
		g.POST("/agent/incidents/:id/notes", agentctrl.Notes)
		g.POST("/agent/incidents/:id/mcp", agentctrl.MCP)
	}
}

var webhookHandlerFactory = webhook.ReceiveWith

func bindWebhooks(g *echo.Group, auditor *auth.AuditLogger) {
	g.POST("/hooks/*", webhookHandlerFactory(auditor))
}

func bindAuth(g *echo.Group, controller *authctrl.Controller) {
	g.GET("/auth/keys", controller.ListKeys)
	g.POST("/auth/keys", controller.CreateKey)
	g.POST("/auth/keys/:id/revoke", controller.RevokeKey)
	g.POST("/auth/keys/:id/rotate", controller.RotateKey)
	g.GET("/auth/audit", controller.QueryAudit)
}
