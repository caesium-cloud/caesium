package connector

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"time"

	"github.com/caesium-cloud/caesium/internal/auth"
	core "github.com/caesium-cloud/caesium/internal/connector"
	"github.com/caesium-cloud/caesium/internal/connector/operations"
	"github.com/caesium-cloud/caesium/internal/connector/temporal"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"go.temporal.io/api/serviceerror"
	"gorm.io/gorm"
)

var ErrMessageBinding = errors.New("connector: undeclared or changed binding or handler")

// Descriptors expose only configured, wired messages. Live availability is
// separate from a remote validator's authoritative business eligibility.
type ActionView struct {
	Name         string          `json:"name"`
	InputSchema  json.RawMessage `json:"input_schema"`
	ResultSchema json.RawMessage `json:"result_schema"`
	MinimumRole  string          `json:"minimum_role"`
	Available    bool            `json:"available"`
}
type BindingView struct {
	Name                    string          `json:"name"`
	Version                 string          `json:"version"`
	DisplayName             string          `json:"display_name"`
	StatusQuery             string          `json:"status_query,omitempty"`
	StatusQueryResultSchema json.RawMessage `json:"status_query_result_schema,omitempty"`
	Available               bool            `json:"available"`
	Actions                 []ActionView    `json:"actions"`
}

func bindingView(cfg core.Connection, workflowType string, available bool) *BindingView {
	for _, b := range cfg.Bindings {
		if b.Name != workflowType {
			continue
		}
		out := &BindingView{Name: b.Name, Version: b.Version, DisplayName: b.DisplayName, Available: available, Actions: []ActionView{}}
		if len(b.StatusQueryResultSchema) > 0 {
			out.StatusQuery = b.StatusQuery
			out.StatusQueryResultSchema = b.StatusQueryResultSchema
		}
		for _, a := range b.Actions {
			out.Actions = append(out.Actions, ActionView{Name: a.Name, InputSchema: a.InputSchema, ResultSchema: a.ResultSchema, MinimumRole: "operator", Available: available})
		}
		return out
	}
	return nil
}
func messageCapabilities(cfg core.Connection) []core.Capability {
	caps := []core.Capability{core.CapabilityReceiptLookup}
	query, action := false, false
	for _, b := range cfg.Bindings {
		query = query || len(b.StatusQueryResultSchema) > 0
		action = action || len(b.Actions) > 0
	}
	if query {
		caps = append(caps, core.CapabilityStatusQuery)
	}
	if action {
		caps = append(caps, core.CapabilityActionSubmission)
	}
	return caps
}

// Receipt deliberately excludes the idempotency key, input, schema snapshot,
// endpoint and internal request fingerprint. Persistence models are not HTTP DTOs.
type Receipt struct {
	compact            bool
	ID                 uuid.UUID                    `json:"id"`
	ConnectionID       string                       `json:"connection_id"`
	ExecutionID        string                       `json:"execution_id"`
	Execution          Reference                    `json:"execution"`
	Action             string                       `json:"action"`
	BindingName        string                       `json:"binding_name"`
	BindingVersion     string                       `json:"binding_version"`
	State              string                       `json:"state"`
	Revision           uint64                       `json:"revision"`
	Attempt            uint64                       `json:"attempt"`
	Actor              core.ActorEnvelope           `json:"actor"`
	ExternalUpdateID   string                       `json:"external_update_id"`
	ConfigurationEpoch string                       `json:"configuration_epoch"`
	Evidence           operations.OperationEvidence `json:"evidence"`
	ResendAudit        json.RawMessage              `json:"resend_audit,omitempty"`
	CreatedAt          time.Time                    `json:"created_at"`
	UpdatedAt          time.Time                    `json:"updated_at"`
}

