package jobdef

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/caesium-cloud/caesium/internal/models"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
)

// ErrNamespaceNotDeclared is wrapped by every undeclared-namespace error.
var ErrNamespaceNotDeclared = errors.New("namespace is not declared in the access policy")

// ValidateDeclaredNamespaces rejects definitions whose metadata.namespace is
// not declared by the access policy file the server loaded
// (identity-and-access design §5.1, plan item B2). It reads the process-wide
// policy holder at call time, so a reload takes effect on the next apply.
//
// Without a policy file every syntactically valid namespace is accepted, as
// B1 shipped namespaces: the env translation declares no namespace settings
// and there is nothing to check against.
func ValidateDeclaredNamespaces(defs []schema.Definition) error {
	holder := policy.ProcessHolder()
	if !holder.FileBacked() {
		return nil
	}
	messages := undeclaredNamespaceMessages(holder, defs)
	if len(messages) == 0 {
		return nil
	}
	return &UndeclaredNamespaceError{Messages: messages}
}

// UndeclaredNamespaceError lists every definition of a batch whose namespace
// is not declared. It wraps ErrNamespaceNotDeclared.
type UndeclaredNamespaceError struct {
	Messages []string
}

func (e *UndeclaredNamespaceError) Error() string { return strings.Join(e.Messages, "; ") }

func (e *UndeclaredNamespaceError) Unwrap() error { return ErrNamespaceNotDeclared }

// UndeclaredNamespaceMessages returns one message per definition whose
// namespace the loaded policy file does not declare, sorted, or nil when there
// is no policy file. Server lint reports each as its own error.
func UndeclaredNamespaceMessages(defs []schema.Definition) []string {
	holder := policy.ProcessHolder()
	if !holder.FileBacked() {
		return nil
	}
	return undeclaredNamespaceMessages(holder, defs)
}

// ValidateDeclaredNamespace is ValidateDeclaredNamespaces for one job created
// outside a definition (the legacy POST /v1/jobs body).
func ValidateDeclaredNamespace(alias, namespace string) error {
	return ValidateDeclaredNamespaces([]schema.Definition{{Metadata: schema.Metadata{Alias: alias, Namespace: namespace}}})
}

func undeclaredNamespaceMessages(holder *policy.Holder, defs []schema.Definition) []string {
	var messages []string
	for i := range defs {
		namespace := models.NamespaceOrDefault(defs[i].Metadata.Namespace)
		if _, declared := holder.Namespace(namespace); declared {
			continue
		}
		messages = append(messages, fmt.Sprintf("metadata.namespace %q of job %q is not declared in the access policy", namespace, defs[i].Metadata.Alias))
	}
	sort.Strings(messages)
	return messages
}
