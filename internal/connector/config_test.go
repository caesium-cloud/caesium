package connector

import (
	"strings"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/secret"
)

const plantedSecret = "planted-secret-VALUE/9f3a"

const validConfig = `
version: 1
connections:
  - id: primary
    provider: temporal
    endpoint: frontend.temporal.svc:7233
    scope: default
    credentials:
      secretRefs:
        - secret://env/TEMPORAL_TOKEN?name=TEMPORAL_TOKEN
      certificatePaths:
        - /var/run/secrets/caesium/temporal/tls.crt
    bindings:
      - name: publication
        version: "1"
        displayName: Publication
        statusQuery: publication_status
        activityAllowlist:
          - activityType: caesium.start
            jobs:
              - publish
              - notify
        actions:
          - name: approve_publication
            inputSchema:
              type: object
              additionalProperties: false
              properties:
                note:
                  type: string
            resultSchema:
              type: object
              additionalProperties: false
              properties:
                approved:
                  type: boolean
`

func TestParseAcceptsVersionedConnection(t *testing.T) {
	cfg := mustParse(t, validConfig, nil)
	if cfg.Version != 1 || len(cfg.Connections) != 1 {
		t.Fatalf("parsed config = %+v", cfg)
	}
	conn := cfg.Connections[0]
	if conn.Provider != ProviderTemporal || conn.Endpoint != "frontend.temporal.svc:7233" || conn.Scope != "default" || !conn.Enabled {
		t.Fatalf("connection = %+v", conn)
	}
	if conn.Limits != DefaultLimits() {
		t.Fatalf("limits = %+v, want defaults %+v", conn.Limits, DefaultLimits())
	}
	if len(conn.Bindings) != 1 || len(conn.Bindings[0].ActivityAllowlist) != 1 {
		t.Fatalf("binding = %+v", conn.Bindings)
	}
	allow := conn.Bindings[0].ActivityAllowlist[0]
	if allow.ActivityType != "caesium.start" || len(allow.Jobs) != 2 || allow.Jobs[0] != "publish" {
		t.Fatalf("allowlist = %+v", allow)
	}
	registry := NewRegistry()
	if _, registered := registry.Get(ProviderTemporal); registered {
		t.Fatal("parsing a temporal connection registered a Temporal adapter")
	}
}

func TestParseRefusesClosedFailures(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		want string
	}{
		{
			name: "unsupported provider",
			doc:  strings.Replace(validConfig, "provider: temporal", "provider: ledger", 1),
			want: "unsupported provider",
		},
		{
			name: "duplicate connection id",
			doc: strings.Replace(validConfig, "  - id: primary\n", `  - id: primary
    provider: temporal
    endpoint: other.temporal.svc:7233
    scope: other
    credentials:
      secretRefs:
        - secret://k8s/other?key=token
  - id: primary
`, 1),
			want: "duplicate connection id",
		},
		{
			name: "unknown field",
			doc:  strings.Replace(validConfig, "scope: default", "scope: default\n    region: us", 1),
			want: "region",
		},
		{
			name: "invalid binding schema",
			doc:  strings.Replace(validConfig, "note:\n                  type: string", "note:\n                  type: noSuch", 1),
			want: "not valid JSON schema",
		},
		{
			name: "inline credential",
			doc:  strings.Replace(validConfig, "secret://env/TEMPORAL_TOKEN?name=TEMPORAL_TOKEN", plantedSecret, 1),
			want: "secret://",
		},
		{
			name: "reserved actor declaration",
			doc:  strings.Replace(validConfig, "note:\n                  type: string", ReservedActorField+":\n                  type: object\n                note:\n                  type: string", 1),
			want: ReservedActorField,
		},
		{
			name: "reserved actor override",
			doc:  strings.Replace(validConfig, "name: approve_publication", "name: approve_publication\n            "+ReservedActorField+":\n              subject: mallory", 1),
			want: ReservedActorField,
		},
		{
			name: "refresh faster than 10s",
			doc:  withLimits(validConfig, "consoleRefresh: 9s"),
			want: "consoleRefresh",
		},
		{
			name: "reads above 60",
			doc:  withLimits(validConfig, "readsPerMinute: 61"),
			want: "readsPerMinute",
		},
		{
			name: "queries above 6",
			doc:  withLimits(validConfig, "queriesPerMinute: 7"),
			want: "queriesPerMinute",
		},
		{
			name: "in-flight above 4",
			doc:  withLimits(validConfig, "maxInFlightRPCs: 5"),
			want: "maxInFlightRPCs",
		},
		{
			name: "deadline above 10s",
			doc:  withLimits(validConfig, "rpcDeadline: 11s"),
			want: "rpcDeadline",
		},
		{
			name: "page entries above 100",
			doc:  withLimits(validConfig, "maxPageEntries: 101"),
			want: "maxPageEntries",
		},
		{
			name: "page metadata above 64KiB",
			doc:  withLimits(validConfig, "maxPageMetadataBytes: 65537"),
			want: "maxPageMetadataBytes",
		},
		{
			name: "snapshot age above 24h",
			doc:  withLimits(validConfig, "unreferencedSnapshotMaxAge: 25h"),
			want: "unreferencedSnapshotMaxAge",
		},
		{
			name: "snapshots above 1000",
			doc:  withLimits(validConfig, "maxUnreferencedSnapshots: 1001"),
			want: "maxUnreferencedSnapshots",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := Parse([]byte(tc.doc), nil)
			if err == nil {
				t.Fatal("expected a closed refusal")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.want)
			}
			if strings.Contains(err.Error(), plantedSecret) {
				t.Fatalf("error contains planted secret bytes: %s", err.Error())
			}
		})
	}
}

