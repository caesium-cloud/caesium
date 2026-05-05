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
	roots := []string{
		filepath.Join("..", "..", "docs", "examples"),
		filepath.Join("..", "..", "docs", "examples-k8s"),
	}

	type manifest struct {
		root string
		name string
	}
	var files []manifest
	for _, root := range roots {
		entries, err := os.ReadDir(root)
		s.Require().NoErrorf(err, "read examples dir %s", root)
		var local []string
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			if filepath.Ext(entry.Name()) != ".yaml" && filepath.Ext(entry.Name()) != ".yml" {
				continue
			}
			local = append(local, entry.Name())
		}
		sort.Strings(local)
		s.Require().NotEmptyf(local, "expected at least one example manifest in %s", root)
		for _, name := range local {
			files = append(files, manifest{root: root, name: name})
		}
	}

	seenAliases := make(map[string]string)
	for _, m := range files {
		path := filepath.Join(m.root, m.name)
		data, err := os.ReadFile(path)
		s.Require().NoErrorf(err, "read example %s", path)

		dec := yaml.NewDecoder(bytes.NewReader(data))
		docCount := 0
		for {
			var def schema.Definition
			err := dec.Decode(&def)
			if errors.Is(err, io.EOF) {
				break
			}
			s.Require().NoErrorf(err, "decode %s doc %d", path, docCount)
			if def.APIVersion == "" && def.Kind == "" && def.Metadata.Alias == "" && len(def.Steps) == 0 {
				continue
			}
			s.Require().NoErrorf(def.Validate(), "validate %s doc %d", path, docCount)
			s.NotEmptyf(def.Metadata.Alias, "alias required in %s doc %d", path, docCount)

			if previous, ok := seenAliases[def.Metadata.Alias]; ok {
				s.Failf("duplicate example alias", "alias %q is used in both %s and %s", def.Metadata.Alias, previous, path)
			}
			seenAliases[def.Metadata.Alias] = path
			docCount++
		}
		s.Greaterf(docCount, 0, "expected at least one definition in %s", path)
	}
}
