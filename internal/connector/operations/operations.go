package operations

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"

	"github.com/caesium-cloud/caesium/internal/connector"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
)

var (
	ErrOperationForbidden    = errors.New("connector: action requires the original unscoped operator or admin principal")
	ErrOperationConfirmation = errors.New("connector: explicit confirmation of an unknown operation is required")
	ErrOperationProvider     = errors.New("connector: operation provider unavailable; consult receipt evidence")
	ErrOperationPending      = errors.New("connector: operation remains pending; consult receipt evidence")
	ErrSensitiveOperation    = errors.New("connector: action content contains sensitive material")
	operationPEM             = regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]+-----.*?-----END [A-Z0-9 ]+-----`)
)

// OperationTarget is provider neutral; only the adapter interprets coordinates.
// BindingName selects the exact external execution type, never the latest run.
type OperationTarget struct {
	Execution      connector.ExecutionReference `json:"execution"`
	BindingName    string                       `json:"binding_name"`
	BindingVersion string                       `json:"binding_version"`
}

// OperationDispatch contains immutable material loaded from the receipt.
type OperationDispatch struct {
	Target           OperationTarget
	Action           string
	ExternalUpdateID string
	Input            map[string]any
	Actor            connector.ActorEnvelope
}

// OperationEvidence describes one bounded provider call. RejectionProof may
// only be validator_rejected when independent evidence proves rejection origin;
// a failed outcome, NotFound, or unavailable Poll is never such proof.
type OperationEvidence struct {
	State            string          `json:"state"`
	NativeStage      string          `json:"native_stage,omitempty"`
	PreflightRPCCode string          `json:"preflight_rpc_code,omitempty"`
	RPCCode          string          `json:"rpc_code,omitempty"`
	Result           json.RawMessage `json:"result,omitempty"`
	FailureKind      string          `json:"failure_kind,omitempty"`
	RejectionProof   string          `json:"rejection_proof,omitempty"`
	ResultOmitted    bool            `json:"result_omitted,omitempty"`
}

// OperationProvider must make at most one send per SubmitOperation call.
// LookupOperation is lookup-only: no dispatch, retry or retargeting is allowed.
type OperationProvider interface {
	StableOperationID(operationID string) (string, error)
	SubmitOperation(context.Context, OperationDispatch) (OperationEvidence, error)
	LookupOperation(context.Context, OperationDispatch) (OperationEvidence, error)
}

// OperationSubmission is assembled by an authenticated service. Input is caller
// JSON; Principal must come from authentication, never the request body.
type OperationSubmission struct {
	IdempotencyKey string
	Target         OperationTarget
	Action         string
	Input          map[string]any
	Principal      *OperationPrincipal
}

// OperationPrincipal is the authenticated identity projection supplied by the
// composing service. Keeping this leaf value neutral avoids coupling provider
// fixtures/adapters to authentication services and their database dependencies.
// StableID is the authenticated Caesium user/key UUID. These fields must never
// be bound from caller JSON.
type OperationPrincipal struct {
	Kind     connector.PrincipalKind
	StableID string
	Subject  string
	Role     string
	Scope    []byte
}

// Service implements durable admission, lookup and explicit resend.
// Construct it from one frozen configuration and enforce the same epoch before
// admission and every provider call. It does not schedule automatic sends.
type Service struct {
	store      *Store
	provider   OperationProvider
	connection connector.Connection
	epoch      string
	secrets    []string
	maxBytes   int
}

func NewService(store *Store, provider OperationProvider, connection connector.Connection, epoch string, secrets ...string) (*Service, error) {
	if store == nil || store.db == nil || provider == nil || !connection.Enabled || connection.ID == "" || epoch == "" {
		return nil, connector.ErrInvalidOperation
	}
	// A JSON roundtrip turns absent optional RawMessages into literal "null",
	// changing the fingerprint's semantics. Copy every mutable value directly.
	frozen := connection
	frozen.SecretRefs = append([]string(nil), connection.SecretRefs...)
	frozen.CertificatePaths = append([]string(nil), connection.CertificatePaths...)
	frozen.Bindings = append([]connector.Binding(nil), connection.Bindings...)
	for i := range frozen.Bindings {
		binding := &frozen.Bindings[i]
		binding.StatusQueryResultSchema = bytes.Clone(binding.StatusQueryResultSchema)
		binding.Actions = append([]connector.Action(nil), binding.Actions...)
		for j := range binding.Actions {
			binding.Actions[j].InputSchema = bytes.Clone(binding.Actions[j].InputSchema)
			binding.Actions[j].ResultSchema = bytes.Clone(binding.Actions[j].ResultSchema)
		}
		binding.ActivityAllowlist = append([]connector.ActivityJob(nil), binding.ActivityAllowlist...)
		for j := range binding.ActivityAllowlist {
			binding.ActivityAllowlist[j].Jobs = append([]string(nil), binding.ActivityAllowlist[j].Jobs...)
		}
		if _, err := operationBindingBytes(*binding); err != nil {
			return nil, connector.ErrInvalidOperation
		}
	}
	limit := frozen.Limits.MaxPageMetadataBytes
	if limit == 0 {
		limit = connector.MaxPageMetadataBytes
	}
	if limit < 1 || limit > connector.MaxPageMetadataBytes {
		return nil, connector.ErrRetentionLimit
	}
	return &Service{store: store, provider: provider, connection: frozen, epoch: epoch, secrets: append([]string(nil), secrets...), maxBytes: limit}, nil
}

func operationActor(principal *OperationPrincipal, id, version string) (connector.ActorEnvelope, error) {
	if principal == nil || (principal.Role != "operator" && principal.Role != "admin") {
		return connector.ActorEnvelope{}, ErrOperationForbidden
	}
	stableID, err := uuid.Parse(principal.StableID)
	if err != nil || stableID == uuid.Nil {
		return connector.ActorEnvelope{}, ErrOperationForbidden
	}
	// Inspect the raw claim separately: an agent with no jobs is still scoped.
	var scope struct {
		Jobs  []string        `json:"jobs"`
		Agent json.RawMessage `json:"agent"`
	}
	if len(principal.Scope) > 0 {
		if json.Unmarshal(principal.Scope, &scope) != nil || (len(scope.Agent) > 0 && string(scope.Agent) != "null") {
			return connector.ActorEnvelope{}, ErrOperationForbidden
		}
		for _, job := range scope.Jobs {
			if strings.TrimSpace(job) != "" {
				return connector.ActorEnvelope{}, ErrOperationForbidden
			}
		}
	}
	actor, err := connector.NewActorEnvelope(principal.Kind, stableID.String(), principal.Subject, principal.Role, id, version)
	if err != nil {
		return connector.ActorEnvelope{}, ErrOperationForbidden
	}
	return actor, nil
}

func (s *Service) checkEpoch(ctx context.Context) error {
	active, err := s.store.catalog.ActiveEpoch(ctx)
	if err != nil {
		return err
	}
	if active != s.epoch {
		return connector.ErrEpochMismatch
	}
	return nil
}

func (s *Service) action(target OperationTarget, name string) (connector.Binding, connector.Action, error) {
	if target.Execution.ConnectionID != s.connection.ID || target.BindingName == "" || target.BindingVersion == "" {
		return connector.Binding{}, connector.Action{}, connector.ErrInvalidOperation
	}
	if _, err := connector.NewExecutionReference(target.Execution.ConnectionID, target.Execution.Coordinates); err != nil {
		return connector.Binding{}, connector.Action{}, connector.ErrInvalidOperation
	}
	for _, binding := range s.connection.Bindings {
		if binding.Name != target.BindingName || binding.Version != target.BindingVersion {
			continue
		}
		for _, action := range binding.Actions {
			if action.Name == name {
				return binding, action, nil
			}
		}
	}
	return connector.Binding{}, connector.Action{}, connector.ErrInvalidOperation
}

// Admit persists the complete dispatch snapshot without contacting the provider.
// A crash immediately afterward is recovered exclusively through Reconcile.
func (s *Service) Admit(ctx context.Context, request OperationSubmission) (connector.OperationDecision, error) {
	if len(request.IdempotencyKey) > 256 {
		return connector.OperationDecision{}, connector.ErrInvalidOperation
	}
	if err := s.nonsecret([]byte(request.IdempotencyKey)); err != nil {
		return connector.OperationDecision{}, err
	}
	if _, spoof := request.Input[connector.ReservedActorField]; spoof {
		return connector.OperationDecision{}, connector.ErrInvalidPayload
	}
	if request.Input == nil {
		request.Input = map[string]any{}
	}
	if err := s.checkEpoch(ctx); err != nil {
		return connector.OperationDecision{}, err
	}
	binding, action, err := s.action(request.Target, request.Action)
	if err != nil {
		return connector.OperationDecision{}, err
	}
	id := uuid.New()
	actor, err := operationActor(request.Principal, id.String(), binding.Version)
	if err != nil {
		return connector.OperationDecision{}, err
	}
	if err := connector.ValidateActionPayload(action.InputSchema, request.Input); err != nil {
		return connector.OperationDecision{}, connector.ErrInvalidPayload
	}
	input, err := json.Marshal(request.Input)
	if err != nil {
		return connector.OperationDecision{}, connector.ErrInvalidPayload
	}
	target, err := json.Marshal(request.Target)
	if err != nil {
		return connector.OperationDecision{}, connector.ErrInvalidPayload
	}
	snapshot, err := operationBindingBytes(binding)
	if err != nil {
		return connector.OperationDecision{}, connector.ErrInvalidPayload
	}
	identity := s.connection.Identity()
	identityJSON, err := json.Marshal(identity)
	if err != nil {
		return connector.OperationDecision{}, connector.ErrInvalidPayload
	}
	actorJSON, _ := json.Marshal(actor)
	// Never silently redact retry input: exact original bytes must be resendable.
	for _, raw := range [][]byte{input, target, snapshot, actorJSON, identityJSON} {
		if err := s.nonsecret(raw); err != nil {
			return connector.OperationDecision{}, err
		}
	}
	if len(input)+len(target)+len(snapshot)+len(actorJSON)+len(identityJSON) > s.maxBytes {
		return connector.OperationDecision{}, connector.ErrPayloadTooLarge
	}
	externalID, err := s.provider.StableOperationID(id.String())
	if err != nil || len(externalID) > 256 || strings.TrimSpace(externalID) == "" {
		return connector.OperationDecision{}, connector.ErrInvalidOperation
	}
	principal := actor
	principal.OperationID = ""
	logical, err := json.Marshal(struct {
		Target    OperationTarget
		Action    string
		Input     json.RawMessage
		Principal connector.ActorEnvelope
		Binding   json.RawMessage
		Identity  connector.ConnectionIdentity
	}{request.Target, request.Action, input, principal, snapshot, identity})
	if err != nil {
		return connector.OperationDecision{}, connector.ErrInvalidPayload
	}
	return s.store.catalog.AdmitOperation(ctx, connector.OperationRequest{ID: id, ConnectionID: s.connection.ID, IdempotencyKey: request.IdempotencyKey, Execution: request.Target.Execution, Action: request.Action, RequestFingerprint: requestFingerprint(logical), Actor: actorJSON, BindingVersion: binding.Version, ExternalUpdateID: externalID, Input: input, Target: target, BindingSnapshot: snapshot, ConnectionIdentity: identityJSON, ConfigurationEpoch: s.epoch}, connector.WriteBounds{MaxMetadataBytes: s.maxBytes})
}

// Submit claims a fresh admission or persisted initial no-send proof before
// dispatch. Other same-key nonterminal replays only reconcile; an admission
// without a claimed send returns pending. A conflicting key never does work.
func (s *Service) Submit(ctx context.Context, request OperationSubmission) (connector.OperationDecision, error) {
	decision, err := s.Admit(ctx, request)
	if err != nil || decision.Conflict || operationTerminal(decision.Operation.State) {
		return decision, err
	}
	if !decision.Created && !decision.Operation.RetryableUnsent {
		// A fresh winner may still be taking its first claim. Do not let a
		// duplicate lookup invalidate that claim, and never infer no-send here.
		if decision.Operation.Revision == 1 && len(decision.Operation.ProviderEvidence) == 0 {
			return decision, ErrOperationPending
		}
		decision.Operation, err = s.Reconcile(ctx, decision.Operation.ID)
		if err == nil && (decision.Operation.State == connector.OperationUnknown || decision.Operation.State == connector.OperationSubmitted) {
			err = ErrOperationPending
		}
		return decision, err
	}
	if _, err = s.dispatch(decision.Operation); err != nil {
		return decision, err
	}
	decision.Operation, err = s.store.claimDispatch(ctx, decision.Operation, s.epoch, decision.Created)
	if err != nil {
		return decision, err
	}
	decision.Operation, err = s.call(ctx, decision.Operation, true)
	return decision, err
}

// Reconcile is safe for restarts/background use and only looks up the same ID.
func (s *Service) Reconcile(ctx context.Context, id uuid.UUID) (models.ConnectorOperation, error) {
	if err := s.checkEpoch(ctx); err != nil {
		return models.ConnectorOperation{}, err
	}
	op, err := s.store.Operation(ctx, id)
	if err != nil || operationTerminal(op.State) {
		return op, err
	}
	if op.RetryableUnsent {
		return op, ErrOperationPending
	}
	return s.call(ctx, op, false)
}

// ConfirmResend requires a fresh authenticated decision from the original
// principal. Role/configuration are rechecked; input, run, actor and ID cannot
// be edited, even if the confirming principal's current role has changed.
func (s *Service) ConfirmResend(ctx context.Context, id uuid.UUID, expectedRevision uint64, principal *OperationPrincipal, confirmed bool) (models.ConnectorOperation, error) {
	if err := s.checkEpoch(ctx); err != nil {
		return models.ConnectorOperation{}, err
	}
	op, err := s.store.Operation(ctx, id)
	if err != nil {
		return op, err
	}
	if !confirmed || op.State != connector.OperationUnknown {
		return op, ErrOperationConfirmation
	}
	if expectedRevision != op.Revision {
		return op, ErrOperationChanged
	}
	current, err := operationActor(principal, id.String(), op.BindingVersion)
	if err != nil {
		return op, err
	}
	currentJSON, err := json.Marshal(current)
	if err != nil {
		return op, connector.ErrInvalidOperation
	}
	if err := s.nonsecret(currentJSON); err != nil {
		return op, err
	}
	var original connector.ActorEnvelope
	if json.Unmarshal(op.Actor, &original) != nil || current.PrincipalKind != original.PrincipalKind || current.StableID != original.StableID {
		return op, ErrOperationForbidden
	}
	if _, err := s.dispatch(op); err != nil {
		return op, err
	}
	if err := s.checkEpoch(ctx); err != nil {
		return op, err
	}
	op, err = s.store.confirmOperationResend(ctx, op, current, s.epoch, s.maxBytes)
	if err != nil {
		return op, err
	}
	return s.call(ctx, op, true)
}

func (s *Service) dispatch(op models.ConnectorOperation) (OperationDispatch, error) {
	var target OperationTarget
	var actor connector.ActorEnvelope
	var input map[string]any
	var snapshot connector.Binding
	var identity connector.ConnectionIdentity
	if op.ConnectionID != s.connection.ID || json.Unmarshal(op.ConnectionIdentity, &identity) != nil || identity.ID != op.ConnectionID || identity.RefuseRepoint(s.connection.Identity()) != nil || json.Unmarshal(op.Target, &target) != nil || json.Unmarshal(op.Actor, &actor) != nil || json.Unmarshal(op.BindingSnapshot, &snapshot) != nil || decodeOperationObject(op.Input, &input) != nil {
		return OperationDispatch{}, connector.ErrInvalidOperation
	}
	binding, action, err := s.action(target, op.ActionName)
	if err != nil {
		return OperationDispatch{}, err
	}
	encoded, err := operationBindingBytes(binding)
	if err != nil {
		return OperationDispatch{}, connector.ErrInvalidOperation
	}
	if !bytes.Equal(encoded, op.BindingSnapshot) || snapshot.Name != target.BindingName || snapshot.Version != op.BindingVersion || target.Execution.OpaqueID() != op.OpaqueID || actor.OperationID != op.ID.String() || actor.BindingVersion != op.BindingVersion || actor.Validate() != nil {
		return OperationDispatch{}, connector.ErrInvalidOperation
	}
	stable, err := s.provider.StableOperationID(op.ID.String())
	if err != nil || stable != op.ExternalUpdateID {
		return OperationDispatch{}, connector.ErrInvalidOperation
	}
	if err := connector.ValidateActionPayload(action.InputSchema, input); err != nil {
		return OperationDispatch{}, connector.ErrInvalidPayload
	}
	for _, raw := range [][]byte{op.Input, op.Actor, op.BindingSnapshot, op.Target, op.ConnectionIdentity} {
		if err := s.nonsecret(raw); err != nil {
			return OperationDispatch{}, err
		}
	}
	return OperationDispatch{Target: target, Action: op.ActionName, ExternalUpdateID: op.ExternalUpdateID, Input: input, Actor: actor}, nil
}

// Use exactly the configuration fingerprint's ordering rules. Reordering
// actions, activity allowlists or job lists is the same epoch and must not
// strand a receipt after restart or change its logical request fingerprint.
func operationBindingBytes(binding connector.Binding) ([]byte, error) {
	return connector.CanonicalBindingJSON(binding)
}

func (s *Service) call(ctx context.Context, op models.ConnectorOperation, send bool) (models.ConnectorOperation, error) {
	if err := s.checkEpoch(ctx); err != nil {
		return op, err
	}
	dispatch, err := s.dispatch(op)
	if err != nil {
		return op, err
	}
	var evidence OperationEvidence
	if send {
		evidence, err = s.provider.SubmitOperation(ctx, dispatch)
	} else {
		// Provider lookup receives no resend material, even for an incorrect adapter.
		dispatch.Input = nil
		dispatch.Actor = connector.ActorEnvelope{}
		evidence, err = s.provider.LookupOperation(ctx, dispatch)
	}
	state := connector.OperationUnknown
	retryableUnsent := false
	evidence.ResultOmitted = false
	switch evidence.State {
	case connector.OperationSubmitted, connector.OperationAccepted:
		state = evidence.State
		if op.State == connector.OperationAccepted && state == connector.OperationSubmitted {
			state = connector.OperationAccepted
		}
	case connector.OperationCompleted:
		_, action, actionErr := s.action(dispatch.Target, op.ActionName)
		if actionErr == nil && len(evidence.Result) <= s.maxBytes && s.nonsecret(evidence.Result) == nil {
			var result map[string]any
			if decodeOperationObject(evidence.Result, &result) == nil && connector.ValidateActionPayload(action.ResultSchema, result) == nil {
				state = connector.OperationCompleted
			}
		}
	case connector.OperationRejected:
		if evidence.RejectionProof == "validator_rejected" {
			state = connector.OperationRejected
		}
	case connector.OperationNotDispatched:
		// Only initial-attempt proof can establish that there was no earlier
		// Update. A transient preflight is retryable, not a business refusal.
		if send && op.Attempt == 1 {
			if definitivePreflight(evidence.PreflightRPCCode) {
				state = connector.OperationNotDispatched
			} else {
				state, retryableUnsent = connector.OperationSubmitted, true
			}
		}
	}
	if state != connector.OperationCompleted {
		evidence.Result = nil
	}
	if err != nil && state != connector.OperationNotDispatched && !retryableUnsent {
		state = connector.OperationUnknown
		evidence.Result = nil
	}
	// Acceptance is durable positive proof. An unavailable, malformed or weaker
	// lookup cannot erase its bytes or authorize an explicit resend.
	if op.State == connector.OperationAccepted && (err != nil || (state != connector.OperationCompleted && evidence.State != connector.OperationAccepted)) {
		current, readErr := s.store.Operation(ctx, op.ID)
		if readErr != nil {
			return current, readErr
		}
		if err != nil {
			return current, ErrOperationProvider
		}
		return current, nil
	}
	evidence = s.safeEvidence(evidence)
	raw, marshalErr := s.fitEvidence(evidence)
	if marshalErr != nil {
		return op, marshalErr
	}
	stored, storeErr := s.store.recordOperationEvidence(ctx, op, state, raw, retryableUnsent)
	if storeErr != nil {
		return stored, storeErr
	}
	if err != nil {
		return stored, ErrOperationProvider
	}
	if retryableUnsent {
		return stored, ErrOperationPending
	}
	return stored, nil
}

func definitivePreflight(code string) bool {
	switch code {
	case "NotFound", "InvalidArgument", "FailedPrecondition", "PermissionDenied":
		return true
	}
	return false
}

// A legal validated result may occupy the entire provider payload budget.
// Drop only its exposed body when envelope overhead does not fit; completion
// and its guard release were proved independently of storage presentation.
func (s *Service) fitEvidence(e OperationEvidence) ([]byte, error) {
	raw, err := json.Marshal(e)
	if err != nil {
		return nil, err
	}
	if len(raw) > s.maxBytes && len(e.Result) > 0 {
		e.Result, e.ResultOmitted = nil, true
		raw, err = json.Marshal(e)
		if err != nil {
			return nil, err
		}
	}
	if len(raw) > s.maxBytes {
		e = OperationEvidence{State: e.State, PreflightRPCCode: e.PreflightRPCCode, RejectionProof: e.RejectionProof, ResultOmitted: e.ResultOmitted}
		raw, err = json.Marshal(e)
		if err != nil {
			return nil, err
		}
	}
	if err := boundJSON(raw, s.maxBytes); err != nil {
		return nil, err
	}
	return raw, nil
}

// Preserve arbitrary JSON integers and decimals exactly across persisted retry
// material and schema validation. float64 would silently retarget numeric input.
func decodeOperationObject(raw []byte, object *map[string]any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(object); err != nil {
		return err
	}
	if *object == nil {
		return connector.ErrInvalidPayload
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return connector.ErrInvalidPayload
	}
	return nil
}

func (s *Service) nonsecret(raw []byte) error {
	check := func(value string) error {
		if operationPEM.MatchString(value) {
			return ErrSensitiveOperation
		}
		for _, secret := range s.secrets {
			if secret != "" && strings.Contains(value, secret) {
				return ErrSensitiveOperation
			}
		}
		return nil
	}
	if err := check(string(raw)); err != nil {
		return err
	}
	// JSON escaping can hide literal credential/PEM bytes on disk while the
	// decoded value still contains them. Inspect every decoded string and key.
	var inspect func(any) error
	inspect = func(value any) error {
		switch typed := value.(type) {
		case string:
			return check(typed)
		case []any:
			for _, child := range typed {
				if err := inspect(child); err != nil {
					return err
				}
			}
		case map[string]any:
			for key, child := range typed {
				if err := check(key); err != nil {
					return err
				}
				if err := inspect(child); err != nil {
					return err
				}
			}
		}
		return nil
	}
	var decoded any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&decoded) == nil {
		return inspect(decoded)
	}
	return nil
}

func (s *Service) safeEvidence(e OperationEvidence) OperationEvidence {
	// Provider strings are metadata tokens, not remote messages or payloads.
	clean := func(value string) string {
		if len(value) > 100 || s.nonsecret([]byte(value)) != nil || strings.ContainsAny(value, "\n\r\t ") {
			return ""
		}
		return value
	}
	e.State = clean(e.State)
	e.NativeStage = clean(e.NativeStage)
	e.RPCCode = clean(e.RPCCode)
	e.PreflightRPCCode = clean(e.PreflightRPCCode)
	if e.FailureKind != "failure_origin_unknown" {
		e.FailureKind = ""
	}
	if e.RejectionProof != "validator_rejected" {
		e.RejectionProof = ""
	}
	if len(e.Result) > s.maxBytes || s.nonsecret(e.Result) != nil {
		e.Result = nil
	}
	return e
}

// Preserve the established length-prefixed request digest across this package
// boundary; only the optional service computes the logical action request.
func requestFingerprint(raw []byte) string {
	hash := sha256.New()
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(raw)))
	_, _ = hash.Write(length[:])
	_, _ = hash.Write(raw)
	return hex.EncodeToString(hash.Sum(nil))
}
