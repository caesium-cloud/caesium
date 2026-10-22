package secret

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/env"
	jobdefschema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"k8s.io/client-go/kubernetes"
)

// ErrSecretDenied is matched (errors.Is) by every denial a ScopedResolver
// returns: the reference is not allowed by its namespace's secret rules.
var ErrSecretDenied = errors.New("secret reference denied by namespace secret rules")

// DeniedMarker is the stable phrase every denial message carries. The incident
// classifier keys the config failure class on it, so it must not change.
const DeniedMarker = "is not allowed by namespace"

const (
	redactedTail    = "…"
	denyNoMatch     = "no rule matches"
	denyUncheckable = "the reference cannot be checked"
	denyUndeclared  = "the namespace is not declared in this node's access policy"
	denyNoScope     = "no secret scope is configured"
	denyNotLoaded   = "the configured access policy is not loaded"
	denyUnavailable = "the run owner's isolation context is unavailable"
)

// SecretDeniedError reports a reference its namespace's secret rules do not
// allow. Ref is already redacted to provider/<first segment>/… so the error is
// safe for task errors, logs, incidents and lint output; the raw reference is
// never stored on it.
type SecretDeniedError struct {
	Namespace string
	Ref       string
	Reason    string
}

func (e *SecretDeniedError) Error() string {
	return fmt.Sprintf("secret %s %s %q secret rules: %s", e.Ref, DeniedMarker, e.Namespace, e.Reason)
}

// Is makes every SecretDeniedError match ErrSecretDenied.
func (e *SecretDeniedError) Is(target error) bool {
	return target == ErrSecretDenied
}

// ScopedResolver resolves secret references on behalf of one job namespace. It
// canonicalises each reference with the inner resolver's own effective-target
// parser (CanonicalTarget) and matches the namespace's allow rules against that
// target before any resolution or verification. A namespace without rules is
// allow-all: references pass straight to the inner resolver, unchanged.
type ScopedResolver struct {
	inner      Resolver
	namespace  string
	allowAll   bool
	undeclared bool
	// pending: a policy file is configured but its holder was not installed
	// when the scope was built; Allow resolves the holder on use.
	pending bool
	// unavailable: the scope's rules come from a run owner's isolation context
	// that could not be read (unreachable owner, a context for another
	// namespace); every reference is denied.
	unavailable bool
	kubeDefault string
	// pin, when set, is the Kubernetes namespace relative secret://k8s
	// references read in for this scope ("" = the resolver's own default),
	// fixed by the task execution that built it (ForNamespaceTarget) rather
	// than read from the live policy's mapping.
	pin   *string
	rules []string
	// reload rebuilds the scope from the process policy as it is now. It is set
	// on process-backed scopes (ForNamespace, ForClientNamespace), so every
	// Allow, and so every reference of one resolution, reads the current
	// policy: a reload while one reference is being read applies to the next.
	// One reference's allow-check and its read or verification use the same
	// rebuilt scope (snapshot), so the rules that allowed it and the inner
	// resolver (with the namespace's mapped Kubernetes default) that reads it
	// come from one policy revision.
	reload func() *ScopedResolver
}

var (
	_ Resolver                 = (*ScopedResolver)(nil)
	_ IdentityVerifier         = (*ScopedResolver)(nil)
	_ ResolvedIdentityVerifier = (*ScopedResolver)(nil)
)

// NewScopedResolver scopes inner to namespace. rules == nil means allow all; a
// non-nil rule set with no entries denies everything. kubeNamespace is the
// Kubernetes namespace a relative k8s/<secret> rule means (the namespace's
// mapped kubernetes.namespace, else CAESIUM_KUBERNETES_NAMESPACE); a relative
// rule is dropped, never widened, when kubeNamespace is not a DNS label.
func NewScopedResolver(inner Resolver, namespace string, rules *policy.SecretRules, kubeNamespace string) *ScopedResolver {
	s := &ScopedResolver{inner: inner, namespace: models.NamespaceOrDefault(namespace)}
	if rules == nil {
		s.allowAll = true
		return s
	}
	s.rules = compileSecretRules(rules.Allow, kubeNamespace)
	return s
}

