package env

import (
	"github.com/caesium-dev/caesium/log"
	"github.com/kelseyhightower/envconfig"
	"github.com/pkg/errors"
)

var variables = new(Environment)

// Process the environment variables set for caesium.
func Process() (err error) {
	if err = envconfig.Process("caesium", variables); err != nil {
		return errors.Wrap(err, "failed to process environment variables")
	}

	// set the log level
	if err = log.SetLevelFromString(variables.LogLevel); err != nil {
		return errors.Wrap(err, "failed to set log level")
	}

	return
}

// Variables returns the processed environment variables.
func Variables() Environment {
	return *variables
}

// Environment defines the environment variables used
// by caesium.
type Environment struct {
	LogLevel string `default:"info"`
}
