package middleware

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/authz"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"gorm.io/gorm"
)

// Namespace resolution for `namespaced` routes (identity-and-access design
// §8.1, plan item A3). Every non-pending namespaced route in
// internal/auth's endpointPolicy has exactly one resolver here, keyed by the
// same "METHOD /normalised/path"; api/auth_rbac_policy_completeness_test.go
// fails the build when one is missing, and the middleware denies a request on
// such a route at runtime. Later items (B4, B6, A5, D4, ...) extend this table
// rather than adding a second resolution path.
//
// A resolver reads the SAME row the handler will act on, with the same lookup
// (uuid.Parse for the routes whose handler uses it, models.FindJobByIDPrefix
// for the routes that accept a job id prefix), and maps a missing row to 404,
// an unparseable id to the handler's 400, and a lookup error to 500. It runs
// only for principals whose cluster role is below the route's role.
//
// Residual risk (accepted in A3; the design's hybrid enforcement): the
// resolver reads the target's namespace before the handler's own transaction,
// so a concurrent move of that exact target by a principal holding operator in
// both namespaces (an apply) can land in between, and the handler then acts on
// the moved row. A confined caller cannot cause the move, and the middleware
// reads the whole request body before deciding, so the window is server-side
// only. Run admission and replay re-check the grant inside their transactions
// (runstorage.WithStartNamespaceGuard, replaysvc.Request.NamespaceGuard), as
// do retries and backfills (B4) and job-definition apply, the legacy create,
// trigger PATCH and the manifest export (B9).

// nsRequirement is one namespace check: the principal needs Role in every
// namespace (or, with Any, in at least one). An empty Namespaces list can never
// be satisfied, which is how "no owner ⇒ cluster-only" fails closed.
// AnyGrant is satisfied by Role in any namespace at all (reading a "*"
// cluster-shared object).
// Disclose names the namespaces the principal lacks in the 403 body; it is
// set only where the caller named them (or an alias living there) itself.
type nsRequirement struct {
	Namespaces []string
	Role       models.Role
	Any        bool
	AnyGrant   bool
	Disclose   bool
}

type namespaceResolver func(c *echo.Context, svc *auth.Service, role models.Role) ([]nsRequirement, error)

// namespaceBodyPeekLimit caps how much of a request body the middleware reads
// to find a create body's namespace. Shared-object bodies are small; a larger
// body from a confined principal is refused with 413 rather than buffered.
const namespaceBodyPeekLimit = 1 << 20

