package imagecheck

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseReference(t *testing.T) {
	const digest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	cases := []struct {
		in   string
		want Reference
	}{
		// Docker Hub shorthand: no registry, no namespace -> library/, latest.
		{"redis", Reference{Registry: DockerHubRegistry, Repository: "library/redis", Tag: "latest"}},
		{"redis:3.23", Reference{Registry: DockerHubRegistry, Repository: "library/redis", Tag: "3.23"}},
		{"myorg/app:1.0", Reference{Registry: DockerHubRegistry, Repository: "myorg/app", Tag: "1.0"}},
		// Explicit Docker Hub hosts normalise to the canonical registry host.
		{"docker.io/library/redis:3.23", Reference{Registry: DockerHubRegistry, Repository: "library/redis", Tag: "3.23"}},
		{"docker.io/redis:3.23", Reference{Registry: DockerHubRegistry, Repository: "library/redis", Tag: "3.23"}},
		{"index.docker.io/myorg/app", Reference{Registry: DockerHubRegistry, Repository: "myorg/app", Tag: "latest"}},
		{"registry-1.docker.io/myorg/app", Reference{Registry: DockerHubRegistry, Repository: "myorg/app", Tag: "latest"}},
		// Private registries: a first component with a dot, a port, or
		// "localhost" is a registry host; nested paths are preserved.
		{"registry.example.com/team/app:v1", Reference{Registry: "registry.example.com", Repository: "team/app", Tag: "v1"}},
		{"registry.example.com:5000/team/app:v1", Reference{Registry: "registry.example.com:5000", Repository: "team/app", Tag: "v1"}},
		{"registry.example.com:5000/app", Reference{Registry: "registry.example.com:5000", Repository: "app", Tag: "latest"}},
		{"localhost/app:dev", Reference{Registry: "localhost", Repository: "app", Tag: "dev"}},
		{"localhost:5000/a/b/c:dev", Reference{Registry: "localhost:5000", Repository: "a/b/c", Tag: "dev"}},
		{"127.0.0.1:5000/private/app:1.0", Reference{Registry: "127.0.0.1:5000", Repository: "private/app", Tag: "1.0"}},
		// Digest references: the digest is carried and the tag (if any) kept.
		{"redis@" + digest, Reference{Registry: DockerHubRegistry, Repository: "library/redis", Digest: digest}},
		{"registry.example.com/app:1.0@" + digest, Reference{Registry: "registry.example.com", Repository: "app", Tag: "1.0", Digest: digest}},
		// Surrounding whitespace is tolerated.
		{"  redis:3.23  ", Reference{Registry: DockerHubRegistry, Repository: "library/redis", Tag: "3.23"}},
	}
	for _, tc := range cases {
		got, err := ParseReference(tc.in)
		require.NoError(t, err, "ParseReference(%q)", tc.in)
		assert.Equal(t, tc.want, got, "ParseReference(%q)", tc.in)
	}
}

func TestParseReference_Invalid(t *testing.T) {
	for _, in := range []string{
		"",
		"   ",
		"registry.example.com/",
		"registry.example.com:5000/",
		"app:",
		"app:tag with space",
		"app@sha256:notahexdigest",
		"app@md5:abcd",
		"has space/app:1.0",
		"registry.example.com//app",
	} {
		_, err := ParseReference(in)
		assert.Error(t, err, "ParseReference(%q) must fail", in)
	}
}

func TestReference_ManifestURL(t *testing.T) {
	ref := Reference{Registry: "registry.example.com:5000", Repository: "team/app", Tag: "v1"}
	assert.Equal(t, "https://registry.example.com:5000/v2/team/app/manifests/v1", ref.manifestURL())

	// A digest reference addresses the manifest by digest.
	const digest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	ref.Digest = digest
	assert.Equal(t, "https://registry.example.com:5000/v2/team/app/manifests/"+digest, ref.manifestURL())

	// Loopback registries are plain HTTP, matching the Docker daemon's default
	// insecure-registry rule for 127.0.0.0/8 and localhost.
	for _, host := range []string{"localhost", "localhost:5000", "127.0.0.1:5000", "[::1]:5000"} {
		r := Reference{Registry: host, Repository: "app", Tag: "1"}
		assert.Equal(t, "http://"+host+"/v2/app/manifests/1", r.manifestURL(), host)
	}
}

func TestNormalizeRegistryHost(t *testing.T) {
	cases := map[string]string{
		"docker.io":                    "docker.io",
		"index.docker.io":              "docker.io",
		"registry-1.docker.io":         "docker.io",
		"https://index.docker.io/v1/":  "docker.io",
		"https://registry.example.com": "registry.example.com",
		"Registry.Example.COM:5000/":   "registry.example.com:5000",
		"  ghcr.io  ":                  "ghcr.io",
		"":                             "",
	}
	for in, want := range cases {
		assert.Equal(t, want, NormalizeRegistryHost(in), "NormalizeRegistryHost(%q)", in)
	}
}
