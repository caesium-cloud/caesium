package secret

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/env"
	jobdefschema "github.com/caesium-cloud/caesium/pkg/jobdef"
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
	pending     bool
	kubeDefault string
	rules       []string
	// reload rebuilds the scope from the process policy as it is now. It is set
	// on process-backed scopes (ForNamespace, ForClientNamespace), so every
	// Allow, and so every reference of one resolution, reads the current
	// policy: a reload while one reference is being read applies to the next.
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
	return liveForProcess(inner, namespace, policyFileConfigured(vars), vars.KubernetesNamespace)
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
	return liveForProcess(inner, namespace, false, env.Variables().KubernetesNamespace)
}

// liveForProcess is forProcess on the holder installed now, refreshed by every
// Allow. AllowsAll reports the policy as it was when the scope was built.
func liveForProcess(inner Resolver, namespace string, fileConfigured bool, kubeDefault string) *ScopedResolver {
	reload := func() *ScopedResolver {
		return forProcess(inner, namespace, policy.ProcessHolder(), fileConfigured, kubeDefault)
	}
	s := reload()
	s.reload = reload
	return s
}

func forProcess(inner Resolver, namespace string, holder *policy.Holder, fileConfigured bool, kubeDefault string) *ScopedResolver {
	if holder == nil && fileConfigured {
		return &ScopedResolver{
			inner:       inner,
			namespace:   models.NamespaceOrDefault(namespace),
			pending:     true,
			kubeDefault: kubeDefault,
		}
	}
	return forNamespace(inner, namespace, holder, kubeDefault)
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

func forNamespace(inner Resolver, namespace string, holder *policy.Holder, defaultKubeNamespace string) *ScopedResolver {
	namespace = models.NamespaceOrDefault(namespace)
	settings, declared := holder.Namespace(namespace)
	if !declared {
		if holder.FileBacked() {
			return &ScopedResolver{inner: inner, namespace: namespace, undeclared: true}
		}
		return &ScopedResolver{inner: inner, namespace: namespace, allowAll: true}
	}
	return NewScopedResolver(inner, namespace, settings.Secrets, mappedKubernetesNamespace(settings, defaultKubeNamespace))
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
	if s.reload != nil {
		return s.reload().Allow(ref)
	}
	if s.allowAll {
		return nil
	}
	if s.pending {
		if holder := awaitProcessHolder(processHolderWait); holder != nil {
			return forNamespace(s.inner, s.namespace, holder, s.kubeDefault).Allow(ref)
		}
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
	if err := s.Allow(ref); err != nil {
		return "", Identity{}, err
	}
	if s.inner == nil {
		return "", Identity{}, errors.New("secret resolver is not configured")
	}
	return s.inner.ResolveWithIdentity(ctx, ref)
}

// CanonicalTarget implements Resolver by delegating to the inner resolver.
func (s *ScopedResolver) CanonicalTarget(ref string) (string, error) {
	if s == nil || s.inner == nil {
		return "", errors.New("secret resolver is not configured")
	}
	return s.inner.CanonicalTarget(ref)
}

// VerifyIdentity implements IdentityVerifier: Allow, then the inner verifier.
func (s *ScopedResolver) VerifyIdentity(ctx context.Context, ref string, expected Identity) (Identity, error) {
	if err := s.Allow(ref); err != nil {
		return Identity{}, err
	}
	verifier, ok := s.inner.(IdentityVerifier)
	if !ok {
		return Identity{}, fmt.Errorf("secret provider %q does not support baseline identity verification", expected.Provider)
	}
	return verifier.VerifyIdentity(ctx, ref, expected)
}

// VerifyResolvedIdentity implements ResolvedIdentityVerifier: Allow, then the
// inner verifier. Replay re-verification therefore honours the rules too.
func (s *ScopedResolver) VerifyResolvedIdentity(ctx context.Context, ref string, expected Identity, resolvedValue string) (Identity, error) {
	if err := s.Allow(ref); err != nil {
		return Identity{}, err
	}
	verifier, ok := s.inner.(ResolvedIdentityVerifier)
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