// Compact receipts retain every coordinate needed for a deliberate original-
// principal confirmation. ExecutionID is the immutable opaque target, not a
// latest-run selector. Large descriptive/evidence bodies are explicitly omitted.
func (r Receipt) MarshalJSON() ([]byte, error) {
	type complete Receipt
	if !r.compact {
		return json.Marshal(complete(r))
	}
	return json.Marshal(struct {
		ID             uuid.UUID `json:"id"`
		ConnectionID   string    `json:"connection_id"`
		ExecutionID    string    `json:"execution_id"`
		Action         string    `json:"action"`
		BindingName    string    `json:"binding_name"`
		BindingVersion string    `json:"binding_version"`
		State          string    `json:"state"`
		Revision       uint64    `json:"revision"`
		Attempt        uint64    `json:"attempt"`
		Actor          struct {
			PrincipalKind core.PrincipalKind `json:"principal_kind"`
			StableID      string             `json:"stable_id"`
		} `json:"actor"`
		ExternalUpdateID string `json:"external_update_id"`
		MetadataOmitted  bool   `json:"metadata_omitted"`
	}{r.ID, r.ConnectionID, r.ExecutionID, r.Action, r.BindingName, r.BindingVersion, r.State, r.Revision, r.Attempt, struct {
		PrincipalKind core.PrincipalKind `json:"principal_kind"`
		StableID      string             `json:"stable_id"`
	}{r.Actor.PrincipalKind, r.Actor.StableID}, r.ExternalUpdateID, true})
}

func receipt(op models.ConnectorOperation) (Receipt, error) {
	var target operations.OperationTarget
	var actor core.ActorEnvelope
	var evidence operations.OperationEvidence
	if json.Unmarshal(op.Target, &target) != nil || json.Unmarshal(op.Actor, &actor) != nil {
		return Receipt{}, core.ErrInvalidOperation
	}
	if len(op.ProviderEvidence) > 0 && json.Unmarshal(op.ProviderEvidence, &evidence) != nil {
		return Receipt{}, core.ErrInvalidOperation
	}
	return Receipt{ID: op.ID, ConnectionID: op.ConnectionID, ExecutionID: core.ReferenceID(target.Execution), Execution: httpReference(target.Execution), Action: op.ActionName, BindingName: target.BindingName, BindingVersion: op.BindingVersion, State: op.State, Revision: op.Revision, Attempt: op.Attempt, Actor: actor, ExternalUpdateID: op.ExternalUpdateID, ConfigurationEpoch: op.ConfigurationEpoch, Evidence: evidence, ResendAudit: json.RawMessage(op.ResendAudit), CreatedAt: op.CreatedAt, UpdatedAt: op.UpdatedAt}, nil
}

type OperationResult struct {
	Operation Receipt `json:"operation"`
	Created   bool    `json:"created"`
	Conflict  bool    `json:"conflict"`
	Pending   bool    `json:"pending"`
}

func fitOperation(op models.ConnectorOperation, created, conflict bool, operationErr error, limit int) (OperationResult, error) {
	view, err := receipt(op)
	if err != nil {
		return OperationResult{}, err
	}
	out := OperationResult{Operation: view, Created: created, Conflict: conflict, Pending: errors.Is(operationErr, operations.ErrOperationPending) || errors.Is(operationErr, operations.ErrOperationProvider)}
	if _, err = bound(out, limit); errors.Is(err, core.ErrPayloadTooLarge) && len(out.Operation.Evidence.Result) > 0 {
		out.Operation.Evidence.Result = nil
		out.Operation.Evidence.ResultOmitted = true
		_, err = bound(out, limit)
	}
	if errors.Is(err, core.ErrPayloadTooLarge) {
		out.Operation.compact = true
		_, err = bound(out, limit)
	}
	if err != nil {
		return out, err
	}
	return out, operationErr
}

// The operation service is constructed without dialing. Frozen binding checks,
// authorization and durable admission therefore precede every remote RPC.
func (r *Runtime) operationService(ctx context.Context, c *connection) (*operations.Service, error) {
	if err := r.Ready(ctx); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if r.closed.Load() {
		return nil, core.ErrEpochMismatch
	}
	if c.operations != nil {
		return c.operations, nil
	}
	key := ""
	if len(c.config.SecretRefs) > 1 {
		return nil, core.ErrInvalidOperation
	}
	if len(c.config.SecretRefs) == 1 {
		if r.resolver == nil {
			return nil, errors.New("connector: credential unavailable")
		}
		var err error
		key, err = r.resolver.Resolve(ctx, c.config.SecretRefs[0])
		if err != nil {
			return nil, errors.New("connector: credential unavailable")
		}
	}
	s, err := operations.NewService(operations.NewStore(r.db), &auditedProvider{provider: &lazyOperationProvider{runtime: r, connection: c}, runtime: r}, c.config, r.fingerprint, key)
	if err == nil {
		c.operations = s
	}
	return s, err
}
func (r *Runtime) messages(ctx context.Context, c *connection) (*temporal.Messenger, error) {
	if err := r.Ready(ctx); err != nil {
		return nil, err
	}
	if r.closed.Load() {
		return nil, core.ErrEpochMismatch
	}
	c.mu.Lock()
	m := c.messenger
	c.mu.Unlock()
	if m != nil {
		return m, nil
	}
	if _, err := r.observer(ctx, c); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.messenger == nil {
		return nil, core.ErrInvalidOperation
	}
	return c.messenger, nil
}

