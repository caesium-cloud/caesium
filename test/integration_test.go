//go:build integration

package test

import (
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/suite"
)

type IntegrationTestSuite struct {
	suite.Suite
	caesiumURL string
}

func (s *IntegrationTestSuite) SetupSuite() {
	host := os.Getenv("CAESIUM_HOST")
	if host == "" {
		host = "localhost"
	}
	s.caesiumURL = fmt.Sprintf("http://%v:8080", host)

	deadline := time.Now().Add(2 * time.Minute)
	for {
		if time.Now().After(deadline) {
			s.T().Fatal("timeout waiting for caesium /health to be ready")
		}

		resp, err := http.Get(fmt.Sprintf("%v/health", s.caesiumURL))
		if err == nil && resp != nil {
			if resp.Body != nil {
				_ = resp.Body.Close()
			}
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		time.Sleep(time.Second)
	}
}

func (s *IntegrationTestSuite) TestHealth() {
	resp, err := http.Get(fmt.Sprintf("%v/health", s.caesiumURL))
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), http.StatusOK, resp.StatusCode)
}

func TestIntegrationTestSuite(t *testing.T) {
	suite.Run(t, new(IntegrationTestSuite))
}
