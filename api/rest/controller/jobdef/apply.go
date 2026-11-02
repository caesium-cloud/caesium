package jobdef

import (
	"errors"
	"net/http"
	"strings"

	authmw "github.com/caesium-cloud/caesium/api/middleware"
	jobdefsvc "github.com/caesium-cloud/caesium/api/rest/service/jobdef"
	contractenforce "github.com/caesium-cloud/caesium/internal/contract"
	internaljobdef "github.com/caesium-cloud/caesium/internal/jobdef"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"gorm.io/gorm"
)

type ApplyProvenance = jobdefsvc.ApplyProvenance

type ApplyRequest = jobdefsvc.ApplyRequest
type ApplyResponse = jobdefsvc.ApplyResponse

func Apply(c *echo.Context) error {
	var req ApplyRequest
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}
	if len(req.Definitions) == 0 {
		return echo.NewHTTPError(http.StatusBadRequest, "definitions are required")
	}

	importer := internaljobdef.NewImporter(catalog())
	ctx := c.Request().Context()
	applied := 0
	aliases := make([]string, 0, len(req.Definitions))
	prov, err := applyProvenanceFromRequest(req.Provenance)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	contractAck, err := allowBreakingFromRequest(req.AllowBreaking, applyActor(c))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	contractWarnings := make([]contractenforce.ContractWarning, 0)
	opts := &internaljobdef.ApplyOptions{
		Force:            req.Force,
		Provenance:       prov,
		ContractAck:      contractAck,
		ContractWarnings: &contractWarnings,
	}
	jobs := make([]jobdefsvc.AppliedJob, 0, len(req.Definitions))
	// A confined writer (namespace-confined, or a job-scoped key) applies
	// atomically (B9); everyone else keeps the per-definition flow below.
	scope, err := writerScope(c)
	if err != nil {
		return err
	}
	if scope != nil {
		return applyConfined(c, importer, &req, opts, scope)
	}
	if err := importer.ValidateBatch(ctx, req.Definitions); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}

	for i := range req.Definitions {
		def := &req.Definitions[i]
		if err := def.Validate(); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}

		aliases = append(aliases, def.Metadata.Alias)
		saved, err := importer.ApplyWithOptions(ctx, def, opts)
		if err != nil {
			if errors.Is(err, internaljobdef.ErrContractBreak) {
				if payload, ok := internaljobdef.ContractBreakResponse(err); ok {
					return c.JSON(http.StatusConflict, payload)
				}
				return echo.NewHTTPError(http.StatusConflict, err.Error())
			}
			if errors.Is(err, internaljobdef.ErrContractAck) {
				return echo.NewHTTPError(http.StatusBadRequest, err.Error())
			}
			if errors.Is(err, internaljobdef.ErrDuplicateJob) || errors.Is(err, internaljobdef.ErrProvenanceConflict) || errors.Is(err, internaljobdef.ErrJobRunning) ||
				errors.Is(err, internaljobdef.ErrTriggerTenancy) {
				return echo.NewHTTPError(http.StatusConflict, err.Error())
			}
			return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
		}
		jobs = append(jobs, appliedJob(saved, def.Metadata.Alias))
		applied++
	}

	pruned := 0
	if req.Prune {
		count, err := importer.PruneMissing(ctx, aliases, nil)
		if err != nil {
			if errors.Is(err, internaljobdef.ErrJobRunning) {
				return echo.NewHTTPError(http.StatusConflict, err.Error())
			}
			return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
		}
		pruned = count
	}

	return c.JSON(http.StatusOK, ApplyResponse{Applied: applied, Pruned: pruned, Jobs: jobs, ContractWarnings: contractWarnings})
}

func appliedJob(saved *models.Job, alias string) jobdefsvc.AppliedJob {
	if saved == nil {
		return jobdefsvc.AppliedJob{Alias: alias}
	}
	if strings.TrimSpace(saved.Alias) != "" {
		alias = saved.Alias
	}
	id := ""
	if saved.ID != uuid.Nil {
		id = saved.ID.String()
	}
	return jobdefsvc.AppliedJob{Alias: alias, ID: id}
}

// writerScope is the tenancy scope of the request's principal for a job
// write (internaljobdef.TenancyScope): its operator namespaces when it is
// namespace-confined, its key's job aliases when it is job-scoped; nil when
// neither (or auth is off). An undecodable key scope denies.
func writerScope(c *echo.Context) (*internaljobdef.TenancyScope, error) {
	namespaces, aliases, err := authmw.WriteConfinement(authmw.GetPrincipal(c), models.RoleOperator)
	if err != nil {
		return nil, echo.NewHTTPError(http.StatusForbidden, "insufficient permissions").Wrap(err)
	}
	return internaljobdef.NewTenancyScope(namespaces, aliases), nil
}

