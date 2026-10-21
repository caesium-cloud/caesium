package jobdef

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"unicode"

	authmw "github.com/caesium-cloud/caesium/api/middleware"
	contractsvc "github.com/caesium-cloud/caesium/api/rest/service/contract"
	"github.com/caesium-cloud/caesium/internal/authz"
	internalcontract "github.com/caesium-cloud/caesium/internal/contract"
	"github.com/caesium-cloud/caesium/internal/freshness"
	internaljobdef "github.com/caesium-cloud/caesium/internal/jobdef"
	jobdiff "github.com/caesium-cloud/caesium/internal/jobdef/diff"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/db"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"gorm.io/gorm"
)

// hiddenJob stands in for a job alias a namespace-confined caller may not see.
const hiddenJob = "a job in another namespace"

// catalog is the connection lint and diff read the persisted world from;
// handler tests swap it for a test database.
var catalog = db.Connection

// readScope is a namespace-confined caller's view of the catalog for server
// lint and diff (identity-and-access design §8.3, plan item B2). Validation
// still runs over every job, so a confined caller gets the same verdict as a
// cluster caller, but what the response says about other namespaces' jobs is
// restricted: the diff baseline holds only the caller's namespaces, contract
// findings keep only edges between jobs the caller can read, and validator
// errors name a foreign job only as "a job in another namespace".
//
// A nil *readScope is an unrestricted caller (a cluster viewer, or auth
// disabled); every method then returns its input unchanged.
type readScope struct {
	db *gorm.DB
	// allowed is the namespaces the caller may read; nil when it may read
	// every namespace (a job-scoped key holding its role at "*"), in which
	// case only aliases restricts it.
	allowed []string
	// aliases is the caller's key job-alias scope (B9): a job outside it is
	// foreign even in a readable namespace. nil when the key is unscoped.
	aliases []string
	// visible caches alias -> whether the caller may see it.
	visible map[string]bool
	// sharesForeign marks aliases whose stored trigger also serves a live job
	// in a namespace the caller cannot read (that trigger is unreadable to it).
	sharesForeign map[string]bool
	// requested maps the request's own aliases to their namespaces.
	requested map[string]string
	// strict applies the design's foreign-edge boundary (§8.3) to validator
	// errors: an error touching a job the caller cannot read names no dataset
	// either. Apply sets it; lint and diff keep B2's rendering.
	strict bool
}

// newReadScope returns the confined view for the request, or nil. An
// authenticated principal whose viewer grants do not cover "*", or whose API
// key is scoped to job aliases (B9), is restricted; with auth disabled there
// is no principal and nothing changes. A principal with no grants at all, or
// an undecodable key scope, is restricted to nothing (fail closed).
func newReadScope(c *echo.Context, conn *gorm.DB, defs []schema.Definition) *readScope {
	principal := authmw.GetPrincipal(c)
	if principal == nil {
		return nil
	}
	allowed, aliases, err := authmw.ReadConfinement(principal)
	if err != nil {
		log.Warn("jobdef read scope: undecodable key scope; restricting the caller to nothing", "error", err)
		return newReadScopeForKey([]string{}, []string{}, conn, defs)
	}
	return newReadScopeForKey(allowed, aliases, conn, defs)
}

// newReadScopeFor is newReadScope for an allowed set (nil: unrestricted).
func newReadScopeFor(allowed []string, conn *gorm.DB, defs []schema.Definition) *readScope {
	return newReadScopeForKey(allowed, nil, conn, defs)
}

// newReadScopeForKey is newReadScopeFor with a key's job-alias scope too
// (nil: unscoped). It is nil when neither restricts the caller. An empty,
// non-nil allowed set or alias scope restricts the caller to nothing.
func newReadScopeForKey(allowed, aliases []string, conn *gorm.DB, defs []schema.Definition) *readScope {
	if allowed == nil && aliases == nil {
		return nil
	}
	scope := &readScope{
		db:            conn,
		allowed:       cloneKeepingEmpty(allowed),
		aliases:       cloneKeepingEmpty(aliases),
		visible:       make(map[string]bool),
		sharesForeign: make(map[string]bool),
		requested:     make(map[string]string, len(defs)),
	}
	for i := range defs {
		scope.requested[strings.TrimSpace(defs[i].Metadata.Alias)] = models.NamespaceOrDefault(defs[i].Metadata.Namespace)
	}
	return scope
}

