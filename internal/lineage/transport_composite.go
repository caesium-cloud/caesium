package lineage

import (
	"context"
	"errors"
)

type compositeTransport struct {
	transports []Transport
}

func NewCompositeTransport(transports ...Transport) Transport {
	return &compositeTransport{transports: transports}
}

func (t *compositeTransport) Emit(ctx context.Context, event RunEvent) error {
	var errs []error
	for _, tr := range t.transports {
		if err := tr.Emit(ctx, event); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (t *compositeTransport) Close() error {
	var errs []error
	for _, tr := range t.transports {
		if err := tr.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
