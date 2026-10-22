//go:build integration

package test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/models"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/google/uuid"
)

// namespaceMatrixRows is the matrix's route inventory: one row per route of
// internal/auth/rbac.go (requireMatrixCoversRouteTable fails on a missing or
// stale row), each restating the design's class and role and building its
// probes from the fixture. Request shapes and admitted statuses are the
// handlers'; Needs mirror the route's resolver in
// api/middleware/auth_namespace_resolvers.go.

const (
	rV = models.RoleViewer
	rR = models.RoleRunner
	rO = models.RoleOperator
	rA = models.RoleAdmin
)

// clusterOnly is a marker namespace no grant names: only a cluster-wide role
// sees it (a cluster-wide key in GET /v1/auth/keys).
const clusterOnly = "cluster-only"

func probe(label, path string, admit int, needs ...matrixNeed) matrixProbe {
	return matrixProbe{Label: label, Path: path, Admit: admit, Needs: needs, Mode: modeRead}
}

func (p matrixProbe) idem() matrixProbe         { p.Mode = modeIdempotent; return p }
func (p matrixProbe) once() matrixProbe         { p.Mode = modeOnce; return p }
func (p matrixProbe) body(b string) matrixProbe { p.Body = b; return p }
func (p matrixProbe) stream() matrixProbe       { p.Stream = true; return p }
func (p matrixProbe) alias(a ...string) matrixProbe {
	p.Aliases = a
	return p
}

func (p matrixProbe) marks(m ...matrixMarker) matrixProbe {
	p.Markers = append(p.Markers, m...)
	return p
}

func mark(text string, namespaces ...string) matrixMarker {
	return matrixMarker{Namespaces: namespaces, Text: text}
}

func optMark(text string, namespaces ...string) matrixMarker {
	return matrixMarker{Namespaces: namespaces, Text: text, Optional: true}
}

// never is a marker no principal may see in that body.
func never(text string) matrixMarker { return matrixMarker{Text: text} }

func (fx *namespaceMatrixFixture) all() []*matrixNamespace {
	return []*matrixNamespace{fx.n(nsDefault), fx.n(nsMarketing), fx.n(nsFinance)}
}

func (fx *namespaceMatrixFixture) pair() []*matrixNamespace {
	return []*matrixNamespace{fx.n(nsMarketing), fx.n(nsFinance)}
}

func (n *matrixNamespace) alias() string { return n.Def.Metadata.Alias }

func (n *matrixNamespace) jobPath(suffix string) string { return "/v1/jobs/" + n.Job.ID + suffix }

func (n *matrixNamespace) runPath(suffix string) string {
	return n.jobPath("/runs/" + n.Run1 + suffix)
}

// jobRoute is one probe per namespace on its main job.
func jobRoute(fx *namespaceMatrixFixture, role models.Role, suffix string, admit int) []matrixProbe {
	var probes []matrixProbe
	for _, n := range fx.all() {
		probes = append(probes, probe(n.Name+" job", n.jobPath(suffix), admit, needEvery(role, n.Name)).alias(n.alias()))
	}
	return probes
}

// runRoute is one probe per namespace on its main job's first run.
func runRoute(fx *namespaceMatrixFixture, role models.Role, suffix string, admit int) []matrixProbe {
	var probes []matrixProbe
	for _, n := range fx.all() {
		probes = append(probes, probe(n.Name+" run", n.runPath(suffix), admit, needEvery(role, n.Name, n.Name)).alias(n.alias()))
	}
	return probes
}

func each(probes []matrixProbe, f func(matrixProbe) matrixProbe) []matrixProbe {
	for i := range probes {
		probes[i] = f(probes[i])
	}
	return probes
}

func (fx *namespaceMatrixFixture) movedPath(suffix string) string {
	return "/v1/jobs/" + fx.Moved.ID + suffix
}

func (fx *namespaceMatrixFixture) definitionsBody(defs ...schema.Definition) string {
	payload, err := json.Marshal(map[string]any{"definitions": defs})
	fx.s.Require().NoError(err)
	return string(payload)
}

// cleanupCreated deletes the object an admitted create returned.
func (fx *namespaceMatrixFixture) cleanupCreated(path string) func(matrixPrincipal, string) {
	return func(_ matrixPrincipal, body string) {
		id := decodeID(fx.s, body)
		fx.mu.Lock()
		defer fx.mu.Unlock()
		fx.f.undo = append(fx.f.undo, func() {
			fx.s.nsRequest(http.MethodDelete, path+"/"+id, fx.s.authAPIKey, "", nil)
		})
	}
}

// requireExternal requires a confined caller who cannot read finance to see
// finance's nodes as placeholders.
func requireExternal(fx *namespaceMatrixFixture) func(matrixPrincipal, string) {
	return func(p matrixPrincipal, body string) {
		if p.clusterRole() == "" && p.sees(nsMarketing, rV) && !p.sees(nsFinance, rV) {
			fx.s.Containsf(body, `"kind":"external"`, "%s sees finance only as external placeholders", p.Name)
		}
	}
}