type lazyOperationProvider struct {
	runtime    *Runtime
	connection *connection
}

func (p *lazyOperationProvider) StableOperationID(id string) (string, error) {
	return temporal.StableUpdateID(id)
}
func (p *lazyOperationProvider) SubmitOperation(ctx context.Context, d operations.OperationDispatch) (operations.OperationEvidence, error) {
	m, err := p.runtime.messages(ctx, p.connection)
	if err != nil {
		return operations.OperationEvidence{State: core.OperationNotDispatched}, err
	}
	return (&temporal.OperationProvider{Messenger: m}).SubmitOperation(ctx, d)
}
func (p *lazyOperationProvider) LookupOperation(ctx context.Context, d operations.OperationDispatch) (operations.OperationEvidence, error) {
	m, err := p.runtime.messages(ctx, p.connection)
	if err != nil {
		return operations.OperationEvidence{State: core.OperationNotDispatched}, err
	}
	return (&temporal.OperationProvider{Messenger: m}).LookupOperation(ctx, d)
}
func messageTarget(c *connection, ref core.ExecutionReference, name, version, handler string, query bool) (temporal.MessageTarget, error) {
	for _, b := range c.config.Bindings {
		if b.Name != name || b.Version != version {
			continue
		}
		declared := query && handler == b.StatusQuery && len(b.StatusQueryResultSchema) > 0
		if !query {
			for _, a := range b.Actions {
				declared = declared || a.Name == handler
			}
		}
		if declared {
			return temporal.MessageTarget{ConnectionID: ref.ConnectionID, WorkflowID: ref.Coordinates["workflow_id"], RunID: ref.Coordinates["run_id"], BindingName: name, BindingVersion: version}, nil
		}
	}
	return temporal.MessageTarget{}, ErrMessageBinding
}
func principalKey(p *operations.OperationPrincipal) string {
	if p == nil {
		return ""
	}
	return string(p.Kind) + ":" + p.StableID
}

// auditMessage records only server-derived identities and metadata tokens. No
// caller input, remote errors, returned result or credential bytes are copied.
func (r *Runtime) auditMessage(action string, p *operations.OperationPrincipal, connectionID, executionID, handler string, op models.ConnectorOperation, callErr error) error {
	if p == nil {
		return operations.ErrOperationForbidden
	}
	// Never copy an unknown route segment into the durable log. Configured
	// handler names and canonical references have explicit finite bounds.
	configured := r.connections[connectionID]
	if configured == nil {
		connectionID = ""
		executionID = ""
		handler = ""
	} else {
		declared := false
		for _, binding := range configured.config.Bindings {
			declared = declared || binding.StatusQuery == handler
			for _, action := range binding.Actions {
				declared = declared || action.Name == handler
			}
		}
		if !declared {
			handler = ""
		}
		if ref, err := parseReference(connectionID, executionID); err == nil {
			executionID = core.ReferenceID(ref)
		} else {
			executionID = ""
		}
	}
	if len(executionID) > 1024 {
		digest := sha256.Sum256([]byte(executionID))
		executionID = "sha256:" + hex.EncodeToString(digest[:])
	}
	outcome := auth.OutcomeSuccess
	if callErr != nil {
		outcome = auth.OutcomeError
		if errors.Is(callErr, operations.ErrOperationForbidden) {
			outcome = auth.OutcomeDenied
		}
	}
	metadata := map[string]any{"principal_kind": p.Kind, "stable_id": p.StableID, "role": p.Role, "connection_id": connectionID, "execution_id": executionID, "action_id": handler}
	if op.ID != uuid.Nil {
		metadata["operation_id"] = op.ID.String()
		metadata["state"] = op.State
	}
	return auth.NewAuditLogger(r.db).Log(auth.AuditEntry{Actor: p.Subject, Action: action, ResourceType: "connector_execution", ResourceID: executionID, Outcome: outcome, Namespace: "*", Metadata: metadata})
}

