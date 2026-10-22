package trigger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	codes "net/http"
	"slices"
	"strings"

	"github.com/caesium-cloud/caesium/api/internal/auditlog"
	authmw "github.com/caesium-cloud/caesium/api/middleware"
	"github.com/caesium-cloud/caesium/api/rest/manualparams"
	jsvc "github.com/caesium-cloud/caesium/api/rest/service/job"
	triggersvc "github.com/caesium-cloud/caesium/api/rest/service/trigger"
	iauth "github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/authz"
	"github.com/caesium-cloud/caesium/internal/jobdef"
	"github.com/caesium-cloud/caesium/internal/models"
	runstorage "github.com/caesium-cloud/caesium/internal/run"
	triggerhttp "github.com/caesium-cloud/caesium/internal/trigger/http"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"gorm.io/gorm"
)

var (
	triggerServiceFactory = triggersvc.Service
	fireHTTPTrigger       = fireTrigger
	listFireJobs          = listTriggerJobs
)

type FireRequest struct {
	Params   map[string]string `json:"params,omitempty"`
	Priority string            `json:"priority,omitempty"`
}

// Fire is FireWith without an auditor (shared-secret use is not audited).
func Fire(c *echo.Context) error {
	return fire(c, nil)
}

// FireWith returns the manual fire handler, auditing shared-secret use with
// auditor.
//
// A caller confined for runner (authmw.WriteConfinement: namespaces where it
// holds runner unless it holds it cluster-wide, and a job-scoped key's exact
// aliases) fires only the trigger's jobs inside that scope
// (identity-and-access A6). The middleware already required runner in every
// job's namespace (and every alias in a job-scoped key's scope); the handler
// re-reads the jobs, refuses with 403 when none is inside the scope any more,
// and carries the scope into each run's admission, so a job moved out or
// added after authorisation never runs. The deprecated
// CAESIUM_MANUAL_TRIGGER_API_KEY never widens a principal.
func FireWith(auditor *iauth.AuditLogger) func(*echo.Context) error {
	return func(c *echo.Context) error {
		return fire(c, auditor)
	}
}

func fire(c *echo.Context, auditor *iauth.AuditLogger) (err error) {
	ctx := c.Request().Context()

	legacy, err := authmw.BeginLegacySecret(c, auditor, iauth.LegacyManualTrigger, env.Variables().ManualTriggerAPIKey)
	if err != nil {
		return err
	}
	defer func() { legacy.Finish(err) }()

	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(codes.StatusBadRequest, "bad request").Wrap(err)
	}

	fireReq, err := parseOptionalFireRequest(c.Request().Body)
	if err != nil {
		return echo.NewHTTPError(codes.StatusBadRequest, "bad request").Wrap(err)
	}
	if err := manualparams.Validate(fireReq.Params); err != nil {
		return echo.NewHTTPError(codes.StatusBadRequest, err.Error())
	}
	if strings.TrimSpace(fireReq.Priority) != "" {
		if _, err := runstorage.PriorityValue(fireReq.Priority); err != nil {
			return echo.NewHTTPError(codes.StatusBadRequest, "bad request").Wrap(err)
		}
	}

	trig, err := triggerServiceFactory(ctx).Get(id)
	switch {
	case err != nil && !errors.Is(err, gorm.ErrRecordNotFound):
		return echo.NewHTTPError(codes.StatusInternalServerError, "internal server error").Wrap(err)
	case errors.Is(err, gorm.ErrRecordNotFound):
		return echo.ErrNotFound
	case trig == nil:
		return echo.ErrNotFound
	case trig.Type != models.TriggerTypeHTTP:
		return echo.NewHTTPError(codes.StatusBadRequest, "bad request").Wrap(
			fmt.Errorf(
				"trigger: '%v' is type: '%v', not '%v'",
				trig.ID,
				trig.Type,
				models.TriggerTypeHTTP,
			),
		)
	}

	principal := authmw.GetPrincipal(c)
	namespaces, aliases, err := authmw.WriteConfinement(principal, models.RoleRunner)
	if err != nil {
		return echo.NewHTTPError(codes.StatusForbidden, "insufficient permissions").Wrap(err)
	}
	scope := jobdef.NewTenancyScope(namespaces, aliases)

	// The jobs are read for a confined caller's preflight and for the
	// shared-secret audit row's namespace; an unconfined caller without the
	// secret fires exactly as before.
	var jobs models.Jobs
	if scope != nil || legacy != nil {
		if jobs, err = listFireJobs(ctx, trig.ID); err != nil {
			return echo.NewHTTPError(codes.StatusInternalServerError, "internal server error").Wrap(err)
		}
	}
	inScope := make([]string, 0, len(jobs))
	var refused []string
	for _, j := range jobs {
		if j == nil {
			continue
		}
		if scope.Allows(j.Alias, j.Namespace) {
			inScope = append(inScope, models.NamespaceOrDefault(j.Namespace))
			continue
		}
		refused = append(refused, models.NamespaceOrDefault(j.Namespace))
	}
	legacy.SetNamespaces(inScope)
	legacy.Set("trigger_id", trig.ID.String())
	if len(refused) > 0 && len(inScope) == 0 {
		// Every job the trigger serves left the caller's scope since the
		// middleware authorised the request.
		slices.Sort(refused)
		return &authz.Denial{
			Reason:     "insufficient_role",
			Required:   models.RoleRunner,
			Namespaces: slices.Compact(refused),
			Message:    "insufficient permissions",
		}
	}

	var fireOpts []triggerhttp.FireOption
	if scope != nil {
		fireOpts = append(fireOpts,
			triggerhttp.WithFireScope(scope.Allows),
			triggerhttp.WithFireStartGuard(fireAdmissionGuard(scope)),
			triggerhttp.WithGuardedRunError(fireRefusalAuditor(auditor, principal, c.RealIP(), trig.ID)))
	}
	if err := fireHTTPTrigger(ctx, trig, fireReq.Params, fireReq.Priority, fireOpts...); err != nil {
		return echo.NewHTTPError(codes.StatusInternalServerError, "internal server error").Wrap(err)
	}

	return c.JSON(codes.StatusAccepted, nil)
}