// cloneKeepingEmpty copies values, trimmed; nil stays nil (unrestricted)
// and an empty set stays a non-nil empty set (restricted to nothing).
func cloneKeepingEmpty(values []string) []string {
	if values == nil {
		return nil
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		out = append(out, strings.TrimSpace(value))
	}
	return out
}

// allows reports whether the caller may read namespace.
func (s *readScope) allows(namespace string) bool {
	return s.allowed == nil || slices.Contains(s.allowed, models.NamespaceOrDefault(namespace))
}

// readable reports whether the caller may read the job alias in namespace:
// the namespace is readable and the alias is inside the key's scope.
func (s *readScope) readable(alias, namespace string) bool {
	return s.allows(namespace) && s.aliasInKeyScope(alias)
}

// aliasInKeyScope reports whether alias, exactly as given, is inside the
// key's alias scope (always, for an unscoped key). An alias with surrounding
// whitespace names a distinct job, never the scoped one.
func (s *readScope) aliasInKeyScope(alias string) bool {
	return s.aliases == nil || (alias == strings.TrimSpace(alias) && slices.Contains(s.aliases, alias))
}

// baselineContext restricts the diff baseline (internal/jobdef/diff
// LoadDatabaseSpecs reads it through authz.NamespaceFilter) to the caller's
// namespaces.
func (s *readScope) baselineContext(ctx context.Context) context.Context {
	if s == nil || s.allowed == nil {
		return ctx
	}
	return authz.WithAllowedNamespaces(ctx, s.allowed)
}

// resolve records, for every alias not yet known, whether the caller may see
// it, and whether its stored trigger is shared outside the caller's
// namespaces. A job is visible when its jobs row (retired rows included) is in
// an allowed namespace; one of the request's own definitions additionally
// needs its requested namespace allowed, and a request alias with no row yet
// is visible on that alone. The stored row is read here, after the handler
// derived what it reports, so a job moved out of the caller's namespaces
// since the middleware authorised the request is hidden, not trusted. An
// alias with no row (and not in the request), or any lookup failure, is
// treated as foreign.
func (s *readScope) resolve(ctx context.Context, aliases []string) {
	var lookup []string
	for _, alias := range aliases {
		alias = strings.TrimSpace(alias)
		if _, done := s.visible[alias]; done || alias == "" {
			continue
		}
		namespace, requested := s.requested[alias]
		if requested && !s.readable(alias, namespace) {
			s.visible[alias] = false
			continue
		}
		s.visible[alias] = requested
		lookup = append(lookup, alias)
	}
	if len(lookup) == 0 || s.db == nil {
		return
	}
	failClosed := func(err error) {
		log.Warn("jobdef read scope: catalog lookup failed; treating the aliases as foreign", "error", err)
		for _, alias := range lookup {
			s.visible[alias] = false
		}
	}
	// The aliases resolved here are trimmed (contract derivation, the
	// request keys and findings all trim), so read every stored row whose
	// alias trims to one of them: the exact rows and every row whose alias
	// carries surrounding whitespace.
	nonCanonical, nonCanonicalArgs := nonCanonicalAliasClause()
	var rows []models.Job
	if err := s.db.WithContext(ctx).Unscoped().Select("alias", "namespace", "trigger_id").
		Where("alias IN ? OR ("+nonCanonical+")", append([]any{lookup}, nonCanonicalArgs...)...).Find(&rows).Error; err != nil {
		failClosed(err)
		return
	}
	wanted := make(map[string]struct{}, len(lookup))
	for _, alias := range lookup {
		wanted[alias] = struct{}{}
	}
	rowsByAlias := make(map[string][]models.Job, len(lookup))
	for _, row := range rows {
		alias := strings.TrimSpace(row.Alias)
		if _, ok := wanted[alias]; ok {
			rowsByAlias[alias] = append(rowsByAlias[alias], row)
		}
	}
	aliasesByTrigger := make(map[uuid.UUID][]string, len(rows))
	for alias, matches := range rowsByAlias {
		// The job is identified by its trimmed alias only when exactly one
		// stored row (live or retired) carries it: a retired "consumer" and
		// a live "consumer " are two jobs the trimmed name cannot tell
		// apart, so neither is shown (PR #668 review r8). The one row's raw
		// alias and namespace decide; a key scope never covers a stored
		// alias with surrounding whitespace.
		if len(matches) != 1 || !s.readable(matches[0].Alias, matches[0].Namespace) {
			s.visible[alias] = false
			continue
		}
		s.visible[alias] = true
		aliasesByTrigger[matches[0].TriggerID] = append(aliasesByTrigger[matches[0].TriggerID], alias)
	}
	if len(aliasesByTrigger) == 0 {
		return
	}
	var peers []models.Job
	if err := s.db.WithContext(ctx).Select("id", "alias", "trigger_id", "namespace").Where("trigger_id IN ?", slices.Collect(maps.Keys(aliasesByTrigger))).Find(&peers).Error; err != nil {
		failClosed(err)
		return
	}
	for _, peer := range peers {
		if !s.readable(peer.Alias, peer.Namespace) {
			for _, alias := range aliasesByTrigger[peer.TriggerID] {
				s.sharesForeign[alias] = true
			}
		}
	}
}

