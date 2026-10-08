package connector

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/secret"
	"golang.org/x/sys/unix"
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

func TestActorReservationAllowsNestedSchemaAndData(t *testing.T) {
	for _, property := range []string{
		"type: object\n                  properties:\n                    _caesium_actor:\n                      type: string",
		"type: object\n                  const:\n                    _caesium_actor: nested",
		"type: object\n                  enum:\n                    - _caesium_actor: nested",
	} {
		doc := strings.Replace(validConfig, "note:\n                  type: string", "note:\n                  "+property, 1)
		if _, err := Parse([]byte(doc), nil); err != nil {
			t.Fatalf("nested actor key in %q rejected: %v", property, err)
		}
	}
	if _, err := Parse([]byte(validConfig+"_caesium_actor: user-value\n"), nil); err == nil {
		t.Fatal("unknown top-level connector field accepted")
	}
}

func TestActorReservationRejectsRootActionSchemaProperty(t *testing.T) {
	doc := strings.Replace(
		validConfig,
		"              properties:\n                note:",
		"              properties:\n                _caesium_actor:\n                  type: string\n                note:",
		1,
	)
	if _, err := Parse([]byte(doc), nil); err == nil || !strings.Contains(err.Error(), ReservedActorField) {
		t.Fatalf("root action schema property %q was not rejected: %v", ReservedActorField, err)
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
        - secret://k8s/other/token
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
			_, err := Parse([]byte(tc.doc), nil)
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
	doc := strings.Replace(validConfig, "secret://env/TEMPORAL_TOKEN?name=TEMPORAL_TOKEN", "secret://k8s/temporal-creds/api-token", 1)
	cfg := mustParse(t, doc, nil)
	if cfg.Connections[0].SecretRefs[0] != "secret://k8s/temporal-creds/api-token" {
		t.Fatalf("refs = %#v", cfg.Connections[0].SecretRefs)
	}
	if cfg.Connections[0].CertificatePaths[0] != "/var/run/secrets/caesium/temporal/tls.crt" {
		t.Fatalf("paths = %#v", cfg.Connections[0].CertificatePaths)
	}
	namespaced := strings.Replace(validConfig, "secret://env/TEMPORAL_TOKEN?name=TEMPORAL_TOKEN", "secret://kubernetes/ops/temporal-creds/api-token", 1)
	if _, err := Parse([]byte(namespaced), nil); err != nil {
		t.Fatalf("namespaced kubernetes reference: %v", err)
	}
	vault := strings.Replace(validConfig, "secret://env/TEMPORAL_TOKEN?name=TEMPORAL_TOKEN", "secret://vault/kv/data/temporal?field=token", 1)
	if _, err := Parse([]byte(vault), nil); err != nil {
		t.Fatalf("vault reference: %v", err)
	}
	vaultPath := strings.Replace(validConfig, "secret://env/TEMPORAL_TOKEN?name=TEMPORAL_TOKEN", "secret://vault/kv/data/temporal/token", 1)
	if _, err := Parse([]byte(vaultPath), nil); err != nil {
		t.Fatalf("vault path reference: %v", err)
	}
}

func TestParseErrorOmitsResolvedSecretBytes(t *testing.T) {
	t.Setenv("TEMPORAL_TOKEN", plantedSecret)
	_, err := Parse([]byte(withLimits(validConfig, "readsPerMinute: 61")), secret.NewEnvResolver())
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
	left, _ := Fingerprint(mustParse(t, validConfig, nil))
	right, err := Fingerprint(mustParse(t, equivalentConfig, nil))
	if err != nil {
		t.Fatal(err)
	}
	if left == "" || left != right {
		t.Fatalf("equivalent fingerprints differ: %s vs %s", left, right)
	}
}

func TestFingerprintChangesOnContractMutations(t *testing.T) {
	base, err := Fingerprint(mustParse(t, validConfig, nil))
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
			fingerprint, err := Fingerprint(mustParse(t, mutation.doc, nil))
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
	if _, err := Parse([]byte(validConfig), resolver); err == nil {
		t.Fatal("unset env var was accepted")
	}
	t.Setenv("TEMPORAL_TOKEN", plantedSecret+"/one")
	firstCfg, err := Parse([]byte(validConfig), resolver)
	if err != nil {
		t.Fatal(err)
	}
	first, err := Fingerprint(firstCfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEMPORAL_TOKEN", plantedSecret+"/two")
	secondCfg, err := Parse([]byte(validConfig), resolver)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Fingerprint(secondCfg)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("rotation changed fingerprint: %s vs %s", first, second)
	}
	if strings.Contains(first, plantedSecret) || strings.Contains(second, "one") {
		t.Fatalf("fingerprint contains secret material: %s", first)
	}
}

func TestFingerprintAllowsSecretThatMatchesPublicText(t *testing.T) {
	resolver := secret.NewEnvResolver()
	t.Setenv("TEMPORAL_TOKEN", "temporal")
	cfg, err := Parse([]byte(validConfig), resolver)
	if err != nil {
		t.Fatal(err)
	}
	first, err := Fingerprint(cfg)
	if err != nil {
		t.Fatalf("fingerprint rejected a secret that matches the provider name: %v", err)
	}
	t.Setenv("TEMPORAL_TOKEN", "frontend.temporal.svc")
	rotated, err := Parse([]byte(validConfig), resolver)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Fingerprint(rotated)
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
	_, err := Parse([]byte(doc), nil)
	if err == nil || !strings.Contains(err.Error(), "local job") {
		t.Fatalf("error = %v", err)
	}
}

func TestReviewRegressions(t *testing.T) {
	t.Run("external schema ref", func(t *testing.T) {
		doc := strings.Replace(validConfig, "note:\n                  type: string", "note:\n                  $ref: file:///etc/caesium/note.json", 1)
		_, err := Parse([]byte(doc), nil)
		if err == nil || strings.Contains(err.Error(), "no such file") || !strings.Contains(err.Error(), "fragment") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("local schema ref", func(t *testing.T) {
		doc := strings.Replace(validConfig, `properties:
                note:
                  type: string`, `properties:
                note:
                  $ref: "#/$defs/note"
              $defs:
                note:
                  type: string`, 1)
		if _, err := Parse([]byte(doc), nil); err != nil {
			t.Fatalf("local fragment ref: %v", err)
		}
	})
	t.Run("secret shapes the resolvers reject", func(t *testing.T) {
		cases := []string{
			"secret://k8s/temporal-creds?key=api-token",
			"secret://k8s#token",
			"secret://vault/kv/data/temporal#token",
			"secret://vault#token",
			"secret://env/TEMPORAL_TOKEN?password=hunter2",
		}
		for _, ref := range cases {
			doc := strings.Replace(validConfig, "secret://env/TEMPORAL_TOKEN?name=TEMPORAL_TOKEN", ref, 1)
			_, err := Parse([]byte(doc), nil)
			if err == nil || strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), ref) {
				t.Fatalf("%s: error = %v", ref, err)
			}
		}
	})
	t.Run("endpoint userinfo", func(t *testing.T) {
		doc := strings.Replace(validConfig, "frontend.temporal.svc:7233", "https://admin:hunter2@frontend.temporal.svc:7233", 1)
		_, err := Parse([]byte(doc), nil)
		if err == nil || !strings.Contains(err.Error(), "host:port") || strings.Contains(err.Error(), "hunter2") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("endpoint is host port only", func(t *testing.T) {
		cases := []string{
			"https://frontend.temporal.svc:7233/?api_key=hunter2hunter2",
			"frontend.temporal.svc:7233#hunter2",
			"https://frontend.temporal.svc:7233/hunter2hunter2",
		}
		for _, endpoint := range cases {
			doc := strings.Replace(validConfig, "endpoint: frontend.temporal.svc:7233", "endpoint: "+strconv.Quote(endpoint), 1)
			_, err := Parse([]byte(doc), nil)
			if err == nil || !strings.Contains(err.Error(), "host:port") || strings.Contains(err.Error(), "hunter2") {
				t.Fatalf("%s: error = %v", endpoint, err)
			}
		}
	})
	t.Run("disabled connection skips unset env", func(t *testing.T) {
		doc := strings.Replace(validConfig, "id: primary", "id: primary\n    enabled: false", 1)
		doc = strings.ReplaceAll(doc, "TEMPORAL_TOKEN", "UNSET_CONNECTOR_TOKEN")
		cfg, err := Parse([]byte(doc), secret.NewEnvResolver())
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Connections[0].Enabled {
			t.Fatal("connection stayed enabled")
		}
		enabled := strings.ReplaceAll(doc, "enabled: false", "enabled: true")
		if _, err := Parse([]byte(enabled), secret.NewEnvResolver()); err == nil {
			t.Fatal("enabled connection accepted an unset env var")
		}
	})
	t.Run("actor schema bypasses", func(t *testing.T) {
		cases := []struct {
			name string
			doc  string
		}{
			{
				name: "patternProperties",
				doc:  strings.Replace(validConfig, "additionalProperties: false\n              properties:", "additionalProperties: false\n              patternProperties:\n                \"^_caesium_actor$\":\n                  type: object\n              properties:", 1),
			},
			{
				name: "required",
				doc:  strings.Replace(validConfig, "type: object\n              additionalProperties: false", "type: object\n              additionalProperties: false\n              required: [_caesium_actor]", 1),
			},
			{
				name: "additionalProperties default",
				doc:  strings.Replace(validConfig, "              additionalProperties: false\n              properties:\n                note:", "              properties:\n                note:", 1),
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				_, err := Parse([]byte(tc.doc), nil)
				if err == nil {
					t.Fatal("schema was accepted")
				}
			})
		}
	})
	t.Run("fractional integers", func(t *testing.T) {
		_, err := Parse([]byte(withLimits(validConfig, "readsPerMinute: 60.9")), nil)
		if err == nil || !strings.Contains(err.Error(), "base-10 integer") {
			t.Fatalf("error = %v", err)
		}
		doc := strings.Replace(validConfig, "version: 1", "version: 1.7", 1)
		_, err = Parse([]byte(doc), nil)
		if err == nil || !strings.Contains(err.Error(), "base-10 integer") {
			t.Fatalf("version error = %v", err)
		}
	})
	t.Run("yaml schema scalars", func(t *testing.T) {
		doc := strings.Replace(validConfig, "note:\n                  type: string", "note:\n                  enum: [2024-01-01, 0x10]", 1)
		_, err := Parse([]byte(doc), nil)
		if err == nil || !strings.Contains(err.Error(), "JSON") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("activity type unique per connection", func(t *testing.T) {
		second := `      - name: cleanup
        version: "1"
        displayName: Cleanup
        statusQuery: cleanup_status
        activityAllowlist:
          - activityType: caesium.start
            jobs:
              - delete-prod
        actions:
          - name: approve_cleanup
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
		doc := strings.Replace(validConfig, "                approved:\n                  type: boolean\n", "                approved:\n                  type: boolean\n"+second, 1)
		_, err := Parse([]byte(doc), nil)
		if err == nil || !strings.Contains(err.Error(), "duplicate activity type") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("large schema integers change the fingerprint", func(t *testing.T) {
		low := strings.Replace(validConfig, "type: object\n              additionalProperties: false\n              properties:\n                note:", "type: object\n              additionalProperties: false\n              maximum: 9007199254740992\n              properties:\n                note:", 1)
		high := strings.Replace(validConfig, "type: object\n              additionalProperties: false\n              properties:\n                note:", "type: object\n              additionalProperties: false\n              maximum: 9007199254740993\n              properties:\n                note:", 1)
		left, err := Fingerprint(mustParse(t, low, nil))
		if err != nil {
			t.Fatal(err)
		}
		right, err := Fingerprint(mustParse(t, high, nil))
		if err != nil {
			t.Fatal(err)
		}
		if left == right {
			t.Fatal("integers above 2^53 collapsed to one fingerprint")
		}
	})
	t.Run("schema integers keep digits past uint64", func(t *testing.T) {
		const literal = "18446744073709551617"
		const neighbor = "18446744073709551618"
		low := strings.Replace(validConfig, "type: object\n              additionalProperties: false\n              properties:\n                note:", "type: object\n              additionalProperties: false\n              maximum: "+literal+"\n              properties:\n                note:", 1)
		high := strings.Replace(validConfig, "type: object\n              additionalProperties: false\n              properties:\n                note:", "type: object\n              additionalProperties: false\n              maximum: "+neighbor+"\n              properties:\n                note:", 1)
		cfg := mustParse(t, low, nil)
		raw := cfg.Connections[0].Bindings[0].Actions[0].InputSchema
		if !bytes.Contains(raw, []byte(literal)) || bytes.Contains(raw, []byte("18446744073709552000")) {
			t.Fatalf("stored schema = %s", raw)
		}
		left, err := Fingerprint(cfg)
		if err != nil {
			t.Fatal(err)
		}
		right, err := Fingerprint(mustParse(t, high, nil))
		if err != nil {
			t.Fatal(err)
		}
		if left == right {
			t.Fatal("integers past 17 significant digits collapsed to one fingerprint")
		}
	})
	t.Run("schema floats keep digits past float64", func(t *testing.T) {
		const literal = "0.10000000000000000001"
		low := strings.Replace(validConfig, "type: object\n              additionalProperties: false\n              properties:\n                note:", "type: object\n              additionalProperties: false\n              multipleOf: "+literal+"\n              properties:\n                note:", 1)
		high := strings.Replace(validConfig, "type: object\n              additionalProperties: false\n              properties:\n                note:", "type: object\n              additionalProperties: false\n              multipleOf: 0.1\n              properties:\n                note:", 1)
		cfg := mustParse(t, low, nil)
		raw := cfg.Connections[0].Bindings[0].Actions[0].InputSchema
		if !bytes.Contains(raw, []byte(literal)) {
			t.Fatalf("stored schema = %s", raw)
		}
		left, err := Fingerprint(cfg)
		if err != nil {
			t.Fatal(err)
		}
		right, err := Fingerprint(mustParse(t, high, nil))
		if err != nil {
			t.Fatal(err)
		}
		if left == right {
			t.Fatal("floats past 17 significant digits collapsed to one fingerprint")
		}
	})
	t.Run("legitimate schemas", func(t *testing.T) {
		cases := []string{
			strings.Replace(validConfig, "properties:\n                note:\n                  type: string", "properties:\n                labels:\n                  type: object\n                  patternProperties:\n                    \"^[a-z_]+$\":\n                      type: string\n                note:\n                  type: string", 1),
			strings.Replace(validConfig, "properties:\n                note:\n                  type: string", "properties:\n                \"$id\":\n                  type: string\n                note:\n                  type: string", 1),
			strings.Replace(validConfig, "note:\n                  type: string", "note:\n                  type: string\n                  default:\n                    \"$ref\": x", 1),
			strings.Replace(validConfig, "type: object\n              additionalProperties: false", "type: object\n              description: _caesium_actor\n              additionalProperties: false", 1),
			strings.Replace(validConfig, "type: object\n              additionalProperties: false", "$schema: https://json-schema.org/draft/2020-12/schema\n              type: object\n              additionalProperties: false", 1),
		}
		for _, doc := range cases {
			if _, err := Parse([]byte(doc), nil); err != nil {
				t.Fatalf("schema rejected: %v\n%s", err, doc)
			}
		}
	})
	t.Run("validate before actor stamp", func(t *testing.T) {
		cfg := mustParse(t, validConfig, nil)
		schema := cfg.Connections[0].Bindings[0].Actions[0].InputSchema
		actor, err := NewActorEnvelope(PrincipalKindUser, "user-1", "ada", "operator", "op-9", "1")
		if err != nil {
			t.Fatal(err)
		}
		caller := map[string]any{"note": "hi"}
		if err := validateCallerPayload(schema, caller); err != nil {
			t.Fatal(err)
		}
		stamped, err := ApplyActor(caller, actor)
		if err != nil {
			t.Fatal(err)
		}
		if err := validateCallerPayload(schema, stamped); err == nil {
			t.Fatal("schema accepted a payload that already contained the actor")
		}
		accepted, err := AcceptActionInput(schema, caller, actor)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := accepted[ReservedActorField].(map[string]any)
		if got["subject"] != "ada" || accepted["note"] != "hi" {
			t.Fatalf("payload = %#v", accepted)
		}
		supplied := map[string]any{"note": "hi", ReservedActorField: map[string]any{"subject": "mallory"}}
		if _, err := AcceptActionInput(schema, supplied, actor); err == nil {
			t.Fatal("caller-supplied actor was accepted")
		}
	})
	t.Run("short secret does not garble the error", func(t *testing.T) {
		t.Setenv("TEMPORAL_TOKEN", "e")
		_, err := Parse([]byte(withLimits(validConfig, "readsPerMinute: 61")), secret.NewEnvResolver())
		if err == nil {
			t.Fatal("expected budget refusal")
		}
		if !strings.Contains(err.Error(), "readsPerMinute must be at most 60") {
			t.Fatalf("error = %s", err.Error())
		}
	})
	t.Run("opaque ids do not collide", func(t *testing.T) {
		left, err := NewExecutionReference("primary", map[string]string{"a=b": "c"})
		if err != nil {
			t.Fatal(err)
		}
		right, err := NewExecutionReference("primary", map[string]string{"a": "b=c"})
		if err != nil {
			t.Fatal(err)
		}
		if left.OpaqueID() == right.OpaqueID() {
			t.Fatal("different coordinates produced one id")
		}
	})
	t.Run("actor overwrite", func(t *testing.T) {
		actor, err := NewActorEnvelope(PrincipalKindUser, "user-1", "ada", "operator", "op-9", "1")
		if err != nil {
			t.Fatal(err)
		}
		out, err := ApplyActor(map[string]any{ReservedActorField: map[string]any{"subject": "mallory"}, "note": "ok"}, actor)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := out[ReservedActorField].(map[string]any)
		if got["subject"] != "ada" || got["stable_id"] != "user-1" || out["note"] != "ok" {
			t.Fatalf("payload = %#v", out)
		}
	})
}

func TestLoadFileReportsTheOSCause(t *testing.T) {
	t.Cleanup(Clear)
	_, _, err := LoadFile(filepath.Join(t.TempDir(), "missing.yaml"))
	if err == nil || !errors.Is(err, os.ErrNotExist) || !strings.Contains(err.Error(), "cannot be read") {
		t.Fatalf("missing file: %v", err)
	}
	_, _, err = LoadFile(t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("directory: %v", err)
	}
	tooBig := filepath.Join(t.TempDir(), "big.yaml")
	file, err := os.Create(tooBig)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(MaxConfigFileBytes + 1); err != nil {
		t.Fatal(err)
	}
	file.Close()
	_, _, err = LoadFile(tooBig)
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("size: %v", err)
	}

	dir := t.TempDir()
	target := filepath.Join(dir, "real.yaml")
	if err := os.WriteFile(target, []byte(validConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.yaml")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEMPORAL_TOKEN", "temporal")
	if _, _, err := LoadFile(link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	dirLink := filepath.Join(dir, "dirlink")
	if err := os.Symlink(dir, dirLink); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadFile(dirLink); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("symlink to directory: %v", err)
	}
	fifo := filepath.Join(dir, "fifo")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	fifoErr := make(chan error, 1)
	go func() {
		_, _, err := LoadFile(fifo)
		fifoErr <- err
	}()
	select {
	case err := <-fifoErr:
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("fifo: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("opening a FIFO blocked startup")
	}

	path := filepath.Join(t.TempDir(), "connectors.yaml")
	if err := os.WriteFile(path, []byte(validConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEMPORAL_TOKEN", "temporal")
	_, first, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEMPORAL_TOKEN", "frontend.temporal.svc")
	_, second, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if first == "" || first != second {
		t.Fatalf("load fingerprints = %s vs %s", first, second)
	}
	cfg, stored, ok := Current()
	if !ok || stored != first || cfg.Version != 1 {
		t.Fatalf("current = %v %s %v", cfg, stored, ok)
	}
	registry := LoadedRegistry()
	if registry == nil {
		t.Fatal("loaded config did not publish a registry")
	}
	if _, registered := registry.Get(ProviderTemporal); registered {
		t.Fatal("loading a temporal connection registered a Temporal adapter")
	}
}

func TestSealIdentitiesRejectsAnEmptyID(t *testing.T) {
	err := sealIdentities(&Config{Connections: []Connection{{ID: ""}}})
	if err == nil || !strings.Contains(err.Error(), "identity") {
		t.Fatalf("empty id: %v", err)
	}
}

type ledgerAdapter struct{}

func (ledgerAdapter) Identity() ProviderIdentity {
	return ProviderIdentity{
		Name:         "ledger",
		Capabilities: []Capability{CapabilityReceiptLookup},
	}
}

func TestDocumentedConnectorExampleIsTheAgentFixture(t *testing.T) {
	root := moduleRoot(t)
	doc, err := os.ReadFile(filepath.Join(root, "docs/connectors.md"))
	if err != nil {
		t.Fatal(err)
	}
	fixture, err := os.ReadFile(filepath.Join(root, "test/fixtures/connectors/connections.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(doc), strings.TrimSpace(string(fixture))) {
		t.Fatal("docs/connectors.md example is not the file the agent lane mounts")
	}
	helmAt := strings.Index(string(doc), "name: CAESIUM_AUTH_KEY_HASH_SECRET")
	if helmAt < 0 {
		t.Fatal("helm example does not set CAESIUM_AUTH_KEY_HASH_SECRET")
	}
	helm := string(doc)[helmAt:]
	if !strings.Contains(helm, "secretKeyRef") || !strings.Contains(helm, "name: TEMPORAL_TOKEN") {
		t.Fatal("helm example does not set TEMPORAL_TOKEN from a Secret")
	}
	t.Setenv("TEMPORAL_TOKEN", "temporal")
	if _, err := Parse(fixture, secret.NewEnvResolver()); err != nil {
		t.Fatal(err)
	}
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not locate module root")
		}
		dir = parent
	}
}

func mustParse(t *testing.T, doc string, resolver secret.Resolver) *Config {
	t.Helper()
	cfg, err := Parse([]byte(doc), resolver)
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
