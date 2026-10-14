//go:build integration

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/caesium-cloud/caesium/internal/connector"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestFixtureConfigMatchesPublicationState(t *testing.T) {
	raw, err := os.ReadFile("../testdata/connections.yaml")
	require.NoError(t, err)
	// Parse the committed file intact. This intentionally fails until B2's
	// statusQueryResultSchema support is merged; no sibling source or schema
	// deletion masks that prerequisite.
	cfg, err := connector.Parse(raw, nil)
	require.NoError(t, err, "fixture config requires merged B2 Query schema support")
	require.Len(t, cfg.Connections, 1)
	require.Len(t, cfg.Connections[0].Bindings, 1)
	require.Equal(t, "publication", cfg.Connections[0].Bindings[0].Name)
	var document struct {
		Connections []struct {
			Bindings []struct {
				Schema map[string]any `yaml:"statusQueryResultSchema"`
			} `yaml:"bindings"`
		} `yaml:"connections"`
	}
	require.NoError(t, yaml.Unmarshal(raw, &document))
	schemaDocument := document.Connections[0].Bindings[0].Schema
	require.NotEmpty(t, schemaDocument)
	encodedSchema, err := json.Marshal(schemaDocument)
	require.NoError(t, err)
	decodedSchema, err := jsonschema.UnmarshalJSON(bytes.NewReader(encodedSchema))
	require.NoError(t, err)
	compiler := jsonschema.NewCompiler()
	require.NoError(t, compiler.AddResource("fixture.json", decodedSchema))
	schema, err := compiler.Compile("fixture.json")
	require.NoError(t, err)
	actor, err := connector.NewActorEnvelope(connector.PrincipalKindAPIKey, "key-id", "key-prefix", "operator", "operation-id", "1")
	require.NoError(t, err)
	hint := connector.CorrelationEnvelope{Version: connector.CorrelationEnvelopeVersion, JobID: "job", IdempotencyKey: "run/activity", Outcome: "created", QueueID: "queue", RunID: "local-run"}
	states := []publicationState{
		{Stage: "starting", Eligible: false},
		{Stage: "awaiting_approval", Eligible: true, First: &hint, ChildRunID: "child-run"},
		{Stage: "published", Eligible: true, Approved: true, ApprovalEffects: 1, Actor: &actor, First: &hint, Second: &hint, ChildRunID: "child-run"},
	}
	for _, state := range states {
		encoded, err := json.Marshal(state)
		require.NoError(t, err)
		var value map[string]any
		require.NoError(t, json.Unmarshal(encoded, &value))
		require.NoError(t, schema.Validate(value), "publicationState drifted from committed Query schema")
		value["undeclared"] = true
		require.Error(t, schema.Validate(value), "Query schema must remain closed")
	}
}

func TestFixtureVersionsRejectUnqualifiedObservations(t *testing.T) {
	const official = "temporal version 1.9.1 (Server 1.32.0, UI 2.54.1)\n"
	observed, err := validateVersions("v1.49.0", official, "1.32.0")
	require.NoError(t, err)
	require.Equal(t, versionMatrix{SDK: "1.49.0", CLI: "1.9.1", Server: "1.32.0"}, observed)
	for _, invalid := range []struct{ sdk, cli, server string }{
		{"v1.48.0", official, "1.32.0"},
		{"v1.49.0", "temporal version 1.9.2 (Server 1.32.0, UI 2.54.1)", "1.32.0"},
		{"v1.49.0", official, "1.33.0"},
		{"v1.49.0", "temporal version 1.9.1 (Server 1.33.0, UI 2.54.1)", "1.32.0"},
		{"v1.49.0", "unrecognized CLI output", "1.32.0"},
	} {
		_, err := validateVersions(invalid.sdk, invalid.cli, invalid.server)
		require.Error(t, err, "unqualified observations must fail before readiness")
	}
}

func TestFixtureJournalRecordsEveryEffectExecution(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	journal, err := newJournal(path)
	require.NoError(t, err)
	actor, err := connector.NewActorEnvelope(connector.PrincipalKindAPIKey, "key-id", "key-prefix", "operator", "operation-id", "1")
	require.NoError(t, err)
	journal.effect("run", "activity-1", 1, actor)
	journal.effect("run", "activity-2", 1, actor)
	reloaded, err := newJournal(path)
	require.NoError(t, err)
	reloaded.effect("run", "activity-1", 2, actor)
	events := reloaded.snapshot()
	require.Len(t, events, 3, "raw evidence must expose a second activity and a retry after reload")
	for _, event := range events {
		require.Equal(t, "approval_effect", event["kind"])
		require.Equal(t, "run", event["run_id"])
		require.NotEmpty(t, event["activity_id"])
		require.NotZero(t, event["attempt"])
	}
}
