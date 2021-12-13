package env

import (
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/kelseyhightower/envconfig"
	"github.com/pkg/errors"
)

var variables = new(Environment)

// Process the environment variables set for caesium.
func Process() error {
	if err := envconfig.Process("caesium", variables); err != nil {
		return errors.Wrap(err, "failed to process environment variables")
	}

	// set the log level
	if err := log.SetLevel(variables.LogLevel); err != nil {
		return errors.Wrap(err, "failed to set log level")
	}

	return nil
}

// Variables returns the processed environment variables.
func Variables() Environment {
	return *variables
}

// Environment defines the environment variables used
// by caesium.
type Environment struct {
	LogLevel            string   `default:"info"`
	Port                int      `default:"8080"`
	KubernetesConfig    string   `default:""`
	KubernetesNamespace string   `default:"default"`
	NodeAddress         string   `default:"127.0.0.1:9001"`
	DatabaseNodes       []string `default:""`
	DBPath              string   `default:"/tmp"`
	DatabaseType        string   `default:"internal"`
	DatabaseDSN         string   `default:"host=postgres user=postgres password=postgres dbname=caesium port=5432 sslmode=disable"`
}
