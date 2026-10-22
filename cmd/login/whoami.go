package login

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"

	"github.com/caesium-cloud/caesium/cmd/cliutil"
	"github.com/caesium-cloud/caesium/internal/clihttp"
	"github.com/spf13/cobra"
)

var (
	whoamiServer string
	whoamiAPIKey string
	whoamiJSON   bool
)

// WhoamiCmd is `caesium whoami`.
var WhoamiCmd = &cobra.Command{
	Use:   "whoami",
	Args:  cobra.NoArgs,
	Short: "Show the principal and grants the CLI acts as",
	Long: "Show who the server sees: the principal (an SSO user or an API key) and its " +
		"grants per namespace. Credentials resolve as for every command: --api-key, then " +
		"CAESIUM_API_KEY, then the credential caesium login stored for --server.",
	RunE: func(cmd *cobra.Command, _ []string) error {
		server, err := cliutil.CanonicalServer(whoamiServer)
		if err != nil {
			return err
		}
		headers := make(http.Header)
		cliutil.SetAuthorization(cmd, headers, whoamiAPIKey)
		body, status, readErr := clihttp.Exchange(cmd.Context(), boundedClient(), http.MethodGet, server+"/auth/whoami", nil, headers)
		if status == http.StatusNotFound {
			return fmt.Errorf("whoami: %s has authentication disabled (no /auth/whoami)", server)
		}
		if err := clihttp.ResponseError("whoami", status, body, readErr); err != nil {
			return err
		}
		if whoamiJSON {
			return cliutil.WritePrettyJSON(cmd, body, "whoami response")
		}
		var who struct {
			Kind        string            `json:"kind"`
			Subject     string            `json:"subject"`
			ClusterRole string            `json:"cluster_role"`
			Grants      map[string]string `json:"grants"`
			IsScoped    bool              `json:"is_scoped"`
		}
		if err := json.Unmarshal(body, &who); err != nil {
			return fmt.Errorf("whoami response was not valid JSON: %w", err)
		}
		out := cmd.OutOrStdout()
		_, _ = fmt.Fprintf(out, "Subject:      %s\n", who.Subject)
		_, _ = fmt.Fprintf(out, "Kind:         %s\n", who.Kind)
		role := who.ClusterRole
		if role == "" {
			role = "-"
		}
		_, _ = fmt.Fprintf(out, "Cluster role: %s\n", role)
		if who.IsScoped {
			_, _ = fmt.Fprintln(out, "Scoped:       yes")
		}
		if len(who.Grants) == 0 {
			_, _ = fmt.Fprintln(out, "Grants:       none")
			return nil
		}
		_, _ = fmt.Fprintln(out, "Grants:")
		namespaces := make([]string, 0, len(who.Grants))
		for ns := range who.Grants {
			namespaces = append(namespaces, ns)
		}
		sort.Strings(namespaces)
		for _, ns := range namespaces {
			_, _ = fmt.Fprintf(out, "  %-24s %s\n", ns, who.Grants[ns])
		}
		return nil
	},
}

func init() {
	WhoamiCmd.Flags().StringVar(&whoamiServer, "server", defaultServer, "Caesium server base URL")
	WhoamiCmd.Flags().StringVar(&whoamiAPIKey, "api-key", "", cliutil.APIKeyFlagUsage)
	WhoamiCmd.Flags().BoolVar(&whoamiJSON, "json", false, "Print the server's whoami response as JSON")
}
