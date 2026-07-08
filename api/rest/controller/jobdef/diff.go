package jobdef

import (
	"net/http"

	contractsvc "github.com/caesium-cloud/caesium/api/rest/service/contract"
	jobdiff "github.com/caesium-cloud/caesium/internal/jobdef/diff"
	"github.com/caesium-cloud/caesium/pkg/db"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/labstack/echo/v5"
)

type DiffRequest struct {
	Definitions []schema.Definition `json:"definitions"`
}

type DiffResponse struct {
	Added    []DiffJobSpec `json:"added"`
	Removed  []DiffJobSpec `json:"removed"`
	Modified []DiffUpdate  `json:"modified"`
}

type DiffJobSpec struct {
	jobdiff.JobSpec
	ContractFindings []contractsvc.Finding `json:"contractFindings,omitempty"`
}

type DiffUpdate struct {
	jobdiff.Update
	ContractFindings []contractsvc.Finding `json:"contractFindings,omitempty"`
}

func Diff(c *echo.Context) error {
	var req DiffRequest
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	desired := make(map[string]jobdiff.JobSpec)
	for i := range req.Definitions {
		def := &req.Definitions[i]
		if err := def.Validate(); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
		desired[def.Metadata.Alias] = jobdiff.FromDefinition(def)
	}

	ctx := c.Request().Context()
	specs, err := jobdiff.LoadDatabaseSpecs(ctx, db.Connection())
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to load database specs").Wrap(err)
	}

	result := jobdiff.Compare(desired, specs)

	var findingsByAlias map[string][]contractsvc.Finding
	if contractsvc.Enabled() {
		graph, err := contractsvc.New(ctx).Graph("", req.Definitions)
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, "failed to derive contract graph").Wrap(err)
		}
		contractsvc.RecordFindings(*graph)
		findingsByAlias = make(map[string][]contractsvc.Finding, len(desired))
		allFindings := contractsvc.FindingsFromGraph(*graph)
		for alias := range desired {
			if findings := contractsvc.FilterFindingsForAlias(allFindings, alias); len(findings) > 0 {
				findingsByAlias[alias] = findings
			}
		}
	}

	resp := DiffResponse{
		Added:    diffJobSpecs(result.Creates, findingsByAlias),
		Removed:  diffJobSpecs(result.Deletes, findingsByAlias),
		Modified: diffUpdates(result.Updates, findingsByAlias),
	}

	return c.JSON(http.StatusOK, resp)
}

func diffJobSpecs(specs []jobdiff.JobSpec, findingsByAlias map[string][]contractsvc.Finding) []DiffJobSpec {
	if len(specs) == 0 {
		return []DiffJobSpec{}
	}
	out := make([]DiffJobSpec, 0, len(specs))
	for _, spec := range specs {
		out = append(out, DiffJobSpec{
			JobSpec:          spec,
			ContractFindings: findingsByAlias[spec.Alias],
		})
	}
	return out
}

func diffUpdates(updates []jobdiff.Update, findingsByAlias map[string][]contractsvc.Finding) []DiffUpdate {
	if len(updates) == 0 {
		return []DiffUpdate{}
	}
	out := make([]DiffUpdate, 0, len(updates))
	for _, update := range updates {
		out = append(out, DiffUpdate{
			Update:           update,
			ContractFindings: findingsByAlias[update.Alias],
		})
	}
	return out
}
