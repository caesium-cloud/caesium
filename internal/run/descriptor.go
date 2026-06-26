package run

import (
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/secret"
	"github.com/caesium-cloud/caesium/internal/models"
	"gorm.io/datatypes"
)

// SecretIdentityDescriptorRef converts a resolved secret identity into the
// immutable TaskRun descriptor shape. It intentionally omits the secret value.
func SecretIdentityDescriptorRef(envKey, ref string, identity secret.Identity) models.TaskExecutionSecretRef {
	capturedAt := time.Now().UTC()
	if ref == "" {
		ref = identity.Ref
	}
	provider := identity.Provider
	metadata := datatypes.JSONMap{}
	if identity.Version != "" {
		metadata["version"] = identity.Version
	}
	if identity.ResourceVersion != "" {
		metadata["resourceVersion"] = identity.ResourceVersion
	}
	if identity.Namespace != "" {
		metadata["namespace"] = identity.Namespace
	}
	if identity.Name != "" {
		metadata["name"] = identity.Name
	}
	if identity.Key != "" {
		metadata["key"] = identity.Key
	}
	if identity.KeyID != "" {
		metadata["keyId"] = identity.KeyID
	}
	if identity.HMACSHA256 != "" {
		metadata["hmacSha256"] = identity.HMACSHA256
	}
	for k, v := range identity.Metadata {
		metadata[k] = v
	}
	return models.TaskExecutionSecretRef{
		Ref:                ref,
		EnvKey:             envKey,
		Provider:           provider,
		Identity:           metadata,
		Verifiable:         identity.Verifiable,
		UnverifiableReason: identity.UnverifiableReason,
		IdentityCapturedAt: &capturedAt,
	}
}
