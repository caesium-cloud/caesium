package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/caesium-cloud/caesium/cmd/cliutil"
	"github.com/caesium-cloud/caesium/internal/clihttp"
	"github.com/spf13/cobra"
)

// userCmd groups user and session administration (identity-and-access E3).
// Every verb needs a cluster admin.
var userCmd = &cobra.Command{
	Use:   "user",
	Short: "Administer SSO users and their sessions (cluster admin)",
}

type userCommonFlags struct {
	server string
	apiKey string
	json   bool
}

func (f *userCommonFlags) register(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.server, "server", "http://localhost:8080", "Caesium server base URL")
	cmd.Flags().StringVar(&f.apiKey, "api-key", "", apiKeyFlagUsage("Admin"))
	cmd.Flags().BoolVar(&f.json, "json", false, "Print the server's JSON response on stdout")
}

func (f *userCommonFlags) reset() {
	f.server, f.apiKey, f.json = "http://localhost:8080", "", false
}

var (
	userListFlags struct {
		userCommonFlags
		email    string
		issuer   string
		disabled string
		limit    int
		offset   int
	}
	userGetFlags     userCommonFlags
	userDisableFlags userCommonFlags
	userEnableFlags  userCommonFlags
	userLogoutFlags  userCommonFlags
)

// userView, userSessionView and the response types mirror the server's
// administration JSON (api/rest/controller/auth/users.go).
type userView struct {
	ID            string     `json:"id"`
	Issuer        string     `json:"issuer"`
	Subject       string     `json:"subject"`
	Email         string     `json:"email"`
	EmailVerified bool       `json:"email_verified"`
	DisplayName   string     `json:"display_name"`
	Groups        []string   `json:"groups"`
	LastLoginRole string     `json:"last_login_role"`
	CreatedAt     time.Time  `json:"created_at"`
	LastLoginAt   *time.Time `json:"last_login_at"`
	DisabledAt    *time.Time `json:"disabled_at"`
}

type userSessionView struct {
	ID                string     `json:"id"`
	AuthMethod        string     `json:"auth_method"`
	CreatedAt         time.Time  `json:"created_at"`
	LastSeenAt        *time.Time `json:"last_seen_at"`
	IdleExpiresAt     time.Time  `json:"idle_expires_at"`
	AbsoluteExpiresAt time.Time  `json:"absolute_expires_at"`
	SourceIP          string     `json:"source_ip"`
}

type userKeyView struct {
	ID          string     `json:"id"`
	KeyPrefix   string     `json:"key_prefix"`
	Description string     `json:"description"`
	CreatedAt   time.Time  `json:"created_at"`
	ExpiresAt   *time.Time `json:"expires_at"`
	LastUsedAt  *time.Time `json:"last_used_at"`
}

type userDetailView struct {
	userView
	Sessions      []userSessionView `json:"sessions"`
	SessionsTotal int64             `json:"sessions_total"`
	UserKeys      []userKeyView     `json:"user_keys"`
	UserKeysTotal int64             `json:"user_keys_total"`
}

type userChangeView struct {
	User            userView `json:"user"`
	Changed         bool     `json:"changed"`
	SessionsRevoked int64    `json:"sessions_revoked"`
	KeysRevoked     int64    `json:"keys_revoked"`
}

var userListCmd = &cobra.Command{
	Use:   "list",
	Args:  cobra.NoArgs,
	Short: "List users (GET /v1/auth/users)",
	Example: `  caesium auth user list --email alice
  caesium auth user list --disabled true --json`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cmd.SilenceUsage = true
		flags := &userListFlags
		params := url.Values{}
		if flags.email != "" {
			params.Set("email", flags.email)
		}
		if flags.issuer != "" {
			params.Set("issuer", flags.issuer)
		}
		if flags.disabled != "" {
			params.Set("disabled", flags.disabled)
		}
		// Any value the operator gave is forwarded, so an out-of-range one is
		// the server's 400 rather than silently replaced by a default.
		if flags.limit != 0 || cmd.Flags().Changed("limit") {
			params.Set("limit", strconv.Itoa(flags.limit))
		}
		if flags.offset != 0 || cmd.Flags().Changed("offset") {
			params.Set("offset", strconv.Itoa(flags.offset))
		}
		path := "/v1/auth/users"
		if encoded := params.Encode(); encoded != "" {
			path += "?" + encoded
		}
		body, header, err := userAdminRequest(cmd, http.MethodGet, flags.server, path, flags.apiKey, "user list")
		if err != nil {
			return err
		}
		if next := header.Get("X-Caesium-Next-Offset"); next != "" {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "%s users in total; next page: --offset %s\n", header.Get("X-Caesium-Total-Count"), next)
		}
		if flags.json {
			return cliutil.WritePrettyJSON(cmd, body, "auth user list response")
		}
		var users []userView
		if err := json.Unmarshal(body, &users); err != nil {
			return fmt.Errorf("auth user list response was not valid JSON: %w", err)
		}
		return writeUserTable(cmd.OutOrStdout(), users)
	},
}

