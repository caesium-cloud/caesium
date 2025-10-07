//go:build integration

package test

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/suite"
)

type IntegrationTestSuite struct {
	suite.Suite
	caesiumURL  string
	cliPath     string
	projectRoot string
}

func (s *IntegrationTestSuite) SetupSuite() {
	cwd, err := os.Getwd()
	if err != nil {
		s.T().Fatalf("getwd: %v", err)
	}
	s.projectRoot = filepath.Clean(filepath.Join(cwd, ".."))

	binPath := filepath.Join(s.projectRoot, "caesium-cli")
	build := exec.Command("go", "build", "-o", binPath, ".")
	build.Dir = s.projectRoot
	build.Env = append(os.Environ(), "GOFLAGS=-buildvcs=false")
	if output, err := build.CombinedOutput(); err != nil {
		s.T().Fatalf("build caesium cli: %v\n%s", err, string(output))
	}
	s.cliPath = binPath
	s.T().Cleanup(func() {
		_ = os.Remove(binPath)
	})

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
