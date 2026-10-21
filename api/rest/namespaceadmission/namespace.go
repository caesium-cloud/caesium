// Package namespaceadmission answers the REST refusals of the namespace
// admission gate (identity-and-access D1): 409 namespace_quota with
// Retry-After, and 409 namespace_undeclared. Retry, partition retry, replay
// and run start controllers share it.
package namespaceadmission

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	runstorage "github.com/caesium-cloud/caesium/internal/run"
	"github.com/labstack/echo/v5"
)

// QuotaResponse is the 409 body of a retry, partition retry or
// replay refused because its namespace is at its concurrent-run quota
// (identity-and-access D1). The response carries Retry-After.
type QuotaResponse struct {
	Error             string `json:"error"`
	Message           string `json:"message"`
	Namespace         string `json:"namespace"`
	MaxConcurrentRuns int    `json:"max_concurrent_runs"`
}

// UndeclaredResponse is the 409 body of a start, retry or replay in
// a namespace the served access policy file no longer declares.
type UndeclaredResponse struct {
	Error     string `json:"error"`
	Message   string `json:"message"`
	Namespace string `json:"namespace"`
}

// WriteConflict answers err when it is a namespace
// admission refusal — 409 namespace_quota with Retry-After, or 409
// namespace_undeclared — and reports whether it did.
func WriteConflict(c *echo.Context, err error) (bool, error) {
	if quota, ok := errors.AsType[*runstorage.NamespaceQuotaError](err); ok {
		c.Response().Header().Set("Retry-After", strconv.Itoa(int(runstorage.NamespaceQuotaRetryAfter/time.Second)))
		return true, c.JSON(http.StatusConflict, QuotaResponse{
			Error: "namespace_quota",
			Message: fmt.Sprintf("namespace %q already has %d run(s) running, its quotas.maxConcurrentRuns; "+
				"retry once one of them finishes", quota.Namespace, quota.Limit),
			Namespace:         quota.Namespace,
			MaxConcurrentRuns: quota.Limit,
		})
	}
	if undeclared, ok := errors.AsType[*runstorage.NamespaceUndeclaredError](err); ok {
		return true, c.JSON(http.StatusConflict, UndeclaredResponse{
			Error: "namespace_undeclared",
			Message: fmt.Sprintf("namespace %q is not declared by the served access policy, so no run may start in it; "+
				"declare it in the policy file or move the job", undeclared.Namespace),
			Namespace: undeclared.Namespace,
		})
	}
	return false, nil
}
