package trigger

import (
	"context"

	"github.com/google/uuid"
)

// Trigger
type Trigger interface {
	Listen(ctx context.Context)
	Fire(ctx context.Context) error
	ID() uuid.UUID
}
