// Package connector composes provider reads with the neutral catalog. Provider
// dependencies belong here rather than in internal/connector's contract.
package connector

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	core "github.com/caesium-cloud/caesium/internal/connector"
	"github.com/caesium-cloud/caesium/internal/connector/correlation"
	"github.com/caesium-cloud/caesium/internal/connector/temporal"
	"github.com/caesium-cloud/caesium/internal/jobdef/secret"
	"github.com/caesium-cloud/caesium/internal/models"
	runstorage "github.com/caesium-cloud/caesium/internal/run"
	"github.com/google/uuid"
	"go.temporal.io/sdk/client"
	"gorm.io/gorm"
)

var ErrInvalidRequest = errors.New("connector: invalid read request")

var ReadCapabilities = []core.Capability{core.CapabilityDiscovery, core.CapabilityInspection, core.CapabilityHistory, core.CapabilityRelationships}

type Runtime struct {
	db            *gorm.DB
	store         *core.Store
	cfg           *core.Config
	fingerprint   string
	activationErr error
	cursorKey     []byte
	readBudget    *core.ReadGuard
	resolver      secret.Resolver
	mu            sync.Mutex
	closed        atomic.Bool
	connections   map[string]*connection
}
type connection struct {
	config   core.Connection
	guard    *core.ReadGuard
	mu       sync.Mutex
	client   client.Client
	observer *temporal.Observer
}

// New activates only catalog configuration. Dialing and certificate reads are
// lazy so a remote outage cannot disrupt the local scheduler's startup.
func New(ctx context.Context, db *gorm.DB, cfg *core.Config, fingerprint, previous string, resolver secret.Resolver, cursorSecret string) *Runtime {
	r := &Runtime{db: db, store: core.NewStore(db), cfg: cfg, fingerprint: fingerprint, resolver: resolver, connections: map[string]*connection{}}
	call, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	r.cursorKey = []byte(cursorSecret)
	r.readBudget = core.NewReadGuard(r.store, fingerprint, core.DefaultLimits())
	if len(r.cursorKey) < 32 {
		r.activationErr = errors.New("connector cursor signing secret unavailable")
	} else {
		r.activationErr = r.store.ActivateConfiguration(call, cfg, previous, fingerprint)
	}
	for _, config := range cfg.Connections {
		if config.Enabled {
			r.connections[config.ID] = &connection{config: config, guard: core.NewReadGuard(r.store, fingerprint, config.Limits)}
		}
	}
	return r
}
func (r *Runtime) Ready(ctx context.Context) error {
	if r == nil {
		return core.ErrEpochMismatch
	}
	if r.activationErr != nil {
		return r.activationErr
	}
	call, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return r.store.RequireEpoch(call, r.fingerprint)
}
func (r *Runtime) Close() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed.Store(true)
	for _, c := range r.connections {
		c.mu.Lock()
		if c.client != nil {
			c.client.Close()
			c.client = nil
			c.observer = nil
		}
		c.mu.Unlock()
	}
}
func (r *Runtime) Fingerprint() string {
	if r == nil {
		return ""
	}
	return r.fingerprint
}

type ConnectionView struct {
	ID               string            `json:"id"`
	Provider         string            `json:"provider"`
	Scope            string            `json:"scope"`
	UIURL            string            `json:"ui_url,omitempty"`
	ConsoleRefreshMS int64             `json:"console_refresh_ms"`
	Enabled          bool              `json:"enabled"`
	Capabilities     []core.Capability `json:"capabilities"`
}
type Connections struct {
	Connections []ConnectionView `json:"connections"`
	Ready       bool             `json:"ready"`
	Fingerprint string           `json:"fingerprint"`
}

