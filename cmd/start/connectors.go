package start

import (
	"github.com/caesium-cloud/caesium/internal/connector"
	"github.com/caesium-cloud/caesium/pkg/env"
)

// loadConnectorFile reads the connector config when the gate is on.
// Other commands call env.Process and do not reach this path, so they do not
// resolve connector secrets.
func loadConnectorFile(vars env.Environment) error {
	if !vars.ConnectorsEnabled {
		connector.Clear()
		return nil
	}
	_, _, err := connector.LoadFile(vars.ConnectorsConfigFile)
	return err
}