func TestParseAcceptsCeilingAndStricterBudgets(t *testing.T) {
	doc := withLimits(validConfig, `
      consoleRefresh: 10s
      readsPerMinute: 60
      queriesPerMinute: 6
      maxInFlightRPCs: 4
      rpcDeadline: 10s
      maxPageEntries: 100
      maxPageMetadataBytes: 65536
      unreferencedSnapshotMaxAge: 24h
      maxUnreferencedSnapshots: 1000`)
	cfg := mustParse(t, doc, nil)
	if cfg.Connections[0].Limits != DefaultLimits() {
		t.Fatalf("ceiling limits = %+v", cfg.Connections[0].Limits)
	}
	stricter := withLimits(validConfig, `
      consoleRefresh: 30s
      readsPerMinute: 1
      queriesPerMinute: 1
      maxInFlightRPCs: 1
      rpcDeadline: 1s
      maxPageEntries: 1
      maxPageMetadataBytes: 1
      unreferencedSnapshotMaxAge: 1h
      maxUnreferencedSnapshots: 1`)
	cfg = mustParse(t, stricter, nil)
	limits := cfg.Connections[0].Limits
	if limits.ConsoleRefresh != 30*time.Second || limits.ReadsPerMinute != 1 || limits.UnreferencedSnapshotMaxAge != time.Hour {
		t.Fatalf("stricter limits = %+v", limits)
	}
}

func TestParseAcceptsEnvAndMountedCredentials(t *testing.T) {
	doc := strings.Replace(validConfig, "secret://env/TEMPORAL_TOKEN?name=TEMPORAL_TOKEN", "secret://k8s/temporal-creds?key=api-token", 1)
	cfg := mustParse(t, doc, nil)
	if cfg.Connections[0].SecretRefs[0] != "secret://k8s/temporal-creds?key=api-token" {
		t.Fatalf("refs = %#v", cfg.Connections[0].SecretRefs)
	}
	if cfg.Connections[0].CertificatePaths[0] != "/var/run/secrets/caesium/temporal/tls.crt" {
		t.Fatalf("paths = %#v", cfg.Connections[0].CertificatePaths)
	}
	vault := strings.Replace(validConfig, "secret://env/TEMPORAL_TOKEN?name=TEMPORAL_TOKEN", "secret://vault/kv/data/temporal#token", 1)
	if _, _, err := Parse([]byte(vault), nil); err != nil {
		t.Fatalf("vault reference: %v", err)
	}
}

func TestParseErrorOmitsResolvedSecretBytes(t *testing.T) {
	t.Setenv("TEMPORAL_TOKEN", plantedSecret)
	_, _, err := Parse([]byte(withLimits(validConfig, "readsPerMinute: 61")), secret.NewEnvResolver())
	if err == nil {
		t.Fatal("expected budget refusal")
	}
	if strings.Contains(err.Error(), plantedSecret) {
		t.Fatalf("resolved secret leaked into error: %s", err.Error())
	}
	if !strings.Contains(err.Error(), "readsPerMinute") {
		t.Fatalf("error = %s", err.Error())
	}
}