func (r *Runtime) Connections(ctx context.Context, principal string) (Connections, error) {
	out := Connections{Connections: []ConnectionView{}, Fingerprint: r.Fingerprint()}
	if err := r.Ready(ctx); err != nil {
		return out, err
	}
	if err := r.readBudget.Allow(ctx, principal); err != nil {
		return out, err
	}
	// List itself is budgeted too, on each configured connection, without RPC.
	for _, cfg := range r.cfg.Connections {
		if c := r.connections[cfg.ID]; c != nil {
			if err := c.guard.Allow(ctx, principal); err != nil {
				return out, err
			}
		}
	}
	for _, cfg := range r.cfg.Connections {
		caps := []core.Capability{}
		if cfg.Enabled {
			caps = append(caps, ReadCapabilities...)
		}
		out.Connections = append(out.Connections, ConnectionView{ID: cfg.ID, Provider: cfg.Provider, Scope: cfg.Scope, UIURL: cfg.UIURL, ConsoleRefreshMS: cfg.Limits.ConsoleRefresh.Milliseconds(), Enabled: cfg.Enabled, Capabilities: caps})
	}
	out.Ready = true
	if len(out.Connections) > core.MaxPageEntries {
		return out, core.ErrPayloadTooLarge
	}
	if _, err := bound(out, core.MaxPageMetadataBytes); err != nil {
		return out, err
	}
	return out, nil
}

