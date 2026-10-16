package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/cmd/cliutil"
	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/caesium-cloud/caesium/internal/clihttp"
	"github.com/spf13/cobra"
)

var (
	policyLintPath string
	policyLintJSON bool

	policyShowServer string
	policyShowAPIKey string
	policyShowJSON   bool

	policyReloadServer string
	policyReloadAPIKey string
	policyReloadJSON   bool
)

// policyCmd is the parent command for access policy operations.
var policyCmd = &cobra.Command{
	Use:   "policy",
	Short: "Validate, show and reload the access policy file",
}

// policyLintResult is the `caesium auth policy lint --json` document.
type policyLintResult struct {
	Path       string   `json:"path"`
	Valid      bool     `json:"valid"`
	Revision   string   `json:"revision,omitempty"`
	Error      string   `json:"error,omitempty"`
	Warnings   []string `json:"warnings"`
	Namespaces []string `json:"namespaces"`
	Bindings   int      `json:"bindings"`
}

var policyLintCmd = &cobra.Command{
	Use:   "lint",
	Args:  cobra.NoArgs,
	Short: "Validate an access policy file offline",
	Long: `Validate an access policy file without contacting a server.

Exits 1 when the file is invalid. Warnings go to stderr. With --json, stdout
carries exactly one JSON document, for a valid or an invalid file. Warnings
that need server state (groups no known user holds, whether an enabled
provider supplies a trusted email) come from GET /v1/auth/policy instead.`,
	Example: `  caesium auth policy lint --path policy.yaml
  caesium auth policy lint --path policy.yaml --json`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cmd.SilenceUsage = true
		path := strings.TrimSpace(policyLintPath)
		if path == "" {
			return errors.New("--path is required")
		}
		result := policyLintResult{Path: path, Warnings: []string{}, Namespaces: []string{}}
		data, hash, err := policy.ReadFile(path)
		var p *policy.AccessPolicy
		if err == nil {
			result.Revision = hash
			p, err = policy.Parse(data)
		}
		if err != nil {
			result.Error = err.Error()
			if policyLintJSON {
				if werr := writeJSON(cmd.OutOrStdout(), result); werr != nil {
					return werr
				}
			}
			return fmt.Errorf("access policy %s is invalid: %w", path, err)
		}

		result.Valid = true
		result.Warnings = policy.LintWarnings(p)
		for name := range p.Namespaces {
			result.Namespaces = append(result.Namespaces, name)
		}
		slices.Sort(result.Namespaces)
		result.Bindings = len(p.Bindings)
		for _, warning := range result.Warnings {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s\n", warning)
		}
		if policyLintJSON {
			return writeJSON(cmd.OutOrStdout(), result)
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "access policy %s is valid: revision %s, %d namespace(s) (%s), %d binding(s)\n",
			path, hash, len(result.Namespaces), strings.Join(result.Namespaces, ", "), result.Bindings)
		return err
	},
}

