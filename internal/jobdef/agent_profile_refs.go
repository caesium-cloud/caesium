package jobdef

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/caesium-cloud/caesium/internal/models"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"gorm.io/gorm"
)

// ValidateAgentProfileRefs verifies that every server-side resource a
// metadata.remediation block references exists and is visible from the job's
// tenancy namespace: the AgentProfile named by remediation.profile and the
// NotificationChannel named by remediation.escalation.channel must live in the
// job's own namespace or the cluster-shared namespace "*" (design §10.4). This
// is the server-side half of the lint split docs/design/agent-in-the-loop.md
// documents: pkg/jobdef's offline validation (which runs with conn == nil,
// e.g. `caesium job lint`) can only check the references are well-formed and
// emits a scope note, because these are server-side state. This function closes
// that gap and is invoked both from POST /v1/jobdefs/lint and from
// Importer.ValidateBatch before the apply transaction, mirroring
// ValidateDatasetGraph's conn-may-be-nil posture. Definitions are checked in
// order (profile before channel), so the first violation reported is
// deterministic.
//
// The built-in models.DefaultTriageOnlyProfileName is seeded into, and
// migrated to, the cluster-shared namespace "*", so every job namespace may
// reference it. It is treated as resolvable while no row exists — it is the
// shipped default jobs are advertised to reference, seeded lazily — and once a
// row exists it is checked like any other profile (job's namespace or "*").
func ValidateAgentProfileRefs(ctx context.Context, conn *gorm.DB, defs []schema.Definition) error {
	if conn == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	type remediationRef struct {
		alias, namespace, profile, channel string
	}
	refs := make([]remediationRef, 0, len(defs))
	profiles := make(map[string]struct{})
	channels := make(map[string]struct{})
	for i := range defs {
		r := defs[i].Metadata.Remediation
		if r == nil {
			continue
		}
		// POST /v1/jobdefs/lint validates copies of the definitions, so the
		// namespace may still be unnormalised here: blank means default.
		ref := remediationRef{
			alias:     strings.TrimSpace(defs[i].Metadata.Alias),
			namespace: models.NamespaceOrDefault(strings.TrimSpace(defs[i].Metadata.Namespace)),
			profile:   strings.TrimSpace(r.Profile),
		}
		if r.Escalation != nil {
			ref.channel = strings.TrimSpace(r.Escalation.Channel)
		}
		if ref.profile == "" && ref.channel == "" {
			continue
		}
		if ref.profile != "" {
			profiles[ref.profile] = struct{}{}
		}
		if ref.channel != "" {
			channels[ref.channel] = struct{}{}
		}
		refs = append(refs, ref)
	}

	profileNamespaces, err := lookupNamespaces(ctx, conn, &models.AgentProfile{}, profiles)
	if err != nil {
		return err
	}
	channelNamespaces, err := lookupNamespaces(ctx, conn, &models.NotificationChannel{}, channels)
	if err != nil {
		return err
	}

	for _, ref := range refs {
		if ref.profile != "" {
			namespace, found := profileNamespaces[ref.profile]
			switch {
			case !found && ref.profile == models.DefaultTriageOnlyProfileName:
				// Built-in default, not seeded on this server yet.
			case !found:
				return fmt.Errorf("metadata.remediation.profile %q does not reference an existing AgentProfile", ref.profile)
			case !models.ObjectVisibleFrom(ref.namespace, namespace):
				return fmt.Errorf("job %q (namespace %q): metadata.remediation.profile %q references an AgentProfile in namespace %q; a job may reference only objects in its own namespace or the shared namespace %q",
					ref.alias, ref.namespace, ref.profile, models.NamespaceOrDefault(namespace), models.ClusterNamespace)
			}
		}
		if ref.channel != "" {
			namespace, found := channelNamespaces[ref.channel]
			switch {
			case !found:
				return fmt.Errorf("metadata.remediation.escalation.channel %q does not reference an existing notification channel", ref.channel)
			case !models.ObjectVisibleFrom(ref.namespace, namespace):
				return fmt.Errorf("job %q (namespace %q): metadata.remediation.escalation.channel %q references a notification channel in namespace %q; a job may reference only objects in its own namespace or the shared namespace %q",
					ref.alias, ref.namespace, ref.channel, models.NamespaceOrDefault(namespace), models.ClusterNamespace)
			}
		}
	}
	return nil
}

// lookupNamespaces returns the tenancy namespace of every live (not
// soft-deleted) row of model whose name is in want, keyed by name. Names with
// no row are absent from the result.
func lookupNamespaces(ctx context.Context, conn *gorm.DB, model any, want map[string]struct{}) (map[string]string, error) {
	if len(want) == 0 {
		return nil, nil
	}
	lookup := make([]string, 0, len(want))
	for name := range want {
		lookup = append(lookup, name)
	}
	slices.Sort(lookup)

	var rows []struct {
		Name      string
		Namespace string
	}
	if err := conn.WithContext(ctx).Model(model).Where("name IN ?", lookup).Select("name", "namespace").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("validate remediation references: %w", err)
	}
	namespaces := make(map[string]string, len(rows))
	for _, row := range rows {
		namespaces[row.Name] = row.Namespace
	}
	return namespaces, nil
}
