package env

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/caesium-cloud/caesium/internal/connector"
	"github.com/caesium-cloud/caesium/internal/jobdef/secret"
)

// connectorRuntime holds the file loaded for an enabled connector gate.
// Resolved secret bytes are not retained.
type connectorRuntime struct {
	config      *connector.Config
	fingerprint string
}

var (
	connectorRuntimeMu sync.RWMutex
	connectorState     connectorRuntime
)

// ConnectorConfig returns the validated connector file and its canonical
// fingerprint when the gate is enabled and the file loaded. The second
// result is empty when connectors are disabled.
func ConnectorConfig() (*connector.Config, string, bool) {
	connectorRuntimeMu.RLock()
	defer connectorRuntimeMu.RUnlock()
	if connectorState.config == nil {
		return nil, "", false
	}
	return connectorState.config, connectorState.fingerprint, true
}

func clearConnectorRuntime() {
	connectorRuntimeMu.Lock()
	connectorState = connectorRuntime{}
	connectorRuntimeMu.Unlock()
}

func storeConnectorRuntime(config *connector.Config, fingerprint string) {
	connectorRuntimeMu.Lock()
	connectorState = connectorRuntime{config: config, fingerprint: fingerprint}
	connectorRuntimeMu.Unlock()
}

func validateConnectorGate() error {
	if !variables.ConnectorsEnabled {
		return nil
	}
	mode := strings.ToLower(strings.TrimSpace(variables.AuthMode))
	if mode != "api-key" && !variables.SSOEnabled() {
		return errors.New("CAESIUM_CONNECTORS_ENABLED requires an active authentication mode: set CAESIUM_AUTH_MODE=api-key or enable an SSO provider")
	}
	if strings.TrimSpace(variables.ConnectorsConfigFile) == "" {
		return errors.New("CAESIUM_CONNECTORS_CONFIG_FILE is required when CAESIUM_CONNECTORS_ENABLED=true")
	}
	return nil
}

func loadConnectorConfig() error {
	path := strings.TrimSpace(variables.ConnectorsConfigFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return errors.New("CAESIUM_CONNECTORS_CONFIG_FILE cannot be read")
	}
	cfg, resolved, err := connector.Parse(data, secret.NewEnvResolver())
	if err != nil {
		return fmt.Errorf("CAESIUM_CONNECTORS_CONFIG_FILE rejected: %w", err)
	}
	fingerprint, err := connector.Fingerprint(cfg, resolved)
	if err != nil {
		return fmt.Errorf("CAESIUM_CONNECTORS_CONFIG_FILE rejected: %w", err)
	}
	storeConnectorRuntime(cfg, fingerprint)
	return nil
}
