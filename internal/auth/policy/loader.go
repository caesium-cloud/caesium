package policy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/caesium-cloud/caesium/internal/metrics"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/log"
)

// This file is the runtime half of the package: the holder that loads the
// policy file, serves its current revision to every reader without locks, and
// reloads it on a poll or on request.

const (
	// MaxFileBytes bounds a policy file read; a larger file is invalid.
	MaxFileBytes = 1 << 20

	// MaxEncodedNamespaceBytes bounds one namespace's settings as JSON. A
	// run owner carries a namespace's settings to its workers (identity-and-
	// access §10.1), and YAML aliases can expand a small file far beyond
	// MaxFileBytes, so the bound is on the decoded value: a policy whose
	// namespace exceeds it is invalid rather than untransportable.
	MaxEncodedNamespaceBytes = 6 * MaxFileBytes

	// SourceFile and SourceEnv name where a revision came from: the policy
	// file, or the CAESIUM_AUTH_ROLE_MAPPING translation when no file is set.
	SourceFile = "file"
	SourceEnv  = "env"

	// TriggerPoll and TriggerAPI name what started a reload attempt.
	TriggerPoll = "poll"
	TriggerAPI  = "api"

	// PollActor is the audit actor of reloads the poller performs.
	PollActor = "system:policy-loader"

	knownGroupsTimeout = 5 * time.Second

	// settleAttempts and settleDelay bound how long the startup load and an
	// API reload retry a read that the file changed under.
	settleAttempts = 5
	settleDelay    = 100 * time.Millisecond
)

// ErrNoPolicyFile reports a reload request on a holder with no policy file.
var ErrNoPolicyFile = errors.New("no access policy file is configured (CAESIUM_AUTH_POLICY_FILE is unset)")

var errFileChanging = errors.New("the file kept changing while it was read; retry once it is fully written")

// Revision is one immutable, validated policy revision. Readers must not
// modify it or anything it points to.
type Revision struct {
	Policy *AccessPolicy
	// Hash is the hex SHA-256 of the exact file bytes, so two nodes reporting
	// the same hash serve the same file. It is empty for the env translation.
	Hash     string
	Source   string
	LoadedAt time.Time
	// Warnings are the non-fatal lint findings computed when the revision was
	// loaded (or refreshed by an unchanged reload).
	Warnings []string
}

// ReloadAttempt records the outcome of the most recent reload attempt.
type ReloadAttempt struct {
	At      time.Time
	Trigger string
	OK      bool
	Changed bool
	// Revision is the hash of the bytes the attempt read; empty when the file
	// could not be read in full.
	Revision string
	Error    string
}

// ReloadEvent describes one audited reload attempt. Successful attempts that
// change nothing are reported only for the API trigger; a poll that finds the
// loaded hash reports nothing, and a poll reports a given bad file once.
type ReloadEvent struct {
	At       time.Time
	Trigger  string
	Actor    string
	SourceIP string
	Path     string
	OK       bool
	Changed  bool
	// PreviousRevision is the hash that was current before the attempt.
	PreviousRevision string
	// Revision is the hash now current on success, or the hash of the
	// rejected bytes on failure (empty when the file was unreadable).
	Revision string
	Error    string
}

// ReloadRequest identifies who asked for an API-triggered reload.
type ReloadRequest struct {
	Actor    string
	SourceIP string
}

// ReloadResult is the outcome of a reload: the revision now current (the
// retained one on failure) and whether it replaced the previous one.
type ReloadResult struct {
	Changed  bool
	Previous *Revision
	Current  *Revision
}

// KnownGroupsFunc returns every IdP group stored on a known user.
type KnownGroupsFunc func(context.Context) (map[string]struct{}, error)