// fireAdmissionGuard re-checks, inside each fired run's admission
// transaction, that the job's namespace (as admission freezes it, or as a
// running run it would adopt was persisted) is still in the caller's scope.
// It only decides: it runs inside the admission transaction, so it must not
// write. A refused job is not admitted, and fireRefusalAuditor records the
// refusal once the run returned.
func fireAdmissionGuard(scope *jobdef.TenancyScope) func(*models.Job, string) error {
	return func(_ *models.Job, namespace string) error {
		namespace = models.NamespaceOrDefault(strings.TrimSpace(namespace))
		if scope.Namespaces == nil || slices.Contains(scope.Namespaces, namespace) {
			return nil
		}
		return &authz.Denial{
			Reason:     "insufficient_role",
			Required:   models.RoleRunner,
			Namespaces: []string{namespace},
			Message:    "insufficient permissions",
		}
	}
}

// fireRefusalAuditor audits a fired job the admission guard refused (the
// client was answered 202 before the run was admitted) as auth.denied against
// the namespace the job was found in. It runs after the run returned, outside
// the admission transaction.
func fireRefusalAuditor(auditor *iauth.AuditLogger, principal *iauth.Principal, sourceIP string, triggerID uuid.UUID) func(*models.Job, error) {
	return func(j *models.Job, err error) {
		denial, ok := errors.AsType[*authz.Denial](err)
		if !ok || auditor == nil {
			return
		}
		namespace := ""
		if len(denial.Namespaces) > 0 {
			namespace = denial.Namespaces[0]
		}
		entry := iauth.AuditEntry{
			Action:       iauth.ActionAuthDenied,
			ResourceType: "job",
			SourceIP:     sourceIP,
			Outcome:      iauth.OutcomeDenied,
			Namespace:    namespace,
			Metadata: map[string]any{
				"reason":     denial.Reason,
				"required":   string(denial.Required),
				"route":      "POST /v1/triggers/:id/fire",
				"trigger_id": triggerID.String(),
				"stage":      "admission",
			},
		}
		if principal != nil {
			entry.Actor = principal.Subject
		}
		if j != nil {
			entry.ResourceID = j.ID.String()
		}
		auditlog.LogFailure(auditor.Log(entry))
	}
}

func parseOptionalFireRequest(body io.Reader) (FireRequest, error) {
	if body == nil {
		return FireRequest{}, nil
	}

	data, err := io.ReadAll(body)
	if err != nil {
		return FireRequest{}, err
	}
	if strings.TrimSpace(string(data)) == "" {
		return FireRequest{}, nil
	}

	var req FireRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return FireRequest{}, err
	}
	return req, nil
}

func fireTrigger(ctx context.Context, trig *models.Trigger, params map[string]string, priority string, opts ...triggerhttp.FireOption) error {
	httpTrigger, err := triggerhttp.New(trig)
	if err != nil {
		return err
	}
	return httpTrigger.FireWithParams(ctx, params, append([]triggerhttp.FireOption{triggerhttp.WithPriority(priority)}, opts...)...)
}

func listTriggerJobs(ctx context.Context, triggerID uuid.UUID) (models.Jobs, error) {
	return jsvc.Service(ctx).List(&jsvc.ListRequest{TriggerID: triggerID.String()})
}