var userGetCmd = &cobra.Command{
	Use:   "get USER_ID",
	Args:  cobra.ExactArgs(1),
	Short: "Show a user, its active sessions and user-bound keys (GET /v1/auth/users/:id)",
	RunE: func(cmd *cobra.Command, args []string) error {
		cmd.SilenceUsage = true
		flags := &userGetFlags
		body, _, err := userAdminRequest(cmd, http.MethodGet, flags.server, "/v1/auth/users/"+url.PathEscape(args[0]), flags.apiKey, "user get")
		if err != nil {
			return err
		}
		if flags.json {
			return cliutil.WritePrettyJSON(cmd, body, "auth user get response")
		}
		var detail userDetailView
		if err := json.Unmarshal(body, &detail); err != nil {
			return fmt.Errorf("auth user get response was not valid JSON: %w", err)
		}
		return writeUserDetail(cmd.OutOrStdout(), detail)
	},
}

var userDisableCmd = &cobra.Command{
	Use:   "disable USER_ID",
	Args:  cobra.ExactArgs(1),
	Short: "Disable a user and revoke all its sessions and user-bound keys",
	Long: `Disable a user (POST /v1/auth/users/:id/disable). The user's sessions and
user-bound API keys are revoked in the same transaction, its stored refresh
grant is cleared, and its logins are refused until it is enabled again.
Disabling a disabled user changes nothing.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runUserChange(cmd, &userDisableFlags, args[0], "disable")
	},
}

var userEnableCmd = &cobra.Command{
	Use:   "enable USER_ID",
	Args:  cobra.ExactArgs(1),
	Short: "Enable a disabled user",
	Long: `Enable a user (POST /v1/auth/users/:id/enable). Sessions and user-bound
keys revoked by the disable stay revoked: the user signs in again.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runUserChange(cmd, &userEnableFlags, args[0], "enable")
	},
}

var userLogoutCmd = &cobra.Command{
	Use:   "logout USER_ID",
	Args:  cobra.ExactArgs(1),
	Short: "Revoke every session and user-bound key of a user (log out everywhere)",
	RunE: func(cmd *cobra.Command, args []string) error {
		cmd.SilenceUsage = true
		flags := &userLogoutFlags
		body, _, err := userAdminRequest(cmd, http.MethodPost, flags.server,
			"/v1/auth/users/"+url.PathEscape(args[0])+"/sessions/revoke", flags.apiKey, "user logout")
		if err != nil {
			return err
		}
		if flags.json {
			return cliutil.WritePrettyJSON(cmd, body, "auth user logout response")
		}
		var result struct {
			UserID          string `json:"user_id"`
			SessionsRevoked int64  `json:"sessions_revoked"`
			KeysRevoked     int64  `json:"keys_revoked"`
		}
		if err := json.Unmarshal(body, &result); err != nil {
			return fmt.Errorf("auth user logout response was not valid JSON: %w", err)
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "revoked %d session(s)%s of user %s\n", result.SessionsRevoked, keysRevokedSuffix(result.KeysRevoked), result.UserID)
		return err
	},
}

func runUserChange(cmd *cobra.Command, flags *userCommonFlags, id, verb string) error {
	cmd.SilenceUsage = true
	body, _, err := userAdminRequest(cmd, http.MethodPost, flags.server,
		"/v1/auth/users/"+url.PathEscape(id)+"/"+verb, flags.apiKey, "user "+verb)
	if err != nil {
		return err
	}
	if flags.json {
		return cliutil.WritePrettyJSON(cmd, body, "auth user "+verb+" response")
	}
	var change userChangeView
	if err := json.Unmarshal(body, &change); err != nil {
		return fmt.Errorf("auth user %s response was not valid JSON: %w", verb, err)
	}
	out := cmd.OutOrStdout()
	if !change.Changed {
		_, err = fmt.Fprintf(out, "user %s (%s) was already %sd\n", change.User.ID, change.User.Email, verb)
		return err
	}
	_, err = fmt.Fprintf(out, "user %s (%s) %sd; %d session(s)%s revoked\n", change.User.ID, change.User.Email, verb, change.SessionsRevoked, keysRevokedSuffix(change.KeysRevoked))
	return err
}

// keysRevokedSuffix mentions revoked user-bound keys only when there were
// some, so a summary about sessions alone reads as before.
func keysRevokedSuffix(keys int64) string {
	if keys <= 0 {
		return ""
	}
	return fmt.Sprintf(" and %d user-bound key(s)", keys)
}

// userAdminRequest performs an authenticated request and returns the body
// and headers of a successful response.
func userAdminRequest(cmd *cobra.Command, method, rawServer, path, flagKey, operation string) ([]byte, http.Header, error) {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(rawServer, "/")+path, nil)
	if err != nil {
		return nil, nil, err
	}
	if apiKey := resolveAPIKey(cmd, flagKey); apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, readErr := io.ReadAll(resp.Body)
	if err := clihttp.ResponseError(operation, resp.StatusCode, body, readErr); err != nil {
		return nil, nil, err
	}
	return body, resp.Header, nil
}

