package command

import (
	"testing"

	"github.com/golang/protobuf/proto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/suite"
)

type CommandTestSuite struct {
	suite.Suite
}

func (s *CommandTestSuite) TestUncompressed() {
	rm := NewRequestMarshaler()
	r := &QueryRequest{
		Request: &Request{
			Statements: []*Statement{
				{
					Sql: `INSERT INTO "names" VALUES(1,'bob','123-45-678')`,
				},
			},
		},
		Timings:   true,
		Freshness: 100,
	}

	b, comp, err := rm.Marshal(r)
	assert.Nil(s.T(), err)
	assert.False(s.T(), comp)

	c := &Command{
		Type:       Command_COMMAND_TYPE_QUERY,
		SubCommand: b,
		Compressed: comp,
	}

	b, err = Marshal(c)
	assert.Nil(s.T(), err)

	var nc Command
	assert.Nil(s.T(), Unmarshal(b, &nc))
	assert.Equal(s.T(), Command_COMMAND_TYPE_QUERY, nc.GetType())
	assert.False(s.T(), nc.GetCompressed())

	var nr QueryRequest
	assert.Nil(s.T(), UnmarshalSubCommand(&nc, &nr))
	assert.Equal(s.T(), r.Timings, nr.GetTimings())
	assert.Equal(s.T(), r.Freshness, nr.GetFreshness())
	assert.Len(s.T(), nr.Request.Statements, 1)
	assert.Equal(s.T(), `INSERT INTO "names" VALUES(1,'bob','123-45-678')`, nr.Request.Statements[0].GetSql())
}

func (s *CommandTestSuite) TestCompressedBatch() {
	rm := NewRequestMarshaler()
	rm.BatchThreshold = 1
	rm.ForceCompression = true

	r := &QueryRequest{
		Request: &Request{
			Statements: []*Statement{
				{
					Sql: `INSERT INTO "names" VALUES(1,'bob','123-45-678')`,
				},
			},
		},
		Timings:   true,
		Freshness: 100,
	}

	b, comp, err := rm.Marshal(r)
	assert.Nil(s.T(), err)
	assert.True(s.T(), comp)

	c := &Command{
		Type:       Command_COMMAND_TYPE_QUERY,
		SubCommand: b,
		Compressed: comp,
	}

	b, err = Marshal(c)
	assert.Nil(s.T(), err)

	var nc Command
	assert.Nil(s.T(), Unmarshal(b, &nc))
	assert.Equal(s.T(), Command_COMMAND_TYPE_QUERY, nc.GetType())
	assert.True(s.T(), nc.GetCompressed())

	var nr QueryRequest
	assert.Nil(s.T(), UnmarshalSubCommand(&nc, &nr))
	assert.True(s.T(), proto.Equal(&nr, r))
}

func (s *CommandTestSuite) TestCompressedSize() {
	rm := NewRequestMarshaler()
	rm.SizeThreshold = 1
	rm.ForceCompression = true

	r := &QueryRequest{
		Request: &Request{
			Statements: []*Statement{
				{
					Sql: `INSERT INTO "names" VALUES(1,'bob','123-45-678')`,
				},
			},
		},
		Timings:   true,
		Freshness: 100,
	}

	b, comp, err := rm.Marshal(r)
	assert.Nil(s.T(), err)
	assert.True(s.T(), comp)

	c := &Command{
		Type:       Command_COMMAND_TYPE_QUERY,
		SubCommand: b,
		Compressed: comp,
	}

	b, err = Marshal(c)
	assert.Nil(s.T(), err)

	var nc Command
	assert.Nil(s.T(), Unmarshal(b, &nc))
	assert.Equal(s.T(), Command_COMMAND_TYPE_QUERY, nc.GetType())
	assert.True(s.T(), nc.GetCompressed())

	var nr QueryRequest
	assert.Nil(s.T(), UnmarshalSubCommand(&nc, &nr))
	assert.True(s.T(), proto.Equal(&nr, r))
}

func (s *CommandTestSuite) TestUncompressedBatch() {
	rm := NewRequestMarshaler()
	rm.BatchThreshold = 1

	r := &QueryRequest{
		Request: &Request{
			Statements: []*Statement{
				{
					Sql: `INSERT INTO "names" VALUES(1,'bob','123-45-678')`,
				},
			},
		},
		Timings:   true,
		Freshness: 100,
	}

	_, comp, err := rm.Marshal(r)
	assert.Nil(s.T(), err)
	assert.False(s.T(), comp)
}

func (s *CommandTestSuite) TestUncompressedSize() {
	rm := NewRequestMarshaler()
	rm.SizeThreshold = 1

	r := &QueryRequest{
		Request: &Request{
			Statements: []*Statement{
				{
					Sql: `INSERT INTO "names" VALUES(1,'bob','123-45-678')`,
				},
			},
		},
		Timings:   true,
		Freshness: 100,
	}

	_, comp, err := rm.Marshal(r)
	assert.Nil(s.T(), err)
	assert.False(s.T(), comp)
}

func TestCommandTestSuite(t *testing.T) {
	suite.Run(t, new(CommandTestSuite))
}