var namespaceResolvers = map[string]namespaceResolver{
	// Jobs by id (uuid.Parse, like their handlers).
	"GET /v1/jobs/:id/tasks":            resolveJobByID,
	"GET /v1/jobs/:id/dag":              resolveJobByID,
	"GET /v1/jobs/:id/queue":            resolveJobByID,
	"GET /v1/jobs/:id/runs":             resolveJobByID,
	"GET /v1/jobs/:id/runs/diff":        resolveJobRunPair,
	"GET /v1/jobs/:id/blame":            resolveJobByID,
	"GET /v1/jobs/:id/topology":         resolveJobByID,
	"GET /v1/jobs/:id/topology/history": resolveJobByID,
	"GET /v1/jobs/:id/cache":            resolveJobByID,
	"GET /v1/jobs/:id/backfills":        resolveJobByID,
	"DELETE /v1/jobs/:id":               resolveJobByID,
	"PUT /v1/jobs/:id/pause":            resolveJobByID,
	"PUT /v1/jobs/:id/unpause":          resolveJobByID,
	"POST /v1/jobs/:id/run":             resolveJobByID,
	"POST /v1/jobs/:id/backfill":        resolveJobByID,
	"DELETE /v1/jobs/:id/queue/:id":     resolveJobByID, // queue entries run in the job's namespace
	"DELETE /v1/jobs/:id/cache":         resolveJobByID,
	"DELETE /v1/jobs/:id/cache/:id":     resolveJobByID,

	// Jobs by id or unique id prefix (models.FindJobByIDPrefix, like get.go
	// and manifest.go).
	"GET /v1/jobs/:id":          resolveJobByIDPrefix,
	"GET /v1/jobs/:id/manifest": resolveJobByIDPrefix,

	// Job definitions read against the catalog, or applied to it: every
	// definition's namespace and the stored namespace of every alias the
	// request names (an apply that moves a job needs operator in both). The
	// apply re-checks both inside its transaction, and prunes only in the
	// namespaces where the caller is operator (B9).
	"POST /v1/jobdefs/lint":  resolveJobdefRead,
	"POST /v1/jobdefs/diff":  resolveJobdefRead,
	"POST /v1/jobdefs/apply": resolveJobdefRead,
	// The legacy create: the body's metadata.namespace and the stored
	// namespace of its alias, re-checked inside the create's transaction.
	"POST /v1/jobs": resolveJobCreate,

	// One run: the job's current namespace AND the run's persisted namespace.
	"GET /v1/jobs/:id/runs/:id":                                 resolveJobRun,
	"GET /v1/jobs/:id/runs/:id/logs":                            resolveJobRun,
	"GET /v1/jobs/:id/runs/:id/why":                             resolveJobRun,
	"GET /v1/jobs/:id/runs/:id/receipt":                         resolveJobRun,
	"POST /v1/jobs/:id/runs/:id/receipt/verify":                 resolveJobRun,
	"GET /v1/jobs/:id/runs/:id/tasks/:id/descriptor":            resolveJobRun,
	"GET /v1/jobs/:id/runs/:id/tasks/:id/partitions":            resolveJobRun,
	"GET /v1/jobs/:id/runs/:id/external-executions":             resolveJobRun,
	"POST /v1/jobs/:id/runs/:id/retry":                          resolveJobRun,
	"POST /v1/jobs/:id/runs/:id/tasks/:id/partitions/:id/retry": resolveJobRun,
	"POST /v1/jobs/:id/runs/:id/callbacks/retry":                resolveJobRun,
	// Replay executes in the job's current namespace (runner there) from a
	// baseline the caller must be able to read (viewer in its namespace).
	"POST /v1/jobs/:id/runs/:id/replay": resolveJobRunReplay,

	// One backfill: the job's namespace AND the backfill's.
	"GET /v1/jobs/:id/backfills/:id":        resolveJobBackfill,
	"PUT /v1/jobs/:id/backfills/:id/cancel": resolveJobBackfill,

	// Triggers: every job the trigger serves.
	"GET /v1/triggers/:id":        resolveTrigger,
	"PATCH /v1/triggers/:id":      resolveTrigger,
	"POST /v1/triggers/:id/fire":  resolveTrigger,
	"GET /v1/triggers/:id/events": resolveTrigger,

	// Incidents: incidents.namespace.
	"GET /v1/incidents/:id":                        resolveIncident,
	"POST /v1/incidents/:id/approvals/:id/approve": resolveIncident,
	"POST /v1/incidents/:id/approvals/:id/reject":  resolveIncident,
	"GET /v1/agent/incidents/:id/bundle":           resolveIncident, // pending A8
	"GET /v1/agent/incidents/:id/context/*":        resolveIncident, // pending A8
	"POST /v1/agent/incidents/:id/actions":         resolveIncident, // pending A8
	"POST /v1/agent/incidents/:id/notes":           resolveIncident, // pending A8
	"POST /v1/agent/incidents/:id/mcp":             resolveIncident, // pending A8

	// Shared objects: the object's namespace; "*" needs the role at "*" to
	// write and viewer anywhere to read. Creates use the body namespace.
	"GET /v1/notifications/channels/:id":    resolveSharedObject(auth.SharedObjectNotificationChannel),
	"PATCH /v1/notifications/channels/:id":  resolveSharedObject(auth.SharedObjectNotificationChannel),
	"DELETE /v1/notifications/channels/:id": resolveSharedObject(auth.SharedObjectNotificationChannel),
	"GET /v1/notifications/policies/:id":    resolveSharedObject(auth.SharedObjectNotificationPolicy),
	"PATCH /v1/notifications/policies/:id":  resolveSharedObject(auth.SharedObjectNotificationPolicy),
	"DELETE /v1/notifications/policies/:id": resolveSharedObject(auth.SharedObjectNotificationPolicy),
	"GET /v1/agentprofiles/:id":             resolveSharedObject(auth.SharedObjectAgentProfile),
	"PATCH /v1/agentprofiles/:id":           resolveSharedObject(auth.SharedObjectAgentProfile),
	"DELETE /v1/agentprofiles/:id":          resolveSharedObject(auth.SharedObjectAgentProfile),
	"POST /v1/notifications/channels":       resolveCreateBodyNamespace,
	"POST /v1/notifications/policies":       resolveCreateBodyNamespace,
	"POST /v1/agentprofiles":                resolveCreateBodyNamespace,

	// Datasets: the declared producers' namespaces — any of them for a read,
	// every one for a write. No declared producer ⇒ cluster-only (a consumer
	// or an external-source declaration owns nothing).
	"GET /v1/datasets/:id/:id":             resolveDatasetAny, // pending B6
	"GET /v1/datasets/:id/:id/derivations": resolveDatasetAny, // pending B6
	"GET /v1/datasets/:id/:id/metrics":     resolveDatasetAny, // pending B6
	"POST /v1/datasets/:id/:id/advance":    resolveDatasetEvery,
	"POST /v1/datasets/holds/:id/release":  resolveDatasetHold,

	// The audit log authorises on ?namespace= (omitted ⇒ "*").
	"GET /v1/auth/audit": resolveAuditNamespace,
	// API keys (A5): minting needs the role in every namespace the body asks
	// for ("*" when it names none); revoke and rotate need it in every
	// namespace the stored key is confined to (NULL, and an unusable value,
	// ⇒ "*": only a cluster principal may act).
	"POST /v1/auth/keys":            resolveKeyCreateNamespaces,
	"POST /v1/auth/keys/:id/revoke": resolveStoredKeyNamespaces,
	"POST /v1/auth/keys/:id/rotate": resolveStoredKeyNamespaces,
}

