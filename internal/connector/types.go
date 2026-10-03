// Package connector holds the provider-neutral execution-connector contract.
// A1 freezes configuration, identity, and schemas. It does not dial a provider,
// persist a catalog, or register HTTP routes.
package connector

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ProviderTemporal is the only provider name the v1 configuration file accepts.
// The registry still accepts other adapter identities; those identities are
// opaque and do not add Temporal fields to this package.
const ProviderTemporal = "temporal"

// ReservedActorField is the server-derived actor envelope. Public schemas and
// connection files cannot declare or override it.
const ReservedActorField = "_caesium_actor"

// CorrelationEnvelopeVersion is the only accepted v1 correlation envelope version.
const CorrelationEnvelopeVersion = "v1"

// Hard ceilings. Configuration may be stricter. Anything looser is rejected.
const (
	MinConsoleRefresh          = 10 * time.Second
	MaxReadsPerMinute          = 60
	MaxQueriesPerMinute        = 6
	MaxInFlightRPCs            = 4
	MaxRPCDeadline             = 10 * time.Second
	MaxPageEntries             = 100
	MaxPageMetadataBytes       = 64 << 10
	MaxUnreferencedSnapshotAge = 24 * time.Hour
	MaxUnreferencedSnapshots   = 1000
)

// Capability is one optional connector surface. Adapters advertise only the
// capabilities the running server actually wires.
type Capability string

const (
	CapabilityDiscovery        Capability = "discovery"
	CapabilityInspection       Capability = "inspection"
	CapabilityHistory          Capability = "history"
	CapabilityRelationships    Capability = "relationships"
	CapabilityStatusQuery      Capability = "status_query"
	CapabilityActionSubmission Capability = "action_submission"
	CapabilityReceiptLookup    Capability = "receipt_lookup"
)

// TemporalProviderIdentity is the capability profile a future Temporal adapter
// would advertise. It is data. This package does not import a Temporal client.
var TemporalProviderIdentity = ProviderIdentity{
	Name: ProviderTemporal,
	Capabilities: []Capability{
		CapabilityDiscovery,
		CapabilityInspection,
		CapabilityHistory,
		CapabilityRelationships,
		CapabilityStatusQuery,
		CapabilityActionSubmission,
		CapabilityReceiptLookup,
	},
}

// ProviderIdentity is an opaque adapter name plus the capability set it
// advertises. The name is not a Temporal workflow, namespace, or run id.
type ProviderIdentity struct {
	Name         string
	Capabilities []Capability
}

// PrincipalKind distinguishes humans from API keys in the actor envelope.
type PrincipalKind string

const (
	PrincipalKindUser   PrincipalKind = "user"
	PrincipalKindAPIKey PrincipalKind = "api_key"
)

// ActorEnvelope is the immutable server-derived _caesium_actor context.
// Callers build it from the authenticated principal. Config files cannot.
type ActorEnvelope struct {
	PrincipalKind  PrincipalKind `json:"principal_kind"`
	StableID       string        `json:"stable_id"`
	Subject        string        `json:"subject"`
	Role           string        `json:"role"`
	OperationID    string        `json:"operation_id"`
	BindingVersion string        `json:"binding_version"`
}

// NewActorEnvelope validates and returns a server-derived actor envelope.
func NewActorEnvelope(kind PrincipalKind, stableID, subject, role, operationID, bindingVersion string) (ActorEnvelope, error) {
	actor := ActorEnvelope{
		PrincipalKind:  kind,
		StableID:       stableID,
		Subject:        subject,
		Role:           role,
		OperationID:    operationID,
		BindingVersion: bindingVersion,
	}
	if err := actor.Validate(); err != nil {
		return ActorEnvelope{}, err
	}
	return actor, nil
}

// Validate checks the frozen actor envelope fields.
func (a ActorEnvelope) Validate() error {
	switch a.PrincipalKind {
	case PrincipalKindUser, PrincipalKindAPIKey:
	default:
		return errors.New("actor envelope principal_kind must be user or api_key")
	}
	if strings.TrimSpace(a.StableID) == "" ||
		strings.TrimSpace(a.Subject) == "" ||
		strings.TrimSpace(a.Role) == "" ||
		strings.TrimSpace(a.OperationID) == "" ||
		strings.TrimSpace(a.BindingVersion) == "" {
		return errors.New("actor envelope requires stable_id, subject, role, operation_id, and binding_version")
	}
	return nil
}

// ApplyActor copies input and sets the reserved actor field to actor.
// A caller-supplied value under that key is overwritten. Call this only after
// the caller payload has passed the action schema. That schema does not
// declare the reserved field, and additionalProperties false rejects it, so
// validating the stamped payload fails every submission. AcceptActionInput
// validates first and then calls ApplyActor. There is no submission route
// in this package yet.
func ApplyActor(input map[string]any, actor ActorEnvelope) (map[string]any, error) {
	if err := actor.Validate(); err != nil {
		return nil, err
	}
	out := make(map[string]any, len(input)+1)
	for key, value := range input {
		out[key] = value
	}
	out[ReservedActorField] = map[string]any{
		"principal_kind":  string(actor.PrincipalKind),
		"stable_id":       actor.StableID,
		"subject":         actor.Subject,
		"role":            actor.Role,
		"operation_id":    actor.OperationID,
		"binding_version": actor.BindingVersion,
	}
	return out, nil
}

