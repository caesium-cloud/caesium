package job

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/caesium-cloud/caesium/api/rest/controller/jobdef"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

var (
	applyPaths  []string
	applyServer string
)

var applyCmd = &cobra.Command{
	Use:   "apply",
	Short: "Apply job definitions via the REST API",
	RunE: func(cmd *cobra.Command, args []string) error {
		defs, err := collectDefinitions(applyPaths)
		if err != nil {
			return err
		}
		if len(defs) == 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "No job definitions found.")
			return nil
		}

		if err := sendApplyRequest(strings.TrimSuffix(applyServer, "/"), defs); err != nil {
			return err
		}

		fmt.Fprintf(cmd.OutOrStdout(), "Applied %d job definition(s)\n", len(defs))
		return nil
	},
}

func init() {
	applyCmd.Flags().StringSliceVarP(&applyPaths, "path", "p", nil, "Paths to job definition files or directories (default: current directory)")
	applyCmd.Flags().StringVar(&applyServer, "server", "http://localhost:8080", "Caesium server base URL")
	Cmd.AddCommand(applyCmd)
}

func collectDefinitions(paths []string) ([]schema.Definition, error) {
	if len(paths) == 0 {
		paths = []string{"."}
	}

	var defs []schema.Definition
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		if info.IsDir() {
			if err := filepath.WalkDir(p, func(path string, d os.DirEntry, walkErr error) error {
				if walkErr != nil {
					return walkErr
				}
				if d.IsDir() {
					return nil
				}
				if !isYAML(path) {
					return nil
				}
				return appendDefinitions(path, &defs)
			}); err != nil {
				return nil, err
			}
		} else {
			if !isYAML(p) {
				return nil, fmt.Errorf("%s is not a YAML file", p)
			}
			if err := appendDefinitions(p, &defs); err != nil {
				return nil, err
			}
		}
	}
	return defs, nil
}

func appendDefinitions(path string, defs *[]schema.Definition) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	dec := yaml.NewDecoder(bytes.NewReader(data))
	for {
		var def schema.Definition
		if err := dec.Decode(&def); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return fmt.Errorf("%s: %w", path, err)
		}
		if isBlankDefinition(&def) {
			continue
		}
		if err := def.Validate(); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		*defs = append(*defs, def)
	}

	return nil
}

func sendApplyRequest(server string, defs []schema.Definition) error {
	reqBody := jobdef.ApplyRequest{Definitions: defs}
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return err
	}

	resp, err := http.Post(server+"/v1/jobdefs/apply", "application/json", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("apply failed: %s", strings.TrimSpace(string(body)))
	}

	return nil
}

func isYAML(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return ext == ".yaml" || ext == ".yml"
}

func isBlankDefinition(def *schema.Definition) bool {
	if def == nil {
		return true
	}
	if strings.TrimSpace(def.Metadata.Alias) != "" {
		return false
	}
	if def.APIVersion != "" || def.Kind != "" {
		return false
	}
	if def.Trigger.Type != "" || len(def.Steps) > 0 || len(def.Callbacks) > 0 {
		return false
	}
	return true
}