func namespaceMatrixRows() map[string]matrixRow {
	missingID := uuid.NewString()
	bearer403 := func(session int) func(matrixPrincipal) int {
		return func(p matrixPrincipal) int {
			if p.Kind == matrixKindSession {
				return session
			}
			return http.StatusForbidden // a bearer credential never confirms a CLI login
		}
	}
	return map[string]matrixRow{
		// --- identity ---
		"GET /auth/whoami": {auth.RouteClassIdentity, rV, surfaceSSO, func(fx *namespaceMatrixFixture) []matrixProbe {
			return []matrixProbe{probe("whoami", "/auth/whoami", http.StatusOK)}
		}},
		"POST /auth/logout": {auth.RouteClassIdentity, rV, surfaceSSO, func(fx *namespaceMatrixFixture) []matrixProbe {
			p := probe("logout", "/auth/logout", http.StatusNoContent).idem()
			p.Final = true
			return []matrixProbe{p}
		}},
		"GET /v1/system/features": {auth.RouteClassIdentity, rV, surfaceCore, func(fx *namespaceMatrixFixture) []matrixProbe {
			return []matrixProbe{probe("features", "/v1/system/features", http.StatusOK)}
		}},
		"GET /auth/cli/finish": {auth.RouteClassIdentity, rV, surfaceSSO, func(fx *namespaceMatrixFixture) []matrixProbe {
			p := probe("unknown state", "/auth/cli/finish?state=mx-missing", 0)
			p.AdmitFor = bearer403(http.StatusNotFound)
			return []matrixProbe{p}
		}},
		"POST /auth/cli/authorize": {auth.RouteClassIdentity, rV, surfaceSSO, func(fx *namespaceMatrixFixture) []matrixProbe {
			p := probe("unknown state", "/auth/cli/authorize", 0).idem().body(`{"state":"mx-missing"}`)
			p.AdmitFor = bearer403(http.StatusNotFound)
			return []matrixProbe{p}
		}},

		// --- cluster ---
		"GET /metrics": {auth.RouteClassCluster, rV, surfaceCore, func(fx *namespaceMatrixFixture) []matrixProbe {
			return []matrixProbe{probe("registry", "/metrics", http.StatusOK)}
		}},
		// Connector routes probe a connection the lane does not configure:
		// the runtime refuses it locally, with no provider call.
		"GET /v1/connectors": {auth.RouteClassCluster, rV, surfaceConnectors, func(fx *namespaceMatrixFixture) []matrixProbe {
			return []matrixProbe{probe("connections", "/v1/connectors", http.StatusOK)}
		}},
		"GET /v1/connectors/:id/executions": {auth.RouteClassCluster, rV, surfaceConnectors, func(fx *namespaceMatrixFixture) []matrixProbe {
			return []matrixProbe{probe("unknown connection", "/v1/connectors/mx-missing/executions", http.StatusNotFound)}
		}},
		"GET /v1/connectors/:id/executions/:id": {auth.RouteClassCluster, rV, surfaceConnectors, func(fx *namespaceMatrixFixture) []matrixProbe {
			return []matrixProbe{probe("unknown connection", "/v1/connectors/mx-missing/executions/mx-exec", http.StatusNotFound)}
		}},
		"GET /v1/connectors/:id/executions/:id/history": {auth.RouteClassCluster, rV, surfaceConnectors, func(fx *namespaceMatrixFixture) []matrixProbe {
			return []matrixProbe{probe("unknown connection", "/v1/connectors/mx-missing/executions/mx-exec/history", http.StatusNotFound)}
		}},
		"GET /v1/connectors/:id/executions/:id/relationships": {auth.RouteClassCluster, rV, surfaceConnectors, func(fx *namespaceMatrixFixture) []matrixProbe {
			return []matrixProbe{probe("unknown connection", "/v1/connectors/mx-missing/executions/mx-exec/relationships", http.StatusNotFound)}
		}},
		"GET /v1/connectors/:id/executions/:id/operations": {auth.RouteClassCluster, rV, surfaceConnectors, func(fx *namespaceMatrixFixture) []matrixProbe {
			return []matrixProbe{probe("unknown connection", "/v1/connectors/mx-missing/executions/mx-exec/operations", http.StatusNotFound)}
		}},
		"GET /v1/connectors/:id/operations/:id": {auth.RouteClassCluster, rV, surfaceConnectors, func(fx *namespaceMatrixFixture) []matrixProbe {
			return []matrixProbe{probe("unknown connection", "/v1/connectors/mx-missing/operations/mx-op", http.StatusNotFound)}
		}},
		"POST /v1/connectors/:id/executions/:id/queries/:id": {auth.RouteClassCluster, rV, surfaceConnectors, func(fx *namespaceMatrixFixture) []matrixProbe {
			return []matrixProbe{probe("unknown connection", "/v1/connectors/mx-missing/executions/mx-exec/queries/mx-query", http.StatusNotFound).idem().body(`{}`)}
		}},
		"POST /v1/connectors/:id/executions/:id/actions/:id": {auth.RouteClassCluster, rO, surfaceConnectors, func(fx *namespaceMatrixFixture) []matrixProbe {
			p := probe("unknown connection", "/v1/connectors/mx-missing/executions/mx-exec/actions/mx-action", http.StatusBadRequest).idem().body(`{}`)
			return []matrixProbe{p}
		}},
		"POST /v1/connectors/:id/operations/:id/resubmit": {auth.RouteClassCluster, rO, surfaceConnectors, func(fx *namespaceMatrixFixture) []matrixProbe {
			return []matrixProbe{probe("unknown connection", "/v1/connectors/mx-missing/operations/mx-op/resubmit", http.StatusNotFound).idem().body(`{}`)}
		}},
		"GET /v1/system/nodes": {auth.RouteClassCluster, rV, surfaceCore, func(fx *namespaceMatrixFixture) []matrixProbe {
			return []matrixProbe{probe("nodes", "/v1/system/nodes", http.StatusOK)}
		}},
		"DELETE /v1/system/nodes/:id": {auth.RouteClassCluster, rA, surfaceCore, func(fx *namespaceMatrixFixture) []matrixProbe {
			// Never a real member: a decimal id no node holds.
			return []matrixProbe{probe("unknown member", "/v1/system/nodes/987654321987", http.StatusNotFound).idem()}
		}},
		"GET /v1/atoms": {auth.RouteClassCluster, rV, surfaceCore, func(fx *namespaceMatrixFixture) []matrixProbe {
			return []matrixProbe{probe("atoms", "/v1/atoms", http.StatusOK)}
		}},
		"GET /v1/atoms/:id": {auth.RouteClassCluster, rV, surfaceCore, func(fx *namespaceMatrixFixture) []matrixProbe {
			return []matrixProbe{probe("unknown atom", "/v1/atoms/"+missingID, http.StatusNotFound)}
		}},
		"POST /v1/atoms": {auth.RouteClassCluster, rO, surfaceCore, func(fx *namespaceMatrixFixture) []matrixProbe {
			return []matrixProbe{probe("malformed body", "/v1/atoms", http.StatusBadRequest).idem().body(`{`)}
		}},
		"DELETE /v1/atoms/:id": {auth.RouteClassCluster, rO, surfaceCore, func(fx *namespaceMatrixFixture) []matrixProbe {
			return []matrixProbe{probe("unknown atom", "/v1/atoms/"+missingID, http.StatusAccepted).idem()}
		}},
		"GET /v1/nodes/:id/workers": {auth.RouteClassCluster, rV, surfaceCore, func(fx *namespaceMatrixFixture) []matrixProbe {
			return []matrixProbe{probe("unknown node", "/v1/nodes/mx-node/workers", http.StatusOK)}
		}},
		"POST /v1/cache/prune": {auth.RouteClassCluster, rO, surfaceCore, func(fx *namespaceMatrixFixture) []matrixProbe {
			return []matrixProbe{probe("prune", "/v1/cache/prune", http.StatusOK).idem()}
		}},
		"POST /v1/triggers": {auth.RouteClassCluster, rO, surfaceCore, func(fx *namespaceMatrixFixture) []matrixProbe {
			// No delete route: a malformed body proves admission, creates nothing.
			return []matrixProbe{probe("malformed body", "/v1/triggers", http.StatusBadRequest).idem().body(`{`)}
		}},
		"GET /v1/logs/level": {auth.RouteClassCluster, rA, surfaceLogConsole, func(fx *namespaceMatrixFixture) []matrixProbe {
			return []matrixProbe{probe("level", "/v1/logs/level", http.StatusOK)}
		}},
		"PUT /v1/logs/level": {auth.RouteClassCluster, rA, surfaceLogConsole, func(fx *namespaceMatrixFixture) []matrixProbe {
			// The lane's own level, so the write changes nothing.
			return []matrixProbe{probe("same level", "/v1/logs/level", http.StatusOK).idem().body(`{"level":"debug"}`)}
		}},
		"GET /v1/logs/stream": {auth.RouteClassCluster, rA, surfaceLogConsole, func(fx *namespaceMatrixFixture) []matrixProbe {
			return []matrixProbe{probe("stream", "/v1/logs/stream", http.StatusOK).stream()}
		}},
		"GET /v1/database/schema": {auth.RouteClassCluster, rA, surfaceDatabaseConsole, func(fx *namespaceMatrixFixture) []matrixProbe {
			return []matrixProbe{probe("schema", "/v1/database/schema", http.StatusOK)}
		}},
		"POST /v1/database/query": {auth.RouteClassCluster, rA, surfaceDatabaseConsole, func(fx *namespaceMatrixFixture) []matrixProbe {
			return []matrixProbe{probe("select 1", "/v1/database/query", http.StatusOK).idem().body(`{"sql":"SELECT 1","limit":1}`)}
		}},
		"GET /v1/auth/policy": {auth.RouteClassCluster, rA, surfaceCore, func(fx *namespaceMatrixFixture) []matrixProbe {
			return []matrixProbe{probe("policy", "/v1/auth/policy", http.StatusOK)}
		}},
		"POST /v1/auth/policy/reload": {auth.RouteClassCluster, rA, surfaceCore, func(fx *namespaceMatrixFixture) []matrixProbe {
			return []matrixProbe{probe("unchanged file", "/v1/auth/policy/reload", http.StatusOK).idem()}
		}},
		"GET /v1/auth/namespaces": {auth.RouteClassCluster, rV, surfaceCore, func(fx *namespaceMatrixFixture) []matrixProbe {
			return []matrixProbe{probe("namespaces", "/v1/auth/namespaces", http.StatusOK)}
		}},
		"GET /v1/auth/users": {auth.RouteClassCluster, rA, surfaceUserAdmin, func(fx *namespaceMatrixFixture) []matrixProbe {
			return []matrixProbe{probe("users", "/v1/auth/users", http.StatusOK)}
		}},
		"GET /v1/auth/users/:id": {auth.RouteClassCluster, rA, surfaceUserAdmin, func(fx *namespaceMatrixFixture) []matrixProbe {
			return []matrixProbe{probe("unknown user", "/v1/auth/users/"+missingID, http.StatusNotFound)}
		}},
		"POST /v1/auth/users/:id/disable": {auth.RouteClassCluster, rA, surfaceUserAdmin, func(fx *namespaceMatrixFixture) []matrixProbe {
			return []matrixProbe{probe("unknown user", "/v1/auth/users/"+missingID+"/disable", http.StatusNotFound).idem()}
		}},
		"POST /v1/auth/users/:id/enable": {auth.RouteClassCluster, rA, surfaceUserAdmin, func(fx *namespaceMatrixFixture) []matrixProbe {
			return []matrixProbe{probe("unknown user", "/v1/auth/users/"+missingID+"/enable", http.StatusNotFound).idem()}
		}},
		"POST /v1/auth/users/:id/sessions/revoke": {auth.RouteClassCluster, rA, surfaceUserAdmin, func(fx *namespaceMatrixFixture) []matrixProbe {
			return []matrixProbe{probe("unknown user", "/v1/auth/users/"+missingID+"/sessions/revoke", http.StatusNotFound).idem()}
		}},
		"GET /v1/auth/sessions": {auth.RouteClassCluster, rA, surfaceUserAdmin, func(fx *namespaceMatrixFixture) []matrixProbe {
			return []matrixProbe{probe("sessions", "/v1/auth/sessions", http.StatusOK)}
		}},

		// --- collections ---
		"GET /v1/jobs": {auth.RouteClassCollection, rV, surfaceCore, func(fx *namespaceMatrixFixture) []matrixProbe {
			d, m, f := fx.n(nsDefault), fx.n(nsMarketing), fx.n(nsFinance)
			jobMark := func(n *matrixNamespace) matrixMarker {
				return matrixMarker{Namespaces: []string{n.Name}, Alias: n.alias(), Text: n.alias()}
			}
			return []matrixProbe{
				probe("all", "/v1/jobs?limit=1000", http.StatusOK).marks(jobMark(d), jobMark(m), jobMark(f), mark(fx.MovedDef.Metadata.Alias, nsMarketing)),
				// F2: ?namespace= only narrows, never widens or refuses.
				probe("narrowed to marketing", "/v1/jobs?limit=1000&namespace=marketing", http.StatusOK).marks(jobMark(m), never(f.alias()), never(d.alias())),
				probe("narrowed to finance", "/v1/jobs?limit=1000&namespace=finance", http.StatusOK).marks(jobMark(f), never(m.alias()), never(d.alias())),
				probe("malformed namespace", "/v1/jobs?namespace=Not_A_Label", http.StatusBadRequest),
			}
		}},
		"GET /v1/events": {auth.RouteClassCollection, rV, surfaceCore, func(fx *namespaceMatrixFixture) []matrixProbe {
			probes := []matrixProbe{probe("global stream", "/v1/events", http.StatusOK).stream()}
			for _, n := range fx.all() {
				p := probe(n.Name+" run stream", "/v1/events?run_id="+n.Run1, http.StatusOK).stream().alias(n.alias())
				namespace := n.Name
				// The handler follows a run only in a namespace the caller
				// reads (an audited *authz.Denial; the route stays collection).
				p.HandlerDeny = func(pr matrixPrincipal) bool { return !pr.sees(namespace, rV) }
				probes = append(probes, p)
			}
			return probes
		}},
		"GET /v1/events/ingested": {auth.RouteClassCollection, rV, surfaceCore, func(fx *namespaceMatrixFixture) []matrixProbe {
			m, f := fx.n(nsMarketing), fx.n(nsFinance)
			return []matrixProbe{probe("ingested", "/v1/events/ingested?limit=500", http.StatusOK).marks(mark(m.EventID, nsMarketing), mark(f.EventID, nsFinance))}
		}},
		"GET /v1/stats": {auth.RouteClassCollection, rV, surfaceCore, func(fx *namespaceMatrixFixture) []matrixProbe {
			f := fx.n(nsFinance)
			return []matrixProbe{probe("stats", "/v1/stats", http.StatusOK).marks(append(fx.failMarks(), optMark(f.alias(), nsFinance))...)}
		}},
		"GET /v1/stats/summary": {auth.RouteClassCollection, rV, surfaceCore, func(fx *namespaceMatrixFixture) []matrixProbe {
			f := fx.n(nsFinance)
			return []matrixProbe{probe("summary", "/v1/stats/summary", http.StatusOK).marks(append(fx.failMarks(), optMark(f.alias(), nsFinance))...)}
		}},
		"GET /v1/triggers": {auth.RouteClassCollection, rV, surfaceCore, func(fx *namespaceMatrixFixture) []matrixProbe {
			d, m, f := fx.n(nsDefault), fx.n(nsMarketing), fx.n(nsFinance)
			return []matrixProbe{
				probe("all", "/v1/triggers?limit=1000", http.StatusOK).marks(mark(d.JobTrigger, nsDefault), mark(m.HTTPTrigger, nsMarketing), mark(f.HTTPTrigger, nsFinance), mark(m.EventTrigger, nsMarketing), mark(f.EventTrigger, nsFinance)),
				probe("narrowed to finance", "/v1/triggers?limit=1000&namespace=finance", http.StatusOK).marks(mark(f.HTTPTrigger, nsFinance), never(m.HTTPTrigger), never(d.JobTrigger)),
			}
		}},
		"GET /v1/incidents": {auth.RouteClassCollection, rV, surfaceIncidents, func(fx *namespaceMatrixFixture) []matrixProbe {
			m, f := fx.n(nsMarketing), fx.n(nsFinance)
			return []matrixProbe{
				probe("all", "/v1/incidents?limit=500", http.StatusOK).marks(mark(m.Incident, nsMarketing), mark(f.Incident, nsFinance)),
				probe("narrowed to marketing", "/v1/incidents?limit=500&namespace=marketing", http.StatusOK).marks(mark(m.Incident, nsMarketing), never(f.Incident)),
			}
		}},
		"GET /v1/datasets": {auth.RouteClassCollection, rV, surfaceCore, func(fx *namespaceMatrixFixture) []matrixProbe {
			m, f := fx.n(nsMarketing), fx.n(nsFinance)
			return []matrixProbe{probe("datasets", "/v1/datasets?limit=1000", http.StatusOK).marks(
				mark(m.Dataset, nsMarketing), mark(f.Dataset, nsFinance), mark(fx.BothDataset, nsMarketing, nsFinance),
				mark(fx.Root, nsMarketing), mark(fx.FinDataset, nsFinance))}
		}},
		"GET /v1/datasets/holds": {auth.RouteClassCollection, rV, surfaceDataAssertions, func(fx *namespaceMatrixFixture) []matrixProbe {
			m, f := fx.n(nsMarketing), fx.n(nsFinance)
			return []matrixProbe{probe("holds", "/v1/datasets/holds?limit=1000", http.StatusOK).marks(
				mark(m.Hold, nsMarketing), mark(f.Hold, nsFinance), mark(fx.BothHold, nsMarketing, nsFinance))}
		}},
		"GET /v1/notifications/channels": {auth.RouteClassCollection, rV, surfaceCore, func(fx *namespaceMatrixFixture) []matrixProbe {
			m, f := fx.n(nsMarketing), fx.n(nsFinance)
			return []matrixProbe{probe("channels", "/v1/notifications/channels", http.StatusOK).marks(
				mark(m.Channel, nsMarketing), mark(f.Channel, nsFinance), mark(fx.SharedChannel, models.ClusterNamespace))}
		}},
		"GET /v1/notifications/policies": {auth.RouteClassCollection, rV, surfaceCore, func(fx *namespaceMatrixFixture) []matrixProbe {
			m, f := fx.n(nsMarketing), fx.n(nsFinance)
			return []matrixProbe{probe("policies", "/v1/notifications/policies", http.StatusOK).marks(
				mark(m.Policy, nsMarketing), mark(f.Policy, nsFinance), mark(fx.SharedPolicy, models.ClusterNamespace))}
		}},
		"GET /v1/agentprofiles": {auth.RouteClassCollection, rV, surfaceCore, func(fx *namespaceMatrixFixture) []matrixProbe {
			m, f := fx.n(nsMarketing), fx.n(nsFinance)
			return []matrixProbe{probe("profiles", "/v1/agentprofiles", http.StatusOK).marks(
				mark(m.Profile, nsMarketing), mark(f.Profile, nsFinance), mark(fx.SharedProfile, models.ClusterNamespace))}
		}},
		"GET /v1/contracts/graph": {auth.RouteClassCollection, rV, surfaceContracts, func(fx *namespaceMatrixFixture) []matrixProbe {
			m, f := fx.n(nsMarketing), fx.n(nsFinance)
			p := probe("graph", "/v1/contracts/graph", http.StatusOK).marks(
				mark(m.alias(), nsMarketing), mark(f.alias(), nsFinance), mark(fx.FinDataset, nsFinance), mark(fx.FinColumn, nsFinance), mark(f.Job.ID, nsFinance))
			p.Check = requireExternal(fx)
			return []matrixProbe{p}
		}},
		"GET /v1/lineage/impact": {auth.RouteClassCollection, rV, surfaceCore, func(fx *namespaceMatrixFixture) []matrixProbe {
			f := fx.n(nsFinance)
			p := probe("root cone", impactPath(fx.Root), http.StatusOK).marks(mark(f.alias(), nsFinance), mark(fx.FinDataset, nsFinance), mark(f.Job.ID, nsFinance))
			p.Check = requireExternal(fx)
			return []matrixProbe{p}
		}},
		"GET /v1/auth/keys": {auth.RouteClassCollection, rA, surfaceCore, func(fx *namespaceMatrixFixture) []matrixProbe {
			m, f := fx.n(nsMarketing), fx.n(nsFinance)
			p := probe("keys", "/v1/auth/keys", http.StatusOK).marks(mark(m.ListedKey, nsMarketing), mark(f.ListedKey, nsFinance), mark(fx.ClusterListedKey, clusterOnly))
			p.MarkerRole = rA
			return []matrixProbe{p}
		}},

		// --- namespaced: jobs ---
		"GET /v1/jobs/:id": {auth.RouteClassNamespaced, rV, surfaceCore, func(fx *namespaceMatrixFixture) []matrixProbe {
			return append(jobRoute(fx, rV, "", http.StatusOK),
				probe("moved job", fx.movedPath(""), http.StatusOK, needEvery(rV, nsMarketing)).alias(fx.MovedDef.Metadata.Alias).marks(mark(fx.NewRun, nsMarketing), never(fx.OldRun)))
		}},
		"GET /v1/jobs/:id/tasks":    {auth.RouteClassNamespaced, rV, surfaceCore, func(fx *namespaceMatrixFixture) []matrixProbe { return jobRoute(fx, rV, "/tasks", http.StatusOK) }},
		"GET /v1/jobs/:id/dag":      {auth.RouteClassNamespaced, rV, surfaceCore, func(fx *namespaceMatrixFixture) []matrixProbe { return jobRoute(fx, rV, "/dag", http.StatusOK) }},
		"GET /v1/jobs/:id/manifest": {auth.RouteClassNamespaced, rV, surfaceCore, func(fx *namespaceMatrixFixture) []matrixProbe { return jobRoute(fx, rV, "/manifest", http.StatusOK) }},
		"GET /v1/jobs/:id/queue":    {auth.RouteClassNamespaced, rV, surfaceCore, func(fx *namespaceMatrixFixture) []matrixProbe { return jobRoute(fx, rV, "/queue", http.StatusOK) }},
		"GET /v1/jobs/:id/runs": {auth.RouteClassNamespaced, rV, surfaceCore, func(fx *namespaceMatrixFixture) []matrixProbe {
			// History under a moved job: each run is shown only to callers
			// who read its persisted namespace (design §8.3).
			return append(jobRoute(fx, rV, "/runs", http.StatusOK),
				probe("moved job", fx.movedPath("/runs"), http.StatusOK, needEvery(rV, nsMarketing)).alias(fx.MovedDef.Metadata.Alias).marks(mark(fx.NewRun, nsMarketing), mark(fx.OldRun, nsFinance)))
		}},
		"GET /v1/jobs/:id/runs/diff": {auth.RouteClassNamespaced, rV, surfaceCore, func(fx *namespaceMatrixFixture) []matrixProbe {
			var probes []matrixProbe
			for _, n := range fx.all() {
				probes = append(probes, probe(n.Name+" runs", n.jobPath("/runs/diff?left="+n.Run1+"&right="+n.Run2), http.StatusOK, needEvery(rV, n.Name, n.Name, n.Name)).alias(n.alias()))
			}
			return append(probes, probe("across the move", fx.movedPath("/runs/diff?left="+fx.OldRun+"&right="+fx.NewRun), http.StatusOK,
				needEvery(rV, nsMarketing, nsFinance, nsMarketing)).alias(fx.MovedDef.Metadata.Alias))
		}},
		"GET /v1/jobs/:id/blame":    {auth.RouteClassNamespaced, rV, surfaceCore, func(fx *namespaceMatrixFixture) []matrixProbe { return jobRoute(fx, rV, "/blame", http.StatusOK) }},
		"GET /v1/jobs/:id/topology": {auth.RouteClassNamespaced, rV, surfaceCore, func(fx *namespaceMatrixFixture) []matrixProbe { return jobRoute(fx, rV, "/topology", http.StatusOK) }},
		"GET /v1/jobs/:id/topology/history": {auth.RouteClassNamespaced, rV, surfaceCore, func(fx *namespaceMatrixFixture) []matrixProbe {
			return jobRoute(fx, rV, "/topology/history", http.StatusOK)
		}},
		"GET /v1/jobs/:id/cache": {auth.RouteClassNamespaced, rV, surfaceCore, func(fx *namespaceMatrixFixture) []matrixProbe {
			return append(jobRoute(fx, rV, "/cache", http.StatusOK),
				probe("moved job", fx.movedPath("/cache"), http.StatusOK, needEvery(rV, nsMarketing)).alias(fx.MovedDef.Metadata.Alias).marks(mark(fx.NewRun, nsMarketing), mark(fx.OldRun, nsFinance)))
		}},
		"GET /v1/jobs/:id/backfills": {auth.RouteClassNamespaced, rV, surfaceCore, func(fx *namespaceMatrixFixture) []matrixProbe {
			probes := jobRoute(fx, rV, "/backfills", http.StatusOK)
			for i, n := range fx.all() {
				probes[i] = probes[i].marks(mark(n.Backfill, n.Name))
			}
			return probes
		}},
		"GET /v1/jobs/:id/backfills/:id": {auth.RouteClassNamespaced, rV, surfaceCore, func(fx *namespaceMatrixFixture) []matrixProbe {
			var probes []matrixProbe
			for _, n := range fx.all() {
				probes = append(probes, probe(n.Name+" backfill", n.jobPath("/backfills/"+n.Backfill), http.StatusOK, needEvery(rV, n.Name, n.Name)).alias(n.alias()))
			}
			return probes
		}},
		"GET /v1/jobs/:id/runs/:id": {auth.RouteClassNamespaced, rV, surfaceCore, func(fx *namespaceMatrixFixture) []matrixProbe {
			return append(runRoute(fx, rV, "", http.StatusOK),
				probe("moved job's old run", fx.movedPath("/runs/"+fx.OldRun), http.StatusOK, needEvery(rV, nsMarketing, nsFinance)).alias(fx.MovedDef.Metadata.Alias))
		}},
		"GET /v1/jobs/:id/runs/:id/logs":    {auth.RouteClassNamespaced, rV, surfaceCore, func(fx *namespaceMatrixFixture) []matrixProbe { return runRoute(fx, rV, "/logs", http.StatusOK) }},
		"GET /v1/jobs/:id/runs/:id/receipt": {auth.RouteClassNamespaced, rV, surfaceCore, func(fx *namespaceMatrixFixture) []matrixProbe { return runRoute(fx, rV, "/receipt", http.StatusOK) }},
		"POST /v1/jobs/:id/runs/:id/receipt/verify": {auth.RouteClassNamespaced, rV, surfaceCore, func(fx *namespaceMatrixFixture) []matrixProbe {
			return each(runRoute(fx, rV, "/receipt/verify", http.StatusOK), matrixProbe.idem)
		}},
		"GET /v1/jobs/:id/runs/:id/tasks/:id/descriptor": {auth.RouteClassNamespaced, rV, surfaceCore, func(fx *namespaceMatrixFixture) []matrixProbe {
			return runRoute(fx, rV, "/tasks/ok/descriptor", http.StatusOK)
		}},
		"GET /v1/jobs/:id/runs/:id/tasks/:id/partitions": {auth.RouteClassNamespaced, rV, surfaceCore, func(fx *namespaceMatrixFixture) []matrixProbe {
			return runRoute(fx, rV, "/tasks/ok/partitions", http.StatusOK)
		}},
		"GET /v1/jobs/:id/runs/:id/external-executions": {auth.RouteClassNamespaced, rV, surfaceConnectors, func(fx *namespaceMatrixFixture) []matrixProbe {
			return runRoute(fx, rV, "/external-executions", http.StatusOK)
		}},
		"GET /v1/jobs/:id/runs/:id/why": {auth.RouteClassNamespaced, rV, surfaceCore, func(fx *namespaceMatrixFixture) []matrixProbe {
			return runRoute(fx, rV, "/why?task=ok", http.StatusOK)
		}},

		// --- namespaced: job writes ---
		"POST /v1/jobs/:id/run": {auth.RouteClassNamespaced, rR, surfaceCore, func(fx *namespaceMatrixFixture) []matrixProbe {
			return each(jobRoute(fx, rR, "/run", http.StatusAccepted), matrixProbe.once)
		}},
		"POST /v1/jobs/:id/runs/:id/replay": {auth.RouteClassNamespaced, rR, surfaceCore, func(fx *namespaceMatrixFixture) []matrixProbe {
			var probes []matrixProbe
			for _, n := range fx.all() {
				probes = append(probes, probe(n.Name+" run", n.runPath("/replay"), http.StatusAccepted, needEvery(rR, n.Name), needEvery(rV, n.Name)).alias(n.alias()).once())
			}
			// Replay of the moved job's old run: viewer where the baseline
			// ran, runner where the replay runs.
			return append(probes, probe("moved job's old run", fx.movedPath("/runs/"+fx.OldRun+"/replay"), http.StatusAccepted,
				needEvery(rR, nsMarketing), needEvery(rV, nsFinance)).alias(fx.MovedDef.Metadata.Alias).once())
		}},
		"POST /v1/jobs/:id/runs/:id/retry": {auth.RouteClassNamespaced, rR, surfaceCore, func(fx *namespaceMatrixFixture) []matrixProbe {
			probes := each(runRoute(fx, rR, "/retry", http.StatusConflict), matrixProbe.idem)
			return append(probes, probe("moved job's old run", fx.movedPath("/runs/"+fx.OldRun+"/retry"), http.StatusConflict,
				needEvery(rR, nsMarketing, nsFinance)).alias(fx.MovedDef.Metadata.Alias).idem().withCheck(func(_ matrixPrincipal, body string) {
				fx.s.Contains(body, "namespace_moved")
			}))
		}},
		"POST /v1/jobs/:id/runs/:id/tasks/:id/partitions/:id/retry": {auth.RouteClassNamespaced, rR, surfaceCore, func(fx *namespaceMatrixFixture) []matrixProbe {
			return each(runRoute(fx, rR, "/tasks/ok/partitions/0/retry", http.StatusConflict), matrixProbe.idem)
		}},
		"POST /v1/jobs/:id/runs/:id/callbacks/retry": {auth.RouteClassNamespaced, rR, surfaceCore, func(fx *namespaceMatrixFixture) []matrixProbe {
			return each(runRoute(fx, rR, "/callbacks/retry", http.StatusAccepted), matrixProbe.idem)
		}},
		"POST /v1/jobs/:id/backfill": {auth.RouteClassNamespaced, rR, surfaceCore, func(fx *namespaceMatrixFixture) []matrixProbe {
			now := time.Now().UTC()
			window := fmt.Sprintf(`{"start":%q,"end":%q}`, now.Add(-4*time.Hour).Format(time.RFC3339), now.Add(-3*time.Hour).Format(time.RFC3339))
			return each(jobRoute(fx, rR, "/backfill", http.StatusAccepted), func(p matrixProbe) matrixProbe { return p.once().body(window) })
		}},
		"POST /v1/jobs": {auth.RouteClassNamespaced, rO, surfaceCore, func(fx *namespaceMatrixFixture) []matrixProbe {
			var probes []matrixProbe
			for _, n := range fx.all() {
				alias := fx.createdAlias(n.Name + "-legacy")
				body, err := json.Marshal(map[string]any{
					"alias":    alias,
					"metadata": map[string]any{"namespace": n.Name},
					"trigger":  map[string]any{"alias": alias, "type": "cron", "configuration": map[string]any{"cron": "0 0 31 2 *"}},
					"tasks":    []any{map[string]any{"atom": map[string]any{"engine": fx.s.engineOrDocker(), "image": "busybox:1.36.1", "command": []string{"true"}}}},
				})
				fx.s.Require().NoError(err)
				probes = append(probes, probe(n.Name+" legacy create", "/v1/jobs", http.StatusCreated, needEvery(rO, n.Name)).alias(alias).once().body(string(body)))
			}
			return probes
		}},
		"DELETE /v1/jobs/:id": {auth.RouteClassNamespaced, rO, surfaceCore, func(fx *namespaceMatrixFixture) []matrixProbe {
			var probes []matrixProbe
			for _, n := range fx.all() {
				probes = append(probes, probe(n.Name+" victim", "/v1/jobs/"+n.Victim.ID, http.StatusAccepted, needEvery(rO, n.Name)).alias(n.Victim.Alias).once())
			}
			return probes
		}},
		"PUT /v1/jobs/:id/pause": {auth.RouteClassNamespaced, rO, surfaceCore, func(fx *namespaceMatrixFixture) []matrixProbe {
			return each(jobRoute(fx, rO, "/pause", http.StatusOK), matrixProbe.idem)
		}},
		"PUT /v1/jobs/:id/unpause": {auth.RouteClassNamespaced, rO, surfaceCore, func(fx *namespaceMatrixFixture) []matrixProbe {
			return each(jobRoute(fx, rO, "/unpause", http.StatusOK), matrixProbe.idem)
		}},
		"DELETE /v1/jobs/:id/queue/:id": {auth.RouteClassNamespaced, rO, surfaceCore, func(fx *namespaceMatrixFixture) []matrixProbe {
			return each(jobRoute(fx, rO, "/queue/"+missingID, http.StatusNotFound), matrixProbe.idem)
		}},
		"DELETE /v1/jobs/:id/cache": {auth.RouteClassNamespaced, rO, surfaceCore, func(fx *namespaceMatrixFixture) []matrixProbe {
			return each(jobRoute(fx, rO, "/cache", http.StatusNoContent), matrixProbe.idem)
		}},
		"DELETE /v1/jobs/:id/cache/:id": {auth.RouteClassNamespaced, rO, surfaceCore, func(fx *namespaceMatrixFixture) []matrixProbe {
			// The parameter is the task name.
			return each(jobRoute(fx, rO, "/cache/ok", http.StatusNoContent), matrixProbe.idem)
		}},
		"PUT /v1/jobs/:id/backfills/:id/cancel": {auth.RouteClassNamespaced, rO, surfaceCore, func(fx *namespaceMatrixFixture) []matrixProbe {
			var probes []matrixProbe
			for _, n := range fx.all() {
				// The backfill scheduled nothing, so it is complete: 409.
				probes = append(probes, probe(n.Name+" backfill", n.jobPath("/backfills/"+n.Backfill+"/cancel"), http.StatusConflict, needEvery(rO, n.Name, n.Name)).alias(n.alias()).idem())
			}
			return probes
		}},

		// --- namespaced: job definitions ---
		"POST /v1/jobdefs/lint": {auth.RouteClassNamespaced, rV, surfaceCore, func(fx *namespaceMatrixFixture) []matrixProbe {
			var probes []matrixProbe
			for _, n := range fx.all() {
				probes = append(probes, probe(n.Name+" definition", "/v1/jobdefs/lint", http.StatusOK, needEvery(rV, n.Name)).idem().body(fx.definitionsBody(n.Def)))
			}
			return probes
		}},
		"POST /v1/jobdefs/diff": {auth.RouteClassNamespaced, rV, surfaceCore, func(fx *namespaceMatrixFixture) []matrixProbe {
			var probes []matrixProbe
			for _, n := range fx.all() {
				probes = append(probes, probe(n.Name+" definition", "/v1/jobdefs/diff", http.StatusOK, needEvery(rV, n.Name)).idem().body(fx.definitionsBody(n.Def)))
			}
			// An empty request diffs against every persisted job the caller
			// may read: every one is "removed", and nothing foreign appears.
			d, m, f := fx.n(nsDefault), fx.n(nsMarketing), fx.n(nsFinance)
			empty := probe("empty body", "/v1/jobdefs/diff", http.StatusOK, needAnyGrant(rV)).idem().body(`{"definitions":[]}`).marks(
				mark(d.alias(), nsDefault), mark(m.alias(), nsMarketing), mark(f.alias(), nsFinance), mark(fx.MovedDef.Metadata.Alias, nsMarketing),
				optMark(f.Job.ID, nsFinance), optMark(fx.FinColumn, nsFinance), optMark(fx.FinDataset, nsFinance)).marks(fx.failMarks()...)
			return append(probes, empty)
		}},
		"POST /v1/jobdefs/apply": {auth.RouteClassNamespaced, rO, surfaceCore, func(fx *namespaceMatrixFixture) []matrixProbe {
			var probes []matrixProbe
			for _, n := range fx.all() {
				// Re-applying the unchanged definition.
				probes = append(probes, probe(n.Name+" definition", "/v1/jobdefs/apply", http.StatusOK, needEvery(rO, n.Name)).alias(n.alias()).idem().body(fx.definitionsBody(n.Def)))
			}
			return probes
		}},

		// --- namespaced: triggers and events ---
		"GET /v1/triggers/:id": {auth.RouteClassNamespaced, rV, surfaceCore, func(fx *namespaceMatrixFixture) []matrixProbe {
			probes := []matrixProbe{probe("default job trigger", "/v1/triggers/"+fx.n(nsDefault).JobTrigger, http.StatusOK, needEvery(rV, nsDefault))}
			for _, n := range fx.pair() {
				probes = append(probes, probe(n.Name+" http trigger", "/v1/triggers/"+n.HTTPTrigger, http.StatusOK, needEvery(rV, n.Name)))
			}
			return probes
		}},
		"GET /v1/triggers/:id/events": {auth.RouteClassNamespaced, rV, surfaceCore, func(fx *namespaceMatrixFixture) []matrixProbe {
			var probes []matrixProbe
			for _, n := range fx.pair() {
				other := fx.n(nsFinance)
				if n.Name == nsFinance {
					other = fx.n(nsMarketing)
				}
				probes = append(probes, probe(n.Name+" event trigger", "/v1/triggers/"+n.EventTrigger+"/events", http.StatusOK, needEvery(rV, n.Name)).marks(mark(n.EventID, n.Name), never(other.EventID)))
			}
			return probes
		}},
		"PATCH /v1/triggers/:id": {auth.RouteClassNamespaced, rO, surfaceCore, func(fx *namespaceMatrixFixture) []matrixProbe {
			var probes []matrixProbe
			for _, n := range fx.pair() {
				probes = append(probes, probe(n.Name+" http trigger", "/v1/triggers/"+n.HTTPTrigger, http.StatusOK, needEvery(rO, n.Name)).idem().body(`{}`))
			}
			return probes
		}},
		"POST /v1/triggers/:id/fire": {auth.RouteClassNamespaced, rR, surfaceCore, func(fx *namespaceMatrixFixture) []matrixProbe {
			var probes []matrixProbe
			for _, n := range fx.pair() {
				probes = append(probes, probe(n.Name+" http trigger", "/v1/triggers/"+n.HTTPTrigger+"/fire", http.StatusAccepted, needEvery(rR, n.Name)).alias(n.HTTPJob.Alias).once())
			}
			return probes
		}},
		"POST /v1/events": {auth.RouteClassNamespaced, rR, surfaceCore, func(fx *namespaceMatrixFixture) []matrixProbe {
			// Runner in any namespace; delivery is confined by the handler.
			// No job listens for this type, so nothing runs.
			body := fmt.Sprintf(`{"type":%q,"source":"mx"}`, fx.name("unrouted"))
			return []matrixProbe{probe("unrouted event", "/v1/events", http.StatusAccepted, needAnyGrant(rR, models.ClusterNamespace)).once().body(body)}
		}},

		// --- namespaced: notifications and agent profiles ---
		"GET /v1/notifications/channels/:id": {auth.RouteClassNamespaced, rV, surfaceCore, func(fx *namespaceMatrixFixture) []matrixProbe {
			return fx.sharedObjectReads("/v1/notifications/channels/", func(n *matrixNamespace) string { return n.Channel }, fx.SharedChannel)
		}},
		"GET /v1/notifications/policies/:id": {auth.RouteClassNamespaced, rV, surfaceCore, func(fx *namespaceMatrixFixture) []matrixProbe {
			return fx.sharedObjectReads("/v1/notifications/policies/", func(n *matrixNamespace) string { return n.Policy }, fx.SharedPolicy)
		}},
		"GET /v1/agentprofiles/:id": {auth.RouteClassNamespaced, rV, surfaceCore, func(fx *namespaceMatrixFixture) []matrixProbe {
			return fx.sharedObjectReads("/v1/agentprofiles/", func(n *matrixNamespace) string { return n.Profile }, fx.SharedProfile)
		}},
		"PATCH /v1/notifications/channels/:id": {auth.RouteClassNamespaced, rO, surfaceCore, func(fx *namespaceMatrixFixture) []matrixProbe {
			return fx.sharedObjectWrites("/v1/notifications/channels/", func(n *matrixNamespace) string { return n.Channel }, fx.SharedChannel, `{"enabled":true}`)
		}},
		"PATCH /v1/notifications/policies/:id": {auth.RouteClassNamespaced, rO, surfaceCore, func(fx *namespaceMatrixFixture) []matrixProbe {
			return fx.sharedObjectWrites("/v1/notifications/policies/", func(n *matrixNamespace) string { return n.Policy }, fx.SharedPolicy, `{"enabled":true}`)
		}},
		"PATCH /v1/agentprofiles/:id": {auth.RouteClassNamespaced, rO, surfaceCore, func(fx *namespaceMatrixFixture) []matrixProbe {
			return fx.sharedObjectWrites("/v1/agentprofiles/", func(n *matrixNamespace) string { return n.Profile }, fx.SharedProfile, `{"image":"busybox:1.36.1"}`)
		}},
		"DELETE /v1/notifications/channels/:id": {auth.RouteClassNamespaced, rO, surfaceCore, func(fx *namespaceMatrixFixture) []matrixProbe {
			return fx.victimDeletes("/v1/notifications/channels/", func(n *matrixNamespace) string { return n.VictimChannel })
		}},
		"DELETE /v1/notifications/policies/:id": {auth.RouteClassNamespaced, rO, surfaceCore, func(fx *namespaceMatrixFixture) []matrixProbe {
			return fx.victimDeletes("/v1/notifications/policies/", func(n *matrixNamespace) string { return n.VictimPolicy })
		}},
		"DELETE /v1/agentprofiles/:id": {auth.RouteClassNamespaced, rO, surfaceCore, func(fx *namespaceMatrixFixture) []matrixProbe {
			return fx.victimDeletes("/v1/agentprofiles/", func(n *matrixNamespace) string { return n.VictimProfile })
		}},
		"POST /v1/notifications/channels": {auth.RouteClassNamespaced, rO, surfaceCore, func(fx *namespaceMatrixFixture) []matrixProbe {
			return fx.creates("/v1/notifications/channels", func(ns string) string { return fx.channelBody(ns, "created-ch-"+strings.ReplaceAll(ns, "*", "shared")) })
		}},
		"POST /v1/notifications/policies": {auth.RouteClassNamespaced, rO, surfaceCore, func(fx *namespaceMatrixFixture) []matrixProbe {
			return fx.creates("/v1/notifications/policies", func(ns string) string {
				channel := fx.SharedChannel
				if n := fx.n(ns); n != nil {
					channel = n.Channel
				}
				return fx.policyBody(ns, channel, "created-pol-"+strings.ReplaceAll(ns, "*", "shared"))
			})
		}},
		"POST /v1/agentprofiles": {auth.RouteClassNamespaced, rO, surfaceCore, func(fx *namespaceMatrixFixture) []matrixProbe {
			return fx.creates("/v1/agentprofiles", func(ns string) string {
				return fx.profileBody(ns, "created-prof-"+strings.ReplaceAll(ns, "*", "shared"))
			})
		}},

		// --- namespaced: incidents and the agent surface ---
		"GET /v1/incidents/:id": {auth.RouteClassNamespaced, rV, surfaceIncidents, func(fx *namespaceMatrixFixture) []matrixProbe {
			return fx.incidentRoute(rV, "/v1/incidents/%s", http.StatusOK)
		}},
		"POST /v1/incidents/:id/approvals/:id/approve": {auth.RouteClassNamespaced, rO, surfaceIncidents, func(fx *namespaceMatrixFixture) []matrixProbe {
			return each(fx.incidentRoute(rO, "/v1/incidents/%s/approvals/"+missingID+"/approve", http.StatusNotFound), func(p matrixProbe) matrixProbe { return p.idem().body(`{"reason":"mx"}`) })
		}},
		"POST /v1/incidents/:id/approvals/:id/reject": {auth.RouteClassNamespaced, rO, surfaceIncidents, func(fx *namespaceMatrixFixture) []matrixProbe {
			return each(fx.incidentRoute(rO, "/v1/incidents/%s/approvals/"+missingID+"/reject", http.StatusNotFound), func(p matrixProbe) matrixProbe { return p.idem().body(`{"reason":"mx"}`) })
		}},
		"GET /v1/agent/incidents/:id/bundle": {auth.RouteClassNamespaced, rV, surfaceIncidents, func(fx *namespaceMatrixFixture) []matrixProbe {
			return fx.incidentRoute(rV, "/v1/agent/incidents/%s/bundle", http.StatusOK)
		}},
		"GET /v1/agent/incidents/:id/context/*": {auth.RouteClassNamespaced, rV, surfaceIncidents, func(fx *namespaceMatrixFixture) []matrixProbe {
			return fx.incidentRoute(rV, "/v1/agent/incidents/%s/context/history", http.StatusOK)
		}},
		"POST /v1/agent/incidents/:id/actions": {auth.RouteClassNamespaced, rR, surfaceIncidents, func(fx *namespaceMatrixFixture) []matrixProbe {
			// An empty proposal is refused by validation: nothing is recorded.
			return each(fx.incidentRoute(rR, "/v1/agent/incidents/%s/actions", http.StatusBadRequest), func(p matrixProbe) matrixProbe { return p.idem().body(`{}`) })
		}},
		"POST /v1/agent/incidents/:id/notes": {auth.RouteClassNamespaced, rR, surfaceIncidents, func(fx *namespaceMatrixFixture) []matrixProbe {
			return each(fx.incidentRoute(rR, "/v1/agent/incidents/%s/notes", http.StatusCreated), func(p matrixProbe) matrixProbe { return p.idem().body(`{"text":"mx note"}`) })
		}},
		"POST /v1/agent/incidents/:id/mcp": {auth.RouteClassNamespaced, rR, surfaceIncidents, func(fx *namespaceMatrixFixture) []matrixProbe {
			return each(fx.incidentRoute(rR, "/v1/agent/incidents/%s/mcp", http.StatusOK), func(p matrixProbe) matrixProbe {
				return p.idem().body(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
			})
		}},

		// --- namespaced: datasets ---
		"GET /v1/datasets/:id/:id": {auth.RouteClassNamespaced, rV, surfaceCore, func(fx *namespaceMatrixFixture) []matrixProbe {
			return fx.datasetReads("")
		}},
		"GET /v1/datasets/:id/:id/derivations": {auth.RouteClassNamespaced, rV, surfaceCore, func(fx *namespaceMatrixFixture) []matrixProbe {
			return fx.datasetReads("/derivations")
		}},
		"GET /v1/datasets/:id/:id/metrics": {auth.RouteClassNamespaced, rV, surfaceDataAssertions, func(fx *namespaceMatrixFixture) []matrixProbe {
			return fx.datasetReads("/metrics")
		}},
		"POST /v1/datasets/:id/:id/advance": {auth.RouteClassNamespaced, rR, surfaceCore, func(fx *namespaceMatrixFixture) []matrixProbe {
			// The manual watermark advance needs runner in EVERY producer.
			m := fx.n(nsMarketing)
			body := `{"watermark":"2026-10-15"}`
			return []matrixProbe{
				probe("marketing dataset", fx.datasetPath(m.Dataset, "/advance"), http.StatusOK, needEvery(rR, nsMarketing)).idem().body(body),
				probe("dataset both produce", fx.datasetPath(fx.BothDataset, "/advance"), http.StatusOK, needEvery(rR, nsMarketing, nsFinance)).idem().body(body),
			}
		}},
		"POST /v1/datasets/holds/:id/release": {auth.RouteClassNamespaced, rO, surfaceDataAssertions, func(fx *namespaceMatrixFixture) []matrixProbe {
			body := `{"reason":"mx namespace matrix"}`
			var probes []matrixProbe
			for _, n := range fx.pair() {
				probes = append(probes, probe(n.Name+" hold", "/v1/datasets/holds/"+n.Hold+"/release", http.StatusOK, needEvery(rO, n.Name)).once().body(body))
			}
			return append(probes, probe("hold on a dataset both produce", "/v1/datasets/holds/"+fx.BothHold+"/release", http.StatusOK, needEvery(rO, nsMarketing, nsFinance)).once().body(body))
		}},

		// --- namespaced: keys and audit ---
		"POST /v1/auth/keys": {auth.RouteClassNamespaced, rA, surfaceCore, func(fx *namespaceMatrixFixture) []matrixProbe {
			var probes []matrixProbe
			for _, n := range fx.pair() {
				p := probe(n.Name+" key", "/v1/auth/keys", http.StatusCreated, needEvery(rA, n.Name)).once().
					body(fmt.Sprintf(`{"role":"viewer","description":"mx created %s","namespaces":[%q]}`, n.Name, n.Name))
				p.Check = fx.revokeCreatedKey
				probes = append(probes, p)
			}
			p := probe("cluster key", "/v1/auth/keys", http.StatusCreated, needEvery(rA, models.ClusterNamespace)).once().body(`{"role":"viewer","description":"mx created cluster"}`)
			p.Check = fx.revokeCreatedKey
			return append(probes, p)
		}},
		"POST /v1/auth/keys/:id/revoke": {auth.RouteClassNamespaced, rA, surfaceCore, func(fx *namespaceMatrixFixture) []matrixProbe {
			return fx.keyWrites("revoke", func(n *matrixNamespace) string { return n.RevokeKey }, fx.ClusterRevokeKey)
		}},
		"POST /v1/auth/keys/:id/rotate": {auth.RouteClassNamespaced, rA, surfaceCore, func(fx *namespaceMatrixFixture) []matrixProbe {
			return fx.keyWrites("rotate", func(n *matrixNamespace) string { return n.RotateKey }, fx.ClusterRotateKey)
		}},
		"GET /v1/auth/audit": {auth.RouteClassNamespaced, rA, surfaceCore, func(fx *namespaceMatrixFixture) []matrixProbe {
			return []matrixProbe{
				probe("marketing rows", "/v1/auth/audit?limit=1&namespace=marketing", http.StatusOK, needEvery(rA, nsMarketing)),
				probe("finance rows", "/v1/auth/audit?limit=1&namespace=finance", http.StatusOK, needEvery(rA, nsFinance)),
				probe("every row", "/v1/auth/audit?limit=1", http.StatusOK, needEvery(rA, models.ClusterNamespace)),
			}
		}},
	}
}

func (p matrixProbe) withCheck(check func(matrixPrincipal, string)) matrixProbe {
	p.Check = check
	return p
}

// sharedObjectReads: a namespaced object needs viewer in its namespace; a
// shared ("*") object needs viewer in any namespace.
func (fx *namespaceMatrixFixture) sharedObjectReads(prefix string, id func(*matrixNamespace) string, shared string) []matrixProbe {
	var probes []matrixProbe
	for _, n := range fx.pair() {
		probes = append(probes, probe(n.Name+" object", prefix+id(n), http.StatusOK, needEvery(rV, n.Name)))
	}
	return append(probes, probe("shared object", prefix+shared, http.StatusOK, needAnyGrant(rV, models.ClusterNamespace)))
}

// sharedObjectWrites: writing a shared object needs the role at "*".
func (fx *namespaceMatrixFixture) sharedObjectWrites(prefix string, id func(*matrixNamespace) string, shared, body string) []matrixProbe {
	var probes []matrixProbe
	for _, n := range fx.pair() {
		probes = append(probes, probe(n.Name+" object", prefix+id(n), http.StatusOK, needEvery(rO, n.Name)).idem().body(body))
	}
	return append(probes, probe("shared object", prefix+shared, http.StatusOK, needEvery(rO, models.ClusterNamespace)).idem().body(body))
}

func (fx *namespaceMatrixFixture) victimDeletes(prefix string, id func(*matrixNamespace) string) []matrixProbe {
	var probes []matrixProbe
	for _, n := range fx.pair() {
		probes = append(probes, probe(n.Name+" victim", prefix+id(n), http.StatusNoContent, needEvery(rO, n.Name)).once())
	}
	return probes
}

// creates posts one object per namespace and one shared object; each admitted
// create is deleted again on close.
func (fx *namespaceMatrixFixture) creates(path string, body func(namespace string) string) []matrixProbe {
	var probes []matrixProbe
	for _, ns := range []string{nsMarketing, nsFinance, models.ClusterNamespace} {
		p := probe(ns+" create", path, http.StatusCreated, needEvery(rO, ns)).once().body(body(ns))
		p.Check = fx.cleanupCreated(path)
		probes = append(probes, p)
	}
	return probes
}

// failMarks are the failing jobs' aliases (only when the lane opens
// incidents, which is when the fixture creates them).
func (fx *namespaceMatrixFixture) failMarks() []matrixMarker {
	var marks []matrixMarker
	for _, n := range fx.pair() {
		if n.FailJob != nil {
			marks = append(marks, optMark(n.FailJob.Alias, n.Name))
		}
	}
	return marks
}

// incidentOrMissing is the namespace's incident, or an id no incident holds
// on a lane without incidents (the route is then probed as unmounted).
func (n *matrixNamespace) incidentOrMissing() string {
	if n.Incident == "" {
		return uuid.NewString()
	}
	return n.Incident
}

func (fx *namespaceMatrixFixture) incidentRoute(role models.Role, format string, admit int) []matrixProbe {
	var probes []matrixProbe
	for _, n := range fx.pair() {
		p := probe(n.Name+" incident", fmt.Sprintf(format, n.incidentOrMissing()), admit, needEvery(role, n.Name))
		p.Incident = n.Incident
		probes = append(probes, p)
	}
	return probes
}

func (fx *namespaceMatrixFixture) datasetPath(name, suffix string) string {
	return "/v1/datasets/_/" + url.PathEscape(name) + suffix
}

// datasetReads: a dataset read needs viewer in ANY declared producer's
// namespace; a dataset nobody declares is cluster-only.
func (fx *namespaceMatrixFixture) datasetReads(suffix string) []matrixProbe {
	m, f := fx.n(nsMarketing), fx.n(nsFinance)
	return []matrixProbe{
		probe("marketing dataset", fx.datasetPath(m.Dataset, suffix), http.StatusOK, needAny(rV, nsMarketing)),
		probe("finance dataset", fx.datasetPath(f.Dataset, suffix), http.StatusOK, needAny(rV, nsFinance)),
		probe("dataset both produce", fx.datasetPath(fx.BothDataset, suffix), http.StatusOK, needAny(rV, nsMarketing, nsFinance)),
		probe("undeclared dataset", fx.datasetPath(fx.OrphanDataset, suffix), http.StatusNotFound, needAny(rV)),
	}
}

func (fx *namespaceMatrixFixture) keyWrites(verb string, id func(*matrixNamespace) string, cluster string) []matrixProbe {
	var probes []matrixProbe
	for _, n := range fx.pair() {
		probes = append(probes, probe(n.Name+" key", "/v1/auth/keys/"+id(n)+"/"+verb, http.StatusOK, needEvery(rA, n.Name)).once())
	}
	return append(probes, probe("cluster key", "/v1/auth/keys/"+cluster+"/"+verb, http.StatusOK, needEvery(rA, models.ClusterNamespace)).once())
}

// revokeCreatedKey revokes a key an admitted POST /v1/auth/keys minted.
func (fx *namespaceMatrixFixture) revokeCreatedKey(_ matrixPrincipal, body string) {
	var created struct {
		APIKey struct {
			ID string `json:"id"`
		} `json:"api_key"`
	}
	if fx.s.NoError(json.Unmarshal([]byte(body), &created), body) && created.APIKey.ID != "" {
		fx.mu.Lock()
		fx.f.keys = append(fx.f.keys, created.APIKey.ID)
		fx.mu.Unlock()
	}
}
