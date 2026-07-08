package jobdef

import (
	"errors"
	"net/http"
	"strings"

	authmw "github.com/caesium-cloud/caesium/api/middleware"
	jobdefsvc "github.com/caesium-cloud/caesium/api/rest/service/jobdef"
	contractenforce "github.com/caesium-cloud/caesium/internal/contract"
	internaljobdef "github.com/caesium-cloud/caesium/internal/jobdef"
	"github.com/caesium-cloud/caesium/pkg/db"
	"github.com/labstack/echo/v5"
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

	importer := internaljobdef.NewImporter(db.Connection())
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
	if err := importer.ValidateBatch(ctx, req.Definitions); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}

	for i := range req.Definitions {
		def := &req.Definitions[i]
		if err := def.Validate(); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}

		aliases = append(aliases, def.Metadata.Alias)
		if _, err := importer.ApplyWithOptions(ctx, def, opts); err != nil {
			if errors.Is(err, internaljobdef.ErrContractBreak) {
				if payload, ok := internaljobdef.ContractBreakResponse(err); ok {
					return c.JSON(http.StatusConflict, payload)
				}
				return echo.NewHTTPError(http.StatusConflict, err.Error())
			}
			if errors.Is(err, internaljobdef.ErrContractAck) {
				return echo.NewHTTPError(http.StatusBadRequest, err.Error())
			}
			if errors.Is(err, internaljobdef.ErrDuplicateJob) || errors.Is(err, internaljobdef.ErrProvenanceConflict) || errors.Is(err, internaljobdef.ErrJobRunning) {
				return echo.NewHTTPError(http.StatusConflict, err.Error())
			}
			return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
		}
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

	return c.JSON(http.StatusOK, ApplyResponse{Applied: applied, Pruned: pruned, ContractWarnings: contractWarnings})
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
