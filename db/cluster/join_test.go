package cluster

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/suite"
)

const (
	numAttempts     int           = 30
	attemptInterval time.Duration = 500 * time.Millisecond
)

type ClusterTestSuite struct {
	suite.Suite
}

func (s *ClusterTestSuite) TestSingleJoin() {
	var body map[string]interface{}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(s.T(), http.MethodPost, r.Method)
		w.WriteHeader(http.StatusOK)

		b, err := ioutil.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		if err := json.Unmarshal(b, &body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
	}))

	defer ts.Close()

	j, err := Join(&JoinRequest{
		SourceIP:        "",
		JoinAddress:     []string{ts.URL},
		ID:              "id0",
		Address:         "127.0.0.1:9090",
		Voter:           false,
		Attempts:        numAttempts,
		AttemptInterval: attemptInterval,
		TLSConfig:       nil,
	})
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), ts.URL+"/join", j)
	assert.Equal(s.T(), body["id"].(string), "id0")
	assert.Equal(s.T(), body["address"].(string), "127.0.0.1:9090")
	assert.False(s.T(), body["voter"].(bool))
}

func (s *ClusterTestSuite) TestSingleJoinZeroAttempts() {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.T().Fatalf("handler should not have been called")
	}))

	_, err := Join(&JoinRequest{
		SourceIP:        "127.0.0.1",
		JoinAddress:     []string{ts.URL},
		ID:              "id0",
		Address:         "127.0.0.1:9090",
		Voter:           false,
		Attempts:        0,
		AttemptInterval: attemptInterval,
		TLSConfig:       nil,
	})
	assert.Equal(s.T(), ErrJoinFailed, err)
}

func (s *ClusterTestSuite) TestSingleJoinMeta() {
	var body map[string]interface{}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(s.T(), http.MethodPost, r.Method)
		w.WriteHeader(http.StatusOK)

		b, err := ioutil.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		if err := json.Unmarshal(b, &body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
	}))
	defer ts.Close()

	req := &JoinRequest{
		SourceIP:        "127.0.0.1",
		JoinAddress:     []string{ts.URL},
		ID:              "id0",
		Address:         "127.0.0.1:9090",
		Voter:           true,
		Metadata:        map[string]string{"foo": "bar"},
		Attempts:        numAttempts,
		AttemptInterval: attemptInterval,
		TLSConfig:       nil,
	}
	j, err := Join(req)
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), ts.URL+"/join", j)
	assert.Equal(s.T(), "id0", body["id"])
	assert.Equal(s.T(), req.Address, body["address"])

	rxMd, _ := body["metadata"].(map[string]interface{})
	assert.Equal(s.T(), len(req.Metadata), len(rxMd))
	assert.Equal(s.T(), "bar", rxMd["foo"])
}

func (s *ClusterTestSuite) TestSingleJoinFailure() {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer ts.Close()

	_, err := Join(&JoinRequest{
		SourceIP:        "",
		JoinAddress:     []string{ts.URL},
		ID:              "id0",
		Address:         "127.0.0.1:9090",
		Voter:           true,
		Attempts:        numAttempts,
		AttemptInterval: attemptInterval,
		TLSConfig:       nil,
	})
	assert.NotNil(s.T(), err)
}

func (s *ClusterTestSuite) TestDoubleJoinFirstNode() {
	ts1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	}))
	defer ts1.Close()
	ts2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	}))
	defer ts2.Close()

	j, err := Join(&JoinRequest{
		SourceIP:        "127.0.0.1",
		JoinAddress:     []string{ts1.URL, ts2.URL},
		ID:              "id0",
		Address:         "127.0.0.1:9090",
		Voter:           true,
		Attempts:        numAttempts,
		AttemptInterval: attemptInterval,
		TLSConfig:       nil,
	})
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), ts1.URL+"/join", j)
}

func (s *ClusterTestSuite) TestDoubleJoinSecondNode() {
	ts1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer ts1.Close()
	ts2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	}))
	defer ts2.Close()

	j, err := Join(&JoinRequest{
		SourceIP:        "",
		JoinAddress:     []string{ts1.URL, ts2.URL},
		ID:              "id0",
		Address:         "127.0.0.1:9090",
		Voter:           true,
		Attempts:        numAttempts,
		AttemptInterval: attemptInterval,
		TLSConfig:       nil,
	})
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), ts2.URL+"/join", j)
}

func (s *ClusterTestSuite) TestDoubleJoinSecondNodeRedirect() {
	ts1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	}))
	defer ts1.Close()
	redirectAddr := fmt.Sprintf("%s%s", ts1.URL, "/join")

	ts2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, redirectAddr, http.StatusMovedPermanently)
	}))
	defer ts2.Close()

	j, err := Join(&JoinRequest{
		SourceIP:        "127.0.0.1",
		JoinAddress:     []string{ts2.URL},
		ID:              "id0",
		Address:         "127.0.0.1:9090",
		Voter:           true,
		Attempts:        numAttempts,
		AttemptInterval: attemptInterval,
		TLSConfig:       nil,
	})
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), redirectAddr, j)
}

func TestClusterTestSuite(t *testing.T) {
	suite.Run(t, new(ClusterTestSuite))
}
