package env

import (
	"github.com/caesium-dev/caesium/log"
	"github.com/kelseyhightower/envconfig"
)

// Variables stores all of the necessary set environment
// variables as outlined in the Environment structure
var Variables *Environment

func init() {
	if err := envconfig.Process("caesium", Variables); err != nil {
		log.Fatal("failed to process environment variables (%v)", err)
	}
}

// Environment defines the environment variables used
// by caesium
type Environment struct {
	LogLevel string `default:"info"`
}
