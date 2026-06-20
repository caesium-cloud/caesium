package lineage

const (
	caesiumFacetSchemaBase = "https://github.com/caesium-cloud/caesium/spec/facets/1-0-0"
)

type CaesiumExecutionFacet struct {
	BaseFacet
	Engine    string   `json:"engine"`
	Image     string   `json:"image"`
	Command   []string `json:"command,omitempty"`
	RuntimeID string   `json:"runtimeId,omitempty"`
	ClaimedBy string   `json:"claimedBy,omitempty"`
}

type CaesiumDAGFacet struct {
	BaseFacet
	TotalTasks    int    `json:"totalTasks"`
	TriggerType   string `json:"triggerType,omitempty"`
	TriggerAlias  string `json:"triggerAlias,omitempty"`
	FailurePolicy string `json:"failurePolicy,omitempty"`
	ExecutionMode string `json:"executionMode,omitempty"`
}

type CaesiumProvenanceFacet struct {
	BaseFacet
	SourceID string `json:"sourceId,omitempty"`
	Repo     string `json:"repo,omitempty"`
	Ref      string `json:"ref,omitempty"`
	Commit   string `json:"commit,omitempty"`
	Path     string `json:"path,omitempty"`
}

// CaesiumDatasetFacet is a dataset-level facet carrying Caesium-specific
// lineage fields (step name, direction, and the structured output keys that
// produced or consumed this dataset).  It is attached to every Dataset entry
// emitted in Inputs/Outputs of a task RunEvent.
type CaesiumDatasetFacet struct {
	BaseFacet
	// StepName is the task/step name that produced or consumed this dataset.
	StepName string `json:"stepName,omitempty"`
	// Direction is "input" or "output" from the step's perspective.
	Direction string `json:"direction"`
	// OutputKeys lists the structured-output keys (from ##caesium::output) whose
	// values contributed to this dataset's identity.  Empty when the dataset
	// was derived from a declared schema rather than structured output.
	OutputKeys []string `json:"outputKeys,omitempty"`
}

// CaesiumSchemaFacet carries the declared JSON Schema for a dataset so
// OpenLineage consumers can perform field-level compatibility checks.
type CaesiumSchemaFacet struct {
	BaseFacet
	// Schema is the raw JSON Schema object (map form) as declared in the job
	// manifest's outputSchema / inputSchema field for this step.
	Schema map[string]interface{} `json:"schema,omitempty"`
}

func newCaesiumBaseFacet(facetName string) BaseFacet {
	return BaseFacet{
		Producer:  producerURI,
		SchemaURL: caesiumFacetSchemaBase + "/" + facetName + ".json",
	}
}
