package middleware

import (
	"net"
	"net/http"
	"strings"

	"github.com/caesium-cloud/caesium/pkg/log"
)

// ParseTrustedProxyRanges parses a comma-separated proxy allowlist into IP
// ranges. Entries may be CIDR ranges or individual IP addresses.
func ParseTrustedProxyRanges(raw string) []*net.IPNet {
	parts := strings.Split(raw, ",")
	ranges := make([]*net.IPNet, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		if _, ipNet, err := net.ParseCIDR(part); err == nil {
			ranges = append(ranges, ipNet)
			continue
		}

		if ip := net.ParseIP(part); ip != nil {
			bits := 32
			if ip.To4() == nil {
				bits = 128
			}
			ranges = append(ranges, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
			continue
		}

		log.Warn("ignoring invalid trusted proxy entry", "value", part)
	}
	return ranges
}

// RequestIsSecure reports whether the original request is HTTPS. Forwarded
// protocol headers are trusted only when the immediate peer is allowlisted.
func RequestIsSecure(r *http.Request, trustedProxies []*net.IPNet) bool {
	if r.TLS != nil {
		return true
	}
	if !requestFromTrustedProxy(r, trustedProxies) {
		return false
	}
	proto := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0])
	return strings.EqualFold(proto, "https")
}

func requestFromTrustedProxy(r *http.Request, trustedProxies []*net.IPNet) bool {
	if len(trustedProxies) == 0 {
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	for _, ipNet := range trustedProxies {
		if ipNet.Contains(ip) {
			return true
		}
	}
	return false
}
