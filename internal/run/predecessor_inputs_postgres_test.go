package run

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// This uses two live connections: a catalog/output update commits between the
// acquisition's name and TaskRun queries. All projections must retain the
// earlier view, rather than pairing an old name with newly committed outputs.
func TestPostgresPredecessorInputsUseOneCommittedView(t *testing.T) {
	db := openDeadlinePostgres(t)
	f := seedPostgresDeadlineFixture(t, db, "predecessor-snapshot")
	require.NoError(t, db.Model(&models.TaskRun{}).Where("id = ?", f.producerRun.ID).Updates(map[string]any{
		"status": string(TaskStatusSucceeded), "output": datatypes.JSON(`{"value":"old"}`), "hash": "old-hash",
	}).Error)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	namesRead := make(chan struct{})
	resume := make(chan struct{})
	var once sync.Once
	const callback = "test:hold_predecessor_name_view"
	require.NoError(t, db.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
		if _, ok := tx.Statement.Dest.(*[]models.Task); ok {
			once.Do(func() {
				close(namesRead)
				select {
				case <-resume:
				case <-ctx.Done():
					tx.AddError(ctx.Err())
				}
			})
		}
	}))
	t.Cleanup(func() { _ = db.Callback().Query().Remove(callback) })
	type acquisition struct {
		inputs PredecessorInputs
		err    error
	}
	done := make(chan acquisition, 1)
	store := NewStore(db)
	go func() {
		inputs, err := store.PredecessorExecutionInputs(ctx, f.runID, f.consumer.ID)
		done <- acquisition{inputs, err}
	}()
	select {
	case <-namesRead:
	case result := <-done:
		t.Fatalf("acquisition returned before name read: %v", result.err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	require.NoError(t, db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&models.Task{}).Where("id = ?", f.producer.ID).Update("name", "renamed").Error; err != nil {
			return err
		}
		return tx.Model(&models.TaskRun{}).Where("id = ?", f.producerRun.ID).Updates(map[string]any{
			"output": datatypes.JSON(`{"value":"new"}`), "hash": "new-hash",
		}).Error
	}))
	close(resume)
	var result acquisition
	select {
	case result = <-done:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	require.NoError(t, result.err)
	require.Equal(t, map[string]map[string]string{"discover": {"value": "old"}}, result.inputs.OutputsByName)
	require.Equal(t, []string{"old-hash"}, result.inputs.Hashes)
	require.Equal(t, map[string]string{"value": "old"}, result.inputs.DescriptorOutputs[f.producer.ID])
	require.Equal(t, "old-hash", result.inputs.DescriptorHashes[f.producer.ID])

	current, err := store.PredecessorExecutionInputs(ctx, f.runID, f.consumer.ID)
	require.NoError(t, err)
	require.Equal(t, map[string]map[string]string{"renamed": {"value": "new"}}, current.OutputsByName)
	require.Equal(t, []string{"new-hash"}, current.Hashes)
}