// HolderOptions configures a file-backed holder.
type HolderOptions struct {
	Path string
	// DefaultRole is granted cluster-wide only when no binding matches
	// (CAESIUM_AUTH_DEFAULT_ROLE). "" means no fallback.
	DefaultRole models.Role
	// TrustedEmailProvider reports that an enabled identity provider can
	// supply a verified email (OIDC with email_verified=true). Without one,
	// user bindings can never match and the loader warns.
	TrustedEmailProvider bool
	// KnownGroups, when set, backs the unmatched-group warning. Its errors
	// are logged and skip the check; they never fail a load.
	KnownGroups KnownGroupsFunc
	// Now overrides the clock (tests).
	Now func() time.Time
}

// Holder serves the current policy revision. Reads are a single atomic load;
// reload writers (the poller and the API trigger) are serialised so that a
// slow attempt can never publish an older file over a newer revision.
type Holder struct {
	path         string
	defaultRole  models.Role
	trustedEmail bool
	knownGroups  KnownGroupsFunc
	now          func() time.Time
	// resolver, when set, answers Resolve instead of the revision (the env
	// translation keeps RoleMapper's group trimming).
	resolver Source

	current atomic.Pointer[Revision]
	attempt atomic.Pointer[ReloadAttempt]

	// mu serialises reload writers: read, validate, publish and the
	// bookkeeping below all happen under it. Readers never take it.
	mu       sync.Mutex
	observer func(ReloadEvent)
	// lastFailedKey identifies the bytes (or read error) of the last failed
	// attempt, so the poller reports one bad file once, not every interval.
	lastFailedKey string
	// pendingKey identifies a change the poller has seen once. It acts only
	// when the next poll reads the same content again, so a file rewritten
	// in place is never published (or reported) from a half-written read.
	pendingKey string
	// afterParse is a test hook run under mu once a changed file has parsed.
	afterParse func(trigger string)
	// afterRead is a test hook run between reading the file and re-checking
	// that it did not change during the read.
	afterRead func()
}

// EmptyPolicy returns the policy in force when nothing is configured: the
// implicit default namespace and no bindings.
func EmptyPolicy() *AccessPolicy {
	return &AccessPolicy{
		APIVersion: APIVersionV1,
		Kind:       KindAccessPolicy,
		Namespaces: map[string]Namespace{DefaultNamespace: {}},
		Bindings:   []Binding{},
	}
}

// NewEnvHolder returns a holder for the no-file configuration. It renders view
// (the translated env mapping, or EmptyPolicy) and resolves grants through
// resolver exactly as resolver would; a nil resolver grants nothing. It cannot
// be reloaded.
func NewEnvHolder(view *AccessPolicy, defaultRole models.Role, resolver Source) *Holder {
	if view == nil {
		view = EmptyPolicy()
	}
	h := &Holder{defaultRole: defaultRole, resolver: resolver, now: time.Now}
	if resolver == nil {
		h.resolver = noGrants{}
	}
	h.current.Store(&Revision{Policy: view, Source: SourceEnv, LoadedAt: h.now().UTC(), Warnings: []string{}})
	return h
}

type noGrants struct{}

func (noGrants) Resolve([]string, string) Grants { return Grants{} }

// LoadFile reads, parses and fully validates the policy file and returns a
// holder serving it. Every error names the file.
func LoadFile(ctx context.Context, opts HolderOptions) (*Holder, error) {
	if strings.TrimSpace(opts.Path) == "" {
		return nil, errors.New("access policy file path is empty")
	}
	if opts.DefaultRole != "" && !models.ValidRole(string(opts.DefaultRole)) {
		return nil, fmt.Errorf("invalid default role %q", opts.DefaultRole)
	}
	h := &Holder{
		path:         opts.Path,
		defaultRole:  opts.DefaultRole,
		trustedEmail: opts.TrustedEmailProvider,
		knownGroups:  opts.KnownGroups,
		now:          opts.Now,
	}
	if h.now == nil {
		h.now = time.Now
	}
	read := h.readSettled(ctx)
	if read.err != nil {
		return nil, h.fileError(read.err)
	}
	p, err := Parse(read.data)
	if err != nil {
		return nil, h.fileError(err)
	}
	rev := &Revision{Policy: p, Hash: read.hash, Source: SourceFile, LoadedAt: h.now().UTC()}
	rev.Warnings = h.warnings(ctx, p)
	h.current.Store(rev)
	log.Info("access policy loaded", "path", h.path, "revision", rev.Hash,
		"namespaces", len(p.Namespaces), "bindings", len(p.Bindings))
	h.logWarnings(rev)
	return h, nil
}

