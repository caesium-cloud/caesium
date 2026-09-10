package imagecheck

import (
	"fmt"
	"net"
	"regexp"
	"strings"
)

// DockerHubRegistry is the canonical registry host used for Docker Hub
// references. Docker Hub is reachable under several names (`docker.io`,
// `index.docker.io`, `registry-1.docker.io`); references and credential keys
// are normalised to this one so a credential configured for any alias applies
// to all of them and the registry API is addressed at its real endpoint.
const DockerHubRegistry = "docker.io"

// dockerHubAPIHost is the host the Docker Registry HTTP API v2 for Docker Hub
// actually lives on; `docker.io` itself does not serve /v2/.
const dockerHubAPIHost = "registry-1.docker.io"

// dockerHubAliases are the hostnames (lower-cased, without scheme or path)
// that all mean Docker Hub.
var dockerHubAliases = map[string]bool{
	"docker.io":               true,
	"index.docker.io":         true,
	"registry-1.docker.io":    true,
	"registry.docker.io":      true,
	"registry.hub.docker.com": true,
}

var (
	tagPattern    = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.\-]{0,127}$`)
	digestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
)

// Reference is a parsed container image reference, split into the parts the
// registry API addresses: the registry host (with port), the repository path
// under /v2/, and the tag and/or digest.
type Reference struct {
	// Registry is the normalised registry host[:port]. Docker Hub aliases
	// collapse to DockerHubRegistry.
	Registry string
	// Repository is the path under /v2/ (e.g. "library/redis", "team/app").
	Repository string
	// Tag is the tag component; defaulted to "latest" when the reference has
	// neither a tag nor a digest.
	Tag string
	// Digest is the sha256:... component of a digest reference, or "".
	Digest string
}

// ParseReference splits an image reference into registry, repository, tag and
// digest following Docker's rules: the first path component is a registry host
// when it contains a "." or ":" or is "localhost"; otherwise the reference is a
// Docker Hub image, and a single-component Hub name gains the "library/"
// namespace. A reference with neither tag nor digest defaults to "latest".
func ParseReference(ref string) (Reference, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return Reference{}, fmt.Errorf("imagecheck: empty image reference")
	}
	if strings.ContainsAny(ref, " \t\r\n") {
		return Reference{}, fmt.Errorf("imagecheck: invalid image reference %q: contains whitespace", ref)
	}

	var out Reference

	rest := ref
	if at := strings.LastIndex(rest, "@"); at >= 0 {
		out.Digest = rest[at+1:]
		rest = rest[:at]
		if !digestPattern.MatchString(out.Digest) {
			return Reference{}, fmt.Errorf("imagecheck: invalid image reference %q: unsupported digest", ref)
		}
	}

	// Split off the registry host when the first component looks like one.
	var registry, path string
	if slash := strings.Index(rest, "/"); slash >= 0 {
		first := rest[:slash]
		if first == "localhost" || strings.ContainsAny(first, ".:") {
			registry = first
			path = rest[slash+1:]
		} else {
			path = rest
		}
	} else {
		path = rest
	}

	// Split off the tag: a ":" after the last "/" (a port in the registry host
	// has already been removed with the host).
	if colon := strings.LastIndex(path, ":"); colon >= 0 && colon > strings.LastIndex(path, "/") {
		out.Tag = path[colon+1:]
		path = path[:colon]
		if !tagPattern.MatchString(out.Tag) {
			return Reference{}, fmt.Errorf("imagecheck: invalid image reference %q: bad tag", ref)
		}
	}

	if path == "" || strings.HasPrefix(path, "/") || strings.HasSuffix(path, "/") || strings.Contains(path, "//") {
		return Reference{}, fmt.Errorf("imagecheck: invalid image reference %q: empty repository", ref)
	}

	out.Registry = NormalizeRegistryHost(registry)
	if out.Registry == "" {
		out.Registry = DockerHubRegistry
	}
	if out.Registry == DockerHubRegistry && !strings.Contains(path, "/") {
		path = "library/" + path
	}
	out.Repository = path

	if out.Tag == "" && out.Digest == "" {
		out.Tag = "latest"
	}
	return out, nil
}

// NormalizeRegistryHost canonicalises a registry host as it may appear in a
// reference or an operator's credential mapping: lower-cased, stripped of any
// scheme and path (so `https://index.docker.io/v1/` — the Docker config.json
// key for Hub — works), and with every Docker Hub alias collapsed to
// DockerHubRegistry. An empty input stays empty.
func NormalizeRegistryHost(host string) string {
	host = strings.TrimSpace(host)
	if i := strings.Index(host, "://"); i >= 0 {
		host = host[i+3:]
	}
	if i := strings.Index(host, "/"); i >= 0 {
		host = host[:i]
	}
	host = strings.ToLower(host)
	if dockerHubAliases[host] {
		return DockerHubRegistry
	}
	return host
}

// apiHost is the host the registry's HTTP API is served from.
func (r Reference) apiHost() string {
	if r.Registry == DockerHubRegistry {
		return dockerHubAPIHost
	}
	return r.Registry
}

// scheme picks plain HTTP for loopback registries — the same rule the Docker
// daemon applies by default (127.0.0.0/8, ::1 and localhost are insecure
// registries) — and HTTPS everywhere else.
func (r Reference) scheme() string {
	if isLoopbackHost(r.Registry) {
		return "http"
	}
	return "https"
}

// manifestURL is the Registry HTTP API v2 manifest endpoint for this
// reference, addressed by digest when one is present and by tag otherwise.
func (r Reference) manifestURL() string {
	target := r.Tag
	if r.Digest != "" {
		target = r.Digest
	}
	return fmt.Sprintf("%s://%s/v2/%s/manifests/%s", r.scheme(), r.apiHost(), r.Repository, target)
}

// isLoopbackHost reports whether host[:port] names the local machine.
func isLoopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// hostWithoutPort strips a trailing :port from a registry host, if present.
func hostWithoutPort(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return hostport
}
