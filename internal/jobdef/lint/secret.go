package lint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	jobdefruntime "github.com/caesium-cloud/caesium/internal/jobdef/runtime"
	"github.com/caesium-cloud/caesium/internal/jobdef/secret"
	"github.com/caesium-cloud/caesium/internal/models"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
)

// CollectSecretReferences returns all unique secret:// references contained within the definition.
func CollectSecretReferences(def schema.Definition) ([]string, error) {
	data, err := json.Marshal(def)
	if err != nil {
		return nil, fmt.Errorf("marshal definition: %w", err)
	}

	var payload any
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("decode definition: %w", err)
	}

	refs := make([]string, 0)
	seen := make(map[string]struct{})

	var walk func(any)
	walk = func(val any) {
		switch v := val.(type) {
		case string:
			if strings.HasPrefix(v, "secret://") {
				if _, ok := seen[v]; !ok {
					seen[v] = struct{}{}
					refs = append(refs, v)
				}
			}
		case []any:
			for _, item := range v {
				walk(item)
			}
		case map[string]any:
			for _, item := range v {
				walk(item)
			}
		}
	}

	walk(payload)
	return refs, nil
}

// CheckSecrets resolves all secret references for the provided definitions and returns a list of error strings.
func CheckSecrets(ctx context.Context, resolver secret.Resolver, defs []schema.Definition) []string {
	seen := make(map[string]error)
	errs := make([]string, 0)

	for _, def := range defs {
		refs, err := CollectSecretReferences(def)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", def.Metadata.Alias, err))
			continue
		}
		for _, ref := range refs {
			if prevErr, ok := seen[ref]; ok {
				if prevErr != nil {
					errs = append(errs, fmt.Sprintf("%s: secret %s: %v", def.Metadata.Alias, ref, prevErr))
				}
				continue
			}
			_, resolveErr := resolver.Resolve(ctx, ref)
			seen[ref] = resolveErr
			if resolveErr != nil {
				errs = append(errs, fmt.Sprintf("%s: secret %s: %v", def.Metadata.Alias, ref, resolveErr))
			}
		}
	}

	return errs
}

// ServerSecretResolver is the resolver chain the server's own environment
// configures (jobdefruntime.ProcessSecretResolver). Server lint and apply
// preview only canonicalise references with it (no secret is read), so they
// match exactly what a run of the same job would resolve.
var ServerSecretResolver = jobdefruntime.ProcessSecretResolver

// CheckSecretsAllowed checks every secret:// reference of each definition
// against its namespace's secret rules (the process access policy, read now)
// without resolving anything, and returns one message per denied reference.
// A definition whose namespace has no rules is skipped, so with no policy rules
// it returns nothing and never builds a resolver. Messages name the redacted
// reference only.
func CheckSecretsAllowed(defs []schema.Definition, resolver func() (secret.Resolver, error)) []string {
	var msgs []string
	var inner secret.Resolver
	var innerErr error
	built := false
	for _, def := range defs {
		namespace := models.NamespaceOrDefault(def.Metadata.Namespace)
		if secret.ForNamespace(nil, namespace).AllowsAll() {
			continue
		}
		refs, err := CollectSecretReferences(def)
		if err != nil {
			msgs = append(msgs, fmt.Sprintf("%s: %v", def.Metadata.Alias, err))
			continue
		}
		if len(refs) == 0 {
			continue
		}
		if !built {
			built = true
			if resolver == nil {
				innerErr = errors.New("no secret resolver is configured")
			} else {
				inner, innerErr = resolver()
			}
		}
		if innerErr != nil {
			msgs = append(msgs, fmt.Sprintf("%s: secret rules of namespace %q cannot be checked: %v", def.Metadata.Alias, namespace, innerErr))
			continue
		}
		scope := secret.ForNamespace(inner, namespace)
		slices.Sort(refs)
		for _, ref := range refs {
			if err := scope.Allow(ref); err != nil {
				msgs = append(msgs, fmt.Sprintf("%s: %v", def.Metadata.Alias, err))
			}
		}
	}
	return msgs
}
