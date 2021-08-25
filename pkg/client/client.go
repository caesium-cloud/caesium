package client

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"

	"github.com/caesium-cloud/caesium/api/rest/service/private/db"
	"github.com/caesium-cloud/caesium/pkg/env"
)

type Caesium interface {
	Query(string) (*db.QueryResponse, error)
}

func Client() Caesium {
	return &client{}
}

type client struct {
}

func (c *client) Query(q string) (*db.QueryResponse, error) {
	body := map[string]interface{}{
		"timings": true,
		"queries": []string{q},
	}

	buf, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	resp, err := http.Post(
		fmt.Sprintf("http://localhost:%v/v1/private/db/query", env.Variables().Port),
		"application/json",
		bytes.NewBuffer(buf),
	)

	if err != nil {
		return nil, err
	}

	if buf, err = ioutil.ReadAll(resp.Body); err != nil {
		return nil, err
	}

	results := &db.QueryResponse{}

	if err = json.Unmarshal(buf, results); err != nil {
		return nil, err
	}

	return results, nil
}