// Resolve implements Source against the current revision. It is safe on a nil
// holder and for concurrent use, and returns a fresh map on every call.
func (h *Holder) Resolve(groups []string, email string) Grants {
	if h == nil {
		return Grants{}
	}
	if h.resolver != nil {
		grants := h.resolver.Resolve(groups, email)
		if grants == nil {
			return Grants{}
		}
		return grants
	}
	rev := h.current.Load()
	if rev == nil {
		return Grants{}
	}
	return (&Static{Policy: rev.Policy, DefaultRole: h.defaultRole}).Resolve(groups, email)
}

// Current returns the revision being served. It is nil only for a nil holder.
func (h *Holder) Current() *Revision {
	if h == nil {
		return nil
	}
	return h.current.Load()
}

// LastReload returns the most recent reload attempt, or nil before the first.
func (h *Holder) LastReload() *ReloadAttempt {
	if h == nil {
		return nil
	}
	return h.attempt.Load()
}

// FileBacked reports whether the holder serves a policy file (and so can be
// reloaded).
func (h *Holder) FileBacked() bool {
	return h != nil && h.path != ""
}

// Path returns the policy file path, or "" for the env translation.
func (h *Holder) Path() string {
	if h == nil {
		return ""
	}
	return h.path
}

// DefaultRole returns the configured fallback role, or "" when there is none.
func (h *Holder) DefaultRole() models.Role {
	if h == nil {
		return ""
	}
	return h.defaultRole
}

// Namespace returns a copy of the settings of namespace name and whether the
// current revision declares it. The default namespace is always declared.
// A nil holder declares only the default namespace, with no settings.
func (h *Holder) Namespace(name string) (Namespace, bool) {
	return h.Current().Namespace(name)
}

// Namespace is Holder.Namespace on this one revision, so a caller that needs
// several settings and the revision hash together reads them all from the
// same policy (a nil revision is the empty policy).
func (r *Revision) Namespace(name string) (Namespace, bool) {
	if r == nil || r.Policy == nil {
		return Namespace{}, name == DefaultNamespace
	}
	ns, declared := r.Policy.Namespaces[name]
	if !declared {
		return Namespace{}, name == DefaultNamespace
	}
	return ns.clone(), true
}

func (ns Namespace) clone() Namespace {
	out := Namespace{}
	if ns.Kubernetes != nil {
		k := *ns.Kubernetes
		out.Kubernetes = &k
	}
	if ns.Secrets != nil {
		out.Secrets = &SecretRules{Allow: slices.Clone(ns.Secrets.Allow)}
		if out.Secrets.Allow == nil {
			out.Secrets.Allow = []string{}
		}
	}
	if ns.Quotas != nil {
		q := *ns.Quotas
		out.Quotas = &q
	}
	return out
}

// SetObserver installs the callback that audits reload attempts. It is called
// with the writer lock held, so events arrive in publish order.
func (h *Holder) SetObserver(observer func(ReloadEvent)) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.observer = observer
}

