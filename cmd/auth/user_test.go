package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func runUserCmd(t *testing.T, leaf *cobra.Command, args ...string) (string, string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	err := leaf.RunE(cmd, args)
	return stdout.String(), stderr.String(), err
}

type recordedRequest struct {
	method, path, query, auth string
}

func userAdminServer(t *testing.T, status int, headers map[string]string, body string) (*httptest.Server, *recordedRequest) {
	t.Helper()
	got := &recordedRequest{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.method, got.path, got.query, got.auth = r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization")
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	t.Setenv("CAESIUM_API_KEY", "admin-token")
	return server, got
}

const cliUserJSON = `{"id":"11111111-1111-1111-1111-111111111111","issuer":"https://idp.example","subject":"sub-1",
"email":"ada@example.com","email_verified":true,"display_name":"Ada","groups":["eng"],"last_login_role":"operator",
"created_at":"2026-10-15T10:00:00Z","last_login_at":"2026-10-16T09:00:00Z","disabled_at":null}`

func TestUserListCommandBuildsQueryAndPrintsTable(t *testing.T) {
	t.Cleanup(func() {
		userListFlags.reset()
		userListFlags.email, userListFlags.issuer, userListFlags.disabled = "", "", ""
		userListFlags.limit, userListFlags.offset = 0, 0
	})
	server, got := userAdminServer(t, http.StatusOK,
		map[string]string{"X-Caesium-Total-Count": "7", "X-Caesium-Next-Offset": "4"}, "["+cliUserJSON+"]")
	userListFlags.server = server.URL + "/"
	userListFlags.email, userListFlags.issuer, userListFlags.disabled = "ada", "https://idp.example", "false"
	userListFlags.limit, userListFlags.offset = 2, 2

	stdout, stderr, err := runUserCmd(t, userListCmd)
	require.NoError(t, err)
	require.Equal(t, http.MethodGet, got.method)
	require.Equal(t, "/v1/auth/users", got.path)
	require.Equal(t, "disabled=false&email=ada&issuer=https%3A%2F%2Fidp.example&limit=2&offset=2", got.query)
	require.Equal(t, "Bearer admin-token", got.auth)
	require.Contains(t, stdout, "LAST LOGIN ROLE")
	require.Contains(t, stdout, "ada@example.com")
	require.Contains(t, stdout, "operator")
	require.Contains(t, stderr, "7 users in total; next page: --offset 4")

	userListFlags.json = true
	stdout, _, err = runUserCmd(t, userListCmd)
	require.NoError(t, err)
	var users []map[string]any
	require.NoError(t, json.Unmarshal([]byte(stdout), &users), "--json prints exactly the response")
	require.Equal(t, "ada@example.com", users[0]["email"])
}

func TestUserListCommandForwardsExplicitPageBounds(t *testing.T) {
	t.Cleanup(func() {
		userListFlags.reset()
		userListFlags.limit, userListFlags.offset = 0, 0
		userListCmd.Flags().Lookup("limit").Changed = false
		userListCmd.Flags().Lookup("offset").Changed = false
	})
	server, got := userAdminServer(t, http.StatusBadRequest, nil, `{"message":"limit must be an integer between 1 and 1000"}`)
	userListFlags.server = server.URL

	// An explicit --limit 0 is sent, not dropped, and the 400 surfaces.
	require.NoError(t, userListCmd.Flags().Set("limit", "0"))
	var stdout, stderr bytes.Buffer
	userListCmd.SetContext(context.Background())
	userListCmd.SetOut(&stdout)
	userListCmd.SetErr(&stderr)
	err := userListCmd.RunE(userListCmd, nil)
	require.ErrorContains(t, err, "user list failed (400)")
	require.Equal(t, "limit=0", got.query)
	require.Empty(t, stdout.String())

	userListCmd.Flags().Lookup("limit").Changed = false
	userListFlags.limit, userListFlags.offset = 0, -1
	_, _, err = runUserCmd(t, userListCmd)
	require.Error(t, err)
	require.Equal(t, "offset=-1", got.query, "a negative offset is sent for the server to refuse")
}

func TestUserGetCommandPrintsDetail(t *testing.T) {
	t.Cleanup(userGetFlags.reset)
	detail := cliUserJSON[:len(cliUserJSON)-1] + `,"sessions":[{"id":"22222222-2222-2222-2222-222222222222","user_id":"11111111-1111-1111-1111-111111111111",
"auth_method":"oidc","created_at":"2026-10-16T09:00:00Z","last_seen_at":null,"idle_expires_at":"2026-10-16T17:00:00Z",
"absolute_expires_at":"2026-10-17T09:00:00Z","source_ip":"198.51.100.7","user_agent":"ua"}],"sessions_total":3}`
	server, got := userAdminServer(t, http.StatusOK, nil, detail)
	userGetFlags.server = server.URL

	stdout, _, err := runUserCmd(t, userGetCmd, "11111111-1111-1111-1111-111111111111")
	require.NoError(t, err)
	require.Equal(t, "/v1/auth/users/11111111-1111-1111-1111-111111111111", got.path)
	require.Contains(t, stdout, "email:           ada@example.com (verified: true)")
	require.Contains(t, stdout, `groups:          ["eng"]`)
	require.Contains(t, stdout, "active sessions: 3")
	require.Contains(t, stdout, "22222222-2222-2222-2222-222222222222")
	require.Contains(t, stdout, "showing the 1 newest of 3")

	userGetFlags.json = true
	stdout, _, err = runUserCmd(t, userGetCmd, "11111111-1111-1111-1111-111111111111")
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(stdout), &out))
	require.Equal(t, float64(3), out["sessions_total"])
}

