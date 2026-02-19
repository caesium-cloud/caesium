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

func newCaesiumBaseFacet(facetName string) BaseFacet {
	return BaseFacet{
		Producer:  producerURI,
		SchemaURL: caesiumFacetSchemaBase + "/" + facetName + ".json",
	}
}