var (
	nonCanonicalAliasOnce sync.Once
	nonCanonicalAliasSQL  string
	nonCanonicalAliasArgs []any
)

// nonCanonicalAliasClause is a SQL condition (with its arguments) selecting
// the jobs rows whose alias starts or ends with a character strings.TrimSpace
// strips (every unicode.IsSpace rune). SQL TRIM strips ASCII spaces only.
func nonCanonicalAliasClause() (string, []any) {
	nonCanonicalAliasOnce.Do(func() {
		var parts []string
		for r := rune(0); r <= unicode.MaxRune; r++ {
			if !unicode.IsSpace(r) {
				continue
			}
			parts = append(parts, "alias LIKE ?", "alias LIKE ?")
			nonCanonicalAliasArgs = append(nonCanonicalAliasArgs, string(r)+"%", "%"+string(r))
		}
		nonCanonicalAliasSQL = strings.Join(parts, " OR ")
	})
	return nonCanonicalAliasSQL, slices.Clone(nonCanonicalAliasArgs)
}

func (s *readScope) aliasVisible(ctx context.Context, alias string) bool {
	if s.aliases != nil && alias != strings.TrimSpace(alias) {
		return false
	}
	s.resolve(ctx, []string{alias})
	return s.visible[strings.TrimSpace(alias)]
}

func (s *readScope) aliasOrHidden(ctx context.Context, alias string) string {
	if s.aliasVisible(ctx, alias) {
		return alias
	}
	return hiddenJob
}

// errorMessage renders a cross-job validator error. Unrestricted callers get
// err.Error() verbatim; a confined caller gets the same message with every
// job alias (and the dataset or object namespace it would reveal) outside its
// namespaces redacted. Errors that name no stored job pass through.
func (s *readScope) errorMessage(ctx context.Context, err error) string {
	if err == nil {
		return ""
	}
	if s == nil {
		return err.Error()
	}
	if cycle, ok := errors.AsType[*internaljobdef.TriggerChainCycleError](err); ok {
		s.resolve(ctx, cycle.Path)
		path := make([]string, len(cycle.Path))
		for i, alias := range cycle.Path {
			path[i] = s.aliasOrHidden(ctx, alias)
		}
		return fmt.Sprintf("%s: %s", internaljobdef.ErrTriggerChainCycle, strings.Join(path, " -> "))
	}
	if jobErr, ok := errors.AsType[*internaljobdef.TriggerChainJobError](err); ok {
		if s.aliasVisible(ctx, jobErr.Alias) {
			return jobErr.Error()
		}
		return fmt.Sprintf("%s has an invalid trigger configuration", hiddenJob)
	}
	if graphErr, ok := errors.AsType[*freshness.GraphError](err); ok {
		return s.graphErrorMessage(ctx, graphErr)
	}
	if refErr, ok := errors.AsType[*internaljobdef.ObjectRefNamespaceError](err); ok {
		if refErr.ObjectNamespace == models.ClusterNamespace || s.allows(refErr.ObjectNamespace) {
			return refErr.Error()
		}
		return refErr.Redacted()
	}
	// Trigger tenancy (B9): a confined caller is told which of its own
	// webhook paths or jobs is affected, never by whom.
	if msg := internaljobdef.RedactedTenancyMessage(err); msg != "" {
		return msg
	}
	return err.Error()
}