// AuditRefusal covers a malformed public envelope refused before service
// invocation. Authentication/scope denials retain the middleware's audit.
func (r *Runtime) AuditRefusal(action string, p *operations.OperationPrincipal, id, opaque, handler string, err error) error {
	return r.auditMessage(action, p, id, opaque, handler, models.ConnectorOperation{}, err)
}

// A dispatch audit is durable before the provider can receive an Update. The
// receipt's immutable actor and operation ID supply the target of that audit.
type auditedProvider struct {
	provider operations.OperationProvider
	runtime  *Runtime
}

func (p *auditedProvider) StableOperationID(id string) (string, error) {
	return p.provider.StableOperationID(id)
}
func (p *auditedProvider) SubmitOperation(ctx context.Context, d operations.OperationDispatch) (operations.OperationEvidence, error) {
	actor := &operations.OperationPrincipal{Kind: d.Actor.PrincipalKind, StableID: d.Actor.StableID, Subject: d.Actor.Subject, Role: d.Actor.Role}
	id, _ := uuid.Parse(d.Actor.OperationID)
	if err := p.runtime.auditMessage(auth.ActionConnectorActionDispatch, actor, d.Target.Execution.ConnectionID, core.ReferenceID(d.Target.Execution), d.Action, models.ConnectorOperation{ID: id, State: core.OperationSubmitted}, nil); err != nil {
		return operations.OperationEvidence{State: core.OperationNotDispatched}, err
	}
	return p.provider.SubmitOperation(ctx, d)
}
func (p *auditedProvider) LookupOperation(ctx context.Context, d operations.OperationDispatch) (operations.OperationEvidence, error) {
	return p.provider.LookupOperation(ctx, d)
}