// HasNamespaceResolver reports whether the namespaced route identified by
// policyKey ("METHOD /normalised/path") has a resolver. The route completeness
// test uses it.
func HasNamespaceResolver(policyKey string) bool {
	_, ok := namespaceResolvers[policyKey]
	return ok
}

// lookupError maps a namespace lookup failure to the handler's status: a
// missing row is 404 (as authorizeScope answers today), anything else 500.
func lookupError(err error) error {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return echo.ErrNotFound
	}
	return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
}

func badRequest(err error) error {
	return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
}

func pathUUID(c *echo.Context, name string) (uuid.UUID, error) {
	id, err := uuid.Parse(c.Param(name))
	if err != nil {
		return uuid.Nil, badRequest(err)
	}
	return id, nil
}

func every(role models.Role, namespaces ...string) nsRequirement {
	return nsRequirement{Namespaces: namespaces, Role: role}
}

func resolveJobByID(c *echo.Context, svc *auth.Service, role models.Role) ([]nsRequirement, error) {
	jobID, err := pathUUID(c, "id")
	if err != nil {
		return nil, err
	}
	namespace, err := svc.JobNamespace(c.Request().Context(), jobID)
	if err != nil {
		return nil, lookupError(err)
	}
	return []nsRequirement{every(role, namespace)}, nil
}

func resolveJobByIDPrefix(c *echo.Context, svc *auth.Service, role models.Role) ([]nsRequirement, error) {
	_, namespace, err := svc.JobNamespaceByIDPrefix(c.Request().Context(), c.Param("id"))
	if err != nil {
		switch {
		case errors.Is(err, models.ErrInvalidJobIDPrefix):
			return nil, echo.NewHTTPError(http.StatusBadRequest, err.Error()).Wrap(err)
		case errors.Is(err, models.ErrAmbiguousJobIDPrefix):
			return nil, echo.NewHTTPError(http.StatusConflict, err.Error()).Wrap(err)
		}
		return nil, lookupError(err)
	}
	return []nsRequirement{every(role, namespace)}, nil
}

// jobdefReadBody is the part of a lint or diff body (controller/jobdef
// LintRequest, DiffRequest: definitions of pkg/jobdef.Definition) the
// resolver needs. It decodes with the same field names and encoding/json
// rules, so the handler's c.Bind sees the same aliases and namespaces
// (auth_namespace_jobdef_test.go pins the parity).
type jobdefReadBody struct {
	Definitions []struct {
		Metadata jobdefReadMetadata `json:"metadata"`
	} `json:"definitions"`
}

// jobdefReadMetadata decodes like pkg/jobdef.Metadata: its UnmarshalJSON
// replaces the whole value, so a repeated "metadata" key (or a null one)
// resets the fields a previous occurrence set rather than merging them.
type jobdefReadMetadata struct {
	Alias     string `json:"alias"`
	Namespace string `json:"namespace"`
}

