package start

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/caesium-cloud/caesium/internal/connector"
	"github.com/caesium-cloud/caesium/pkg/env"
)

func TestLoadConnectorFileReadsOnlyWhenEnabled(t *testing.T) {
	t.Cleanup(connector.Clear)
	dir := t.TempDir()
	if err := loadConnectorFile(env.Environment{ConnectorsConfigFile: dir}); err != nil {
		t.Fatal(err)
	}
	if _, _, loaded := connector.Current(); loaded {
		t.Fatal("disabled startup loaded a connector file")
	}

	_, _, err := connector.LoadFile(dir)
	if err == nil {
		t.Fatal("directory was accepted as a connector file")
	}

	path := filepath.Join(dir, "connectors.yaml")
	if err := os.WriteFile(path, []byte(startConnectorYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEMPORAL_TOKEN", "temporal")
	if err := loadConnectorFile(env.Environment{ConnectorsEnabled: true, ConnectorsConfigFile: path}); err != nil {
		t.Fatal(err)
	}
	cfg, fingerprint, loaded := connector.Current()
	if !loaded || fingerprint == "" || cfg == nil || cfg.Version != 1 {
		t.Fatalf("loaded = %v fingerprint %q", loaded, fingerprint)
	}
	t.Setenv("TEMPORAL_TOKEN", "frontend.temporal.svc")
	if err := loadConnectorFile(env.Environment{ConnectorsEnabled: true, ConnectorsConfigFile: path}); err != nil {
		t.Fatal(err)
	}
	_, rotated, loaded := connector.Current()
	if !loaded || rotated != fingerprint {
		t.Fatalf("rotation changed fingerprint %s -> %s", fingerprint, rotated)
	}
}

const startConnectorYAML = `
version: 1
connections:
  - id: primary
    provider: temporal
    endpoint: frontend.temporal.svc:7233
    scope: default
    credentials:
      secretRefs:
        - secret://env/TEMPORAL_TOKEN?name=TEMPORAL_TOKEN
      certificatePaths:
        - /var/run/secrets/caesium/temporal/tls.crt
`