// findingVisible reports whether a confined caller may read a contract
// finding: both of its jobs, and for an inferred edge (derived from a
// consumer's trigger) neither trigger shared with a job it cannot read, as
// filterGraph decides for lint. Unresolvable aliases are foreign.
func (s *readScope) findingVisible(ctx context.Context, finding internalcontract.ContractFinding) bool {
	if s == nil {
		return true
	}
	producer, consumer := strings.TrimSpace(finding.Producer), strings.TrimSpace(finding.Consumer)
	if producer == "" || consumer == "" {
		return false
	}
	if s.aliases != nil && (producer != finding.Producer || consumer != finding.Consumer) {
		// Not the trimmed alias a key scope names (see aliasInKeyScope).
		return false
	}
	s.resolve(ctx, []string{producer, consumer})
	if !s.visible[producer] || !s.visible[consumer] {
		return false
	}
	if internalcontract.EdgeClass(finding.EdgeClass) == internalcontract.EdgeClassInferred && (s.sharesForeign[producer] || s.sharesForeign[consumer]) {
		return false
	}
	return true
}

// contractAudience is the importer's ContractAudience for a confined apply:
// bound to the apply transaction (whose REPEATABLE READ snapshot on
// PostgreSQL also produced the findings), with the visibility caches cleared
// so a retried attempt never reuses a verdict from an earlier snapshot.
func (s *readScope) contractAudience(ctx context.Context) func(tx *gorm.DB) func(internalcontract.ContractFinding) bool {
	if s == nil {
		return nil
	}
	return func(tx *gorm.DB) func(internalcontract.ContractFinding) bool {
		s.db = tx
		clear(s.visible)
		clear(s.sharesForeign)
		return func(finding internalcontract.ContractFinding) bool { return s.findingVisible(ctx, finding) }
	}
}

func (s *readScope) graphErrorMessage(ctx context.Context, e *freshness.GraphError) string {
	s.resolve(ctx, e.Jobs)
	var visible []string
	hidden := 0
	for _, alias := range e.Jobs {
		if s.aliasVisible(ctx, alias) {
			visible = append(visible, alias)
		} else {
			hidden++
		}
	}
	// The dataset is named only when one of the caller's own jobs declares it
	// (and, for apply, only when no foreign job is involved at all).
	dataset := "a dataset"
	if len(visible) > 0 && (!s.strict || hidden == 0) {
		dataset = fmt.Sprintf("%q", e.Dataset)
	}
	switch {
	case errors.Is(e, freshness.ErrDatasetMultipleProducers):
		owners := slices.Clone(visible)
		if hidden > 0 {
			owners = append(owners, fmt.Sprintf("%d job(s) in other namespaces", hidden))
		}
		return fmt.Sprintf("%s: %s produced by %s", freshness.ErrDatasetMultipleProducers, dataset, strings.Join(owners, ", "))
	case errors.Is(e, freshness.ErrDatasetUnresolvedConsumes):
		consumer := hiddenJob
		if len(visible) > 0 {
			consumer = fmt.Sprintf("job %q", visible[0])
		}
		return fmt.Sprintf("%s: %s consumes %s which is not produced by any job or declared as a source", freshness.ErrDatasetUnresolvedConsumes, consumer, dataset)
	default:
		path := make([]string, len(e.Jobs))
		for i, alias := range e.Jobs {
			path[i] = s.aliasOrHidden(ctx, alias)
		}
		return fmt.Sprintf("%s: %s", e.Err, strings.Join(path, " -> "))
	}
}

// afterCatalogRead, when set (tests only), runs inside a confined caller's
// snapshot between reading the catalog (a validator's verdict, the contract
// graph) and deciding what the caller may see of it, where a concurrent apply
// would interleave.
var afterCatalogRead func()

// withSnapshot runs fn over the catalog. For a confined caller, fn and every
// ownership check the scope makes while it runs read one read-only snapshot
// (internal/jobdef/diff ReadSnapshot), and the visibility caches are cleared
// on entry: what fn derives from the catalog (a validator error, the contract
// graph) and what the caller is then shown of it come from the same catalog
// state. A concurrent apply that moves a job between namespaces and edits it
// can never pair a namespace verdict with data from another moment, in either
// order. For an unrestricted caller (nil scope), or without a catalog, fn
// runs on conn exactly as before.
func (s *readScope) withSnapshot(ctx context.Context, conn *gorm.DB, fn func(conn *gorm.DB) error) error {
	if s == nil || s.db == nil {
		return fn(conn)
	}
	outer := s.db
	defer func() { s.db = outer }()
	return jobdiff.ReadSnapshot(ctx, outer, func(tx *gorm.DB) error {
		s.db = tx
		clear(s.visible)
		clear(s.sharesForeign)
		return fn(tx)
	})
}

