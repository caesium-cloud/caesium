package dataset

import (
	"fmt"
	"io"
)

func validatePagination(limit, offset int) error {
	if limit < 1 || limit > 200 {
		return fmt.Errorf("--limit must be between 1 and 200")
	}
	if offset < 0 {
		return fmt.Errorf("--offset must be non-negative")
	}
	return nil
}

func renderPagination(w io.Writer, noun string, count int, total int64, limit, offset int) {
	_, _ = fmt.Fprintf(w, "\nShowing %d of %d %s (limit %d, offset %d).\n", count, total, noun, limit, offset)
	if next := offset + count; int64(next) < total && count > 0 {
		_, _ = fmt.Fprintf(w, "More %s available; use --offset %d for the next page.\n", noun, next)
	}
}