func TestFingerprintEquivalentConfigsMatch(t *testing.T) {
	left, _ := Fingerprint(mustParse(t, validConfig, nil), nil)
	right, err := Fingerprint(mustParse(t, equivalentConfig, nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	if left == "" || left != right {
		t.Fatalf("equivalent fingerprints differ: %s vs %s", left, right)
	}
}

func TestFingerprintChangesOnContractMutations(t *testing.T) {
	base, err := Fingerprint(mustParse(t, validConfig, nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	mutations := []struct {
		name string
		doc  string
	}{
		{name: "enabled", doc: strings.Replace(validConfig, "id: primary", "id: primary\n    enabled: false", 1)},
		{name: "endpoint", doc: strings.Replace(validConfig, "frontend.temporal.svc:7233", "other.temporal.svc:7233", 1)},
		{name: "credential reference", doc: strings.Replace(validConfig, "secret://env/TEMPORAL_TOKEN?name=TEMPORAL_TOKEN", "secret://env/OTHER_TOKEN?name=OTHER_TOKEN", 1)},
		{name: "schema", doc: strings.Replace(validConfig, "note:", "comment:", 1)},
		{name: "limit", doc: withLimits(validConfig, "readsPerMinute: 30")},
	}
	seen := map[string]string{base: "base"}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			fingerprint, err := Fingerprint(mustParse(t, mutation.doc, nil), nil)
			if err != nil {
				t.Fatal(err)
			}
			if fingerprint == base {
				t.Fatal("mutation kept the base fingerprint")
			}
			if previous, ok := seen[fingerprint]; ok {
				t.Fatalf("fingerprint collided with %s", previous)
			}
			seen[fingerprint] = mutation.name
		})
	}
}

func TestFingerprintIgnoresResolvedSecretBytes(t *testing.T) {
	resolver := secret.NewEnvResolver()
	t.Setenv("TEMPORAL_TOKEN", plantedSecret+"/one")
	firstCfg, firstResolved, err := Parse([]byte(validConfig), resolver)
	if err != nil {
		t.Fatal(err)
	}
	if firstResolved["secret://env/TEMPORAL_TOKEN?name=TEMPORAL_TOKEN"] != plantedSecret+"/one" {
		t.Fatalf("resolver result = %#v", firstResolved)
	}
	first, err := Fingerprint(firstCfg, firstResolved)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEMPORAL_TOKEN", plantedSecret+"/two")
	secondCfg, secondResolved, err := Parse([]byte(validConfig), resolver)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Fingerprint(secondCfg, secondResolved)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("rotation changed fingerprint: %s vs %s", first, second)
	}
	if strings.Contains(first, plantedSecret) || strings.Contains(second, "one") {
		t.Fatalf("fingerprint contains secret material: %s", first)
	}
	unchangedRef, err := Fingerprint(firstCfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if unchangedRef != first {
		t.Fatal("dropping resolved bytes changed the fingerprint")
	}
}

func TestFingerprintAllowsSecretThatMatchesPublicText(t *testing.T) {
	resolver := secret.NewEnvResolver()
	const ref = "secret://env/TEMPORAL_TOKEN?name=TEMPORAL_TOKEN"
	t.Setenv("TEMPORAL_TOKEN", "temporal")
	cfg, resolved, err := Parse([]byte(validConfig), resolver)
	if err != nil {
		t.Fatal(err)
	}
	if resolved[ref] != "temporal" {
		t.Fatalf("resolved = %#v", resolved)
	}
	first, err := Fingerprint(cfg, resolved)
	if err != nil {
		t.Fatalf("fingerprint rejected a secret that matches the provider name: %v", err)
	}
	t.Setenv("TEMPORAL_TOKEN", "frontend.temporal.svc")
	rotated, rotatedBytes, err := Parse([]byte(validConfig), resolver)
	if err != nil {
		t.Fatal(err)
	}
	if rotatedBytes[ref] != "frontend.temporal.svc" {
		t.Fatalf("resolved = %#v", rotatedBytes)
	}
	second, err := Fingerprint(rotated, rotatedBytes)
	if err != nil {
		t.Fatal(err)
	}
	if first == "" || first != second {
		t.Fatalf("public-text secret changed fingerprint: %s vs %s", first, second)
	}
}

func TestNonTemporalAdapterRegisters(t *testing.T) {
	registry := NewRegistry()
	if err := registry.Register(ledgerAdapter{}); err != nil {
		t.Fatal(err)
	}
	adapter, ok := registry.Get("ledger")
	if !ok {
		t.Fatal("ledger adapter was not registered")
	}
	identity := adapter.Identity()
	if identity.Name == TemporalProviderIdentity.Name {
		t.Fatal("test adapter reused the Temporal identity")
	}
	if sameCapabilities(identity.Capabilities, TemporalProviderIdentity.Capabilities) {
		t.Fatalf("capabilities = %#v", identity.Capabilities)
	}
	ref, err := NewExecutionReference("primary", map[string]string{"entry": "42"})
	if err != nil {
		t.Fatal(err)
	}
	if ref.OpaqueID() == "" {
		t.Fatal("opaque id was empty")
	}
	again, err := NewExecutionReference("primary", map[string]string{"entry": "42"})
	if err != nil {
		t.Fatal(err)
	}
	if ref.OpaqueID() != again.OpaqueID() {
		t.Fatal("stateless reference id was not stable")
	}
	if _, registered := registry.Get(ProviderTemporal); registered {
		t.Fatal("registry installed a Temporal adapter")
	}
}

func TestConnectionIdentityIsImmutable(t *testing.T) {
	current := ConnectionIdentity{ID: "primary", Provider: ProviderTemporal, Endpoint: "frontend:7233", Scope: "default"}
	if err := current.RefuseRepoint(current); err != nil {
		t.Fatal(err)
	}
	moved := current
	moved.Endpoint = "other:7233"
	if err := current.RefuseRepoint(moved); err == nil {
		t.Fatal("repoint was accepted")
	}
	replacement := current
	replacement.ID = "primary-v2"
	replacement.Endpoint = "other:7233"
	if err := current.RefuseRepoint(replacement); err != nil {
		t.Fatal(err)
	}
}

func TestCorrelationEnvelopeContract(t *testing.T) {
	envelope := CorrelationEnvelope{
		Version:        CorrelationEnvelopeVersion,
		JobID:          "publish",
		IdempotencyKey: "run-1",
		Outcome:        "succeeded",
	}
	if err := envelope.Validate(); err != nil {
		t.Fatal(err)
	}
	envelope.QueueID = "queue-1"
	envelope.RunID = "run-1"
	if err := envelope.Validate(); err != nil {
		t.Fatal(err)
	}
	missing := envelope
	missing.JobID = ""
	if err := missing.Validate(); err == nil {
		t.Fatal("missing job_id was accepted")
	}
	wrongVersion := envelope
	wrongVersion.Version = "v2"
	if err := wrongVersion.Validate(); err == nil {
		t.Fatal("version v2 was accepted")
	}
	actor, err := NewActorEnvelope(PrincipalKindAPIKey, "key-1", "ci", "operator", "op-1", "1")
	if err != nil {
		t.Fatal(err)
	}
	if actor.PrincipalKind != PrincipalKindAPIKey || actor.BindingVersion != "1" {
		t.Fatalf("actor = %+v", actor)
	}
}

func TestActivityAllowlistRejectsEmptyJobs(t *testing.T) {
	doc := strings.Replace(validConfig, "jobs:\n              - publish\n              - notify", "jobs: []", 1)
	_, _, err := Parse([]byte(doc), nil)
	if err == nil || !strings.Contains(err.Error(), "local job") {
		t.Fatalf("error = %v", err)
	}
}

type ledgerAdapter struct{}

func (ledgerAdapter) Identity() ProviderIdentity {
	return ProviderIdentity{
		Name:         "ledger",
		Capabilities: []Capability{CapabilityReceiptLookup},
	}
}

func mustParse(t *testing.T, doc string, resolver secret.Resolver) *Config {
	t.Helper()
	cfg, _, err := Parse([]byte(doc), resolver)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return cfg
}

func withLimits(doc, limits string) string {
	var indented strings.Builder
	for _, line := range strings.Split(limits, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		indented.WriteString("      ")
		indented.WriteString(line)
		indented.WriteByte('\n')
	}
	return strings.Replace(doc, "    credentials:", "    limits:\n"+indented.String()+"    credentials:", 1)
}

func sameCapabilities(left, right []Capability) bool {
	if len(left) != len(right) {
		return false
	}
	counts := make(map[Capability]int, len(left))
	for _, capability := range left {
		counts[capability]++
	}
	for _, capability := range right {
		counts[capability]--
		if counts[capability] < 0 {
			return false
		}
	}
	return true
}

const equivalentConfig = `
# equivalent document: key order, explicit ceilings, and duration spelling differ
version: 1
connections:
  - enabled: true
    credentials:
      certificatePaths:
        - /var/run/secrets/caesium/temporal/tls.crt
      secretRefs:
        - secret://env/TEMPORAL_TOKEN?name=TEMPORAL_TOKEN
    limits:
      maxUnreferencedSnapshots: 1000
      unreferencedSnapshotMaxAge: 86400000000000ns
      maxPageMetadataBytes: 65536
      maxPageEntries: 100
      rpcDeadline: 10000000000ns
      maxInFlightRPCs: 4
      queriesPerMinute: 6
      readsPerMinute: 60
      consoleRefresh: 10s
    bindings:
      - actions:
          - resultSchema:
              type: object
              additionalProperties: false
              properties:
                approved:
                  type: boolean
            inputSchema:
              additionalProperties: false
              type: object
              properties:
                note:
                  type: string
            name: approve_publication
        activityAllowlist:
          - jobs:
              - notify
              - publish
            activityType: caesium.start
        statusQuery: publication_status
        displayName: Publication
        version: "1"
        name: publication
    scope: default
    endpoint: frontend.temporal.svc:7233
    provider: temporal
    id: primary
`
