package agent

import (
	authmw "github.com/caesium-cloud/caesium/api/middleware"
	"github.com/caesium-cloud/caesium/internal/auth"
	iincident "github.com/caesium-cloud/caesium/internal/incident"
	"github.com/labstack/echo/v5"
)

// readerConfinement classifies the caller for the incident's legacy agent
// content (identity-and-access A8): an agent session and a namespace-confined
// principal are confined; a principal holding viewer at "*", and every
// request with authentication disabled, are not. An undecodable scope is
// confined.
func readerConfinement(c *echo.Context) iincident.ReaderConfinement {
	principal := authmw.GetPrincipal(c)
	if principal == nil {
		return iincident.ReaderUnrestricted
	}
	claim, err := auth.DecodeAgentClaim(principal.Scope)
	if err != nil || claim != nil {
		return iincident.ReaderConfined
	}
	if principal.AllowedNamespaces() != nil {
		return iincident.ReaderConfined
	}
	return iincident.ReaderUnrestricted
}

// legacyAuthor reports whether the caller writes with an agent credential
// minted before agent sessions were namespace-confined (identity-and-access
// A8): such a session may have read other namespaces' jobs, so its notes and
// proposals are withheld from confined readers. An undecodable scope counts
// as legacy; a human principal, or no principal (auth disabled), does not.
func legacyAuthor(c *echo.Context) bool {
	principal := authmw.GetPrincipal(c)
	if principal == nil {
		return false
	}
	claim, err := auth.DecodeAgentClaim(principal.Scope)
	if err != nil {
		return true
	}
	return claim != nil && !claim.NamespaceConfined
}
