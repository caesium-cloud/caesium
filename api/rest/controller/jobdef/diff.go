package jobdef

import (
	"net/http"
	"slices"
	"strings"

	contractsvc "github.com/caesium-cloud/caesium/api/rest/service/contract"
	jobdiff "github.com/caesium-cloud/caesium/internal/jobdef/diff"
	"github.com/caesium-cloud/caesium/internal/jobdef/lint"
	"github.com/caesium-cloud/caesium/internal/models"
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
	// Namespaces lists, sorted, every namespace an apply of these definitions
	// would touch: those of the added jobs, and the stored and requested
	// namespaces of the modified ones (a move touches both). Removed jobs
	// carry their own namespace and are touched only by a pruning apply.
	Namespaces []string `json:"namespaces"`
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
	// Namespace secret rules (identity-and-access C4): the apply preview
	// rejects a secret:// reference its definition's namespace does not allow,
	// as server lint does. Apply itself does not; the run fails instead.
	if msgs := lint.CheckSecretsAllowed(req.Definitions, lint.ServerSecretResolver); len(msgs) > 0 {
		return echo.NewHTTPError(http.StatusBadRequest, strings.Join(msgs, "; "))
	}

	ctx := c.Request().Context()
	// A namespace-confined caller compares against its own namespaces' jobs
	// only (nil scope: unrestricted, unchanged).
	scope := newReadScope(c, catalog(), req.Definitions)
	specs, err := jobdiff.LoadDatabaseSpecs(scope.baselineContext(ctx), catalog())
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to load database specs").Wrap(err)
	}

	result := jobdiff.Compare(desired, specs)

	var findingsByAlias map[string][]contractsvc.Finding
	if contractsvc.Enabled() {
		// A confined caller sees only edges between jobs it can read.
		graph, visible, err := scope.contractGraph(ctx, req.Definitions, nil)
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, "failed to derive contract graph").Wrap(err)
		}
		contractsvc.RecordFindings(graph)
		findingsByAlias = make(map[string][]contractsvc.Finding, len(desired))
		allFindings := contractsvc.FindingsFromGraph(visible)
		for alias := range desired {
			if findings := contractsvc.FilterFindingsForAlias(allFindings, alias); len(findings) > 0 {
				findingsByAlias[alias] = findings
			}
		}
	}

	resp := DiffResponse{
		Added:      diffJobSpecs(result.Creates, findingsByAlias),
		Removed:    diffJobSpecs(result.Deletes, findingsByAlias),
		Modified:   diffUpdates(result.Updates, findingsByAlias),
		Namespaces: touchedNamespaces(result, desired, specs),
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

// touchedNamespaces is DiffResponse.Namespaces.
func touchedNamespaces(result jobdiff.Diff, desired, actual map[string]jobdiff.JobSpec) []string {
	namespaces := make([]string, 0, len(result.Creates)+2*len(result.Updates))
	for _, spec := range result.Creates {
		namespaces = append(namespaces, models.NamespaceOrDefault(spec.Namespace))
	}
	for _, update := range result.Updates {
		namespaces = append(namespaces,
			models.NamespaceOrDefault(desired[update.Alias].Namespace),
			models.NamespaceOrDefault(actual[update.Alias].Namespace))
	}
	slices.Sort(namespaces)
	return slices.Compact(namespaces)
}
