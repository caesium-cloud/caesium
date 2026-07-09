package main

import (
	"os"

	"github.com/caesium-cloud/caesium/cmd"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/caesium-cloud/caesium/pkg/log"
)

func main() {
	if err := env.Process(); err != nil {
		log.Fatal("environment failure", "error", err)
	}

	if err := cmd.Execute(); err != nil {
		if code, ok := cmd.ExitCode(err); ok {
			os.Exit(code)
		}
		log.Fatal("caesium failure", "error", err)
	}
}
