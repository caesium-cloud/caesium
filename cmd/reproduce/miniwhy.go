package reproduce

import (
	"fmt"
	"regexp"
	"strings"
)

// miniWhySensitiveKey matches credential-like field names. CamelCase is split
// first so apiToken and kubernetesSecretNamespace both match.
var miniWhySensitiveKey = regexp.MustCompile(`(?i)(^|[_-])(password|passwd|secret|token|credential|api[_-]?key|authorization|private[_-]?key)($|[_-])`)

// formatMiniWhy renders the inline explanation for stderr. Redacted and
// credential-like values are never printed; secret:// references are.
func formatMiniWhy(why *miniWhy) string {
	if why == nil {
		return ""
	}
	if why.BaselineKind == "none" {
		return "mini-why: no prior run to compare against"
	}
	if why.Degraded != "" {
		return "mini-why: " + why.Degraded
	}
	compared := ""
	if why.ComparedToRunID != "" {
		compared = " (prior run " + why.ComparedToRunID + ")"
	}
	if len(why.Fields) == 0 {
		return "mini-why: no input field differs from the prior run" + compared
	}
	const maxFields = 8
	parts := make([]string, 0, len(why.Fields))
	for _, field := range why.Fields {
		parts = append(parts, formatMiniWhyField(field))
	}
	extra := ""
	if len(parts) > maxFields {
		extra = fmt.Sprintf(" and %d more", len(parts)-maxFields)
		parts = parts[:maxFields]
	}
	return "mini-why: " + strings.Join(parts, "; ") + extra + compared
}

func formatMiniWhyField(field miniWhyField) string {
	if miniWhyHideValues(field) {
		switch field.Change {
		case "added":
			return field.Field + " added (redacted)"
		case "removed":
			return field.Field + " removed (redacted)"
		default:
			return field.Field + " changed (redacted)"
		}
	}
	switch field.Change {
	case "added":
		if field.After == "" {
			return field.Field + " added"
		}
		return fmt.Sprintf("%s added (%s)", field.Field, field.After)
	case "removed":
		if field.Before == "" {
			return field.Field + " removed"
		}
		return fmt.Sprintf("%s removed (%s)", field.Field, field.Before)
	default:
		if field.Before == "" && field.After == "" {
			return field.Field + " changed"
		}
		return fmt.Sprintf("%s changed %s→%s", field.Field, miniWhyDash(field.Before), miniWhyDash(field.After))
	}
}

// miniWhyHideValues drops before/after unless every present value is a
// secret:// reference. A redacted flag from the server wins even if a value
// was left populated.
func miniWhyHideValues(field miniWhyField) bool {
	if field.Redacted {
		return true
	}
	if !miniWhySensitive(field.Field) {
		return false
	}
	if field.Before != "" && !strings.HasPrefix(field.Before, "secret://") {
		return true
	}
	if field.After != "" && !strings.HasPrefix(field.After, "secret://") {
		return true
	}
	return field.Before == "" && field.After == ""
}

func miniWhySensitive(field string) bool {
	last := field
	if i := strings.LastIndex(field, "."); i >= 0 {
		last = field[i+1:]
	}
	return miniWhySensitiveKey.MatchString(miniWhyCamelBoundaries(last))
}

func miniWhyCamelBoundaries(s string) string {
	var b strings.Builder
	for i, r := range s {
		if i > 0 && r >= 'A' && r <= 'Z' {
			prev := s[i-1]
			if prev != '_' && prev != '-' {
				b.WriteByte('_')
			}
		}
		b.WriteRune(r)
	}
	return b.String()
}

func miniWhyDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
