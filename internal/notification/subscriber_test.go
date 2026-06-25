package notification

import (
	"context"
	"testing"

	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	metrictestutil "github.com/caesium-cloud/caesium/internal/metrics/testutil"
	"github.com/stretchr/testify/require"
)

func TestSubscriberDropsQuarantinedEventsBeforeMetrics(t *testing.T) {
	TaskFailuresTotal.Reset()

	db := testutil.OpenTestDB(t)
	t.Cleanup(func() {
		testutil.CloseDB(db)
	})

	sub := &Subscriber{db: db}
	sub.handleEvent(context.Background(), event.Event{
		Type:       event.TypeTaskFailed,
		Payload:    mustJSON(map[string]string{"job_alias": "pipeline"}),
		Quarantine: true,
	})

	require.Equal(t, 0.0, metrictestutil.CounterValue(t, TaskFailuresTotal, "pipeline"))

	sub.handleEvent(context.Background(), event.Event{
		Type:    event.TypeTaskFailed,
		Payload: mustJSON(map[string]string{"job_alias": "pipeline"}),
	})

	require.Equal(t, 1.0, metrictestutil.CounterValue(t, TaskFailuresTotal, "pipeline"))
}
