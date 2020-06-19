package main

import (
	"github.com/caesium-dev/caesium/cmd"
	"github.com/caesium-dev/caesium/log"
)

func main() {
	log.Info("launching caesium")

	if err := cmd.Execute(); err != nil {
		log.Fatal("caesium failure (%v)", err)
	}
}
