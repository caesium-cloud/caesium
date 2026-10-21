package middleware_test

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func jobdefBody(pairs ...string) string {
	defs := make([]string, 0, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		defs = append(defs, `{"apiVersion":"v1","kind":"Job","metadata":{"alias":"`+pairs[i]+`","namespace":"`+pairs[i+1]+`"}}`)
	}
	return `{"definitions":[` + strings.Join(defs, ",") + `]}`
}

// TestJobdefReadRoutesResolveNamespaces: lint and diff authorise a confined
// principal on every definition's namespace and on the stored namespace of
// every alias they name; the 403 names the namespaces the caller lacks.
func TestJobdefReadRoutesResolveNamespaces(t *testing.T) {
	f := newNSFixture(t)
	mktViewer := f.key(t, models.RoleViewer, `["marketing"]`)
	both := f.key(t, models.RoleViewer, `["marketing","finance"]`)

	for _, route := range []string{"/v1/jobdefs/lint", "/v1/jobdefs/diff"} {
		t.Run(route, func(t *testing.T) {
			ok := f.do(t, mktViewer, post(route, nil, jobdefBody("brand-new", "marketing")))
			require.Equal(t, http.StatusOK, ok.status)
			require.Equal(t, []string{"marketing"}, ok.decision.Namespaces)
			require.Equal(t, jobdefBody("brand-new", "marketing"), ok.body, "the handler reads the whole body")

			// A definition in a namespace the caller lacks.
			res := f.do(t, mktViewer, post(route, nil, jobdefBody("brand-new", "finance")))
			require.Equal(t, http.StatusForbidden, res.status)
			require.Equal(t, `insufficient permissions: viewer required in namespace "finance"`, res.message)
			requireAuditMetadata(t, f.lastDenial(t), map[string]any{"reason": "insufficient_role", "namespaces": []any{"finance"}})

			// An alias that already lives in finance, requested into marketing.
			res = f.do(t, mktViewer, post(route, nil, jobdefBody("ns-fin", "marketing")))
			require.Equal(t, http.StatusForbidden, res.status)
			require.Equal(t, `insufficient permissions: viewer required in namespace "finance"`, res.message)
			require.Equal(t, "*", f.lastDenial(t).Namespace, "the decision spans finance and marketing")

			// Blank is the default namespace; several missing ones are listed.
			res = f.do(t, mktViewer, post(route, nil, jobdefBody("x", "", "y", "sales")))
			require.Equal(t, http.StatusForbidden, res.status)
			require.Equal(t, `insufficient permissions: viewer required in namespaces "default", "sales"`, res.message)

			require.Equal(t, http.StatusOK, f.do(t, both, post(route, nil, jobdefBody("ns-fin", "marketing"))).status)

			// No definitions names no namespace: any viewer grant passes and
			// the handler restricts what it returns.
			for _, body := range []string{`{"definitions":[]}`, `{}`, ""} {
				res := f.do(t, mktViewer, post(route, nil, body))
				require.Equal(t, http.StatusOK, res.status, "body %q", body)
				require.Nil(t, res.decision.Namespaces)
			}

			// Bodies the resolver cannot read the way the handler would.
			res = f.do(t, mktViewer, nsCall{method: http.MethodPost, pattern: route, body: jobdefBody("a", "marketing"), contentType: "text/plain"})
			require.Equal(t, http.StatusUnsupportedMediaType, res.status)
			require.Equal(t, http.StatusBadRequest, f.do(t, mktViewer, post(route, nil, `{"definitions":`)).status)
			require.Equal(t, http.StatusBadRequest, f.do(t, mktViewer, post(route, nil, `{"definitions":[{"metadata":{"namespace":7}}]}`)).status)
		})
	}
}

// TestJobdefReadRoutesUnrestrictedOracle: a principal holding viewer at "*"
// is decided exactly as before B2 — no namespace lookup, no audit row for a
// successful lint or diff, the body untouched — with and without a policy
// file loaded (the middleware never reads the process holder).
func TestJobdefReadRoutesUnrestrictedOracle(t *testing.T) {
	t.Cleanup(func() { policy.SetProcessHolder(nil) })
	path := filepath.Join(t.TempDir(), "policy.yaml")
	require.NoError(t, os.WriteFile(path, []byte("apiVersion: v1\nkind: AccessPolicy\nnamespaces:\n  marketing: {}\n"), 0o600))
	fileHolder, err := policy.LoadFile(t.Context(), policy.HolderOptions{Path: path})
	require.NoError(t, err)

	for name, holder := range map[string]*policy.Holder{"no policy file": nil, "policy file": fileHolder} {
		t.Run(name, func(t *testing.T) {
			policy.SetProcessHolder(holder)
			f := newNSFixture(t)
			for _, who := range []cred{f.key(t, models.RoleViewer, ""), f.key(t, models.RoleAdmin, "")} {
				for _, route := range []string{"/v1/jobdefs/lint", "/v1/jobdefs/diff"} {
					var before int64
					require.NoError(t, f.db.Model(&models.AuditLog{}).Count(&before).Error)
					body := jobdefBody("ns-fin", "undeclared-anywhere")
					res := f.do(t, who, post(route, nil, body))
					require.Equal(t, http.StatusOK, res.status, route)
					require.True(t, res.decision.Unrestricted, route)
					require.Nil(t, res.decision.Namespaces, "no lookup for an unrestricted principal")
					require.Equal(t, body, res.body)
					var after int64
					require.NoError(t, f.db.Model(&models.AuditLog{}).Count(&after).Error)
					require.Equal(t, before, after, "a successful lint or diff writes no audit row: %s", route)
				}
			}
		})
	}
}