func (r *Runtime) Query(ctx context.Context, p *operations.OperationPrincipal, id, opaque, queryID, name, version string) (raw []byte, resultErr error) {
	defer func() {
		if err := r.auditMessage(auth.ActionConnectorStatusQuery, p, id, opaque, queryID, models.ConnectorOperation{}, resultErr); err != nil {
			raw = nil
			resultErr = err
		}
	}()
	c, err := r.connection(ctx, id, principalKey(p))
	if err != nil {
		return nil, err
	}
	ref, err := parseReference(id, opaque)
	if err != nil {
		return nil, err
	}
	target, err := messageTarget(c, ref, name, version, queryID, true)
	if err != nil {
		return nil, err
	}
	if err = r.queryBudget.Allow(ctx, principalKey(p)); err != nil {
		return nil, err
	}
	if err = c.queryBudget.Allow(ctx, principalKey(p)); err != nil {
		return nil, err
	}
	raw, callErr := c.guard.Do(ctx, "query:"+uuid.NewString(), func(call context.Context) ([]byte, error) {
		m, err := r.messages(call, c)
		if err != nil {
			return nil, err
		}
		result, err := m.QueryStatus(call, target)
		if err != nil {
			return nil, err
		}
		return bound(struct {
			Result json.RawMessage `json:"result"`
		}{result}, c.config.Limits.MaxPageMetadataBytes)
	})
	return raw, callErr
}
func (r *Runtime) Submit(ctx context.Context, p *operations.OperationPrincipal, id, opaque, actionID, key, name, version string, input map[string]any) (result OperationResult, resultErr error) {
	var decision core.OperationDecision
	defer func() {
		if err := r.auditMessage(auth.ActionConnectorActionSubmit, p, id, opaque, actionID, decision.Operation, resultErr); err != nil {
			result = OperationResult{}
			resultErr = err
		}
	}()
	c, err := r.connection(ctx, id, principalKey(p))
	if err != nil {
		return OperationResult{}, err
	}
	ref, err := parseReference(id, opaque)
	if err != nil {
		return OperationResult{}, err
	}
	if _, err = messageTarget(c, ref, name, version, actionID, false); err != nil {
		return OperationResult{}, err
	}
	if err := validateSubmission(c, p, name, version, actionID, input); err != nil {
		return OperationResult{}, err
	}
	if err := r.reserveReceipt(c, ref, name, version, actionID, p); err != nil {
		return OperationResult{}, err
	}
	_, callErr := c.guard.Do(ctx, "submit:"+uuid.NewString(), func(call context.Context) ([]byte, error) {
		s, err := r.operationService(call, c)
		if err != nil {
			return nil, err
		}
		// Replays remain receipt recovery even when the provider is unavailable.
		// New stateless discovery references require a trusted direct observation.
		var existing models.ConnectorOperation
		existingProbe := r.db.WithContext(call).Select("id").Where("(connection_id = ? AND idempotency_key = ?) OR open_key = ?", id, key, models.OperationOpenKey(id, ref.OpaqueID(), actionID)).Find(&existing)
		existingErr := existingProbe.Error
		if existingErr == nil && existingProbe.RowsAffected == 0 {
			existingErr = gorm.ErrRecordNotFound
		}
		if errors.Is(existingErr, gorm.ErrRecordNotFound) {
			if err := r.inspectActionTarget(call, c, ref, name); err != nil {
				// An identical concurrent winner may have admitted while this
				// observation was in flight. Recover that durable request rather
				// than misreporting a superseded observation as a submission failure.
				reload := r.db.WithContext(call).Select("id").Where("(connection_id = ? AND idempotency_key = ?) OR open_key = ?", id, key, models.OperationOpenKey(id, ref.OpaqueID(), actionID)).Find(&existing)
				if reload.Error != nil {
					return nil, reload.Error
				}
				if reload.RowsAffected == 0 {
					return nil, err
				}
			}
		} else if existingErr != nil {
			return nil, existingErr
		}
		decision, err = s.Submit(call, operations.OperationSubmission{IdempotencyKey: key, Target: operations.OperationTarget{Execution: ref, BindingName: name, BindingVersion: version}, Action: actionID, Input: input, Principal: p})
		return nil, err
	})
	if decision.Operation.ID == uuid.Nil {
		return OperationResult{}, callErr
	}
	return fitOperation(decision.Operation, decision.Created, decision.Conflict, callErr, c.config.Limits.MaxPageMetadataBytes)
}
func validateSubmission(c *connection, p *operations.OperationPrincipal, name, version, handler string, input map[string]any) error {
	if p == nil || (p.Role != "operator" && p.Role != "admin") {
		return operations.ErrOperationForbidden
	}
	var scope struct {
		Agent json.RawMessage `json:"agent"`
	}
	if len(p.Scope) > 0 && (json.Unmarshal(p.Scope, &scope) != nil || (len(scope.Agent) > 0 && string(scope.Agent) != "null")) {
		return operations.ErrOperationForbidden
	}
	scoped, err := auth.IsScoped(p.Scope)
	if err != nil || scoped {
		return operations.ErrOperationForbidden
	}
	if _, err := core.NewActorEnvelope(p.Kind, p.StableID, p.Subject, p.Role, uuid.NewString(), version); err != nil {
		return operations.ErrOperationForbidden
	}
	if _, spoof := input[core.ReservedActorField]; spoof {
		return core.ErrInvalidPayload
	}
	for _, b := range c.config.Bindings {
		if b.Name == name && b.Version == version {
			for _, a := range b.Actions {
				if a.Name == handler {
					if core.ValidateActionPayload(a.InputSchema, input) != nil {
						return core.ErrInvalidPayload
					}
					return nil
				}
			}
		}
	}
	return ErrMessageBinding
}
func operationFilter(opaque string) string {
	digest := sha256.Sum256([]byte(opaque))
	return "operations:" + hex.EncodeToString(digest[:])
}

