// Package sqlcell decodes SQL evidence without silently losing numeric precision.
package sqlcell

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// Decode preserves dynamic numeric cells as json.Number and accepts one JSON value.
func Decode(raw []byte, dst any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err != nil {
			return fmt.Errorf("trailing JSON: %w", err)
		}
		return fmt.Errorf("multiple JSON values")
	}
	return nil
}
