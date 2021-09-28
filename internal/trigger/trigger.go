package trigger

import (
	"context"

	"github.com/google/uuid"
)

// Trigger
type Trigger interface {
	Listen(ctx context.Context) <-chan struct{}
	Fire() error
	ID() uuid.UUID
}