func (m *jobdefReadMetadata) UnmarshalJSON(data []byte) error {
	type plain jobdefReadMetadata
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*m = jobdefReadMetadata(decoded)
	return nil
}

// resolveJobdefRead resolves POST /v1/jobdefs/lint, /diff and /apply: the
// role in every definition's namespace (blank means default) and in the
// stored namespace of every alias the request names that already has a jobs
// row, so a caller cannot lint, diff or apply against another namespace's job
// by naming its alias (for apply, a move needs operator in both namespaces).
// A request with no definitions names no namespace: any grant of the role
// passes, and the handler returns only the caller's namespaces' rows (apply
// refuses it with 400).
func resolveJobdefRead(c *echo.Context, svc *auth.Service, role models.Role) ([]nsRequirement, error) {
	var body jobdefReadBody
	if err := peekJSONBody(c, confinedBodyBufferLimit, &body); err != nil {
		return nil, err
	}
	if len(body.Definitions) == 0 {
		return []nsRequirement{{Role: role, AnyGrant: true}}, nil
	}
	namespaces := make([]string, 0, len(body.Definitions))
	aliases := make([]string, 0, len(body.Definitions))
	for _, def := range body.Definitions {
		namespaces = append(namespaces, models.NamespaceOrDefault(def.Metadata.Namespace))
		aliases = append(aliases, def.Metadata.Alias)
	}
	stored, err := svc.JobNamespacesByAlias(c.Request().Context(), aliases)
	if err != nil {
		return nil, lookupError(err)
	}
	requirement := every(role, append(namespaces, stored...)...)
	requirement.Disclose = true
	return []nsRequirement{requirement}, nil
}

// jobCreateBody is the part of a legacy create body (controller/job
// PostRequest) the resolver needs, decoded with the same field names and
// shapes (a pointer metadata object), so the handler's c.Bind sees the same
// alias and namespace.
type jobCreateBody struct {
	Alias    string `json:"alias"`
	Metadata *struct {
		Namespace string `json:"namespace"`
	} `json:"metadata"`
}

// resolveJobCreate resolves POST /v1/jobs: the role in the body's namespace
// (blank means default) and in the stored namespace of its alias, if it
// already has a jobs row (retired rows included). An invalid namespace is the
// handler's 400.
func resolveJobCreate(c *echo.Context, svc *auth.Service, role models.Role) ([]nsRequirement, error) {
	var body jobCreateBody
	if err := peekJSONBody(c, confinedBodyBufferLimit, &body); err != nil {
		return nil, err
	}
	namespace := ""
	if body.Metadata != nil {
		namespace = body.Metadata.Namespace
	}
	if err := jobdef.ValidateNamespace(namespace); err != nil {
		return nil, echo.NewHTTPError(http.StatusBadRequest, err.Error()).Wrap(err)
	}
	stored, err := svc.JobNamespacesByAlias(c.Request().Context(), []string{body.Alias})
	if err != nil {
		return nil, lookupError(err)
	}
	requirement := every(role, append([]string{models.NamespaceOrDefault(namespace)}, stored...)...)
	requirement.Disclose = true
	return []nsRequirement{requirement}, nil
}

// jobAndRunNamespaces resolves the job in the path and the run in the path,
// and refuses (404) a run that does not belong to that job.
func jobAndRunNamespaces(c *echo.Context, svc *auth.Service) (string, string, error) {
	jobID, err := pathUUID(c, "id")
	if err != nil {
		return "", "", err
	}
	runID, err := pathUUID(c, "run_id")
	if err != nil {
		return "", "", err
	}
	ctx := c.Request().Context()
	jobNamespace, err := svc.JobNamespace(ctx, jobID)
	if err != nil {
		return "", "", lookupError(err)
	}
	runJobID, runNamespace, err := svc.RunNamespace(ctx, runID)
	if err != nil {
		return "", "", lookupError(err)
	}
	if runJobID != jobID {
		return "", "", echo.ErrNotFound
	}
	return jobNamespace, runNamespace, nil
}

func resolveJobRun(c *echo.Context, svc *auth.Service, role models.Role) ([]nsRequirement, error) {
	jobNamespace, runNamespace, err := jobAndRunNamespaces(c, svc)
	if err != nil {
		return nil, err
	}
	return []nsRequirement{every(role, jobNamespace, runNamespace)}, nil
}