var policyShowCmd = &cobra.Command{
	Use:   "show",
	Args:  cobra.NoArgs,
	Short: "Show the server's effective access policy and its revision",
	Long: `Show the effective access policy (GET /v1/auth/policy, cluster admin): its
source (the policy file, or the translated CAESIUM_AUTH_ROLE_MAPPING), the
revision hash of the served file, warnings, and the last reload attempt.
Compare revisions across nodes to spot drift.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cmd.SilenceUsage = true
		body, err := policyRequest(cmd, http.MethodGet, policyShowServer, "/v1/auth/policy", policyShowAPIKey, "policy show")
		if err != nil {
			return err
		}
		if policyShowJSON {
			return cliutil.WritePrettyJSON(cmd, body, "auth policy response")
		}
		var view policyView
		if err := json.Unmarshal(body, &view); err != nil {
			return fmt.Errorf("auth policy response was not valid JSON: %w", err)
		}
		return writePolicyView(cmd.OutOrStdout(), view)
	},
}

var policyReloadCmd = &cobra.Command{
	Use:   "reload",
	Args:  cobra.NoArgs,
	Short: "Reload the server's access policy file now",
	Long: `Ask the server to re-read its policy file (POST /v1/auth/policy/reload,
cluster admin). An invalid or unreadable file is rejected, the served revision
stays in force, and the command exits 1. With --json, stdout carries the
server's JSON response, including for a rejected file.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cmd.SilenceUsage = true
		server := strings.TrimSuffix(policyReloadServer, "/")
		apiKey := resolveAPIKey(cmd, policyReloadAPIKey)
		headers := make(http.Header)
		if apiKey != "" {
			headers.Set("Authorization", "Bearer "+apiKey)
		}
		body, status, readErr := clihttp.Exchange(cmd.Context(), http.DefaultClient, http.MethodPost, server+"/v1/auth/policy/reload", nil, headers)
		if status == 0 && readErr != nil {
			return readErr
		}
		if readErr != nil {
			return fmt.Errorf("reading policy reload response (%d): %w", status, readErr)
		}
		if status == http.StatusUnprocessableEntity {
			var failure struct {
				Message  string `json:"message"`
				Revision string `json:"revision"`
			}
			if err := json.Unmarshal(body, &failure); err != nil {
				return fmt.Errorf("policy reload failed (%d): %s", status, strings.TrimSpace(string(body)))
			}
			if policyReloadJSON {
				if err := cliutil.WritePrettyJSON(cmd, body, "auth policy reload response"); err != nil {
					return err
				}
			}
			return fmt.Errorf("policy reload rejected; revision %s stays in force: %s", failure.Revision, failure.Message)
		}
		if status >= http.StatusBadRequest {
			return fmt.Errorf("policy reload failed (%d): %s", status, strings.TrimSpace(string(body)))
		}
		if policyReloadJSON {
			return cliutil.WritePrettyJSON(cmd, body, "auth policy reload response")
		}
		var result struct {
			Changed          bool     `json:"changed"`
			Revision         string   `json:"revision"`
			PreviousRevision string   `json:"previous_revision"`
			Warnings         []string `json:"warnings"`
		}
		if err := json.Unmarshal(body, &result); err != nil {
			return fmt.Errorf("auth policy reload response was not valid JSON: %w", err)
		}
		for _, warning := range result.Warnings {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s\n", warning)
		}
		if result.Changed {
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "access policy reloaded: revision %s (was %s)\n", result.Revision, result.PreviousRevision)
			return err
		}
		_, err := fmt.Fprintf(cmd.OutOrStdout(), "access policy unchanged: revision %s\n", result.Revision)
		return err
	},
}

// policyRequest performs an authenticated request and returns the body of a
// successful response.
func policyRequest(cmd *cobra.Command, method, rawServer, path, flagKey, label string) ([]byte, error) {
	server := strings.TrimSuffix(rawServer, "/")
	apiKey := resolveAPIKey(cmd, flagKey)
	headers := make(http.Header)
	if apiKey != "" {
		headers.Set("Authorization", "Bearer "+apiKey)
	}
	body, status, readErr := clihttp.Exchange(cmd.Context(), http.DefaultClient, method, server+path, nil, headers)
	if status == 0 && readErr != nil {
		return nil, readErr
	}
	if status >= http.StatusBadRequest {
		if readErr != nil {
			return nil, fmt.Errorf("%s failed (%d): %s (reading response: %w)", label, status, strings.TrimSpace(string(body)), readErr)
		}
		return nil, fmt.Errorf("%s failed (%d): %s", label, status, strings.TrimSpace(string(body)))
	}
	if readErr != nil {
		return nil, fmt.Errorf("reading %s response: %w", label, readErr)
	}
	return body, nil
}

type policyView struct {
	Source      string               `json:"source"`
	Path        string               `json:"path"`
	Revision    string               `json:"revision"`
	LoadedAt    time.Time            `json:"loaded_at"`
	DefaultRole string               `json:"default_role"`
	Warnings    []string             `json:"warnings"`
	LastReload  *policyViewAttempt   `json:"last_reload"`
	Policy      *policy.AccessPolicy `json:"policy"`
}