func (r *Runtime) connection(ctx context.Context, id, principal string) (*connection, error) {
	if err := r.Ready(ctx); err != nil {
		return nil, err
	}
	if err := r.readBudget.Allow(ctx, principal); err != nil {
		return nil, err
	}
	c, ok := r.connections[id]
	if !ok {
		return nil, core.ErrNotFound
	}
	if err := c.guard.Allow(ctx, principal); err != nil {
		return nil, err
	}
	return c, nil
}
func (r *Runtime) observer(ctx context.Context, c *connection) (*temporal.Observer, error) {
	if err := r.Ready(ctx); err != nil {
		return nil, err
	}
	r.mu.Lock()
	closed := r.closed.Load()
	r.mu.Unlock()
	if closed {
		return nil, core.ErrEpochMismatch
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if r.closed.Load() {
		return nil, core.ErrEpochMismatch
	}
	if err := r.Ready(ctx); err != nil {
		return nil, err
	}
	if c.observer != nil {
		return c.observer, nil
	}
	apiKey := ""
	if len(c.config.SecretRefs) > 1 {
		return nil, errors.New("connector: at most one API key reference is supported")
	}
	if len(c.config.SecretRefs) == 1 {
		if r.resolver == nil {
			return nil, errors.New("connector: secret resolver unavailable")
		}
		var err error
		apiKey, err = r.resolver.Resolve(ctx, c.config.SecretRefs[0])
		if err != nil {
			return nil, errors.New("connector: credential unavailable")
		}
	}
	cfg := c.config
	cli, err := temporal.Dial(ctx, temporal.DialConfig{Endpoint: cfg.Endpoint, Namespace: cfg.Scope, APIKey: apiKey, CertificatePaths: cfg.CertificatePaths, BeforeRPC: r.Ready})
	if err != nil {
		return nil, err
	}
	// Reserve space for the public envelope, opaque ids and bound cursor. The
	// final complete response is checked again before it reaches HTTP.
	observer, err := temporal.NewObserverFromClient(cli, cfg.Scope, apiKey, cfg.Limits.RPCDeadline, cfg.Limits.MaxPageEntries, max(1, cfg.Limits.MaxPageMetadataBytes/3))
	if err != nil {
		cli.Close()
		return nil, err
	}
	c.client = cli
	c.observer = observer
	return observer, nil
}

type Reference struct {
	ConnectionID string            `json:"connection_id"`
	Coordinates  map[string]string `json:"coordinates"`
}

func httpReference(ref core.ExecutionReference) Reference {
	return Reference{ConnectionID: ref.ConnectionID, Coordinates: ref.Coordinates}
}

type Execution struct {
	ID                 string             `json:"id"`
	Reference          Reference          `json:"reference"`
	Metadata           temporal.Execution `json:"metadata"`
	Availability       string             `json:"availability"`
	Completeness       string             `json:"completeness"`
	ObservedAt         time.Time          `json:"observed_at"`
	SnapshotObservedAt *time.Time         `json:"snapshot_observed_at,omitempty"`
}

func execution(id string, x temporal.Execution) (Execution, error) {
	ref, err := core.NewExecutionReference(id, x.Coordinates)
	if err != nil {
		return Execution{}, err
	}
	complete := core.CompletenessComplete
	if x.Incomplete {
		complete = core.CompletenessPartial
	}
	return Execution{ID: core.ReferenceID(ref), Reference: httpReference(ref), Metadata: x, Availability: core.AvailabilityAvailable, Completeness: complete, ObservedAt: x.ObservedAt}, nil
}

type Page struct {
	Executions []Execution `json:"executions"`
	Cursor     string      `json:"cursor,omitempty"`
	Incomplete bool        `json:"incomplete,omitempty"`
}
type cursor struct {
	Connection string `json:"connection"`
	Epoch      string `json:"epoch"`
	Filter     string `json:"filter"`
	Token      []byte `json:"token"`
}

func (r *Runtime) decodeCursor(id, filter, value string) ([]byte, error) {
	if value == "" {
		return nil, nil
	}
	if len(value) > 16384 {
		return nil, ErrInvalidRequest
	}
	parts := strings.Split(value, ".")
	if len(parts) != 2 {
		return nil, ErrInvalidRequest
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, ErrInvalidRequest
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, ErrInvalidRequest
	}
	mac := hmac.New(sha256.New, r.cursorKey)
	_, _ = mac.Write(raw)
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return nil, ErrInvalidRequest
	}
	var c cursor
	if json.Unmarshal(raw, &c) != nil || c.Connection != id || c.Epoch != r.fingerprint || c.Filter != filter {
		return nil, ErrInvalidRequest
	}
	return c.Token, nil
}
func (r *Runtime) encodeCursor(id, filter string, token []byte) string {
	if len(token) == 0 {
		return ""
	}
	raw, _ := json.Marshal(cursor{Connection: id, Epoch: r.fingerprint, Filter: filter, Token: token})
	mac := hmac.New(sha256.New, r.cursorKey)
	_, _ = mac.Write(raw)
	value := base64.RawURLEncoding.EncodeToString(raw) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if len(value) > 16384 {
		return ""
	}
	return value
}
func bound(v any, limit int) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err == nil && len(raw) > limit {
		return nil, core.ErrPayloadTooLarge
	}
	return raw, err
}
func pageSize(size int, limits core.Limits) (int, error) {
	if size < 0 || size > limits.MaxPageEntries {
		return 0, ErrInvalidRequest
	}
	if size == 0 {
		return limits.MaxPageEntries, nil
	}
	return size, nil
}
func (r *Runtime) List(ctx context.Context, principal, id, workflowType, pageCursor string, size int) ([]byte, error) {
	c, err := r.connection(ctx, id, principal)
	if err != nil {
		return nil, err
	}
	size, err = pageSize(size, c.config.Limits)
	if err != nil {
		return nil, err
	}
	if len(workflowType) > 256 || strings.ContainsAny(workflowType, "'\\\r\n") {
		return nil, ErrInvalidRequest
	}
	filter := "list:" + workflowType + ":" + strconv.Itoa(size)
	token, err := r.decodeCursor(id, filter, pageCursor)
	if err != nil {
		return nil, err
	}
	return c.guard.Do(ctx, filter+":"+pageCursor, func(call context.Context) ([]byte, error) {
		o, err := r.observer(call, c)
		if err != nil {
			return nil, err
		}
		query := ""
		if workflowType != "" {
			query = "WorkflowType = '" + workflowType + "'"
		}
		page, err := o.List(call, query, token, size)
		if err != nil {
			return nil, err
		}
		out := Page{Executions: []Execution{}, Cursor: r.encodeCursor(id, filter, page.NextPageToken), Incomplete: page.Incomplete}
		if len(page.NextPageToken) > 0 && out.Cursor == "" {
			out.Incomplete = true
		}
		for _, x := range page.Executions {
			e, err := execution(id, x)
			if err != nil {
				return nil, err
			}
			out.Executions = append(out.Executions, e)
		}

		for {
			raw, err := bound(out, c.config.Limits.MaxPageMetadataBytes)
			if err == nil {
				return raw, nil
			}
			if !errors.Is(err, core.ErrPayloadTooLarge) || len(out.Executions) == 0 {
				return nil, err
			}
			out.Executions = out.Executions[:len(out.Executions)-1]
			out.Cursor = ""
			out.Incomplete = true
		}
	})
}
func parseReference(id, opaque string) (core.ExecutionReference, error) {
	ref, err := core.ParseReferenceID(id, opaque)
	if err != nil {
		return ref, err
	}
	if len(ref.Coordinates) != 2 || ref.Coordinates["workflow_id"] == "" || ref.Coordinates["run_id"] == "" {
		return ref, core.ErrInvalidReference
	}
	return ref, nil
}
func (r *Runtime) Detail(ctx context.Context, principal, id, opaque string) ([]byte, error) {
	c, err := r.connection(ctx, id, principal)
	if err != nil {
		return nil, err
	}
	ref, err := parseReference(id, opaque)
	if err != nil {
		return nil, err
	}
	return c.guard.Do(ctx, "detail:"+opaque, func(call context.Context) ([]byte, error) { return r.detail(call, c, ref) })
}
func (r *Runtime) detail(ctx context.Context, c *connection, ref core.ExecutionReference) ([]byte, error) {
	generation, err := r.store.NextObservationGeneration(ctx, ref)
	if err != nil {
		return nil, err
	}
	o, err := r.observer(ctx, c)
	var x temporal.Execution
	if err == nil {
		x, err = o.Describe(ctx, ref.Coordinates["workflow_id"], ref.Coordinates["run_id"])
	}
	if err != nil {
		// Epoch mismatch is never an outage and never authorizes a stale read.
		cacheCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		if fence := r.Ready(cacheCtx); fence != nil {
			return nil, fence
		}
		snapshot, cacheErr := r.store.Snapshot(cacheCtx, ref, c.config.Limits)
		if cacheErr != nil {
			return nil, err
		}
		if json.Unmarshal(snapshot.Metadata, &x) != nil {
			return nil, err
		}
		out, decodeErr := execution(c.config.ID, x)
		if decodeErr != nil {
			return nil, decodeErr
		}
		out.Availability = core.AvailabilityStale
		out.ObservedAt = snapshot.ObservedAt
		out.SnapshotObservedAt = &snapshot.ObservedAt
		return bound(out, c.config.Limits.MaxPageMetadataBytes)
	}
	if x.WorkflowID != ref.Coordinates["workflow_id"] || x.RunID != ref.Coordinates["run_id"] {
		return nil, core.ErrInvalidReference
	}
	out, err := execution(c.config.ID, x)
	if err != nil {
		return nil, err
	}
	metadata := x
	metadata.ObservedAt = time.Time{}
	raw, err := json.Marshal(metadata)
	if err != nil {
		return nil, err
	}
	stored, err := r.store.RecordDirect(ctx, r.fingerprint, core.Observation{Execution: ref, Generation: generation, SourceEventID: strconv.FormatInt(x.SourceEventID, 10), NativeStatus: x.NativeStatus, DisplayStatus: x.DisplayStatus, Availability: core.AvailabilityAvailable, Completeness: out.Completeness, Metadata: raw, SourceKind: core.SourceDirect, ObservedAt: out.ObservedAt}, c.config.Limits)
	if err != nil {
		return nil, err
	}
	out.SnapshotObservedAt = &stored.ObservedAt
	return bound(out, c.config.Limits.MaxPageMetadataBytes)
}