// resolveJobRunPair resolves GET /v1/jobs/:id/runs/diff: the job in the path
// and the two runs named by ?left= and ?right=, parsed and checked exactly as
// the handler does (a missing or unparseable id is the handler's 400, a run of
// another job its 404). The caller needs the role in the job's current
// namespace and in both runs' persisted namespaces, like a single-run read.
func resolveJobRunPair(c *echo.Context, svc *auth.Service, role models.Role) ([]nsRequirement, error) {
	jobID, err := pathUUID(c, "id")
	if err != nil {
		return nil, err
	}
	leftRaw, rightRaw := c.QueryParam("left"), c.QueryParam("right")
	if leftRaw == "" || rightRaw == "" {
		return nil, echo.NewHTTPError(http.StatusBadRequest, "left and right query parameters are required")
	}
	runIDs := make([]uuid.UUID, 0, 2)
	for _, raw := range []string{leftRaw, rightRaw} {
		id, err := uuid.Parse(raw)
		if err != nil {
			return nil, badRequest(err)
		}
		runIDs = append(runIDs, id)
	}
	ctx := c.Request().Context()
	jobNamespace, err := svc.JobNamespace(ctx, jobID)
	if err != nil {
		return nil, lookupError(err)
	}
	namespaces := []string{jobNamespace}
	for _, runID := range runIDs {
		runJobID, runNamespace, err := svc.RunNamespace(ctx, runID)
		if err != nil {
			return nil, lookupError(err)
		}
		if runJobID != jobID {
			return nil, echo.ErrNotFound
		}
		namespaces = append(namespaces, runNamespace)
	}
	return []nsRequirement{every(role, namespaces...)}, nil
}

func resolveJobRunReplay(c *echo.Context, svc *auth.Service, role models.Role) ([]nsRequirement, error) {
	jobNamespace, runNamespace, err := jobAndRunNamespaces(c, svc)
	if err != nil {
		return nil, err
	}
	return []nsRequirement{
		every(role, jobNamespace),
		every(models.RoleViewer, runNamespace),
	}, nil
}

func resolveJobBackfill(c *echo.Context, svc *auth.Service, role models.Role) ([]nsRequirement, error) {
	jobID, err := pathUUID(c, "id")
	if err != nil {
		return nil, err
	}
	backfillID, err := pathUUID(c, "backfill_id")
	if err != nil {
		return nil, err
	}
	ctx := c.Request().Context()
	jobNamespace, err := svc.JobNamespace(ctx, jobID)
	if err != nil {
		return nil, lookupError(err)
	}
	backfillJobID, backfillNamespace, err := svc.BackfillNamespace(ctx, backfillID)
	if err != nil {
		return nil, lookupError(err)
	}
	if backfillJobID != jobID {
		return nil, echo.ErrNotFound
	}
	return []nsRequirement{every(role, jobNamespace, backfillNamespace)}, nil
}

func resolveTrigger(c *echo.Context, svc *auth.Service, role models.Role) ([]nsRequirement, error) {
	triggerID, err := pathUUID(c, "id")
	if err != nil {
		return nil, err
	}
	namespaces, err := svc.TriggerJobNamespaces(c.Request().Context(), triggerID)
	if err != nil {
		return nil, lookupError(err)
	}
	// A trigger that serves no job has no owner: empty ⇒ cluster-only.
	return []nsRequirement{every(role, namespaces...)}, nil
}

func resolveIncident(c *echo.Context, svc *auth.Service, role models.Role) ([]nsRequirement, error) {
	incidentID, err := pathUUID(c, "id")
	if err != nil {
		return nil, err
	}
	namespace, err := svc.IncidentNamespace(c.Request().Context(), incidentID)
	if err != nil {
		return nil, lookupError(err)
	}
	return []nsRequirement{every(role, namespace)}, nil
}

func resolveSharedObject(kind auth.SharedObjectKind) namespaceResolver {
	return func(c *echo.Context, svc *auth.Service, role models.Role) ([]nsRequirement, error) {
		objectID, err := pathUUID(c, "id")
		if err != nil {
			return nil, err
		}
		namespace, err := svc.SharedObjectNamespace(c.Request().Context(), kind, objectID)
		if err != nil {
			return nil, lookupError(err)
		}
		return sharedObjectRequirement(c.Request().Method, namespace, role), nil
	}
}

