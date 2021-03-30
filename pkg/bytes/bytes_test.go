package bytes

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/suite"
)

type BytesTestSuite struct {
	suite.Suite
}

func (s *BytesTestSuite) TestUint64() {
	u := uint64(math.MaxUint64)
	buf := FromUint64(u)
	assert.Equal(s.T(), ToUint64(buf), u)
}

func TestBytesTestSuite(t *testing.T) {
	suite.Run(t, new(BytesTestSuite))
}
