package lint

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/caesium-cloud/caesium/internal/jobdef/secret"
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
