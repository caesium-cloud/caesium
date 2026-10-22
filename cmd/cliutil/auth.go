package cliutil

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

const APIKeyEnvVar = "CAESIUM_API_KEY"

// APIKeyFlagUsage is the standard --api-key flag description.
const APIKeyFlagUsage = "API key for authentication (prefer " + APIKeyEnvVar + " or caesium login; --api-key is visible in process listings)"

// ResolveAPIKey is the CLI's single credential resolver (design §13.1):
// the --api-key flag, then preferredEnvVar and fallbackEnvVars, then the
// credential `caesium login` stored for the command's --server. A stored
// credential past its expiry is not sent (a hint goes to stderr).
func ResolveAPIKey(cmd *cobra.Command, flagValue, preferredEnvVar string, fallbackEnvVars ...string) string {
	if strings.TrimSpace(flagValue) != "" {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "warning: --api-key is visible in process listings; prefer %s\n", preferredEnvVar)
		return strings.TrimSpace(flagValue)
	}

	for _, envVar := range append([]string{preferredEnvVar}, fallbackEnvVars...) {
		if value := strings.TrimSpace(os.Getenv(envVar)); value != "" {
			return value
		}
	}
	return storedAPIKey(cmd)
}

// SetAuthorization sets the bearer token ResolveAPIKey finds for cmd on
// headers, if there is one.
func SetAuthorization(cmd *cobra.Command, headers http.Header, flagValue string) {
	if key := ResolveAPIKey(cmd, flagValue, APIKeyEnvVar); key != "" {
		headers.Set("Authorization", "Bearer "+key)
	}
}

// ServerFlag returns the value of the command's --server flag (its own or
// inherited), or "" when it has none.
func ServerFlag(cmd *cobra.Command) string {
	if cmd == nil {
		return ""
	}
	flag := cmd.Flags().Lookup("server")
	if flag == nil {
		flag = cmd.InheritedFlags().Lookup("server")
	}
	if flag == nil {
		return ""
	}
	return strings.TrimSpace(flag.Value.String())
}

func storedAPIKey(cmd *cobra.Command) string {
	server := ServerFlag(cmd)
	if server == "" {
		return ""
	}
	if _, err := CanonicalServer(server); err != nil {
		return ""
	}
	store, err := LoadCredentials()
	if err != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "warning: ignoring stored credentials: %v\n", err)
		return ""
	}
	cred, ok := store.Lookup(server)
	if !ok {
		return ""
	}
	if cred.Expired(time.Now()) {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "warning: the stored login for %s expired at %s; run caesium login\n",
			cred.Server, cred.ExpiresAt.Format(time.RFC3339))
		return ""
	}
	return cred.Key
}