type policyViewAttempt struct {
	At       time.Time `json:"at"`
	Trigger  string    `json:"trigger"`
	OK       bool      `json:"ok"`
	Changed  bool      `json:"changed"`
	Revision string    `json:"revision"`
	Error    string    `json:"error"`
}

func writePolicyView(w io.Writer, view policyView) error {
	var b strings.Builder
	source := view.Source
	if view.Path != "" {
		source += " (" + view.Path + ")"
	}
	fmt.Fprintf(&b, "source:       %s\n", source)
	revision := view.Revision
	if revision == "" {
		revision = "-"
	}
	fmt.Fprintf(&b, "revision:     %s\n", revision)
	fmt.Fprintf(&b, "loaded at:    %s\n", view.LoadedAt.Format(time.RFC3339))
	defaultRole := view.DefaultRole
	if defaultRole == "" {
		defaultRole = "-"
	}
	fmt.Fprintf(&b, "default role: %s\n", defaultRole)
	switch attempt := view.LastReload; {
	case attempt == nil:
		fmt.Fprintf(&b, "last reload:  -\n")
	case attempt.OK:
		fmt.Fprintf(&b, "last reload:  %s ok (%s)\n", attempt.At.Format(time.RFC3339), attempt.Trigger)
	default:
		fmt.Fprintf(&b, "last reload:  %s FAILED (%s): %s\n", attempt.At.Format(time.RFC3339), attempt.Trigger, attempt.Error)
	}
	if view.Policy != nil {
		names := make([]string, 0, len(view.Policy.Namespaces))
		for name := range view.Policy.Namespaces {
			names = append(names, name)
		}
		slices.Sort(names)
		fmt.Fprintf(&b, "namespaces:   %s\n", strings.Join(names, ", "))
		fmt.Fprintf(&b, "bindings:\n")
		if len(view.Policy.Bindings) == 0 {
			fmt.Fprintf(&b, "  (none)\n")
		}
		for _, binding := range view.Policy.Bindings {
			var subjects []string
			if len(binding.Subjects.Groups) > 0 {
				subjects = append(subjects, "groups "+quoteAll(binding.Subjects.Groups))
			}
			if len(binding.Subjects.Users) > 0 {
				subjects = append(subjects, "users "+quoteAll(binding.Subjects.Users))
			}
			fmt.Fprintf(&b, "  - %s -> %s @ %s\n", strings.Join(subjects, " "), binding.Role, strings.Join(binding.Namespaces, ", "))
		}
	}
	if len(view.Warnings) > 0 {
		fmt.Fprintf(&b, "warnings:\n")
		for _, warning := range view.Warnings {
			fmt.Fprintf(&b, "  - %s\n", warning)
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func quoteAll(values []string) string {
	quoted := make([]string, len(values))
	for i, value := range values {
		quoted[i] = fmt.Sprintf("%q", value)
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

func writeJSON(w io.Writer, value any) error {
	out, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "%s\n", out)
	return err
}

func init() {
	policyLintCmd.Flags().StringVar(&policyLintPath, "path", "", "Path to the access policy file")
	policyLintCmd.Flags().BoolVar(&policyLintJSON, "json", false, "Print the lint result as one JSON document on stdout")

	policyShowCmd.Flags().StringVar(&policyShowServer, "server", "http://localhost:8080", "Caesium server base URL")
	policyShowCmd.Flags().StringVar(&policyShowAPIKey, "api-key", "", apiKeyFlagUsage("Admin"))
	policyShowCmd.Flags().BoolVar(&policyShowJSON, "json", false, "Print the server's JSON response on stdout")

	policyReloadCmd.Flags().StringVar(&policyReloadServer, "server", "http://localhost:8080", "Caesium server base URL")
	policyReloadCmd.Flags().StringVar(&policyReloadAPIKey, "api-key", "", apiKeyFlagUsage("Admin"))
	policyReloadCmd.Flags().BoolVar(&policyReloadJSON, "json", false, "Print the server's JSON response on stdout")

	policyCmd.AddCommand(policyLintCmd, policyShowCmd, policyReloadCmd)
	Cmd.AddCommand(policyCmd)
}