// Reserve a compact response and a one-item paginated recovery response before
// permitting a new send. Revision/attempt widths cover their persisted ceiling.
func (r *Runtime) reserveReceipt(c *connection, ref core.ExecutionReference, name, version, action string, p *operations.OperationPrincipal) error {
	id := uuid.New()
	externalID, err := temporal.StableUpdateID(id.String())
	if err != nil {
		return err
	}
	view := Receipt{compact: true, ID: id, ConnectionID: c.config.ID, ExecutionID: core.ReferenceID(ref), Action: action, BindingName: name, BindingVersion: version, State: core.OperationNotDispatched, Revision: math.MaxUint64, Attempt: math.MaxUint64, ExternalUpdateID: externalID}
	if p != nil {
		view.Actor.PrincipalKind = p.Kind
		view.Actor.StableID = p.StableID
	}
	if _, err := bound(OperationResult{Operation: view}, c.config.Limits.MaxPageMetadataBytes); err != nil {
		return err
	}
	token, _ := json.Marshal(operationCursor{CreatedAt: time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC), ID: id})
	cursor := r.encodeCursor(c.config.ID, operationFilter(view.ExecutionID), token)
	_, err = bound(struct {
		Operations []Receipt `json:"operations"`
		Cursor     string    `json:"cursor"`
	}{[]Receipt{view}, cursor}, c.config.Limits.MaxPageMetadataBytes)
	return err
}
func (r *Runtime) inspectActionTarget(ctx context.Context, c *connection, ref core.ExecutionReference, bindingName string) error {
	generation, err := r.store.NextObservationGeneration(ctx, ref)
	if err != nil {
		return err
	}
	o, err := r.observer(ctx, c)
	if err != nil {
		return err
	}
	observed, err := o.Describe(ctx, ref.Coordinates["workflow_id"], ref.Coordinates["run_id"])
	if err != nil {
		var notFound *serviceerror.NotFound
		if errors.As(err, &notFound) {
			return core.ErrNotFound
		}
		return err
	}
	if observed.WorkflowID != ref.Coordinates["workflow_id"] || observed.RunID != ref.Coordinates["run_id"] || observed.WorkflowType != bindingName {
		return ErrMessageBinding
	}
	complete := core.CompletenessComplete
	if observed.Incomplete {
		complete = core.CompletenessPartial
	}
	metadata := observed
	metadata.ObservedAt = time.Time{}
	raw, err := json.Marshal(metadata)
	if err != nil {
		return err
	}
	_, err = r.store.RecordDirect(ctx, r.fingerprint, core.Observation{Execution: ref, Generation: generation, SourceEventID: strconv.FormatInt(observed.SourceEventID, 10), NativeStatus: observed.NativeStatus, DisplayStatus: observed.DisplayStatus, Availability: core.AvailabilityAvailable, Completeness: complete, Metadata: raw, SourceKind: core.SourceDirect, ObservedAt: observed.ObservedAt}, c.config.Limits)
	return err
}

func (r *Runtime) operation(ctx context.Context, c *connection, id uuid.UUID) (models.ConnectorOperation, error) {
	op, err := operations.NewStore(r.db).Operation(ctx, id)
	if err != nil {
		return op, err
	}
	if op.ConnectionID != c.config.ID {
		return models.ConnectorOperation{}, core.ErrNotFound
	}
	return op, nil
}
func (r *Runtime) Lookup(ctx context.Context, p *operations.OperationPrincipal, id string, operationID uuid.UUID) (OperationResult, error) {
	c, err := r.connection(ctx, id, principalKey(p))
	if err != nil {
		return OperationResult{}, err
	}
	op, err := r.operation(ctx, c, operationID)
	if err != nil {
		return OperationResult{}, err
	}
	if op.State == core.OperationCompleted || op.State == core.OperationRejected || op.State == core.OperationNotDispatched {
		return fitOperation(op, false, false, nil, c.config.Limits.MaxPageMetadataBytes)
	}
	_, callErr := c.guard.Do(ctx, "lookup:"+uuid.NewString(), func(call context.Context) ([]byte, error) {
		s, err := r.operationService(call, c)
		if err != nil {
			return nil, err
		}
		observed, err := s.Reconcile(call, operationID)
		if observed.ID != uuid.Nil {
			op = observed
		}
		return nil, err
	})
	executionID := ""
	if view, e := receipt(op); e == nil {
		executionID = view.ExecutionID
	}
	if err := r.auditMessage(auth.ActionConnectorOperationLookup, p, id, executionID, op.ActionName, op, callErr); err != nil {
		return OperationResult{}, err
	}
	return fitOperation(op, false, false, callErr, c.config.Limits.MaxPageMetadataBytes)
}
func (r *Runtime) Resubmit(ctx context.Context, p *operations.OperationPrincipal, id string, operationID uuid.UUID, revision uint64, confirmed bool) (OperationResult, error) {
	c, err := r.connection(ctx, id, principalKey(p))
	if err != nil {
		return OperationResult{}, err
	}
	op, err := r.operation(ctx, c, operationID)
	if err != nil {
		return OperationResult{}, err
	}
	var target operations.OperationTarget
	if json.Unmarshal(op.Target, &target) != nil {
		return OperationResult{}, core.ErrInvalidOperation
	}
	var original core.ActorEnvelope
	if json.Unmarshal(op.Actor, &original) != nil {
		return OperationResult{}, core.ErrInvalidOperation
	}
	originalPrincipal := &operations.OperationPrincipal{Kind: original.PrincipalKind, StableID: original.StableID}
	if err := r.reserveReceipt(c, target.Execution, target.BindingName, target.BindingVersion, op.ActionName, originalPrincipal); err != nil {
		return OperationResult{}, err
	}
	_, callErr := c.guard.Do(ctx, "resubmit:"+uuid.NewString(), func(call context.Context) ([]byte, error) {
		s, err := r.operationService(call, c)
		if err != nil {
			return nil, err
		}
		observed, err := s.ConfirmResend(call, operationID, revision, p, confirmed)
		if observed.ID != uuid.Nil {
			op = observed
		}
		return nil, err
	})
	executionID := ""
	if view, e := receipt(op); e == nil {
		executionID = view.ExecutionID
	}
	if err := r.auditMessage(auth.ActionConnectorActionResubmit, p, id, executionID, op.ActionName, op, callErr); err != nil {
		return OperationResult{}, err
	}
	return fitOperation(op, false, false, callErr, c.config.Limits.MaxPageMetadataBytes)
}

