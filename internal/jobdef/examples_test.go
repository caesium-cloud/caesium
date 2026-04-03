package jobdef

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
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
	root := filepath.Join("..", "..", "docs", "examples")
	entries, err := os.ReadDir(root)
	s.Require().NoError(err)

	var files []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if filepath.Ext(entry.Name()) != ".yaml" && filepath.Ext(entry.Name()) != ".yml" {
			continue
		}
		files = append(files, entry.Name())
	}
	sort.Strings(files)
	s.Require().NotEmpty(files, "expected at least one example manifest")

	seenAliases := make(map[string]string)
	for _, file := range files {
		path := filepath.Join(root, file)
		data, err := os.ReadFile(path)
		s.Require().NoErrorf(err, "read example %s", file)

		dec := yaml.NewDecoder(bytes.NewReader(data))
		docCount := 0
		for {
			var def schema.Definition
			err := dec.Decode(&def)
			if errors.Is(err, io.EOF) {
				break
			}
			s.Require().NoErrorf(err, "decode %s doc %d", file, docCount)
			if def.APIVersion == "" && def.Kind == "" && def.Metadata.Alias == "" && len(def.Steps) == 0 {
				continue
			}
			s.Require().NoErrorf(def.Validate(), "validate %s doc %d", file, docCount)
			s.NotEmptyf(def.Metadata.Alias, "alias required in %s doc %d", file, docCount)

			if previous, ok := seenAliases[def.Metadata.Alias]; ok {
				s.Failf("duplicate example alias", "alias %q is used in both %s and %s", def.Metadata.Alias, previous, file)
			}
			seenAliases[def.Metadata.Alias] = file
			docCount++
		}
		s.Greaterf(docCount, 0, "expected at least one definition in %s", file)
	}
}
