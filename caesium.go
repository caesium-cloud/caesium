package main

import (
	"github.com/caesium-cloud/caesium/cmd"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/caesium-cloud/caesium/pkg/log"
)

func main() {
	log.Info("launching caesium")

	if err := env.Process(); err != nil {
		log.Fatal("environment failure (%v)", err)
	}

	if err := cmd.Execute(); err != nil {
		log.Fatal("caesium failure (%v)", err)
	}
}