// ForNamespace scopes inner to a job namespace using this process's access
// policy (policy.ProcessHolder), read now: callers build it at the point of
// use and never keep it across tasks, so a reload applies to the next check.
//
//   - declared namespace without secret rules: allow all;
//   - declared namespace with rules: enforce them (an empty list denies all);
//   - undeclared namespace on a node serving a policy file: deny all — the
//     rules the namespace should carry cannot be known here;
//   - undeclared namespace with no policy file: allow all (no rules exist).
//
// A process configured with a policy file (CAESIUM_AUTH_POLICY_FILE) whose
// holder is not installed yet — work launched during startup, before cmd/start
// loads the file — gets a pending scope: the first Allow waits up to
// processHolderWait for the holder and denies if it never appears (the rules
// exist but cannot be read, so nothing is allowed). Steps without secret
// references never wait. A client process uses ForClientNamespace instead.
func ForNamespace(inner Resolver, namespace string) *ScopedResolver {
	vars := env.Variables()
	return liveForProcess(inner, namespace, policyFileConfigured(vars), vars.KubernetesNamespace, nil)
}

// ForNamespaceTarget is ForNamespace for one task execution whose namespace
// mapping was resolved once, before its cache identity
// (jobdefruntime.KubernetesMapping): relative secret://k8s/<secret>/<key>
// references read in kubeNamespace (the execution's mapped Kubernetes
// namespace; "" keeps the resolver's CAESIUM_JOBDEF_SECRETS_KUBE_NAMESPACE)
// for the whole execution, so the pod, its cache identity and the secrets it
// reads agree even if the policy reloads mid-execution. The secret rules are
// still read from the live policy at every use: a remap mid-execution makes a
// relative reference stop matching, never widens it.
func ForNamespaceTarget(inner Resolver, namespace, kubeNamespace string) *ScopedResolver {
	vars := env.Variables()
	pin := kubeNamespace
	return liveForProcess(inner, namespace, policyFileConfigured(vars), vars.KubernetesNamespace, &pin)
}

// ForClientNamespace is ForNamespace for a run a client process executes
// (`caesium dev`, `caesium reproduce`, a no-server `caesium run retry`). Only a
// holder this process installed applies, and a client installs none, so every
// namespace allows every reference: the operator's own credentials decide, as
// for offline `caesium job lint --check-secrets`. A CAESIUM_AUTH_POLICY_FILE
// inherited from the environment does not make the scope pending: only server
// initialisation (cmd/start) loads that file, so in a client the wait could
// only stall every secret-bearing step for processHolderWait and then deny.
func ForClientNamespace(inner Resolver, namespace string) *ScopedResolver {
	return liveForProcess(inner, namespace, false, env.Variables().KubernetesNamespace, nil)
}

// liveForProcess is forProcess on the holder installed now, refreshed by every
// Allow. AllowsAll reports the policy as it was when the scope was built.
func liveForProcess(inner Resolver, namespace string, fileConfigured bool, kubeDefault string, pin *string) *ScopedResolver {
	reload := func() *ScopedResolver {
		return forProcess(inner, namespace, policy.ProcessHolder(), fileConfigured, kubeDefault, pin)
	}
	s := reload()
	s.reload = reload
	return s
}

func forProcess(inner Resolver, namespace string, holder *policy.Holder, fileConfigured bool, kubeDefault string, pin *string) *ScopedResolver {
	if holder == nil && fileConfigured {
		return &ScopedResolver{
			inner:       inner,
			namespace:   models.NamespaceOrDefault(namespace),
			pending:     true,
			kubeDefault: kubeDefault,
			pin:         pin,
		}
	}
	return forNamespace(inner, namespace, holder, kubeDefault, pin)
}