func TestUserCommandsReportUserBoundKeys(t *testing.T) {
	t.Cleanup(func() { userGetFlags.reset(); userDisableFlags.reset(); userLogoutFlags.reset() })
	detail := cliUserJSON[:len(cliUserJSON)-1] + `,"sessions":[],"sessions_total":0,
"user_keys":[{"id":"33333333-3333-3333-3333-333333333333","key_prefix":"csk_user_AbCd","description":"laptop",
"created_at":"2026-10-16T09:00:00Z","expires_at":"2026-10-16T21:00:00Z","last_used_at":null}],"user_keys_total":2}`
	server, _ := userAdminServer(t, http.StatusOK, nil, detail)
	userGetFlags.server = server.URL
	stdout, _, err := runUserCmd(t, userGetCmd, "11111111-1111-1111-1111-111111111111")
	require.NoError(t, err)
	require.Contains(t, stdout, "active sessions: 0\nactive user-bound keys: 2\n")
	require.Contains(t, stdout, "33333333-3333-3333-3333-333333333333")
	require.Contains(t, stdout, "csk_user_AbCd")
	require.Contains(t, stdout, "2026-10-16T21:00:00Z")
	require.Contains(t, stdout, "laptop")
	require.Contains(t, stdout, "showing the 1 newest of 2 user-bound keys")
	require.NotContains(t, stdout, "IDLE EXPIRES", "no session table without sessions")

	changed := `{"user":` + cliUserJSON + `,"changed":true,"sessions_revoked":1,"keys_revoked":2}`
	server, _ = userAdminServer(t, http.StatusOK, nil, changed)
	userDisableFlags.server = server.URL
	stdout, _, err = runUserCmd(t, userDisableCmd, "11111111-1111-1111-1111-111111111111")
	require.NoError(t, err)
	require.Equal(t, "user 11111111-1111-1111-1111-111111111111 (ada@example.com) disabled; 1 session(s) and 2 user-bound key(s) revoked\n", stdout)

	server, _ = userAdminServer(t, http.StatusOK, nil, `{"user_id":"11111111-1111-1111-1111-111111111111","sessions_revoked":0,"keys_revoked":1}`)
	userLogoutFlags.server = server.URL
	stdout, _, err = runUserCmd(t, userLogoutCmd, "11111111-1111-1111-1111-111111111111")
	require.NoError(t, err)
	require.Equal(t, "revoked 0 session(s) and 1 user-bound key(s) of user 11111111-1111-1111-1111-111111111111\n", stdout)
}

