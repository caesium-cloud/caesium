package lineage

import (
	"context"
	"encoding/json"

	"github.com/caesium-cloud/caesium/pkg/log"
)

type consoleTransport struct{}

func NewConsoleTransport() Transport {
	return &consoleTransport{}
}

func (t *consoleTransport) Emit(_ context.Context, event RunEvent) error {
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	log.Info("openlineage event",
		"event_type", string(event.EventType),
		"job", event.Job.Name,
		"run_id", event.Run.RunID.String(),
		"payload", string(data),
	)
	return nil
}

func (t *consoleTransport) Close() error { return nil }
