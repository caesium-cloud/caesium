package strutil

import "strings"

// FirstNonBlank returns the first nonblank candidate without changing its bytes.
func FirstNonBlank(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
