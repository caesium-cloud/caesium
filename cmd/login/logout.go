package login

import (
	"fmt"
	"net/http"

	"github.com/caesium-cloud/caesium/cmd/cliutil"
	"github.com/caesium-cloud/caesium/internal/clihttp"
	"github.com/spf13/cobra"
)

var logoutServer string

// LogoutCmd is `caesium logout`.
var LogoutCmd = &cobra.Command{
	Use:   "logout",
	Args:  cobra.NoArgs,
	Short: "Revoke and forget the CLI's stored login for a server",
	Long: "Revoke the user-bound key caesium login stored for --server (POST /auth/logout " +
		"with that key) and delete it from the credentials file. It acts only on the stored " +
		"login, never on --api-key or CAESIUM_API_KEY. The local entry is deleted even when " +
		"the server cannot be reached; the command then exits non-zero.",
	RunE: func(cmd *cobra.Command, _ []string) error {
		server, err := cliutil.CanonicalServer(logoutServer)
		if err != nil {
			return err
		}
		store, err := cliutil.LoadCredentials()
		if err != nil {
			return err
		}
		cred, ok := store.Lookup(server)
		if !ok {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Not logged in to %s.\n", server)
			return nil
		}
		headers := make(http.Header)
		headers.Set("Authorization", "Bearer "+cred.Key)
		body, status, readErr := clihttp.Exchange(cmd.Context(), boundedClient(), http.MethodPost, server+"/auth/logout", nil, headers)
		revokeErr := clihttp.ResponseError("logout", status, body, readErr)

		// Remove only the entry this logout revoked, from the file as it is
		// now (another process may have changed it meanwhile).
		if err := cliutil.UpdateCredentials(func(store *cliutil.CredentialStore) error {
			if current, ok := store.Lookup(server); ok && current.Key == cred.Key {
				store.Remove(server)
			}
			return nil
		}); err != nil {
			return fmt.Errorf("delete the stored login: %w", err)
		}
		if revokeErr != nil {
			return fmt.Errorf("deleted the stored login for %s, but the server did not revoke it: %w", server, revokeErr)
		}
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Logged out of %s.\n", server)
		return nil
	},
}

func init() {
	LogoutCmd.Flags().StringVar(&logoutServer, "server", defaultServer, "Caesium server base URL")
}
