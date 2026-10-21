package auth

import (
	"net/http"
	"sort"
	"time"

	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/labstack/echo/v5"
)

// NamespacesResponse is the body of GET /v1/auth/namespaces: the declared
// namespaces and their isolation settings, readable by any cluster viewer.
// It deliberately carries no bindings, subjects, groups, emails or file path.
type NamespacesResponse struct {
	Source     string                   `json:"source"`
	Revision   string                   `json:"revision"`
	LoadedAt   time.Time                `json:"loaded_at"`
	LastReload *NamespacesReloadAttempt `json:"last_reload"`
	Namespaces []NamespaceView          `json:"namespaces"`
}

// NamespacesReloadAttempt is the last reload attempt without its error text.
type NamespacesReloadAttempt struct {
	At       time.Time `json:"at"`
	Trigger  string    `json:"trigger"`
	OK       bool      `json:"ok"`
	Changed  bool      `json:"changed"`
	Revision string    `json:"revision,omitempty"`
}

// NamespaceView is one declared namespace and its settings.
type NamespaceView struct {
	Name       string                     `json:"name"`
	Kubernetes *policy.KubernetesSettings `json:"kubernetes,omitempty"`
	// Secrets is the allow-list; null means the namespace allows every secret.
	Secrets *policy.SecretRules `json:"secrets"`
	Quotas  *policy.Quotas      `json:"quotas,omitempty"`
}

// GetNamespaces returns the declared namespaces (cluster viewer).
func (ctrl *Controller) GetNamespaces(c *echo.Context) error {
	holder := ctrl.service.PolicyHolder()
	rev := holder.Current()
	if rev == nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "access policy holder is not configured")
	}
	others := []string{}
	if rev.Policy != nil {
		for name := range rev.Policy.Namespaces {
			if name != policy.DefaultNamespace {
				others = append(others, name)
			}
		}
	}
	sort.Strings(others)
	names := append([]string{policy.DefaultNamespace}, others...)
	resp := NamespacesResponse{
		Source:     rev.Source,
		Revision:   rev.Hash,
		LoadedAt:   rev.LoadedAt,
		Namespaces: make([]NamespaceView, 0, len(names)),
	}
	for _, name := range names {
		// From the captured revision, never holder.Namespace (which re-reads
		// the current one): a concurrent reload must not mix revisions.
		var settings policy.Namespace
		if rev.Policy != nil {
			settings = rev.Policy.Namespaces[name]
		}
		resp.Namespaces = append(resp.Namespaces, NamespaceView{
			Name:       name,
			Kubernetes: settings.Kubernetes,
			Secrets:    settings.Secrets,
			Quotas:     settings.Quotas,
		})
	}
	if attempt := holder.LastReload(); attempt != nil {
		// The error text names the policy file path and rejected tokens, which
		// stay cluster-admin (GET /v1/auth/policy); a viewer only sees that the
		// attempt failed.
		resp.LastReload = &NamespacesReloadAttempt{
			At:       attempt.At,
			Trigger:  attempt.Trigger,
			OK:       attempt.OK,
			Changed:  attempt.Changed,
			Revision: attempt.Revision,
		}
	}
	return c.JSON(http.StatusOK, resp)
}