// IsolationRules is the secret part of a run owner's isolation context
// (identity-and-access design §10.1): the namespace's allow rules exactly as the
// owner's policy states them (nil allows all, an empty list denies all), the
// Kubernetes namespace a relative k8s/<secret> rule means there, and whether a
// file-backed owner policy leaves the namespace undeclared (deny all).
type IsolationRules struct {
	Namespace         string
	Rules             *policy.SecretRules
	RuleKubeNamespace string
	Undeclared        bool
}

// ForIsolation scopes inner to a dispatched task's namespace with rules the run
// owner sent: this process's access policy is never read, so a worker serving
// no policy file, or a stale one, enforces exactly the owner's rules.
// readKubeNamespace is the Kubernetes namespace relative secret://k8s
// references read in for the whole execution (the owner's mapping, else the
// owner's default), so neither the pod nor the secret object depends on the
// worker's environment.
//
// With current == nil the scope enforces initial. Otherwise every check (each
// Allow, resolution and verification, as with ForNamespace) asks current for
// the owner's rules as they are now, so a rule the owner revokes applies to the
// next operation; an error from current, or rules for another namespace, deny
// every reference (fail closed).
func ForIsolation(inner Resolver, namespace, readKubeNamespace string, initial IsolationRules, current func() (IsolationRules, error)) *ScopedResolver {
	namespace = models.NamespaceOrDefault(namespace)
	pinned := withKubernetesNamespace(inner, readKubeNamespace)
	build := func(r IsolationRules) *ScopedResolver {
		switch {
		case models.NamespaceOrDefault(r.Namespace) != namespace:
			return &ScopedResolver{inner: pinned, namespace: namespace, unavailable: true}
		case r.Undeclared:
			return &ScopedResolver{inner: pinned, namespace: namespace, undeclared: true}
		}
		return NewScopedResolver(pinned, namespace, r.Rules, r.RuleKubeNamespace)
	}
	if current == nil {
		return build(initial)
	}
	// Every operation rebuilds from the owner's current rules, so the scope
	// itself never claims to allow all: a caller that skips checks for an
	// allow-all scope (AllowsAll) must still consult the owner, who may have
	// added rules since the dispatch, or be unreachable.
	return &ScopedResolver{inner: pinned, namespace: namespace, reload: func() *ScopedResolver {
		r, err := current()
		if err != nil {
			return &ScopedResolver{inner: pinned, namespace: namespace, unavailable: true}
		}
		return build(r)
	}}
}

// ServerPolicy returns the access policy this server process enforces for work
// about to run: the installed holder, waiting up to processHolderWait when a
// policy file is configured but not installed yet (work launched during
// startup). A nil holder with ok is the empty policy (no file configured); ok
// is false when a configured policy file never loaded, and callers deny.
func ServerPolicy() (holder *policy.Holder, ok bool) {
	if holder := policy.ProcessHolder(); holder != nil {
		return holder, true
	}
	if !policyFileConfigured(env.Variables()) {
		return nil, true
	}
	if holder := awaitProcessHolder(processHolderWait); holder != nil {
		return holder, true
	}
	return nil, false
}

// processHolderWait bounds how long a pending scope waits at startup for the
// policy holder of a configured policy file.
var processHolderWait = 30 * time.Second

func policyFileConfigured(vars env.Environment) bool {
	return strings.TrimSpace(vars.AuthPolicyFile) != ""
}

// awaitProcessHolder polls until cmd/start installs the process holder or
// the wait elapses (nil).
func awaitProcessHolder(wait time.Duration) *policy.Holder {
	if holder := policy.ProcessHolder(); holder != nil {
		return holder
	}
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		time.Sleep(25 * time.Millisecond)
		if holder := policy.ProcessHolder(); holder != nil {
			return holder
		}
	}
	return nil
}

