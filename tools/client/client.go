package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"

	"github.com/caesium-cloud/caesium/pkg/env"
)

func init() {
	if err := env.Process(); err != nil {
		panic(err)
	}
}

func main() {
	query := os.Args[1]

	buf, err := json.Marshal(
		map[string]interface{}{
			"timings": true,
			"queries": []string{query},
		},
	)

	if err != nil {
		panic(err)
	}

	resp, err := http.Post(
		fmt.Sprintf("http://localhost:%v/v1/private/db/query", env.Variables().Port),
		"application/json",
		bytes.NewBuffer(buf),
	)

	if err != nil {
		panic(err)
	}

	buf, err = ioutil.ReadAll(resp.Body)
	if err != nil {
		panic(err)
	}

	fmt.Print(string(buf))
}
