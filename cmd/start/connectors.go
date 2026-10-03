package start

import (
	"github.com/caesium-cloud/caesium/internal/connector"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/caesium-cloud/caesium/pkg/log"
)

// loadConnectorFile reads the connector config when the gate is on.
// Other commands call env.Process and do not reach this path, so they do not
// resolve connector secrets.
func loadConnectorFile(vars env.Environment) error {
	if !vars.ConnectorsEnabled {
		connector.Clear()
		return nil
	}
	cfg, fingerprint, err := connector.LoadFile(vars.ConnectorsConfigFile)
	if err != nil {
		return err
	}
	connections := 0
	if cfg != nil {
		connections = len(cfg.Connections)
	}
	// Operators copy this value into CAESIUM_CONNECTORS_CONFIG_PREVIOUS_FINGERPRINT
	// before a later rollout. The digest contains no secret bytes.
	log.Info("connector config loaded", "fingerprint", fingerprint, "connections", connections)
	return nil
}