// TestJobdefWriteRoutesResolveNamespaces (B9): apply and the legacy create
// need operator in every namespace the body names and in the stored namespace
// of each alias it names (a move needs both); the 403 names the namespaces
// the caller lacks, as it named them itself.
func TestJobdefWriteRoutesResolveNamespaces(t *testing.T) {
	f := newNSFixture(t)
	mktOperator := f.key(t, models.RoleOperator, `["marketing"]`)
	both := f.key(t, models.RoleOperator, `["marketing","finance"]`)
	mktViewer := f.key(t, models.RoleViewer, `["marketing"]`)

	ok := f.do(t, mktOperator, post("/v1/jobdefs/apply", nil, jobdefBody("brand-new", "marketing")))
	require.Equal(t, http.StatusOK, ok.status)
	require.Equal(t, []string{"marketing"}, ok.decision.Namespaces)
	require.Equal(t, jobdefBody("brand-new", "marketing"), ok.body, "the handler reads the whole body")

	for _, body := range []string{jobdefBody("brand-new", "finance"), jobdefBody("ns-fin", "marketing")} {
		res := f.do(t, mktOperator, post("/v1/jobdefs/apply", nil, body))
		require.Equal(t, http.StatusForbidden, res.status, body)
		require.Equal(t, `insufficient permissions: operator required in namespace "finance"`, res.message, body)
		requireAuditMetadata(t, f.lastDenial(t), map[string]any{"reason": "insufficient_role"})
	}
	require.Equal(t, http.StatusOK, f.do(t, both, post("/v1/jobdefs/apply", nil, jobdefBody("ns-fin", "marketing"))).status, "a move with operator in both")
	require.Equal(t, http.StatusForbidden, f.do(t, mktViewer, post("/v1/jobdefs/apply", nil, jobdefBody("brand-new", "marketing"))).status)

	create := func(body string) nsResult { return f.do(t, mktOperator, post("/v1/jobs", nil, body)) }
	res := create(`{"alias":"brand-new","metadata":{"namespace":"marketing"}}`)
	require.Equal(t, http.StatusOK, res.status)
	require.Equal(t, []string{"marketing"}, res.decision.Namespaces)
	for body, want := range map[string]string{
		`{"alias":"brand-new","metadata":{"namespace":"finance"}}`: `insufficient permissions: operator required in namespace "finance"`,
		`{"alias":"ns-fin","metadata":{"namespace":"marketing"}}`:  `insufficient permissions: operator required in namespace "finance"`,
		`{"alias":"brand-new"}`:                 `insufficient permissions: operator required in namespace "default"`,
		`{"alias":"brand-new","metadata":null}`: `insufficient permissions: operator required in namespace "default"`,
	} {
		res := create(body)
		require.Equal(t, http.StatusForbidden, res.status, body)
		require.Equal(t, want, res.message, body)
	}
	require.Equal(t, http.StatusBadRequest, create(`{"alias":"x","metadata":{"namespace":"Bad_NS"}}`).status, "the handler's 400")
	require.Equal(t, http.StatusBadRequest, create(`{"alias":`).status)

	// PATCH and the manifest resolve the trigger's jobs and the job.
	patch := func(id uuid.UUID) nsResult {
		return f.do(t, mktOperator, nsCall{method: http.MethodPatch, pattern: "/v1/triggers/:id", params: pv("id", id.String()), body: `{}`})
	}
	require.Equal(t, http.StatusOK, patch(f.mktTrigger).status)
	require.Equal(t, http.StatusForbidden, patch(f.sharedTrigger).status, "a trigger also serving a finance job")
	require.Equal(t, http.StatusForbidden, patch(f.idleTrigger).status, "a trigger serving no job is cluster-only")
	require.Equal(t, http.StatusOK, f.do(t, mktViewer, get("/v1/jobs/:id/manifest", pv("id", f.mktJob.String()))).status)
	require.Equal(t, http.StatusForbidden, f.do(t, mktViewer, get("/v1/jobs/:id/manifest", pv("id", f.finJob.String()))).status)
}

// TestJobdefWriteRoutesUnrestrictedOracle: a principal holding operator at
// "*" is decided as before B9 (no namespace check, the body untouched); the
// only change is that the audit row of an apply or a legacy create now
// records the namespaces the request names, as every other namespaced write
// already does (B9 decision Q3).
func TestJobdefWriteRoutesUnrestrictedOracle(t *testing.T) {
	f := newNSFixture(t)
	for _, who := range []cred{f.key(t, models.RoleOperator, ""), f.key(t, models.RoleAdmin, "")} {
		for _, call := range []nsCall{
			post("/v1/jobdefs/apply", nil, jobdefBody("brand-new", "finance")),
			post("/v1/jobs", nil, `{"alias":"brand-new","metadata":{"namespace":"finance"}}`),
		} {
			res := f.do(t, who, call)
			require.Equal(t, http.StatusOK, res.status, call.pattern)
			require.True(t, res.decision.Unrestricted, call.pattern)
			require.Equal(t, call.body, res.body)
			var entry models.AuditLog
			require.NoError(t, f.db.Order("timestamp desc").First(&entry).Error)
			require.Equal(t, "finance", entry.Namespace, call.pattern)
		}
	}
}