type History struct {
	temporal.HistoryPage
	Cursor string `json:"cursor,omitempty"`
}

func (r *Runtime) History(ctx context.Context, principal, id, opaque, pageCursor string, size int) ([]byte, error) {
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
	filter := "history:" + opaque + ":" + strconv.Itoa(size)
	token, err := r.decodeCursor(id, filter, pageCursor)
	if err != nil {
		return nil, err
	}
	return c.guard.Do(ctx, filter+":"+pageCursor, func(call context.Context) ([]byte, error) {
		o, err := r.observer(call, c)
		if err != nil {
			return nil, err
		}
		page, err := o.History(call, ref.Coordinates["workflow_id"], ref.Coordinates["run_id"], token, size)
		if err != nil {
			return nil, err
		}
		out := History{HistoryPage: page, Cursor: r.encodeCursor(id, filter, page.NextPageToken)}
		out.NextPageToken = nil
		if len(page.NextPageToken) > 0 && out.Cursor == "" {
			out.Incomplete = true
		}
		return bound(out, c.config.Limits.MaxPageMetadataBytes)
	})
}

type Relationship struct {
	Type      string    `json:"type"`
	ID        string    `json:"id"`
	Reference Reference `json:"reference"`
	EventID   int64     `json:"event_id,omitempty"`
}
type Relationships struct {
	Relationships []Relationship                        `json:"relationships"`
	Correlations  []models.ExternalExecutionCorrelation `json:"correlations"`
	Incomplete    bool                                  `json:"incomplete"`
	Cursor        string                                `json:"cursor,omitempty"`
}