func forNamespace(inner Resolver, namespace string, holder *policy.Holder, defaultKubeNamespace string, pin *string) *ScopedResolver {
	namespace = models.NamespaceOrDefault(namespace)
	settings, declared := holder.Namespace(namespace)
	// Relative secret://k8s references read in the namespace's mapped
	// Kubernetes namespace (design §10.1): the execution's pinned mapping, else
	// the live policy's. No mapping keeps the inner resolver unchanged.
	readNamespace := ""
	if settings.Kubernetes != nil {
		readNamespace = settings.Kubernetes.Namespace
	}
	if pin != nil {
		readNamespace = *pin
	}
	inner = withKubernetesNamespace(inner, readNamespace)
	if !declared {
		if holder.FileBacked() {
			return &ScopedResolver{inner: inner, namespace: namespace, undeclared: true}
		}
		return &ScopedResolver{inner: inner, namespace: namespace, allowAll: true}
	}
	return NewScopedResolver(inner, namespace, settings.Secrets, mappedKubernetesNamespace(settings, defaultKubeNamespace))
}

// withKubernetesNamespace returns inner with its Kubernetes provider (both the
// k8s and kubernetes provider keys) reading relative references in namespace.
// An empty namespace, or an inner resolver without a Kubernetes provider,
// returns inner unchanged.
func withKubernetesNamespace(inner Resolver, namespace string) Resolver {
	if namespace == "" {
		return inner
	}
	switch r := inner.(type) {
	case *KubernetesResolver:
		if r == nil {
			return inner
		}
		return &mappedKubernetesResolver{parent: r, namespace: namespace}
	case *MultiResolver:
		if r == nil {
			return inner
		}
		providers := maps.Clone(r.providers)
		mapped := false
		for _, key := range []string{providerKubernetes, "kubernetes"} {
			if kube, ok := providers[key].(*KubernetesResolver); ok && kube != nil {
				providers[key] = &mappedKubernetesResolver{parent: kube, namespace: namespace}
				mapped = true
			}
		}
		if !mapped {
			return inner
		}
		return &MultiResolver{providers: providers}
	}
	return inner
}

// mappedKubernetesResolver is a KubernetesResolver whose default namespace for
// relative references (secret://k8s/<secret>/<key>) is a job namespace's
// mapped Kubernetes namespace instead of CAESIUM_JOBDEF_SECRETS_KUBE_NAMESPACE.
// A path namespace or ?namespace= still wins, exactly as for the parent. It
// shares the parent's client.
type mappedKubernetesResolver struct {
	parent    *KubernetesResolver
	namespace string
}

func (m *mappedKubernetesResolver) view(client kubernetes.Interface) *KubernetesResolver {
	return &KubernetesResolver{client: client, config: KubernetesConfig{KubeConfigPath: m.parent.config.KubeConfigPath, Namespace: m.namespace}}
}

func (m *mappedKubernetesResolver) Resolve(ctx context.Context, ref string) (string, error) {
	value, _, err := m.ResolveWithIdentity(ctx, ref)
	return value, err
}

func (m *mappedKubernetesResolver) ResolveWithIdentity(ctx context.Context, ref string) (string, Identity, error) {
	client, err := m.parent.clientset()
	if err != nil {
		return "", Identity{}, err
	}
	return m.view(client).ResolveWithIdentity(ctx, ref)
}

func (m *mappedKubernetesResolver) CanonicalTarget(ref string) (string, error) {
	return m.view(nil).CanonicalTarget(ref)
}

// RelativeKubernetesReference reports whether any value of env is a
// secret://k8s (or kubernetes) reference without a namespace of its own (two
// path segments, no ?namespace=): one whose read namespace is the job
// namespace's mapping. Callers fold that mapping into the cache identity.
func RelativeKubernetesReference(env map[string]string) bool {
	for _, value := range env {
		value = strings.TrimSpace(value)
		if !strings.HasPrefix(value, scheme+"://") {
			continue
		}
		ref, err := Parse(value)
		if err != nil || (ref.Provider != providerKubernetes && ref.Provider != "kubernetes") {
			continue
		}
		if len(ref.Segments) == 2 && strings.TrimSpace(ref.Query.Get("namespace")) == "" {
			return true
		}
	}
	return false
}