// AcceptActionInput validates the caller payload against the action schema
// and then stamps the server-derived actor. The actor is not part of the
// public schema. Validating after ApplyActor rejects every payload.
func AcceptActionInput(schema json.RawMessage, input map[string]any, actor ActorEnvelope) (map[string]any, error) {
	if err := validateCallerPayload(schema, input); err != nil {
		return nil, err
	}
	return ApplyActor(input, actor)
}

// CorrelationEnvelope is the frozen v1 activity heartbeat/result hint.
// queue_id and run_id are optional. Binding files cannot replace these fields.
type CorrelationEnvelope struct {
	Version        string `json:"version"`
	JobID          string `json:"job_id"`
	IdempotencyKey string `json:"idempotency_key"`
	Outcome        string `json:"outcome"`
	QueueID        string `json:"queue_id,omitempty"`
	RunID          string `json:"run_id,omitempty"`
}

// Validate checks the required v1 correlation fields. Empty queue_id and
// run_id stay optional.
func (e CorrelationEnvelope) Validate() error {
	if e.Version != CorrelationEnvelopeVersion {
		return fmt.Errorf("correlation envelope version must be %q", CorrelationEnvelopeVersion)
	}
	if strings.TrimSpace(e.JobID) == "" || strings.TrimSpace(e.IdempotencyKey) == "" || strings.TrimSpace(e.Outcome) == "" {
		return errors.New("correlation envelope requires job_id, idempotency_key, and outcome")
	}
	return nil
}

// ConnectionIdentity is the immutable identity of a configured connection.
// Repointing provider, endpoint, or scope under the same id is refused.
type ConnectionIdentity struct {
	ID       string
	Provider string
	Endpoint string
	Scope    string
}

// sealIdentities rejects a blank connection id before that identity is published.
// Parsed files already have ids. This is the same immutable-identity rule a
// later rollout uses, applied to the loaded identity before it becomes the
// process baseline.
func sealIdentities(cfg *Config) error {
	if cfg == nil {
		return errors.New("connector config is required")
	}
	for _, conn := range cfg.Connections {
		baseline := conn.Identity()
		if err := baseline.RefuseRepoint(conn.Identity()); err != nil {
			return fmt.Errorf("connection %q: %w", conn.ID, err)
		}
	}
	return nil
}

// RefuseRepoint reports an error when next keeps this id and changes the
// provider, endpoint, or scope. A different id is a different connection.
func (id ConnectionIdentity) RefuseRepoint(next ConnectionIdentity) error {
	if strings.TrimSpace(id.ID) == "" || strings.TrimSpace(next.ID) == "" {
		return errors.New("connection identity requires an id")
	}
	if id.ID != next.ID {
		return nil
	}
	if id.Provider != next.Provider || id.Endpoint != next.Endpoint || id.Scope != next.Scope {
		return fmt.Errorf("connection %q identity is immutable; allocate a new id to change provider, endpoint, or scope", id.ID)
	}
	return nil
}

// ExecutionReference is a stateless opaque handle. It stores no catalog row
// and has no provider-specific fields such as a workflow id or run id.
type ExecutionReference struct {
	ConnectionID string
	Coordinates  map[string]string
}

// NewExecutionReference copies opaque coordinates. The coordinate keys belong
// to the adapter that produced them.
func NewExecutionReference(connectionID string, coordinates map[string]string) (ExecutionReference, error) {
	if strings.TrimSpace(connectionID) == "" {
		return ExecutionReference{}, errors.New("execution reference requires a connection id")
	}
	if len(coordinates) == 0 {
		return ExecutionReference{}, errors.New("execution reference requires opaque coordinates")
	}
	copied := make(map[string]string, len(coordinates))
	for key, value := range coordinates {
		if strings.TrimSpace(key) == "" || strings.TrimSpace(value) == "" {
			return ExecutionReference{}, errors.New("execution reference coordinates must be non-empty")
		}
		copied[key] = value
	}
	return ExecutionReference{ConnectionID: connectionID, Coordinates: copied}, nil
}

// OpaqueID is a stable digest of the connection id and coordinates. Computing
// it does not insert a catalog row.
func (r ExecutionReference) OpaqueID() string {
	keys := make([]string, 0, len(r.Coordinates))
	for key := range r.Coordinates {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	digest := sha256.New()
	// %q keeps connection ids and coordinates unambiguous. {"a=b":"c"} and
	// {"a":"b=c"} must not share an id, and embedded newlines must not add pairs.
	_, _ = fmt.Fprintf(digest, "%q\n", r.ConnectionID)
	for _, key := range keys {
		_, _ = fmt.Fprintf(digest, "%q=%q\n", key, r.Coordinates[key])
	}
	return hex.EncodeToString(digest.Sum(nil))
}