func (r *Runtime) Relationships(ctx context.Context, principal, id, opaque, pageCursor string, size int) ([]byte, error) {
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
	filter := "relationships:" + opaque + ":" + strconv.Itoa(size)
	token, err := r.decodeCursor(id, filter, pageCursor)
	if err != nil {
		return nil, err
	}
	return c.guard.Do(ctx, filter+":"+pageCursor, func(call context.Context) ([]byte, error) {
		raw, err := r.detail(call, c, ref)
		if err != nil {
			return nil, err
		}
		var inspected Execution
		if json.Unmarshal(raw, &inspected) != nil {
			return nil, ErrInvalidRequest
		}
		if inspected.Availability != core.AvailabilityAvailable {
			return nil, errors.New("connector: relationships unavailable")
		}
		o, err := r.observer(call, c)
		if err != nil {
			return nil, err
		}
		page, err := o.History(call, ref.Coordinates["workflow_id"], ref.Coordinates["run_id"], token, size)
		if err != nil {
			return nil, err
		}
		out := Relationships{Relationships: []Relationship{}, Correlations: []models.ExternalExecutionCorrelation{}, Incomplete: page.Incomplete || len(page.NextPageToken) > 0, Cursor: r.encodeCursor(id, filter, page.NextPageToken)}
		relations := page.Relations
		if inspected.Metadata.Parent != nil {
			relations = append(relations, *inspected.Metadata.Parent)
		}
		seenRelations := map[string]bool{}
		for _, rel := range relations {
			if rel.WorkflowID == "" || rel.RunID == "" {
				out.Incomplete = true
				continue
			}
			to, _ := core.NewExecutionReference(id, map[string]string{"workflow_id": rel.WorkflowID, "run_id": rel.RunID})
			relationKey := rel.Type + ":" + to.OpaqueID()
			if seenRelations[relationKey] {
				continue
			}
			seenRelations[relationKey] = true
			evidence, _ := json.Marshal(rel)
			if err := r.store.RecordObservedRelation(call, r.fingerprint, ref, to, rel.Type, evidence, c.config.Limits); err != nil {
				return nil, err
			}
			out.Relationships = append(out.Relationships, Relationship{Type: rel.Type, ID: core.ReferenceID(to), Reference: httpReference(to), EventID: rel.EventID})
		}
		catalog := correlation.NewStore(r.db)
		verifier := correlation.NewCorrelator(runstorage.NewStore(r.db), catalog)
		for _, binding := range c.config.Bindings {
			if binding.Name != inspected.Metadata.WorkflowType || len(binding.ActivityAllowlist) == 0 {
				continue
			}
			evidence, readErr := o.CorrelationActivities(call, id, ref.Coordinates["workflow_id"], ref.Coordinates["run_id"], binding, token, size)
			if readErr != nil {
				out.Incomplete = true
				continue
			}
			out.Incomplete = out.Incomplete || evidence.Incomplete || len(evidence.NextPageToken) > 0
			for _, activity := range evidence.Activities {
				if err := r.Ready(call); err != nil {
					return nil, err
				}
				_, verifyErr := verifier.Verify(call, binding, activity, core.WriteBounds{ExpectedEpoch: r.fingerprint, MaxMetadataBytes: c.config.Limits.MaxPageMetadataBytes, MaxUnreferencedSnapshots: c.config.Limits.MaxUnreferencedSnapshots})
				if verifyErr != nil {
					out.Incomplete = true
				}
			}
		}
		if err := r.Ready(call); err != nil {
			return nil, err
		}
		out.Correlations, err = catalog.Correlations(call, ref)
		if err != nil {
			return nil, err
		}
		return boundRelationships(out, c.config.Limits)
	})
}
func (r *Runtime) RunLinks(ctx context.Context, principal string, jobID, runID uuid.UUID) ([]models.ExternalExecutionCorrelation, error) {
	ctx, cancel := context.WithTimeout(ctx, core.MaxRPCDeadline)
	defer cancel()
	if err := r.Ready(ctx); err != nil {
		return nil, err
	}
	if err := r.readBudget.Allow(ctx, principal); err != nil {
		return nil, err
	}
	rows, err := correlation.NewStore(r.db).RunCorrelations(ctx, jobID, runID)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	entries, metadata := core.MaxPageEntries, core.MaxPageMetadataBytes
	for _, row := range rows {
		if !seen[row.ConnectionID] {
			if c, ok := r.connections[row.ConnectionID]; ok {
				if err := c.guard.Allow(ctx, principal); err != nil {
					return nil, err
				}
				entries = min(entries, c.config.Limits.MaxPageEntries)
				metadata = min(metadata, c.config.Limits.MaxPageMetadataBytes)
			}
			seen[row.ConnectionID] = true
		}
	}
	// Historical links outlive enabled provider clients. Disabled connections
	// still impose their configured response ceilings; removed connections use
	// the process ceilings above. Neither requires contacting the provider.
	for _, c := range r.cfg.Connections {
		if seen[c.ID] && !c.Enabled {
			entries = min(entries, c.Limits.MaxPageEntries)
			metadata = min(metadata, c.Limits.MaxPageMetadataBytes)
		}
	}
	if len(rows) > entries {
		return nil, core.ErrPayloadTooLarge
	}
	if _, err := bound(map[string]any{"executions": rows}, metadata); err != nil {
		return nil, err
	}
	return rows, nil
}

func (r *Runtime) String() string { return fmt.Sprintf("connector runtime %s", r.fingerprint) }

func boundRelationships(out Relationships, limits core.Limits) ([]byte, error) {
	for len(out.Relationships)+len(out.Correlations) > limits.MaxPageEntries {
		out.Incomplete = true
		out.Cursor = ""
		if len(out.Correlations) > 0 {
			out.Correlations = out.Correlations[:len(out.Correlations)-1]
		} else {
			out.Relationships = out.Relationships[:len(out.Relationships)-1]
		}
	}
	for {
		raw, err := bound(out, limits.MaxPageMetadataBytes)
		if err == nil {
			return raw, nil
		}
		if !errors.Is(err, core.ErrPayloadTooLarge) {
			return nil, err
		}
		out.Incomplete = true
		out.Cursor = ""
		switch {
		case len(out.Correlations) > 0:
			out.Correlations = out.Correlations[:len(out.Correlations)-1]
		case len(out.Relationships) > 0:
			out.Relationships = out.Relationships[:len(out.Relationships)-1]
		default:
			return nil, err
		}
	}
}
