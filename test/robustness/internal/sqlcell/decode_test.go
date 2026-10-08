package sqlcell

import (
	"encoding/json"
	"testing"
)

func TestDecodePreservesExactCellsAndMetadata(t *testing.T) {
	var response struct {
		Rows     [][]any `json:"rows"`
		RowCount int     `json:"row_count"`
	}
	err := Decode([]byte(`{"rows":[[9007199254740993,"7",null,true]],"row_count":1}`), &response)
	if err != nil {
		t.Fatal(err)
	}
	if response.RowCount != 1 || len(response.Rows) != 1 || len(response.Rows[0]) != 4 {
		t.Fatalf("metadata/rows changed: %+v", response)
	}
	if got := response.Rows[0][0]; got != json.Number("9007199254740993") {
		t.Fatalf("numeric evidence rounded: %#v", got)
	}
	if response.Rows[0][1] != "7" || response.Rows[0][2] != nil || response.Rows[0][3] != true {
		t.Fatalf("nonnumeric cells changed: %#v", response.Rows[0])
	}
}

func TestDecodeRequiresSingleCompleteValue(t *testing.T) {
	for _, raw := range []string{``, `{"x":`, `{} {}`, `{} null`, `{} trailing`} {
		t.Run(raw, func(t *testing.T) {
			var value any
			if err := Decode([]byte(raw), &value); err == nil {
				t.Fatalf("accepted invalid/trailing JSON %q", raw)
			}
		})
	}
	var value any
	if err := Decode([]byte(" \n {\"x\":1} \t\n"), &value); err != nil {
		t.Fatal(err)
	}
}
