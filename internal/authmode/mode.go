package authmode

import "strings"

// Active reports whether a configured authentication mode or SSO is active.
func Active(mode string, ssoEnabled bool) bool {
	mode = strings.ToLower(strings.TrimSpace(mode))
	return mode != "" && mode != "none" || ssoEnabled
}
