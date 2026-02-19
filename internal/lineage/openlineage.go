package lineage

import (
	"time"

	"github.com/google/uuid"
)

const (
	producerURI = "https://github.com/caesium-cloud/caesium"
	schemaURL   = "https://openlineage.io/spec/2-0-2/OpenLineage.json#/$defs/RunEvent"
)

type EventType string

const (
	EventTypeStart    EventType = "START"
	EventTypeRunning  EventType = "RUNNING"
	EventTypeComplete EventType = "COMPLETE"
	EventTypeFail     EventType = "FAIL"
	EventTypeAbort    EventType = "ABORT"
)

type RunEvent struct {
	EventTime time.Time `json:"eventTime"`
	EventType EventType `json:"eventType"`
	Producer  string    `json:"producer"`
	SchemaURL string    `json:"schemaURL"`
	Run       Run       `json:"run"`
	Job       Job       `json:"job"`
	Inputs    []Dataset `json:"inputs"`
	Outputs   []Dataset `json:"outputs"`
}

type Run struct {
	RunID  uuid.UUID              `json:"runId"`
	Facets map[string]interface{} `json:"facets,omitempty"`
}

type Job struct {
	Namespace string                 `json:"namespace"`
	Name      string                 `json:"name"`
	Facets    map[string]interface{} `json:"facets,omitempty"`
}

type Dataset struct {
	Namespace string                 `json:"namespace"`
	Name      string                 `json:"name"`
	Facets    map[string]interface{} `json:"facets,omitempty"`
}

type BaseFacet struct {
	Producer  string `json:"_producer"`
	SchemaURL string `json:"_schemaURL"`
}

func newBaseFacet(schemaRef string) BaseFacet {
	return BaseFacet{
		Producer:  producerURI,
		SchemaURL: schemaRef,
	}
}

type ParentRunFacet struct {
	BaseFacet
	Run ParentRunRef `json:"run"`
	Job ParentJobRef `json:"job"`
}

type ParentRunRef struct {
	RunID uuid.UUID `json:"runId"`
}

type ParentJobRef struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

type ErrorMessageFacet struct {
	BaseFacet
	Message             string `json:"message"`
	ProgrammingLanguage string `json:"programmingLanguage,omitempty"`
	StackTrace          string `json:"stackTrace,omitempty"`
}

type SourceCodeLocationFacet struct {
	BaseFacet
	Type    string `json:"type"`
	URL     string `json:"url"`
	RepoURL string `json:"repoUrl,omitempty"`
	Path    string `json:"path,omitempty"`
	Version string `json:"version,omitempty"`
	Tag     string `json:"tag,omitempty"`
	Branch  string `json:"branch,omitempty"`
}

type JobTypeFacet struct {
	BaseFacet
	ProcessingType string `json:"processingType"`
	Integration    string `json:"integration"`
	JobType        string `json:"jobType"`
}

type NominalTimeFacet struct {
	BaseFacet
	NominalStartTime string `json:"nominalStartTime"`
	NominalEndTime   string `json:"nominalEndTime,omitempty"`
}
