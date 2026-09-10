package imagecheck

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/caesium-cloud/caesium/internal/jobdef/secret"
)

// CredentialsFromSecrets builds a CredentialFunc from an operator mapping of
// registry host -> `secret://` URI (CAESIUM_REGISTRY_AUTH) resolved through the
// configured secret providers (env, k8s, vault). Keys are normalised with
// NormalizeRegistryHost, so `docker.io`, `index.docker.io` and the config.json
// form `https://index.docker.io/v1/` all name Docker Hub. A key without a port
// matches that host on any port; a key with a port matches exactly, and wins.
//
// The resolved secret value is parsed by ParseCredentialValue. The secret is
// re-resolved on every lookup — lookups only happen on a digest resolution,
// which the Resolver already caches per TTL, and re-reading means a rotated
// credential is picked up without a restart.
//
// A nil or empty mapping yields a func that reports no credentials for every
// host (anonymous probing), which is the pre-existing behaviour.
func CredentialsFromSecrets(mapping map[string]string, resolver secret.Resolver) CredentialFunc {
	normalized := make(map[string]string, len(mapping))
	for host, ref := range mapping {
		if key := NormalizeRegistryHost(host); key != "" {
			normalized[key] = strings.TrimSpace(ref)
		}
	}
	return func(ctx context.Context, registry string) (Credentials, bool, error) {
		ref, ok := lookupRegistryKey(normalized, registry)
		if !ok {
			return Credentials{}, false, nil
		}
		if resolver == nil {
			return Credentials{}, false, fmt.Errorf("registry %s is mapped to %s but no secret resolver is configured", registry, ref)
		}
		value, err := resolver.Resolve(ctx, ref)
		if err != nil {
			// The secret providers' errors name the reference, never the value.
			return Credentials{}, false, fmt.Errorf("resolve %s: %w", ref, err)
		}
		creds, err := ParseCredentialValue(value, registry)
		if err != nil {
			return Credentials{}, false, fmt.Errorf("%s: %w", ref, err)
		}
		return creds, true, nil
	}
}

// lookupRegistryKey finds the mapping entry for a registry host: exact
// host[:port] first, then the bare host when the reference carries a port.
func lookupRegistryKey(mapping map[string]string, registry string) (string, bool) {
	host := NormalizeRegistryHost(registry)
	if host == "" {
		return "", false
	}
	if ref, ok := mapping[host]; ok {
		return ref, true
	}
	if bare := hostWithoutPort(host); bare != host {
		if ref, ok := mapping[bare]; ok {
			return ref, true
		}
	}
	return "", false
}

// ParseCredentialValue interprets the resolved secret value for a registry.
// Accepted shapes, so an operator can point at whatever they already store:
//
//   - `username:password` — the password may itself contain colons; the split
//     is on the first one.
//   - a JSON object `{"username": "...", "password": "..."}`.
//   - a Docker config.json / Kubernetes `.dockerconfigjson` document
//     (`{"auths": {"<host>": {"auth": "<base64 user:pass>"}}}`, or with
//     explicit `username`/`password`), from which the entry for registry is
//     taken — so an existing imagePullSecret can be reused verbatim.
//
// Errors never echo the value.
func ParseCredentialValue(value, registry string) (Credentials, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return Credentials{}, errors.New("registry credential secret is empty")
	}
	if strings.HasPrefix(value, "{") {
		return parseJSONCredential(value, registry)
	}
	user, pass, ok := strings.Cut(value, ":")
	if !ok || strings.TrimSpace(user) == "" {
		return Credentials{}, errors.New("registry credential secret must be `username:password` or a JSON object")
	}
	return Credentials{Username: strings.TrimSpace(user), Password: pass}, nil
}

type jsonAuthEntry struct {
	Auth     string `json:"auth"`
	Username string `json:"username"`
	Password string `json:"password"`
}

func parseJSONCredential(value, registry string) (Credentials, error) {
	var doc struct {
		jsonAuthEntry
		Auths map[string]jsonAuthEntry `json:"auths"`
	}
	if err := json.Unmarshal([]byte(value), &doc); err != nil {
		return Credentials{}, errors.New("registry credential secret is not valid JSON")
	}
	entry := doc.jsonAuthEntry
	if len(doc.Auths) > 0 {
		normalized := make(map[string]jsonAuthEntry, len(doc.Auths))
		for host, e := range doc.Auths {
			normalized[NormalizeRegistryHost(host)] = e
		}
		want := NormalizeRegistryHost(registry)
		e, ok := normalized[want]
		if !ok {
			e, ok = normalized[hostWithoutPort(want)]
		}
		if !ok {
			return Credentials{}, fmt.Errorf("registry credential secret has no auths entry for %s", registry)
		}
		entry = e
	}
	if entry.Auth != "" {
		raw, err := base64.StdEncoding.DecodeString(entry.Auth)
		if err != nil {
			return Credentials{}, errors.New("registry credential secret has a non-base64 auth field")
		}
		user, pass, ok := strings.Cut(string(raw), ":")
		if !ok || user == "" {
			return Credentials{}, errors.New("registry credential secret auth field is not `username:password`")
		}
		return Credentials{Username: user, Password: pass}, nil
	}
	if entry.Username == "" {
		return Credentials{}, errors.New("registry credential secret JSON needs username/password or auth")
	}
	return Credentials{Username: entry.Username, Password: entry.Password}, nil
}
