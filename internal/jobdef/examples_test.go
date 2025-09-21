package jobdef

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/stretchr/testify/suite"
	"gopkg.in/yaml.v3"
)

type ExamplesSuite struct {
	suite.Suite
}

func TestExamplesSuite(t *testing.T) {
	suite.Run(t, new(ExamplesSuite))
}

func (s *ExamplesSuite) TestExampleManifestsConformToSchema() {
	testCases := map[string][]string{
		"minimal.job.yaml":        {"nightly-etl"},
		"callbacks.job.yaml":      {"csv-to-parquet"},
		"explicit-links.job.yaml": {"explicit-links"},
	}

	root := filepath.Join("..", "..", "docs", "examples")

	for file, aliases := range testCases {
		path := filepath.Join(root, file)
		data, err := os.ReadFile(path)
		s.Require().NoErrorf(err, "read example %s", file)

		dec := yaml.NewDecoder(bytes.NewReader(data))
		idx := 0
		for {
			var def schema.Definition
			err := dec.Decode(&def)
			if errors.Is(err, io.EOF) {
				break
			}
			s.Require().NoErrorf(err, "decode %s doc %d", file, idx)
			s.Require().Less(idx, len(aliases), "unexpected extra document in %s", file)
			s.Require().NoErrorf(def.Validate(), "validate %s doc %d", file, idx)

			s.Equalf(aliases[idx], def.Metadata.Alias, "alias mismatch in %s doc %d", file, idx)
			idx++
		}
		s.Equalf(len(aliases), idx, "expected %d docs in %s", len(aliases), file)
	}
}
