// Package orderby parses SQL order terms against a caller-provided allowlist.
package orderby

import (
	"fmt"
	"strings"
)

// Parse validates and normalizes comma-separated order terms. Accepted format
// per term: "column", "column asc", or "column desc".
func Parse(raw string, allowed map[string]struct{}) ([]string, error) {
	parts := strings.Split(raw, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		tokens := strings.Fields(part)
		col := strings.ToLower(tokens[0])
		if _, ok := allowed[col]; !ok {
			return nil, fmt.Errorf("invalid order_by column: %q", tokens[0])
		}
		dir := "asc"
		if len(tokens) > 1 {
			switch strings.ToLower(tokens[1]) {
			case "asc":
				dir = "asc"
			case "desc":
				dir = "desc"
			default:
				return nil, fmt.Errorf("invalid order_by direction: %q", tokens[1])
			}
		}
		if len(tokens) > 2 {
			return nil, fmt.Errorf("invalid order_by term: %q", part)
		}
		result = append(result, col+" "+dir)
	}
	return result, nil
}