func TestUserDisableEnableLogoutCommands(t *testing.T) {
	t.Cleanup(func() { userDisableFlags.reset(); userEnableFlags.reset(); userLogoutFlags.reset() })
	changed := `{"user":` + cliUserJSON + `,"changed":true,"sessions_revoked":2}`
	server, got := userAdminServer(t, http.StatusOK, nil, changed)
	userDisableFlags.server = server.URL

	stdout, _, err := runUserCmd(t, userDisableCmd, "11111111-1111-1111-1111-111111111111")
	require.NoError(t, err)
	require.Equal(t, http.MethodPost, got.method)
	require.Equal(t, "/v1/auth/users/11111111-1111-1111-1111-111111111111/disable", got.path)
	require.Equal(t, "user 11111111-1111-1111-1111-111111111111 (ada@example.com) disabled; 2 session(s) revoked\n", stdout)

	unchanged := `{"user":` + cliUserJSON + `,"changed":false,"sessions_revoked":0}`
	server, got = userAdminServer(t, http.StatusOK, nil, unchanged)
	userEnableFlags.server = server.URL
	stdout, _, err = runUserCmd(t, userEnableCmd, "11111111-1111-1111-1111-111111111111")
	require.NoError(t, err)
	require.Equal(t, "/v1/auth/users/11111111-1111-1111-1111-111111111111/enable", got.path)
	require.Equal(t, "user 11111111-1111-1111-1111-111111111111 (ada@example.com) was already enabled\n", stdout)
	userEnableFlags.json = true
	stdout, _, err = runUserCmd(t, userEnableCmd, "11111111-1111-1111-1111-111111111111")
	require.NoError(t, err)
	var change map[string]any
	require.NoError(t, json.Unmarshal([]byte(stdout), &change))
	require.Equal(t, false, change["changed"])

	server, got = userAdminServer(t, http.StatusOK, nil, `{"user_id":"11111111-1111-1111-1111-111111111111","sessions_revoked":3}`)
	userLogoutFlags.server = server.URL
	stdout, _, err = runUserCmd(t, userLogoutCmd, "11111111-1111-1111-1111-111111111111")
	require.NoError(t, err)
	require.Equal(t, http.MethodPost, got.method)
	require.Equal(t, "/v1/auth/users/11111111-1111-1111-1111-111111111111/sessions/revoke", got.path)
	require.Equal(t, "revoked 3 session(s) of user 11111111-1111-1111-1111-111111111111\n", stdout)
}

func TestUserCommandsSurfaceHTTPErrors(t *testing.T) {
	t.Cleanup(func() { userGetFlags.reset(); userDisableFlags.reset() })
	server, _ := userAdminServer(t, http.StatusNotFound, nil, `{"message":"user not found"}`)
	userGetFlags.server = server.URL
	userGetFlags.json = true
	stdout, _, err := runUserCmd(t, userGetCmd, "11111111-1111-1111-1111-111111111111")
	require.ErrorContains(t, err, "user get failed (404)")
	require.Empty(t, stdout, "nothing reaches stdout on failure, even with --json")

	server, _ = userAdminServer(t, http.StatusConflict, nil, `{"message":"cannot disable the user making this request"}`)
	userDisableFlags.server = server.URL
	_, _, err = runUserCmd(t, userDisableCmd, "11111111-1111-1111-1111-111111111111")
	require.ErrorContains(t, err, "user disable failed (409)")
}

func TestUserLeafCommandsValidateArguments(t *testing.T) {
	require.Error(t, userListCmd.Args(userListCmd, []string{"extra"}))
	for _, leaf := range []*cobra.Command{userGetCmd, userDisableCmd, userEnableCmd, userLogoutCmd} {
		require.Error(t, leaf.Args(leaf, nil), leaf.Name())
		require.NoError(t, leaf.Args(leaf, []string{"id"}), leaf.Name())
	}
}