// ScopingActive reports whether any namespace of this process's policy can
// restrict secrets: a policy file is served, or a declared namespace carries
// rules. When false every ForNamespace result is allow-all, so callers may skip
// work (such as listing jobs) that only feeds the allow-check.
func ScopingActive() bool {
	holder := policy.ProcessHolder()
	if holder.FileBacked() || (holder == nil && policyFileConfigured(env.Variables())) {
		return true
	}
	rev := holder.Current()
	if rev == nil || rev.Policy == nil {
		return false
	}
	for _, ns := range rev.Policy.Namespaces {
		if ns.Secrets != nil {
			return true
		}
	}
	return false
}

// mappedKubernetesNamespace is the Kubernetes namespace a relative k8s rule of
// this namespace refers to: the policy mapping, else the engine default.
func mappedKubernetesNamespace(settings policy.Namespace, fallback string) string {
	if settings.Kubernetes != nil && settings.Kubernetes.Namespace != "" {
		return settings.Kubernetes.Namespace
	}
	if fallback == "" {
		return defaultNamespace
	}
	return fallback
}

// compileSecretRules expands relative Kubernetes rules (one path segment after
// the provider, k8s/** included) against kubeNamespace, so k8s/<secret> only
// ever grants the mapped namespace. Every other rule is kept verbatim;
// policy.MatchGlob canonicalises the kubernetes alias on both sides.
func compileSecretRules(allow []string, kubeNamespace string) []string {
	relativeOK := kubeNamespace != "" && jobdefschema.ValidateNamespace(kubeNamespace) == nil
	out := make([]string, 0, len(allow))
	for _, rule := range allow {
		provider, rest, ok := strings.Cut(rule, "/")
		if !ok {
			continue
		}
		if policy.CanonicalSecretProvider(provider) == providerKubernetes && !strings.Contains(rest, "/") {
			if !relativeOK {
				continue
			}
			rule = providerKubernetes + "/" + kubeNamespace + "/" + rest
		}
		out = append(out, rule)
	}
	return out
}

// Namespace returns the job namespace this resolver is scoped to.
func (s *ScopedResolver) Namespace() string {
	if s == nil {
		return ""
	}
	return s.namespace
}

// AllowsAll reports whether the namespace has no secret rules, in which case
// Allow never denies and resolution is the inner resolver's, unchanged. For a
// process-backed scope it reports the policy when the scope was built; Allow
// itself always reads the current one.
func (s *ScopedResolver) AllowsAll() bool {
	return s != nil && s.allowAll
}

// Resolver returns s as a Resolver, or nil when there is no inner resolver, so
// callers' existing "no secret resolver configured" handling is unchanged.
func (s *ScopedResolver) Resolver() Resolver {
	if s == nil || s.inner == nil {
		return nil
	}
	return s
}

// Allow checks ref against the namespace's rules without resolving it: parse,
// canonicalise with the provider's own parser, match. Any failure denies.
func (s *ScopedResolver) Allow(ref string) error {
	if s == nil {
		return &SecretDeniedError{Ref: redactSecretRef(ref), Reason: denyNoScope}
	}
	return s.snapshot().allow(ref)
}

// snapshot is the scope as of the current policy: a process-backed scope is
// rebuilt from the installed holder, and a pending one waits for it. One
// reference's allow-check and its read or verification use one snapshot.
func (s *ScopedResolver) snapshot() *ScopedResolver {
	snap := s
	if snap.reload != nil {
		snap = snap.reload()
	}
	if snap.pending {
		if holder := awaitProcessHolder(processHolderWait); holder != nil {
			snap = forNamespace(snap.inner, snap.namespace, holder, snap.kubeDefault, snap.pin)
		}
	}
	return snap
}

