package tcp

import (
	"os"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/db/testdata/x509"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/suite"
)

type TCPTestSuite struct {
	suite.Suite
}

func (s *TCPTestSuite) TestTransportOpenClose() {
	tn := NewTransport()
	assert.Nil(s.T(), tn.Open("localhost:0"))
	assert.Regexp(s.T(), "127.0.0.1:[0-9]+", tn.Addr().String())
	assert.Nil(s.T(), tn.Close())
}

func (s *TCPTestSuite) TestTransportDial() {
	tn1 := NewTransport()
	defer tn1.Close()
	tn1.Open("localhost:0")
	go tn1.Accept()

	tn2 := NewTransport()
	defer tn2.Close()
	_, err := tn2.Dial(tn1.Addr().String(), time.Second)
	assert.Nil(s.T(), err)
}

func (s *TCPTestSuite) TestTLSTransportOpenClose() {
	c := x509.CertFile("")
	defer os.Remove(c)
	k := x509.KeyFile("")
	defer os.Remove(k)

	tn := NewTLSTransport(c, k, true)
	assert.Nil(s.T(), tn.Open("localhost:0"))
	assert.Regexp(s.T(), "127.0.0.1:[0-9]+", tn.Addr().String())
	assert.Nil(s.T(), tn.Close())
}

func TestTCPTestSuite(t *testing.T) {
	suite.Run(t, new(TCPTestSuite))
}
