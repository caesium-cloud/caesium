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
	LogLevel            string   `default:"info" split_words:"true"`
	Port                int      `default:"8080"`
	DockerHost          string   `default:"" split_words:"true"`
	KubernetesConfig    string   `default:"" split_words:"true"`
	KubernetesNamespace string   `default:"default" split_words:"true"`
	PodmanURI           string   `default:"" split_words:"true"`
	NodeAddress         string   `default:"127.0.0.1:9001" split_words:"true"`
	DatabaseNodes       []string `default:"" split_words:"true"`
	DatabasePath        string   `default:"/opt/caesium/dqlite" split_words:"true"`
	DatabaseType        string   `default:"internal" split_words:"true"`
	DatabaseDSN         string   `default:"host=postgres user=postgres password=postgres dbname=caesium port=5432 sslmode=disable" split_words:"true"`
}
