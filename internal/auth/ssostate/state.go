// Package ssostate contains provider-independent login state primitives.
package ssostate

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/url"
	"strings"
)

func ValidateReturnTo(returnTo string, publicOrigin *url.URL, invalid error) (string, error) {
	returnTo = strings.TrimSpace(returnTo)
	if returnTo == "" {
		return "/", nil
	}
	if strings.HasPrefix(returnTo, `\`) || strings.HasPrefix(returnTo, `//`) {
		return "", invalid
	}

	u, err := url.Parse(returnTo)
	if err != nil {
		return "", fmt.Errorf("%w: %v", invalid, err)
	}
	if u.IsAbs() {
		if !sameOrigin(u, publicOrigin) {
			return "", invalid
		}
		return requestURI(u), nil
	}
	if u.Host != "" || !strings.HasPrefix(u.Path, "/") {
		return "", invalid
	}
	return requestURI(u), nil
}

func sameOrigin(a, b *url.URL) bool {
	if a == nil || b == nil {
		return false
	}
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Host, b.Host)
}

func requestURI(u *url.URL) string {
	out := u.EscapedPath()
	if out == "" {
		out = "/"
	}
	if u.RawQuery != "" {
		out += "?" + u.RawQuery
	}
	if u.Fragment != "" {
		out += "#" + u.EscapedFragment()
	}
	return out
}

func RandomURLSafe(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate random value: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
