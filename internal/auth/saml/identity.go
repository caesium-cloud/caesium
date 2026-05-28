package saml

import (
	"errors"
	"strings"

	authpkg "github.com/caesium-cloud/caesium/internal/auth"
	crewsaml "github.com/crewjam/saml"
)

var ErrInvalidAssertion = errors.New("invalid saml assertion")

func identityFromAssertion(assertion *crewsaml.Assertion, groupsAttribute string) (*authpkg.ExternalIdentity, error) {
	if assertion == nil {
		return nil, fmtInvalidAssertion("missing assertion")
	}
	if assertion.Subject == nil || assertion.Subject.NameID == nil || strings.TrimSpace(assertion.Subject.NameID.Value) == "" {
		return nil, fmtInvalidAssertion("missing subject NameID")
	}
	issuer := strings.TrimSpace(assertion.Issuer.Value)
	if issuer == "" {
		return nil, fmtInvalidAssertion("missing issuer")
	}

	subject := strings.TrimSpace(assertion.Subject.NameID.Value)
	email := firstAttributeValue(assertion, samlEmailAttributes...)
	displayName := firstAttributeValue(assertion, samlDisplayNameAttributes...)
	if displayName == "" {
		displayName = email
	}
	if displayName == "" {
		displayName = subject
	}

	return &authpkg.ExternalIdentity{
		Issuer:      issuer,
		Subject:     subject,
		Email:       email,
		DisplayName: displayName,
		Groups:      attributeValues(assertion, groupsAttribute),
	}, nil
}

var samlEmailAttributes = []string{
	"email",
	"mail",
	"emailAddress",
	"http://schemas.xmlsoap.org/ws/2005/05/identity/claims/emailaddress",
}

var samlDisplayNameAttributes = []string{
	"displayName",
	"name",
	"cn",
	"http://schemas.xmlsoap.org/ws/2005/05/identity/claims/name",
}

func firstAttributeValue(assertion *crewsaml.Assertion, names ...string) string {
	for _, name := range names {
		if values := attributeValues(assertion, name); len(values) > 0 {
			return values[0]
		}
	}
	return ""
}

func attributeValues(assertion *crewsaml.Assertion, name string) []string {
	name = strings.TrimSpace(name)
	if assertion == nil || name == "" {
		return nil
	}

	var out []string
	seen := make(map[string]struct{})
	for _, statement := range assertion.AttributeStatements {
		for _, attribute := range statement.Attributes {
			if !attributeNameMatches(attribute, name) {
				continue
			}
			for _, value := range attribute.Values {
				raw := value.Value
				if raw == "" && value.NameID != nil {
					raw = value.NameID.Value
				}
				raw = strings.TrimSpace(raw)
				if raw == "" {
					continue
				}
				if _, ok := seen[raw]; ok {
					continue
				}
				seen[raw] = struct{}{}
				out = append(out, raw)
			}
		}
	}
	return out
}

func attributeNameMatches(attribute crewsaml.Attribute, name string) bool {
	return strings.EqualFold(strings.TrimSpace(attribute.Name), name) ||
		strings.EqualFold(strings.TrimSpace(attribute.FriendlyName), name)
}

func fmtInvalidAssertion(detail string) error {
	return errors.Join(ErrInvalidAssertion, errors.New(detail))
}
