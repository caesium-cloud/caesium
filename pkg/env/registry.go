package env

import (
	"fmt"
	"strings"
)

// RegistryAuth maps container registry hosts to the `secret://` reference that
// holds that registry's pull credentials, parsed from CAESIUM_REGISTRY_AUTH.
//
// The value is a comma-separated list of `host=secret://provider/path` pairs:
//
//	CAESIUM_REGISTRY_AUTH="ghcr.io=secret://env/GHCR_PULL,registry.example.com:5000=secret://vault/kv/data/registry#creds"
//
// Hosts are matched case-insensitively; Docker Hub may be written as
// `docker.io`, `index.docker.io` or the config.json form
// `https://index.docker.io/v1/`. A host without a port matches that host on
// any port; a host with a port matches exactly. The referenced secret's value
// is `username:password`, a `{"username","password"}` JSON object, or a Docker
// config.json / Kubernetes `.dockerconfigjson` document (see
// internal/imagecheck.ParseCredentialValue). Only the reference is configured
// here — no credential ever appears in the environment variable itself.
type RegistryAuth map[string]string

// Decode implements envconfig.Decoder.
func (r *RegistryAuth) Decode(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		*r = nil
		return nil
	}
	out := make(RegistryAuth)
	for _, pair := range strings.Split(value, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		host, ref, ok := strings.Cut(pair, "=")
		host = strings.ToLower(strings.TrimSpace(host))
		ref = strings.TrimSpace(ref)
		if !ok || host == "" || ref == "" {
			return fmt.Errorf("decode registry auth: entry %q must be host=secret://provider/path", pair)
		}
		if !strings.HasPrefix(strings.ToLower(ref), "secret://") {
			return fmt.Errorf("decode registry auth: entry for %s must reference a secret:// URI, not a literal credential", host)
		}
		if _, dup := out[host]; dup {
			return fmt.Errorf("decode registry auth: host %s is listed more than once", host)
		}
		out[host] = ref
	}
	if len(out) == 0 {
		*r = nil
		return nil
	}
	*r = out
	return nil
}
