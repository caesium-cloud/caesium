//go:build integration

package test

import (
	"fmt"
	"net/http"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/suite"
)

type IntegrationTestSuite struct {
	suite.Suite
	caesiumURL string
}

func (s *IntegrationTestSuite) SetupTest() {
	host := os.Getenv("CAESIUM_HOST")
	if host == "" {
		host = "localhost"
	}
	s.caesiumURL = fmt.Sprintf("http://%v:8080", host)

	// migrate DB
	resp, err := http.Post(
		fmt.Sprintf("%v/v1/private/db/migrate", s.caesiumURL),
		"application/json",
		nil,
	)
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), http.StatusNoContent, resp.StatusCode)
}

func (s *IntegrationTestSuite) TestHealth() {
	resp, err := http.Get(fmt.Sprintf("%v/health", s.caesiumURL))
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), http.StatusOK, resp.StatusCode)
}

func TestIntegrationTestSuite(t *testing.T) {
	suite.Run(t, new(IntegrationTestSuite))
}