func writeUserTable(w io.Writer, users []userView) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ID\tEMAIL\tISSUER\tLAST LOGIN ROLE\tLAST LOGIN\tDISABLED")
	for _, u := range users {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			u.ID, dash(u.Email), u.Issuer, dash(u.LastLoginRole), formatOptionalTime(u.LastLoginAt), formatOptionalTime(u.DisabledAt))
	}
	return tw.Flush()
}

func writeUserDetail(w io.Writer, d userDetailView) error {
	var b strings.Builder
	fmt.Fprintf(&b, "id:              %s\n", d.ID)
	fmt.Fprintf(&b, "email:           %s (verified: %t)\n", dash(d.Email), d.EmailVerified)
	fmt.Fprintf(&b, "display name:    %s\n", dash(d.DisplayName))
	fmt.Fprintf(&b, "issuer:          %s\n", d.Issuer)
	fmt.Fprintf(&b, "subject:         %s\n", d.Subject)
	fmt.Fprintf(&b, "groups:          %s\n", quoteAll(d.Groups))
	fmt.Fprintf(&b, "last login role: %s\n", dash(d.LastLoginRole))
	fmt.Fprintf(&b, "created:         %s\n", d.CreatedAt.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "last login:      %s\n", formatOptionalTime(d.LastLoginAt))
	fmt.Fprintf(&b, "disabled:        %s\n", formatOptionalTime(d.DisabledAt))
	fmt.Fprintf(&b, "active sessions: %d\n", d.SessionsTotal)
	fmt.Fprintf(&b, "active user-bound keys: %d\n", d.UserKeysTotal)
	if _, err := io.WriteString(w, b.String()); err != nil {
		return err
	}
	if err := writeUserSessions(w, d); err != nil {
		return err
	}
	return writeUserKeys(w, d)
}

func writeUserSessions(w io.Writer, d userDetailView) error {
	if len(d.Sessions) == 0 {
		return nil
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "\nSESSION\tMETHOD\tCREATED\tLAST SEEN\tIDLE EXPIRES\tSOURCE IP")
	for _, s := range d.Sessions {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			s.ID, dash(s.AuthMethod), s.CreatedAt.UTC().Format(time.RFC3339), formatOptionalTime(s.LastSeenAt),
			s.IdleExpiresAt.UTC().Format(time.RFC3339), dash(s.SourceIP))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if int64(len(d.Sessions)) < d.SessionsTotal {
		_, err := fmt.Fprintf(w, "showing the %d newest of %d; page the rest with GET /v1/auth/sessions?user_id=%s\n", len(d.Sessions), d.SessionsTotal, d.ID)
		return err
	}
	return nil
}

// writeUserKeys lists the user's active user-bound API keys (E4); every key
// of the user is in `caesium auth key list`.
func writeUserKeys(w io.Writer, d userDetailView) error {
	if len(d.UserKeys) == 0 {
		return nil
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "\nUSER KEY\tPREFIX\tCREATED\tEXPIRES\tLAST USED\tDESCRIPTION")
	for _, k := range d.UserKeys {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			k.ID, k.KeyPrefix, k.CreatedAt.UTC().Format(time.RFC3339), formatOptionalTime(k.ExpiresAt),
			formatOptionalTime(k.LastUsedAt), dash(k.Description))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if int64(len(d.UserKeys)) < d.UserKeysTotal {
		_, err := fmt.Fprintf(w, "showing the %d newest of %d user-bound keys; see the rest with caesium auth key list\n", len(d.UserKeys), d.UserKeysTotal)
		return err
	}
	return nil
}

func formatOptionalTime(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return t.UTC().Format(time.RFC3339)
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func init() {
	userListCmd.Flags().StringVar(&userListFlags.email, "email", "", "Only users whose email contains this text (case-insensitive)")
	userListCmd.Flags().StringVar(&userListFlags.issuer, "issuer", "", "Only users from this exact issuer")
	userListCmd.Flags().StringVar(&userListFlags.disabled, "disabled", "", "Only disabled (true) or enabled (false) users")
	userListCmd.Flags().IntVar(&userListFlags.limit, "limit", 0, "Page size (server default 100, at most 1000)")
	userListCmd.Flags().IntVar(&userListFlags.offset, "offset", 0, "Number of users to skip")
	userListFlags.register(userListCmd)
	userGetFlags.register(userGetCmd)
	userDisableFlags.register(userDisableCmd)
	userEnableFlags.register(userEnableCmd)
	userLogoutFlags.register(userLogoutCmd)

	userCmd.AddCommand(userListCmd, userGetCmd, userDisableCmd, userEnableCmd, userLogoutCmd)
	Cmd.AddCommand(userCmd)
}
