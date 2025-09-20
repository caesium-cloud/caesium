package run

import (
	"context"

	"github.com/google/uuid"
)

type contextKey struct{}

var runKey contextKey

func WithContext(ctx context.Context, id uuid.UUID) context.Context {
	return context.WithValue(ctx, runKey, id)
}

func FromContext(ctx context.Context) (uuid.UUID, bool) {
	if ctx == nil {
		return uuid.UUID{}, false
	}

	value := ctx.Value(runKey)
	if value == nil {
		return uuid.UUID{}, false
	}

	id, ok := value.(uuid.UUID)
	return id, ok
}
