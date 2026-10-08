package run

import (
	"errors"
	"testing"

	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"gorm.io/gorm"
)

func TestSchemaValidationCatalogAndInstancePolicyParity(t *testing.T) {
	for _, mode := range []string{jobdef.SchemaValidationWarn, jobdef.SchemaValidationFail} {
		for _, form := range []string{"catalog", "instance", "nil instance fallback"} {
			t.Run(mode+"/"+form, func(t *testing.T) {
				f := newFanOutFixture(t, nil)
				row := f.instances(t)[0]
				core, logs := observer.New(zap.WarnLevel)
				undo := zap.ReplaceGlobals(zap.New(core))
				t.Cleanup(undo)
				bus := event.New()
				f.store.SetBus(bus)
				events, err := bus.Subscribe(t.Context(), event.Filter{Types: []event.Type{event.TypeSchemaViolationRecorded}})
				require.NoError(t, err)
				schema := []byte(`{"type":"object","required":["v"],"properties":{"v":{"type":"string"}}}`)
				switch form {
				case "catalog":
					err = ValidateTaskOutputSchema(f.store, f.runID, f.consumer.ID, nil, schema, mode)
				case "instance":
					err = ValidateTaskOutputSchemaInstance(f.store, f.runID, f.consumer.ID, row.ID, nil, schema, mode)
				case "nil instance fallback":
					err = ValidateTaskOutputSchemaInstance(f.store, f.runID, f.consumer.ID, uuid.Nil, nil, schema, mode)
				}
				if mode == jobdef.SchemaValidationFail {
					require.EqualError(t, err, "task "+f.consumer.ID.String()+" output violates declared schema: 1 violation(s)")
				} else {
					require.NoError(t, err)
				}
				require.NoError(t, f.db.First(&row, "id = ?", row.ID).Error)
				require.NotEmpty(t, row.SchemaViolations)
				entries := logs.FilterMessage("task output schema violations").All()
				require.Len(t, entries, 1)
				if form == "catalog" {
					require.NotContains(t, entries[0].ContextMap(), "task_run_id")
				} else {
					require.Contains(t, entries[0].ContextMap(), "task_run_id")
				}
				if mode == jobdef.SchemaValidationWarn {
					select {
					case event := <-events:
						require.Equal(t, f.runID, event.RunID)
						require.Equal(t, f.consumer.ID, event.TaskID)
					default:
						t.Fatal("warn validation must publish a schema event")
					}
				} else {
					select {
					case <-events:
						t.Fatal("fail validation must rely on task failure event")
					default:
					}
				}
			})
		}
	}
}

func TestSchemaValidationInstanceSelectsOneSibling(t *testing.T) {
	f := newFanOutFixture(t, &jobdef.FanOut{From: "discover", MaxPartitions: 16})
	_, err := f.expand(t, strParts("a", "b"))
	require.NoError(t, err)
	rows := f.instances(t)
	schema := []byte(`{"type":"object","required":["v"]}`)
	require.NoError(t, ValidateTaskOutputSchemaInstance(f.store, f.runID, f.consumer.ID, rows[1].ID, nil, schema, jobdef.SchemaValidationWarn))
	rows = f.instances(t)
	require.Empty(t, rows[0].SchemaViolations)
	require.NotEmpty(t, rows[1].SchemaViolations)
}

func TestSchemaValidationPersistenceWarningsKeepFormFields(t *testing.T) {
	for _, instance := range []bool{false, true} {
		t.Run(map[bool]string{false: "catalog", true: "instance"}[instance], func(t *testing.T) {
			f := newFanOutFixture(t, nil)
			core, logs := observer.New(zap.WarnLevel)
			undo := zap.ReplaceGlobals(zap.New(core))
			t.Cleanup(undo)
			require.NoError(t, f.db.Callback().Update().Before("gorm:update").Register("test:schema_save_failure", func(tx *gorm.DB) { tx.AddError(errors.New("schema write unavailable")) }))
			t.Cleanup(func() { _ = f.db.Callback().Update().Remove("test:schema_save_failure") })
			schema := []byte(`{"type":"object","required":["v"]}`)
			if instance {
				require.NoError(t, ValidateTaskOutputSchemaInstance(f.store, f.runID, f.consumer.ID, f.instances(t)[0].ID, nil, schema, jobdef.SchemaValidationWarn))
			} else {
				require.NoError(t, ValidateTaskOutputSchema(f.store, f.runID, f.consumer.ID, nil, schema, jobdef.SchemaValidationWarn))
			}
			entries := logs.FilterMessage("failed to persist schema violations").All()
			require.Len(t, entries, 1)
			if instance {
				require.Contains(t, entries[0].ContextMap(), "task_run_id")
			} else {
				require.NotContains(t, entries[0].ContextMap(), "task_run_id")
			}
		})
	}
}