// allow is Allow on a snapshot.
func (s *ScopedResolver) allow(ref string) error {
	if s.unavailable {
		return s.deny(s.redact(ref), denyUnavailable)
	}
	if s.allowAll {
		return nil
	}
	if s.pending {
		return s.deny(s.redact(ref), denyNotLoaded)
	}
	if s.inner == nil {
		return s.deny(redactSecretRef(ref), denyUncheckable)
	}
	target, err := s.inner.CanonicalTarget(ref)
	if err != nil {
		// The provider's error can quote the raw reference; it is dropped.
		return s.deny(redactSecretRef(ref), denyUncheckable)
	}
	if s.undeclared {
		return s.deny(redactSecretTarget(target), denyUndeclared)
	}
	for _, rule := range s.rules {
		if matched, matchErr := policy.MatchGlob(rule, target); matchErr == nil && matched {
			return nil
		}
	}
	return s.deny(redactSecretTarget(target), denyNoMatch)
}

// redact is the best redaction available for ref: its canonical target's
// first segment when the inner resolver can canonicalise it.
func (s *ScopedResolver) redact(ref string) string {
	if s.inner != nil {
		if target, err := s.inner.CanonicalTarget(ref); err == nil {
			return redactSecretTarget(target)
		}
	}
	return redactSecretRef(ref)
}

func (s *ScopedResolver) deny(redacted, reason string) error {
	return &SecretDeniedError{Namespace: s.namespace, Ref: redacted, Reason: reason}
}

// Resolve implements Resolver: Allow, then the inner resolver.
func (s *ScopedResolver) Resolve(ctx context.Context, ref string) (string, error) {
	value, _, err := s.ResolveWithIdentity(ctx, ref)
	return value, err
}

// ResolveWithIdentity implements Resolver: Allow, then the inner resolver.
func (s *ScopedResolver) ResolveWithIdentity(ctx context.Context, ref string) (string, Identity, error) {
	if s == nil {
		return "", Identity{}, s.Allow(ref)
	}
	snap := s.snapshot()
	if err := snap.allow(ref); err != nil {
		return "", Identity{}, err
	}
	if snap.inner == nil {
		return "", Identity{}, errors.New("secret resolver is not configured")
	}
	return snap.inner.ResolveWithIdentity(ctx, ref)
}

// CanonicalTarget implements Resolver by delegating to the inner resolver of
// the current snapshot (so a relative k8s reference canonicalises in the
// namespace's mapped Kubernetes namespace).
func (s *ScopedResolver) CanonicalTarget(ref string) (string, error) {
	if s == nil || s.inner == nil {
		return "", errors.New("secret resolver is not configured")
	}
	snap := s.snapshot()
	if snap.inner == nil {
		return "", errors.New("secret resolver is not configured")
	}
	return snap.inner.CanonicalTarget(ref)
}

// VerifyIdentity implements IdentityVerifier: Allow, then the inner verifier.
func (s *ScopedResolver) VerifyIdentity(ctx context.Context, ref string, expected Identity) (Identity, error) {
	if s == nil {
		return Identity{}, s.Allow(ref)
	}
	snap := s.snapshot()
	if err := snap.allow(ref); err != nil {
		return Identity{}, err
	}
	verifier, ok := snap.inner.(IdentityVerifier)
	if !ok {
		return Identity{}, fmt.Errorf("secret provider %q does not support baseline identity verification", expected.Provider)
	}
	return verifier.VerifyIdentity(ctx, ref, expected)
}

// VerifyResolvedIdentity implements ResolvedIdentityVerifier: Allow, then the
// inner verifier. Replay re-verification therefore honours the rules too.
func (s *ScopedResolver) VerifyResolvedIdentity(ctx context.Context, ref string, expected Identity, resolvedValue string) (Identity, error) {
	if s == nil {
		return Identity{}, s.Allow(ref)
	}
	snap := s.snapshot()
	if err := snap.allow(ref); err != nil {
		return Identity{}, err
	}
	verifier, ok := snap.inner.(ResolvedIdentityVerifier)
	if !ok {
		return Identity{}, fmt.Errorf("secret provider %q does not support resolved baseline identity verification", expected.Provider)
	}
	return verifier.VerifyResolvedIdentity(ctx, ref, expected, resolvedValue)
}