// Run polls the file every interval until ctx is done. A poll acts only when
// the file's bytes hash differently from the loaded revision (mtimes are not
// trusted: a ConfigMap update swaps symlinks and can keep one), and only once
// the change is stable: a read the file changed under is discarded, and a
// change is published (or reported as a failure) when two consecutive polls
// read the same bytes. A change therefore takes effect within two intervals.
func (h *Holder) Run(ctx context.Context, interval time.Duration) {
	if !h.FileBacked() {
		return
	}
	if interval <= 0 {
		log.Error("access policy poller not started: the reload interval must be positive", "path", h.path, "interval", interval)
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			h.poll(ctx)
		}
	}
}

// Reload re-reads the file on request and acts on it without waiting for a
// second poll (a read the file changed under is retried briefly). It always
// reports one event, even when the file is unchanged, and on failure returns
// the retained revision.
func (h *Holder) Reload(ctx context.Context, req ReloadRequest) (ReloadResult, error) {
	if !h.FileBacked() {
		cur := h.Current()
		return ReloadResult{Previous: cur, Current: cur}, ErrNoPolicyFile
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.apply(ctx, h.readSettled(ctx), TriggerAPI, req.Actor, req.SourceIP)
}

func (h *Holder) poll(ctx context.Context) {
	h.mu.Lock()
	defer h.mu.Unlock()
	read := h.readFile()
	if read.unstable {
		// A writer changed the file while it was read; look again next tick.
		// Forget any earlier sighting too: two matching reads only prove the
		// file is stable when no write was observed between them.
		h.pendingKey = ""
		return
	}
	cur := h.current.Load()
	if read.err == nil && read.hash == cur.Hash {
		// The file matches the served revision. Clear a failure recorded
		// for content that has since been reverted; nothing is audited.
		h.pendingKey = ""
		if last := h.attempt.Load(); last != nil && !last.OK {
			h.lastFailedKey = ""
			h.attempt.Store(&ReloadAttempt{At: h.now().UTC(), Trigger: TriggerPoll, OK: true, Revision: read.hash})
		}
		return
	}
	key := read.contentKey()
	if key == h.lastFailedKey {
		h.pendingKey = ""
		return
	}
	if key != h.pendingKey {
		// First sighting of this content: an in-place rewrite (cp, cat >,
		// an editor) may still be mid-write. Act only if the next poll
		// reads the same bytes.
		h.pendingKey = key
		return
	}
	_, _ = h.apply(ctx, read, TriggerPoll, PollActor, "")
}

func (h *Holder) readFile() fileRead {
	return readPolicyFile(h.path, h.afterRead)
}

// readSettled reads the file for the startup load and the API trigger, which
// act on one read instead of waiting for a second poll: a read the file
// changed under is retried briefly, and a file that keeps changing fails.
func (h *Holder) readSettled(ctx context.Context) fileRead {
	if ctx == nil {
		ctx = context.Background()
	}
	for attempt := 1; ; attempt++ {
		read := h.readFile()
		if !read.unstable {
			return read
		}
		if attempt == settleAttempts {
			return fileRead{err: errFileChanging, errKey: errFileChanging.Error()}
		}
		select {
		case <-ctx.Done():
			return fileRead{err: ctx.Err(), errKey: ctx.Err().Error()}
		case <-time.After(settleDelay):
		}
	}
}

// apply validates read and publishes it. The caller holds h.mu.
func (h *Holder) apply(ctx context.Context, read fileRead, trigger, actor, sourceIP string) (ReloadResult, error) {
	h.pendingKey = ""
	prev := h.current.Load()
	now := h.now().UTC()
	event := ReloadEvent{
		At: now, Trigger: trigger, Actor: actor, SourceIP: sourceIP, Path: h.path,
		PreviousRevision: prev.Hash, Revision: read.hash,
	}

	var p *AccessPolicy
	err := read.err
	if err == nil && read.hash != prev.Hash {
		p, err = Parse(read.data)
		if err == nil && h.afterParse != nil {
			h.afterParse(trigger)
		}
	}
	if err != nil {
		err = h.fileError(err)
		h.lastFailedKey = read.contentKey()
		h.attempt.Store(&ReloadAttempt{At: now, Trigger: trigger, Revision: read.hash, Error: err.Error()})
		metrics.AuthPolicyReloadFailuresTotal.WithLabelValues(trigger).Inc()
		log.Error("access policy reload failed; keeping the last good revision",
			"path", h.path, "trigger", trigger, "rejected_revision", read.hash,
			"retained_revision", prev.Hash, "error", err)
		event.Error = err.Error()
		h.emit(event)
		return ReloadResult{Previous: prev, Current: prev}, err
	}

	h.lastFailedKey = ""
	if read.hash == prev.Hash {
		// Unchanged (only an API reload gets here). Refresh the warnings,
		// which depend on the users known now, without touching the policy.
		next := prev
		if warnings := h.warnings(ctx, prev.Policy); !slices.Equal(warnings, prev.Warnings) {
			refreshed := *prev
			refreshed.Warnings = warnings
			next = &refreshed
			h.current.Store(next)
		}
		h.attempt.Store(&ReloadAttempt{At: now, Trigger: trigger, OK: true, Revision: read.hash})
		event.OK = true
		h.emit(event)
		return ReloadResult{Previous: prev, Current: next}, nil
	}

	next := &Revision{Policy: p, Hash: read.hash, Source: SourceFile, LoadedAt: now}
	next.Warnings = h.warnings(ctx, p)
	h.current.Store(next)
	h.attempt.Store(&ReloadAttempt{At: now, Trigger: trigger, OK: true, Changed: true, Revision: read.hash})
	log.Info("access policy reloaded", "path", h.path, "trigger", trigger,
		"previous_revision", prev.Hash, "revision", next.Hash)
	h.logWarnings(next)
	event.OK, event.Changed = true, true
	h.emit(event)
	return ReloadResult{Changed: true, Previous: prev, Current: next}, nil
}

func (h *Holder) emit(event ReloadEvent) {
	if h.observer != nil {
		h.observer(event)
	}
}

func (h *Holder) fileError(err error) error {
	return fmt.Errorf("access policy file %s: %w", h.path, err)
}

func (h *Holder) logWarnings(rev *Revision) {
	for _, warning := range rev.Warnings {
		log.Warn("access policy warning", "path", h.path, "revision", rev.Hash, "warning", warning)
	}
}

// warnings returns the lint warnings for p in this server's configuration.
func (h *Holder) warnings(ctx context.Context, p *AccessPolicy) []string {
	warnings := []string{}
	warnings = append(warnings, p.Warnings()...)
	if !h.trustedEmail {
		for i, binding := range p.Bindings {
			if len(binding.Subjects.Users) > 0 {
				warnings = append(warnings, fmt.Sprintf(
					"bindings[%d].subjects.users: no enabled identity provider supplies a trusted email (only OIDC with email_verified=true does), so these users never match", i))
			}
		}
	}
	return append(warnings, h.unmatchedGroupWarnings(ctx, p)...)
}

func (h *Holder) unmatchedGroupWarnings(ctx context.Context, p *AccessPolicy) []string {
	if h.knownGroups == nil {
		return nil
	}
	needed := false
	for _, binding := range p.Bindings {
		for _, group := range binding.Subjects.Groups {
			needed = needed || group != "*"
		}
	}
	if !needed {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	queryCtx, cancel := context.WithTimeout(ctx, knownGroupsTimeout)
	defer cancel()
	known, err := h.knownGroups(queryCtx)
	if err != nil {
		log.Warn("access policy lint: cannot read known user groups; skipping the unmatched-group check", "path", h.path, "error", err)
		return nil
	}
	var warnings []string
	for i, binding := range p.Bindings {
		for _, group := range binding.Subjects.Groups {
			if group == "*" {
				continue
			}
			if _, ok := known[group]; !ok {
				warnings = append(warnings, fmt.Sprintf("bindings[%d].subjects.groups: %q matches no known user's groups", i, group))
			}
		}
	}
	return warnings
}

// LintWarnings returns the offline lint warnings for p: A1's namespace
// warnings plus a note on each user binding, since only OIDC logins with
// email_verified=true supply the trusted email user bindings match against.
func LintWarnings(p *AccessPolicy) []string {
	warnings := []string{}
	warnings = append(warnings, p.Warnings()...)
	if p == nil {
		return warnings
	}
	for i, binding := range p.Bindings {
		if len(binding.Subjects.Users) > 0 {
			warnings = append(warnings, fmt.Sprintf(
				"bindings[%d].subjects.users: only OIDC logins with email_verified=true supply a trusted email; SAML and LDAP logins never match user bindings", i))
		}
	}
	return warnings
}

// ReadFile reads a policy file of at most MaxFileBytes, refusing anything that
// is not a regular file. It returns the exact bytes and their hex SHA-256.
func ReadFile(path string) ([]byte, string, error) {
	read := readPolicyFile(path, nil)
	return read.data, read.hash, read.err
}

type fileRead struct {
	data []byte
	hash string
	err  error
	// errKey identifies a read failure for de-duplication.
	errKey string
	// unstable reports that the file changed (size, mtime or identity)
	// while it was read, so the bytes may be a half-written file.
	unstable bool
}

// contentKey identifies these bytes (or this read failure), so the poller
// can require the same content on two consecutive polls and report a given
// bad file once.
func (r fileRead) contentKey() string {
	if r.err == nil || r.hash != "" {
		return "sha256:" + r.hash
	}
	return "read:" + r.errKey
}

// readPolicyFile reads path. afterRead, when set, runs between the read and
// the check that the file did not change during it (a test hook).
func readPolicyFile(path string, afterRead func()) fileRead {
	// Refuse a non-regular file before opening it: opening a FIFO would
	// block the writer lock until something wrote to it.
	pre, err := os.Stat(path)
	if err != nil {
		return fileRead{err: err, errKey: err.Error()}
	}
	if !pre.Mode().IsRegular() {
		err := fmt.Errorf("not a regular file (mode %s)", pre.Mode().Type())
		return fileRead{err: err, errKey: err.Error()}
	}
	f, err := os.Open(path)
	if err != nil {
		return fileRead{err: err, errKey: err.Error()}
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return fileRead{err: err, errKey: err.Error()}
	}
	if !info.Mode().IsRegular() {
		err := fmt.Errorf("not a regular file (mode %s)", info.Mode().Type())
		return fileRead{err: err, errKey: err.Error()}
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxFileBytes+1))
	if err != nil {
		return fileRead{err: err, errKey: err.Error()}
	}
	if afterRead != nil {
		afterRead()
	}
	unstable := changedDuringRead(path, info, len(data))
	if len(data) > MaxFileBytes {
		// The bytes are never hashed in full; size and mtime identify this
		// oversize file for de-duplication.
		err := fmt.Errorf("file exceeds %d bytes", MaxFileBytes)
		return fileRead{err: err, errKey: fmt.Sprintf("%s size=%d mtime=%d", err, info.Size(), info.ModTime().UnixNano()), unstable: unstable}
	}
	return fileRead{data: data, hash: Hash(data), unstable: unstable}
}

// changedDuringRead reports whether path no longer names the file that was
// opened, or that file's size or mtime moved, or fewer or more bytes were
// read than it held when opened.
func changedDuringRead(path string, opened os.FileInfo, read int) bool {
	now, err := os.Stat(path)
	if err != nil || !os.SameFile(opened, now) {
		return true
	}
	if now.Size() != opened.Size() || !now.ModTime().Equal(opened.ModTime()) {
		return true
	}
	return read <= MaxFileBytes && int64(read) != opened.Size()
}

// Hash returns the hex SHA-256 of policy file bytes, the revision identifier.
func Hash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