// validate runs a cross-job validator over the catalog (conn for an
// unrestricted caller) and returns its error as the caller may see it
// (errorMessage), or "" when it passes. For a confined caller the validator
// and the redaction of its error share one snapshot (withSnapshot); if that
// snapshot cannot be read, validation fails closed with a generic message.
func (s *readScope) validate(ctx context.Context, conn *gorm.DB, validator func(conn *gorm.DB) error) string {
	var message string
	if err := s.withSnapshot(ctx, conn, func(conn *gorm.DB) error {
		if err := validator(conn); err != nil {
			if afterCatalogRead != nil {
				afterCatalogRead()
			}
			message = s.errorMessage(ctx, err)
		}
		return nil
	}); err != nil {
		log.Warn("jobdef read scope: catalog snapshot failed; failing validation closed", "error", err)
		return "failed to read the catalog for validation"
	}
	return message
}

// contractGraph derives the contract graph for defs, narrows it with view
// (lint scopes it to the request; nil keeps it whole), and returns that graph
// together with the part of it the caller may read. For an unrestricted
// caller both are the same graph, derived exactly as before. For a confined
// caller the derivation (the stored producer schemas and trigger
// configurations the findings are built from) and the ownership checks that
// filter it share one snapshot (withSnapshot).
func (s *readScope) contractGraph(ctx context.Context, defs []schema.Definition, view func(internalcontract.Graph) internalcontract.Graph) (scoped, visible internalcontract.Graph, err error) {
	err = s.withSnapshot(ctx, nil, func(conn *gorm.DB) error {
		svc := contractsvc.NewWithDatabase(ctx, conn)
		if conn == nil {
			svc = contractsvc.New(ctx)
		}
		graph, err := svc.Graph("", defs)
		if err != nil {
			return err
		}
		scoped = *graph
		if view != nil {
			scoped = view(scoped)
		}
		if afterCatalogRead != nil {
			afterCatalogRead()
		}
		visible = s.filterGraph(ctx, scoped)
		return nil
	})
	if err != nil {
		return internalcontract.Graph{}, internalcontract.Graph{}, err
	}
	return scoped, visible, nil
}

// filterGraph keeps only the contract edges whose producer and consumer jobs
// the caller can both read (and, for an inferred edge, whose triggers it can
// read), and the nodes those edges (or the caller's own jobs) reference.
// Every derived edge is job -> job (internal/contract).
func (s *readScope) filterGraph(ctx context.Context, graph internalcontract.Graph) internalcontract.Graph {
	if s == nil {
		return graph
	}
	aliases := make([]string, 0, 2*len(graph.Edges)+len(graph.Nodes))
	for _, edge := range graph.Edges {
		aliases = append(aliases, jobNodeAlias(edge.From), jobNodeAlias(edge.To))
	}
	for _, node := range graph.Nodes {
		if node.Kind == internalcontract.NodeKindJob {
			aliases = append(aliases, jobNodeAlias(node.ID))
		}
	}
	s.resolve(ctx, aliases)

	out := internalcontract.Graph{Nodes: []internalcontract.Node{}, Edges: []internalcontract.Edge{}}
	var datasets []internalcontract.DatasetRef
	for _, edge := range graph.Edges {
		from, to := jobNodeAlias(edge.From), jobNodeAlias(edge.To)
		if from == "" || to == "" || !s.visible[from] || !s.visible[to] {
			continue
		}
		// An inferred edge is derived from a trigger's configuration (its
		// event pattern and paramMapping); one shared with a job the caller
		// cannot read is unreadable to it, so is everything derived from it.
		if edge.Class == internalcontract.EdgeClassInferred && (s.sharesForeign[from] || s.sharesForeign[to]) {
			continue
		}
		out.Edges = append(out.Edges, edge)
		if edge.Dataset != nil {
			datasets = append(datasets, *edge.Dataset)
		}
	}
	for _, node := range graph.Nodes {
		switch {
		case node.Kind == internalcontract.NodeKindJob:
			if alias := jobNodeAlias(node.ID); alias != "" && s.visible[alias] {
				out.Nodes = append(out.Nodes, node)
			}
		case node.Dataset != nil && slices.Contains(datasets, *node.Dataset):
			out.Nodes = append(out.Nodes, node)
		}
	}
	return out
}

// jobNodeAlias inverts internal/contract JobNodeID; "" for a non-job node.
func jobNodeAlias(nodeID string) string {
	alias, ok := strings.CutPrefix(strings.TrimSpace(nodeID), "job:")
	if !ok {
		return ""
	}
	return strings.TrimSpace(alias)
}