// sharedObjectRequirement applies the "*" rule: reading a cluster-shared
// object needs the role in any namespace; writing one needs it at "*".
func sharedObjectRequirement(method, namespace string, role models.Role) []nsRequirement {
	if namespace == models.ClusterNamespace && method == http.MethodGet {
		return []nsRequirement{{Namespaces: []string{models.ClusterNamespace}, Role: role, AnyGrant: true}}
	}
	return []nsRequirement{every(role, namespace)}
}

func resolveCreateBodyNamespace(c *echo.Context, _ *auth.Service, role models.Role) ([]nsRequirement, error) {
	raw, err := peekJSONNamespace(c)
	if err != nil {
		return nil, err
	}
	// The same normalisation the services apply (blank ⇒ default, "*"
	// allowed, otherwise a DNS label).
	namespace, err := models.NormalizeObjectNamespace(raw)
	if err != nil {
		return nil, echo.NewHTTPError(http.StatusBadRequest, err.Error()).Wrap(err)
	}
	return []nsRequirement{every(role, namespace)}, nil
}

func resolveDatasetAny(c *echo.Context, svc *auth.Service, role models.Role) ([]nsRequirement, error) {
	return resolveDataset(c, svc, role, true)
}

func resolveDatasetEvery(c *echo.Context, svc *auth.Service, role models.Role) ([]nsRequirement, error) {
	return resolveDataset(c, svc, role, false)
}

func resolveDataset(c *echo.Context, svc *auth.Service, role models.Role, any bool) ([]nsRequirement, error) {
	datasetNamespace, name := DatasetPathIdentity(c)
	if name == "" {
		return nil, echo.NewHTTPError(http.StatusBadRequest, "bad request")
	}
	namespaces, err := svc.DatasetProducerNamespaces(c.Request().Context(), datasetNamespace, name)
	if err != nil {
		return nil, lookupError(err)
	}
	return []nsRequirement{{Namespaces: namespaces, Role: role, Any: any}}, nil
}

func resolveDatasetHold(c *echo.Context, svc *auth.Service, role models.Role) ([]nsRequirement, error) {
	holdID, err := uuid.Parse(strings.TrimSpace(c.Param("id")))
	if err != nil {
		return nil, badRequest(err)
	}
	namespaces, err := svc.HoldProducerNamespaces(c.Request().Context(), holdID)
	if err != nil {
		return nil, lookupError(err)
	}
	return []nsRequirement{every(role, namespaces...)}, nil
}

func resolveAuditNamespace(c *echo.Context, _ *auth.Service, role models.Role) ([]nsRequirement, error) {
	namespace, present, err := authz.ParseNamespaceQuery(c.QueryParam("namespace"))
	if err != nil {
		return nil, echo.NewHTTPError(http.StatusBadRequest, "invalid namespace parameter").Wrap(err)
	}
	if !present {
		namespace = models.ClusterNamespace
	}
	return []nsRequirement{every(role, namespace)}, nil
}

// resolveKeyCreateNamespaces reads the namespaces a key create body asks for,
// normalised exactly as the handler normalises them (omitted ⇒ "*").
func resolveKeyCreateNamespaces(c *echo.Context, _ *auth.Service, role models.Role) ([]nsRequirement, error) {
	var payload struct {
		Namespaces *[]string `json:"namespaces"`
	}
	if err := peekJSONBody(c, namespaceBodyPeekLimit, &payload); err != nil {
		return nil, err
	}
	_, namespaces, err := auth.NormalizeKeyNamespaces(payload.Namespaces)
	if err != nil {
		return nil, echo.NewHTTPError(http.StatusBadRequest, err.Error()).Wrap(err)
	}
	return []nsRequirement{every(role, namespaces...)}, nil
}

// resolveStoredKeyNamespaces reads the namespaces the stored key is confined
// to ("*" for a cluster key and for an unusable stored value).
func resolveStoredKeyNamespaces(c *echo.Context, svc *auth.Service, role models.Role) ([]nsRequirement, error) {
	keyID, err := pathUUID(c, "id")
	if err != nil {
		return nil, err
	}
	namespaces, err := svc.KeyNamespacesByID(c.Request().Context(), keyID)
	if err != nil {
		return nil, lookupError(err)
	}
	return []nsRequirement{every(role, namespaces...)}, nil
}