// redactSecretTarget keeps the provider and the first path segment of a
// canonical target: env/<NAME>/…, k8s/<namespace>/…, vault/<mount>/….
func redactSecretTarget(target string) string {
	provider, rest, _ := strings.Cut(target, "/")
	provider = policy.CanonicalSecretProvider(provider)
	if provider == "" {
		return "secret/" + redactedTail
	}
	first, _, _ := strings.Cut(rest, "/")
	first, _, _ = strings.Cut(first, "#")
	if first == "" {
		return provider + "/" + redactedTail
	}
	return provider + "/" + first + "/" + redactedTail
}

// redactSecretRef is the redaction for a reference that could not be
// canonicalised: only a supported provider name survives.
func redactSecretRef(ref string) string {
	reference, err := Parse(ref)
	if err != nil {
		return "secret/" + redactedTail
	}
	provider := policy.CanonicalSecretProvider(reference.Provider)
	if provider == "" {
		return "secret/" + redactedTail
	}
	return provider + "/" + redactedTail
}

// CanonicalTarget implements Resolver: env/<effective name>, where ?name=
// takes precedence over the path exactly as ResolveWithIdentity reads it.
func (r *EnvResolver) CanonicalTarget(ref string) (string, error) {
	reference, err := Parse(ref)
	if err != nil {
		return "", err
	}
	if reference.Provider != providerEnv {
		return "", fmt.Errorf("env resolver cannot handle provider %q", reference.Provider)
	}
	name, err := EnvVarName(reference)
	if err != nil {
		return "", err
	}
	return providerEnv + "/" + name, nil
}

// CanonicalTarget implements Resolver: vault/<path>#<field> from the same
// path/field parser the read uses (?field= or the final path segment). # is the
// reserved delimiter, so a decoded path or field containing it (a literal
// %23) is rejected rather than matched.
func (r *VaultResolver) CanonicalTarget(ref string) (string, error) {
	reference, err := Parse(ref)
	if err != nil {
		return "", err
	}
	if reference.Provider != providerVault {
		return "", fmt.Errorf("vault resolver cannot handle provider %q", reference.Provider)
	}
	path, field, err := parseVaultPathField(reference)
	if err != nil {
		return "", err
	}
	if strings.Contains(path, "#") || strings.Contains(field, "#") {
		return "", errors.New("vault secret path and field must not contain #")
	}
	return providerVault + "/" + path + "#" + field, nil
}

// CanonicalTarget implements Resolver: k8s/<namespace>/<secret> from the same
// parser the read uses — the path namespace or the resolver's configured
// namespace, then the ?namespace=, ?name= overrides. The key is not part of the
// target: rules grant a whole Secret, as Vault rules grant every field.
func (r *KubernetesResolver) CanonicalTarget(ref string) (string, error) {
	reference, err := Parse(ref)
	if err != nil {
		return "", err
	}
	if reference.Provider != providerKubernetes && reference.Provider != "kubernetes" {
		return "", fmt.Errorf("kubernetes resolver cannot handle provider %q", reference.Provider)
	}
	namespace, name, _, err := r.parseReference(reference)
	if err != nil {
		return "", err
	}
	if strings.Contains(namespace, "/") || strings.Contains(name, "/") {
		return "", errors.New("kubernetes secret namespace and name must not contain /")
	}
	return providerKubernetes + "/" + namespace + "/" + name, nil
}

// CanonicalTarget implements Resolver by dispatching to the reference's
// provider; an unconfigured provider is an error.
func (m *MultiResolver) CanonicalTarget(ref string) (string, error) {
	resolver, err := m.providerForRef(ref)
	if err != nil {
		return "", err
	}
	return resolver.CanonicalTarget(ref)
}
