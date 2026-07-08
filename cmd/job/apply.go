package job

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	jobdefsvc "github.com/caesium-cloud/caesium/api/rest/service/jobdef"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

var (
	applyPaths              []string
	applyServer             string
	applyForce              bool
	applyPrune              bool
	applyProvenanceSourceID string
	applyProvenanceRepo     string
	applyProvenanceRef      string
	applyProvenanceCommit   string
	applyProvenancePath     string
	applyAllowBreaking      string
	applyReason             string
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
			if _, err := fmt.Fprintln(cmd.OutOrStdout(), "No job definitions found."); err != nil {
				return err
			}
			return nil
		}

		allowBreaking, err := allowBreakingFromFlags()
		if err != nil {
			return err
		}

		resp, err := sendApplyRequest(cmd.Context(), strings.TrimSuffix(applyServer, "/"), defs, applyForce, applyPrune, applyProvenanceFromFlags(), allowBreaking)
		if err != nil {
			return err
		}
		for _, warning := range resp.ContractWarnings {
			if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "Warning: %s\n", warning.Message); err != nil {
				return err
			}
		}

		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Applied %d job definition(s)\n", len(defs)); err != nil {
			return err
		}
		return nil
	},
}

func init() {
	applyCmd.Flags().StringSliceVarP(&applyPaths, "path", "p", nil, "Paths to job definition files or directories (default: current directory)")
	applyCmd.Flags().StringVar(&applyServer, "server", "http://localhost:8080", "Caesium server base URL")
	applyCmd.Flags().BoolVar(&applyForce, "force", false, "Override provenance ownership checks when applying definitions")
	applyCmd.Flags().BoolVar(&applyPrune, "prune", false, "Retire active jobs that are missing from the supplied path set")
	applyCmd.Flags().StringVar(&applyProvenanceSourceID, "provenance-source-id", "", "Record the source ID that produced the applied definitions")
	applyCmd.Flags().StringVar(&applyProvenanceRepo, "provenance-repo", "", "Record the repository URL that produced the applied definitions")
	applyCmd.Flags().StringVar(&applyProvenanceRef, "provenance-ref", "", "Record the repository ref that produced the applied definitions")
	applyCmd.Flags().StringVar(&applyProvenanceCommit, "provenance-commit", "", "Record the commit that produced the applied definitions")
	applyCmd.Flags().StringVar(&applyProvenancePath, "provenance-path", "", "Record the manifest path that produced the applied definitions")
	applyCmd.Flags().StringVar(&applyAllowBreaking, "allow-breaking", "", "Acknowledge an intentional breaking contract change (grammar: dataset=<name>)")
	applyCmd.Flags().StringVar(&applyReason, "reason", "", "Reason recorded with --allow-breaking")
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

func applyProvenanceFromFlags() *jobdefsvc.ApplyProvenance {
	prov := &jobdefsvc.ApplyProvenance{
		SourceID: strings.TrimSpace(applyProvenanceSourceID),
		Repo:     strings.TrimSpace(applyProvenanceRepo),
		Ref:      strings.TrimSpace(applyProvenanceRef),
		Commit:   strings.TrimSpace(applyProvenanceCommit),
		Path:     strings.TrimSpace(applyProvenancePath),
	}
	if prov.SourceID == "" && prov.Repo == "" && prov.Ref == "" && prov.Commit == "" && prov.Path == "" {
		return nil
	}
	return prov
}

func allowBreakingFromFlags() (*jobdefsvc.AllowBreakingRequest, error) {
	raw := strings.TrimSpace(applyAllowBreaking)
	reason := strings.TrimSpace(applyReason)
	if raw == "" {
		if reason != "" {
			return nil, errors.New("--reason requires --allow-breaking dataset=<name>")
		}
		return nil, nil
	}
	key, value, ok := strings.Cut(raw, "=")
	if !ok || strings.TrimSpace(key) != "dataset" || strings.TrimSpace(value) == "" {
		return nil, errors.New(`--allow-breaking must use grammar dataset=<name>`)
	}
	return &jobdefsvc.AllowBreakingRequest{
		Dataset: strings.TrimSpace(value),
		Reason:  reason,
	}, nil
}

func sendApplyRequest(ctx context.Context, server string, defs []schema.Definition, force, prune bool, provenance *jobdefsvc.ApplyProvenance, allowBreaking *jobdefsvc.AllowBreakingRequest) (*jobdefsvc.ApplyResponse, error) {
	reqBody := jobdefsvc.ApplyRequest{
		Definitions:   defs,
		Force:         force,
		Prune:         prune,
		Provenance:    provenance,
		AllowBreaking: allowBreaking,
	}
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server+"/v1/jobdefs/apply", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= http.StatusBadRequest {
		return nil, fmt.Errorf("apply failed: %s", strings.TrimSpace(string(body)))
	}

	var applyResp jobdefsvc.ApplyResponse
	if len(bytes.TrimSpace(body)) > 0 {
		if err := json.Unmarshal(body, &applyResp); err != nil {
			return nil, err
		}
	}

	return &applyResp, nil
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
