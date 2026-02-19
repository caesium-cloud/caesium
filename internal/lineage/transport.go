package lineage

import (
	"context"
)

type Transport interface {
	Emit(ctx context.Context, event RunEvent) error

	Close() error
}