// applyConfined is a confined writer's apply (identity-and-access B9): every
// definition and the prune commit in one transaction or not at all. The
// importer re-checks, inside it, each definition's requested and stored
// namespace (a job created or moved since the middleware decided) and trigger
// tenancy, and a refusal is an audited 403 naming only what the caller named.
// A namespace-confined caller prunes only within the namespaces where it is
// operator; jobs elsewhere are never candidates. Contract findings, warnings
// and validator errors that touch a job the caller cannot read are withheld.
func applyConfined(c *echo.Context, importer *internaljobdef.Importer, req *ApplyRequest, opts *internaljobdef.ApplyOptions, scope *internaljobdef.TenancyScope) error {
	ctx := c.Request().Context()
	if req.Prune && scope.JobAliases != nil {
		// The middleware already refuses a job-scoped key's prune.
		return echo.NewHTTPError(http.StatusForbidden, "insufficient permissions")
	}
	view := newReadScope(c, catalog(), req.Definitions)
	if view != nil {
		view.strict = true
	}
	if msg := view.validate(ctx, catalog(), func(conn *gorm.DB) error {
		return internaljobdef.NewImporter(conn).ValidateBatch(ctx, req.Definitions)
	}); msg != "" {
		return echo.NewHTTPError(http.StatusBadRequest, msg)
	}

	withheld := 0
	recorded := make([]internaljobdef.RecordedJob, 0, len(req.Definitions))
	opts.NamespaceAllowlist = scope.Namespaces
	opts.JobAliasScope = scope.JobAliases
	opts.ContractAudience = view.contractAudience(ctx)
	opts.ContractWarningsWithheld = &withheld
	opts.RecordedJobs = &recorded
	plans := make([]internaljobdef.BatchPlan, 0, len(req.Definitions))
	for i := range req.Definitions {
		plans = append(plans, internaljobdef.BatchPlan{Def: &req.Definitions[i], Opts: opts})
	}
	result, err := importer.ApplyConfinedBatch(ctx, plans, internaljobdef.ConfinedBatchOptions{Prune: req.Prune, PruneNamespaces: scope.Namespaces})
	if err != nil {
		if errors.Is(err, internaljobdef.ErrContractBreak) {
			if payload, ok := internaljobdef.ContractBreakResponse(err); ok {
				return c.JSON(http.StatusConflict, payload)
			}
		}
		return confinedApplyError(err, scope)
	}
	jobs := make([]jobdefsvc.AppliedJob, 0, len(recorded))
	for _, job := range recorded {
		jobs = append(jobs, appliedJob(&models.Job{ID: job.ID, Alias: job.Alias}, job.Alias))
	}
	return c.JSON(http.StatusOK, ApplyResponse{
		Applied:                  result.Applied,
		Pruned:                   result.Pruned,
		Jobs:                     jobs,
		ContractWarnings:         *opts.ContractWarnings,
		WithheldContractWarnings: withheld,
	})
}

// confinedApplyError maps a refused confined apply. Nothing was written.
func confinedApplyError(err error, scope *internaljobdef.TenancyScope) error {
	if denial, _ := internaljobdef.WriteRefusal(err, scope); denial != nil {
		return denial
	}
	switch {
	case errors.Is(err, internaljobdef.ErrContractBreak):
		return echo.NewHTTPError(http.StatusConflict, "contract enforcement blocked the apply")
	case errors.Is(err, internaljobdef.ErrContractAck):
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	case errors.Is(err, internaljobdef.ErrBatchInvalid):
		// The stored graph changed under the apply (the validation before it
		// passed): say so without naming jobs the caller may not read.
		return echo.NewHTTPError(http.StatusConflict, "the stored job graph changed during the apply; nothing was applied, retry").Wrap(err)
	case errors.Is(err, internaljobdef.ErrDuplicateJob) || errors.Is(err, internaljobdef.ErrProvenanceConflict) || errors.Is(err, internaljobdef.ErrJobRunning):
		// Each names a job of the caller's own namespaces: the requested
		// definition, or a prune candidate (only its namespaces are pruned).
		return echo.NewHTTPError(http.StatusConflict, err.Error())
	}
	return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
}

func applyProvenanceFromRequest(p *ApplyProvenance) (*internaljobdef.Provenance, error) {
	if p == nil {
		return nil, nil
	}

	prov := &internaljobdef.Provenance{
		SourceID: strings.TrimSpace(p.SourceID),
		Repo:     strings.TrimSpace(p.Repo),
		Ref:      strings.TrimSpace(p.Ref),
		Commit:   strings.TrimSpace(p.Commit),
		Path:     strings.TrimSpace(p.Path),
	}
	if prov.SourceID == "" && prov.Repo == "" && prov.Ref == "" && prov.Commit == "" && prov.Path == "" {
		return nil, nil
	}
	// A provenance record without a source_id is incoherent: it would stamp a
	// commit/repo onto a snapshot while leaving the job's ownership empty (so a
	// later plain apply could mutate it freely). Require source_id whenever any
	// other provenance field is set.
	if prov.SourceID == "" {
		return nil, errors.New("provenance.source_id is required when any other provenance field is set")
	}
	return prov, nil
}

func allowBreakingFromRequest(req *jobdefsvc.AllowBreakingRequest, actor string) (*contractenforce.AllowBreaking, error) {
	if req == nil {
		return nil, nil
	}
	dataset := strings.TrimSpace(req.Dataset)
	if dataset == "" {
		return nil, errors.New("allow_breaking.dataset is required")
	}
	return &contractenforce.AllowBreaking{
		Dataset: dataset,
		Reason:  strings.TrimSpace(req.Reason),
		Actor:   actor,
	}, nil
}

func applyActor(c *echo.Context) string {
	if principal := authmw.GetPrincipal(c); principal != nil && strings.TrimSpace(principal.Subject) != "" {
		return strings.TrimSpace(principal.Subject)
	}
	return "anonymous"
}
