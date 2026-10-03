package connector

import (
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/caesium-cloud/caesium/internal/jobdef/secret"
)

// MaxConfigFileBytes is the largest connector file caesium start will read.
// Anything larger, including a device node that reports a huge size, is refused
// before the bytes are loaded.
const MaxConfigFileBytes int64 = 1 << 20

// runtime is the file loaded by server startup. Resolved secret bytes are not kept.
type runtime struct {
	config      *Config
	fingerprint string
}

var (
	runtimeMu    sync.RWMutex
	runtimeState runtime
)

// Current returns the config and fingerprint loaded by LoadFile.
func Current() (*Config, string, bool) {
	runtimeMu.RLock()
	defer runtimeMu.RUnlock()
	if runtimeState.config == nil {
		return nil, "", false
	}
	return runtimeState.config, runtimeState.fingerprint, true
}

// Clear drops the loaded config. Server startup calls it when the gate is off.
func Clear() {
	runtimeMu.Lock()
	runtimeState = runtime{}
	runtimeMu.Unlock()
}

// LoadFile reads, parses, and fingerprints the connector file. The OS error is
// wrapped so a missing file, a permission failure, and a directory stay
// distinguishable. The file must be a regular file no larger than
// MaxConfigFileBytes.
func LoadFile(path string) (*Config, string, error) {
	cfg, fingerprint, err := readConfigFile(path)
	if err != nil {
		Clear()
		return nil, "", err
	}
	runtimeMu.Lock()
	runtimeState = runtime{config: cfg, fingerprint: fingerprint}
	runtimeMu.Unlock()
	return cfg, fingerprint, nil
}

func readConfigFile(path string) (*Config, string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, "", fmt.Errorf("CAESIUM_CONNECTORS_CONFIG_FILE %q cannot be read: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, "", fmt.Errorf("CAESIUM_CONNECTORS_CONFIG_FILE %q cannot be read: not a regular file", path)
	}
	if info.Size() > MaxConfigFileBytes {
		return nil, "", fmt.Errorf("CAESIUM_CONNECTORS_CONFIG_FILE %q cannot be read: file exceeds %d bytes", path, MaxConfigFileBytes)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, "", fmt.Errorf("CAESIUM_CONNECTORS_CONFIG_FILE %q cannot be read: %w", path, err)
	}
	data, readErr := io.ReadAll(io.LimitReader(file, MaxConfigFileBytes+1))
	closeErr := file.Close()
	if readErr != nil {
		return nil, "", fmt.Errorf("CAESIUM_CONNECTORS_CONFIG_FILE %q cannot be read: %w", path, readErr)
	}
	if closeErr != nil {
		return nil, "", fmt.Errorf("CAESIUM_CONNECTORS_CONFIG_FILE %q cannot be read: %w", path, closeErr)
	}
	if int64(len(data)) > MaxConfigFileBytes {
		return nil, "", fmt.Errorf("CAESIUM_CONNECTORS_CONFIG_FILE %q cannot be read: file exceeds %d bytes", path, MaxConfigFileBytes)
	}
	cfg, err := Parse(data, secret.NewEnvResolver())
	if err != nil {
		return nil, "", fmt.Errorf("CAESIUM_CONNECTORS_CONFIG_FILE rejected: %w", err)
	}
	fingerprint, err := Fingerprint(cfg)
	if err != nil {
		return nil, "", fmt.Errorf("CAESIUM_CONNECTORS_CONFIG_FILE rejected: %w", err)
	}
	return cfg, fingerprint, nil
}