// DatasetPathIdentity returns the dataset identity a /v1/datasets/:ns/:name
// route addresses. It is the one decoding the dataset handlers and the auth
// middleware share: Echo leaves parameters escaped when the request has a
// RawPath and decoded otherwise, so they are unescaped exactly once; "_"
// names the empty dataset namespace; surrounding space is trimmed.
func DatasetPathIdentity(c *echo.Context) (string, string) {
	namespace, name := c.Param("ns"), c.Param("name")
	if c.Request().URL.RawPath != "" {
		if decoded, err := url.PathUnescape(namespace); err == nil {
			namespace = decoded
		}
		if decoded, err := url.PathUnescape(name); err == nil {
			name = decoded
		}
	}
	namespace = strings.TrimSpace(namespace)
	if namespace == "_" {
		namespace = ""
	}
	return namespace, strings.TrimSpace(name)
}

// peekJSONNamespace returns the "namespace" field of a create body exactly as
// the handler's c.Bind will see it, and leaves the body readable for the
// handler. Echo binds a body only when Content-Length is non-zero and only
// as JSON for media type application/json; any other media type would be
// bound differently (form, XML) or refused, so a confined principal must send
// JSON (415 otherwise). The decode is json.Unmarshal over the whole body, as
// Echo's default serializer does.
func peekJSONNamespace(c *echo.Context) (string, error) {
	var payload struct {
		Namespace string `json:"namespace"`
	}
	if err := peekJSONBody(c, namespaceBodyPeekLimit, &payload); err != nil {
		return "", err
	}
	return payload.Namespace, nil
}

// peekJSONBody decodes a request body into target exactly as the handler's
// c.Bind will (see peekJSONNamespace) and leaves the body readable. An empty
// body leaves target zero, as Echo binds nothing then; a body over limit is
// 413, a non-JSON media type 415 and an undecodable body 400.
func peekJSONBody(c *echo.Context, limit int64, target any) error {
	req := c.Request()
	if req.ContentLength == 0 {
		return nil
	}
	base, _, _ := strings.Cut(req.Header.Get(echo.HeaderContentType), ";")
	if strings.TrimSpace(base) != echo.MIMEApplicationJSON {
		return echo.NewHTTPError(http.StatusUnsupportedMediaType, "request body must be application/json")
	}
	body, err := readAndRestoreBodyLimited(c, limit)
	if err != nil {
		if errors.Is(err, errBodyTooLarge) {
			return echo.NewHTTPError(http.StatusRequestEntityTooLarge, "request body too large").Wrap(err)
		}
		return badRequest(err)
	}
	if err := json.Unmarshal(body, target); err != nil {
		return badRequest(err)
	}
	return nil
}

// confinedBodyBufferLimit caps the body a namespace-confined principal may
// send to a namespaced route: the middleware buffers it before authorising.
const confinedBodyBufferLimit = 8 << 20

// bufferConfinedBody reads a request body completely (up to
// confinedBodyBufferLimit, else 413) and puts it back for the handler.
func bufferConfinedBody(c *echo.Context) error {
	req := c.Request()
	if req.Body == nil || req.Body == http.NoBody || req.ContentLength == 0 {
		return nil
	}
	if _, err := readAndRestoreBodyLimited(c, confinedBodyBufferLimit); err != nil {
		if errors.Is(err, errBodyTooLarge) {
			return echo.NewHTTPError(http.StatusRequestEntityTooLarge, "request body too large").Wrap(err)
		}
		return badRequest(err)
	}
	return nil
}

// errBodyTooLarge reports a body longer than the peek limit.
var errBodyTooLarge = errors.New("request body too large")

// readAndRestoreBodyLimited reads at most limit bytes of the body and puts
// back everything it read in front of whatever it did not, so the handler
// always sees the complete original body, even when the read stopped at the
// limit or failed part-way. A body longer than limit is errBodyTooLarge.
func readAndRestoreBodyLimited(c *echo.Context, limit int64) ([]byte, error) {
	req := c.Request()
	if req.Body == nil {
		return nil, nil
	}
	original := req.Body
	data, err := io.ReadAll(io.LimitReader(original, limit+1))
	req.Body = readCloser{Reader: io.MultiReader(bytes.NewReader(data), original), Closer: original}
	c.SetRequest(req)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errBodyTooLarge
	}
	return data, nil
}

type readCloser struct {
	io.Reader
	io.Closer
}