type operationCursor struct {
	CreatedAt time.Time `json:"created_at"`
	ID        uuid.UUID `json:"id"`
}

func (r *Runtime) Operations(ctx context.Context, principal, id, opaque, pageCursor string, size int) ([]byte, error) {
	c, err := r.connection(ctx, id, principal)
	if err != nil {
		return nil, err
	}
	ref, err := parseReference(id, opaque)
	if err != nil {
		return nil, err
	}
	size, err = pageSize(size, c.config.Limits)
	if err != nil {
		return nil, err
	}
	filter := operationFilter(opaque)
	token, err := r.decodeCursor(id, filter, pageCursor)
	if err != nil {
		return nil, err
	}
	query := r.db.WithContext(ctx).Where("connection_id = ? AND opaque_id = ?", id, ref.OpaqueID())
	if len(token) > 0 {
		var cursor operationCursor
		if json.Unmarshal(token, &cursor) != nil || cursor.ID == uuid.Nil || cursor.CreatedAt.IsZero() {
			return nil, ErrInvalidRequest
		}
		query = query.Where("created_at < ? OR (created_at = ? AND id < ?)", cursor.CreatedAt, cursor.CreatedAt, cursor.ID)
	}
	var rows []models.ConnectorOperation
	if err = query.Order("created_at DESC, id DESC").Limit(size + 1).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := struct {
		Operations []Receipt `json:"operations"`
		Cursor     string    `json:"cursor,omitempty"`
	}{Operations: []Receipt{}}
	more := len(rows) > size
	if more {
		rows = rows[:size]
	}
	for _, row := range rows {
		view, err := receipt(row)
		if err != nil {
			return nil, err
		}
		out.Operations = append(out.Operations, view)
	}
	for {
		if more && len(out.Operations) > 0 {
			last := out.Operations[len(out.Operations)-1]
			token, _ := json.Marshal(operationCursor{CreatedAt: last.CreatedAt, ID: last.ID})
			out.Cursor = r.encodeCursor(id, filter, token)
		}
		raw, err := bound(out, c.config.Limits.MaxPageMetadataBytes)
		if err == nil {
			return raw, r.Ready(ctx)
		}
		if !errors.Is(err, core.ErrPayloadTooLarge) || len(out.Operations) == 0 {
			return nil, err
		}
		last := &out.Operations[len(out.Operations)-1]
		if len(last.Evidence.Result) > 0 {
			last.Evidence.Result = nil
			last.Evidence.ResultOmitted = true
			continue
		}
		if !last.compact {
			last.compact = true
			continue
		}
		if len(out.Operations) == 1 {
			return nil, err
		}
		out.Operations = out.Operations[:len(out.Operations)-1]
		more = true
	}
}
