package main

import (
	"github.com/caesium-cloud/caesium/cmd"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/caesium-cloud/caesium/pkg/log"
)

func main() {
	if err := env.Process(); err != nil {
		log.Fatal("environment failure", "error", err)
	}

	if err := cmd.Execute(); err != nil {
		log.Fatal("caesium failure", "error", err)
	}
}
