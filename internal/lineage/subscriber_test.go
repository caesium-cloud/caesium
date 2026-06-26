package lineage

import (
	"context"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type subscriberRecordingTransport struct {
	events []RunEvent
}

func (t *subscriberRecordingTransport) Emit(_ context.Context, evt RunEvent) error {
	t.events = append(t.events, evt)
	return nil
}

func (t *subscriberRecordingTransport) Close() error {
	return nil
}

func TestSubscriberDropsQuarantinedEventBeforeTransportAndDatasetWrites(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	transport := &subscriberRecordingTransport{}
	sub := NewSubscriber(nil, transport, "caesium-test", db)
	sub.handleEvent(context.Background(), event.Event{
		Type:       event.TypeTaskSucceeded,
		JobID:      uuid.New(),
		RunID:      uuid.New(),
		TaskID:     uuid.New(),
		Timestamp:  time.Now().UTC(),
		Quarantine: true,
	})

	require.Empty(t, transport.events)
	var datasets int64
	require.NoError(t, db.Model(&models.LineageDataset{}).Count(&datasets).Error)
	require.Zero(t, datasets)
}
