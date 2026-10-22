package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"
	"sync"

	"github.com/caesium-cloud/caesium/pkg/log"
)

// The deprecated static shared secrets (design §7.5, identity-and-access A6).
// CAESIUM_EVENT_INGEST_API_KEY gates POST /v1/events and
// CAESIUM_MANUAL_TRIGGER_API_KEY gates POST /v1/triggers/:id/fire. They are
// honoured for one release:
//
//   - With authentication enabled, the authenticated principal decides what
//     the request may do. The secret is never required, never widens the
//     principal, and is only ever read from the explicit LegacySecretHeader
//     (the Authorization header carries the principal's own credential). A
//     presented secret that does not match is refused.
//   - With authentication disabled, the secret is the request's only gate,
//     exactly as before: required when configured (503 when not), read from
//     the header or, failing that, a bearer token.
//
// Every request a valid secret was presented on is audited with the secret's
// legacy actor (ActionLegacySecretUsed), whatever its outcome.

// LegacySecretHeader is the header a deprecated shared secret is presented in.
const LegacySecretHeader = "X-Caesium-API-Key"

// LegacySecretReplacement names what replaces the shared secrets, for the
// startup warning and the audit row.
const LegacySecretReplacement = "a runner API key confined to a namespace or job list (caesium auth key create --role runner --namespace <ns> or --jobs <alias>)"

// LegacySecret describes one deprecated shared secret.
type LegacySecret struct {
	// EnvVar is the environment variable that configures it.
	EnvVar string
	// Actor is the audit actor of the requests it authenticates.
	Actor string
	// Route is the route it gates, for the audit row.
	Route string
	// DisabledMessage is the 503 message when it is unset and auth is off.
	DisabledMessage string
}

var (
	// LegacyEventIngest is CAESIUM_EVENT_INGEST_API_KEY.
	LegacyEventIngest = LegacySecret{
		EnvVar:          "CAESIUM_EVENT_INGEST_API_KEY",
		Actor:           ActorLegacyEventIngest,
		Route:           "POST /v1/events",
		DisabledMessage: "event ingest is disabled",
	}
	// LegacyManualTrigger is CAESIUM_MANUAL_TRIGGER_API_KEY.
	LegacyManualTrigger = LegacySecret{
		EnvVar:          "CAESIUM_MANUAL_TRIGGER_API_KEY",
		Actor:           ActorLegacyManualTrigger,
		Route:           "POST /v1/triggers/:id/fire",
		DisabledMessage: "manual trigger fire is disabled",
	}
)

// LegacyDecision is the outcome of LegacyGate.
type LegacyDecision struct {
	// Used reports that a valid secret was presented: the request is audited
	// with the secret's legacy actor.
	Used bool
	// Status is 0 when the request may proceed, else the HTTP status to
	// refuse it with (401 or 503) and Message the refusal's message.
	Status  int
	Message string
}

// LegacyGate decides a request against a deprecated shared secret.
// expected is the configured secret ("" when unset); hasPrincipal reports
// that the auth middleware authenticated the request; authEnabled reports
// that authentication is enabled on this server.
func LegacyGate(r *http.Request, secret LegacySecret, expected string, hasPrincipal, authEnabled bool) LegacyDecision {
	expected = strings.TrimSpace(expected)
	if hasPrincipal {
		if expected == "" {
			return LegacyDecision{}
		}
		provided := strings.TrimSpace(r.Header.Get(LegacySecretHeader))
		if provided == "" {
			return LegacyDecision{}
		}
		if !legacySecretMatches(provided, expected) {
			return LegacyDecision{Status: http.StatusUnauthorized, Message: "invalid api key"}
		}
		return LegacyDecision{Used: true}
	}
	if authEnabled {
		// The middleware authenticates every request on these routes; a
		// request without a principal here never passed it.
		return LegacyDecision{Status: http.StatusUnauthorized, Message: "missing credentials"}
	}
	if expected == "" {
		return LegacyDecision{Status: http.StatusServiceUnavailable, Message: secret.DisabledMessage}
	}
	provided := strings.TrimSpace(r.Header.Get(LegacySecretHeader))
	if provided == "" {
		authorization := strings.TrimSpace(r.Header.Get("Authorization"))
		if strings.HasPrefix(strings.ToLower(authorization), "bearer ") {
			provided = strings.TrimSpace(authorization[7:])
		}
	}
	if !legacySecretMatches(provided, expected) {
		return LegacyDecision{Status: http.StatusUnauthorized, Message: "invalid api key"}
	}
	return LegacyDecision{Used: true}
}

// legacySecretMatches compares fixed-length digests in constant time, so
// neither the secret's length nor a matching prefix leaks through timing.
func legacySecretMatches(provided, expected string) bool {
	providedHash := sha256.Sum256([]byte(provided))
	expectedHash := sha256.Sum256([]byte(expected))
	return subtle.ConstantTimeCompare(providedHash[:], expectedHash[:]) == 1
}

// LegacySecretWarnings returns the startup deprecation warning of every
// configured shared secret, naming its variable and the replacement, never
// its value.
func LegacySecretWarnings(manualTriggerKey, eventIngestKey string) []string {
	var warnings []string
	for _, configured := range []struct {
		secret LegacySecret
		value  string
	}{
		{LegacyManualTrigger, manualTriggerKey},
		{LegacyEventIngest, eventIngestKey},
	} {
		if strings.TrimSpace(configured.value) == "" {
			continue
		}
		warnings = append(warnings, configured.secret.EnvVar+" is deprecated and will be removed in the next release; requests presenting it are audited as "+configured.secret.Actor+". Replace it with "+LegacySecretReplacement+".")
	}
	return warnings
}

var legacySecretWarnOnce sync.Once

// WarnLegacySharedSecrets logs LegacySecretWarnings once per process.
func WarnLegacySharedSecrets(manualTriggerKey, eventIngestKey string) {
	legacySecretWarnOnce.Do(func() {
		for _, warning := range LegacySecretWarnings(manualTriggerKey, eventIngestKey) {
			log.Warn("deprecated shared secret configured", "detail", warning)
		}
	})
}

// LegacySecretUse is one audited use of a shared secret.
type LegacySecretUse struct {
	Secret    LegacySecret
	SourceIP  string
	Outcome   string
	Principal *Principal
	// Namespaces are the namespaces of the jobs the request routed to or
	// fired; Metadata is merged into the row (delivery counts).
	Namespaces []string
	Metadata   map[string]any
}

// LegacySecretAuditEntry builds the audit row of one shared-secret use. The
// row's namespace is the jobs' namespace when there is one, "*" (with the set
// in metadata) for several, and "" for none. The secret is never recorded.
func LegacySecretAuditEntry(use LegacySecretUse) AuditEntry {
	namespaces := normalizeNamespaceSet(use.Namespaces)
	metadata := map[string]any{
		"route":       use.Secret.Route,
		"replacement": LegacySecretReplacement,
	}
	for key, value := range use.Metadata {
		metadata[key] = value
	}
	if use.Principal != nil {
		metadata["principal"] = use.Principal.Subject
	}
	entry := AuditEntry{
		Actor:        use.Secret.Actor,
		Action:       ActionLegacySecretUsed,
		ResourceType: "route",
		ResourceID:   use.Secret.Route,
		SourceIP:     use.SourceIP,
		Outcome:      use.Outcome,
		Metadata:     metadata,
	}
	switch len(namespaces) {
	case 0:
	case 1:
		entry.Namespace = namespaces[0]
	default:
		entry.Namespace = "*"
		metadata["namespaces"] = namespaces
	}
	return entry
}
