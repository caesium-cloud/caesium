package models

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// DefaultNamespace is the tenancy of jobs created before namespace ownership.
const DefaultNamespace = "default"

// ClusterNamespace is the cluster-shared sentinel for shared objects
// (notification channels and policies, agent profiles). It is never a job
// namespace.
const ClusterNamespace = "*"

// ErrNamespaceImmutable reports an update that tried to move a shared object to
// another namespace. Moving an object is delete + create.
var ErrNamespaceImmutable = errors.New("namespace is create-only")

// objectNamespaceLabel is a local copy of the DNS-label syntax used for job
// namespaces (pkg/jobdef.ValidateNamespace, internal/auth/policy). models cannot
// import internal/auth/policy (it imports models), so a parity unit test pins
// this copy to pkg/jobdef.ValidateNamespace.
var objectNamespaceLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// NamespaceOrDefault interprets legacy empty ownership as the default namespace.
func NamespaceOrDefault(namespace string) string {
	if namespace == "" {
		return DefaultNamespace
	}
	return namespace
}

// NormalizeObjectNamespace resolves the namespace a shared-object create body
// asks for: surrounding space is trimmed, blank means DefaultNamespace (never
// ClusterNamespace — the shared namespace must be explicit), "*" is allowed,
// and anything else must be a DNS label. Route authorisation (A3) must reuse
// this for POST bodies so middleware and service resolve the same namespace.
func NormalizeObjectNamespace(namespace string) (string, error) {
	namespace = strings.TrimSpace(namespace)
	switch {
	case namespace == "":
		return DefaultNamespace, nil
	case namespace == ClusterNamespace:
		return ClusterNamespace, nil
	case objectNamespaceLabel.MatchString(namespace):
		return namespace, nil
	default:
		return "", fmt.Errorf("namespace %q must be %q or a DNS label (1-63 lowercase letters, digits or hyphens, beginning and ending with a letter or digit)", namespace, ClusterNamespace)
	}
}

// ObjectVisibleFrom reports whether an object owned by objectNamespace may be
// referenced from ownerNamespace: the object is cluster-shared or lives in the
// owner's own namespace. Legacy empty values on either side mean default.
func ObjectVisibleFrom(ownerNamespace, objectNamespace string) bool {
	if objectNamespace == ClusterNamespace {
		return true
	}
	return NamespaceOrDefault(objectNamespace) == NamespaceOrDefault(ownerNamespace)
}

// CheckNamespaceUnchanged enforces that namespace is create-only on update
// bodies. An omitted (nil) or blank request means no change; otherwise the
// requested value must normalise to the stored namespace. The returned error
// wraps ErrNamespaceImmutable on a move and is a syntax error otherwise.
func CheckNamespaceUnchanged(stored string, requested *string) error {
	if requested == nil || strings.TrimSpace(*requested) == "" {
		return nil
	}
	namespace, err := NormalizeObjectNamespace(*requested)
	if err != nil {
		return err
	}
	if namespace != NamespaceOrDefault(stored) {
		return fmt.Errorf("%w: object is in namespace %q; moving it requires delete + create", ErrNamespaceImmutable, NamespaceOrDefault(stored))
	}
	return nil
}
