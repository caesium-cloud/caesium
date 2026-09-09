package dataset

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/cmd/cliutil"
	runstore "github.com/caesium-cloud/caesium/internal/run"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
)

var errNoActiveHold = errors.New("no active hold exists")

var releaseCmd = newReleaseCommand()

func newReleaseCommand() *cobra.Command {
	var releaseReason string
	var releaseTolerate []string
	var releaseJSON bool
	cmd := &cobra.Command{
		Use:   "release <name> --reason <reason> [--namespace <ns>]",
		Short: "Release the active hold on an exact dataset identity",
		Long:  "Release the active hold on a dataset. Slashes remain part of the dataset name; use --namespace for a separate namespace. Tolerance windows are recorded as advisory evidence and do not suppress future breaches.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			namespace, name, err := splitDatasetRef(args[0])
			if err != nil {
				return err
			}
			reason := strings.TrimSpace(releaseReason)
			if reason == "" {
				return fmt.Errorf("--reason is required")
			}
			tolerances, err := parseReleaseTolerances(releaseTolerate)
			if err != nil {
				return err
			}
			holdID, err := fetchActiveHold(cmd, namespace, name)
			if err != nil {
				return err
			}
			payload, err := json.Marshal(struct {
				Reason   string            `json:"reason"`
				Tolerate map[string]string `json:"tolerate,omitempty"`
			}{reason, tolerances})
			if err != nil {
				return err
			}
			body, err := request(cmd, http.MethodPost, serverBase()+"/v1/datasets/holds/"+url.PathEscape(holdID)+"/release", bytes.NewReader(payload))
			if err != nil {
				var statusErr *httpStatusError
				if errors.As(err, &statusErr) && statusErr.StatusCode == http.StatusConflict {
					return releaseConflict(cmd, namespace, name, holdID, err)
				}
				return err
			}
			if releaseJSON {
				return cliutil.WritePrettyJSON(cmd, body, "dataset release")
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Released hold %s on dataset %s/%s\n", holdID, displayNamespace(namespace), name)
			return err
		},
	}
	cmd.Flags().StringVar(&releaseReason, "reason", "", "Required justification for releasing the hold")
	cmd.Flags().StringArrayVar(&releaseTolerate, "tolerate", nil, "Record an advisory assertion window (<assertion>=<duration>); repeatable")
	cmd.Flags().BoolVar(&releaseJSON, "json", false, "Print JSON")
	return cmd
}

func fetchActiveHold(cmd *cobra.Command, namespace, name string) (string, error) {
	// The exact filtered feed also covers holds on emitted datasets that have
	// no registered DatasetState and therefore cannot be read through Detail.
	params := url.Values{"status": {"active"}, "namespace": {namespace}, "name": {name}}
	body, err := request(cmd, http.MethodGet, serverBase()+"/v1/datasets/holds?"+params.Encode(), nil)
	if err != nil {
		return "", err
	}
	return resolveActiveHold(body, namespace, name)
}

func releaseConflict(cmd *cobra.Command, namespace, name, holdID string, conflict error) error {
	currentID, err := fetchActiveHold(cmd, namespace, name)
	identity := displayNamespace(namespace) + "/" + name
	if errors.Is(err, errNoActiveHold) {
		return fmt.Errorf("%w; no active hold is currently recorded for dataset %s", conflict, identity)
	}
	if err != nil {
		return fmt.Errorf("%w; current hold state for dataset %s could not be verified: %v", conflict, identity, err)
	}
	if currentID != holdID {
		return fmt.Errorf("%w; dataset %s is still held by newer active hold %s; inspect it before retrying release", conflict, identity, currentID)
	}
	return fmt.Errorf("%w; dataset %s is still held by active hold %s; inspect it before retrying release", conflict, identity, currentID)
}

func resolveActiveHold(body []byte, namespace, name string) (string, error) {
	var result holdsResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("dataset holds response was not valid JSON: %w", err)
	}
	identity := displayNamespace(namespace) + "/" + name
	if result.Total == 0 && len(result.Holds) == 0 {
		return "", fmt.Errorf("%w for dataset %s (namespace %q; use --namespace to target another namespace)", errNoActiveHold, identity, displayNamespace(namespace))
	}
	if result.Total != 1 || len(result.Holds) != 1 {
		return "", fmt.Errorf("could not resolve a unique active hold for dataset %s", identity)
	}
	hold := result.Holds[0]
	if hold.Namespace != namespace || hold.Name != name || hold.Status != "active" {
		return "", fmt.Errorf("active hold response did not match dataset %s", identity)
	}
	id, err := uuid.Parse(hold.ID)
	if err != nil || id == uuid.Nil {
		return "", fmt.Errorf("active hold for dataset %s has an invalid id", identity)
	}
	return id.String(), nil
}

func parseReleaseTolerances(entries []string) (map[string]string, error) {
	result := make(map[string]string, len(entries))
	for _, entry := range entries {
		assertion, duration, ok := strings.Cut(entry, "=")
		assertion, duration = strings.TrimSpace(assertion), strings.TrimSpace(duration)
		if !ok {
			return nil, fmt.Errorf("--tolerate requires <assertion>=<duration>")
		}
		// The enforceable kinds only: `unavailable` never opens a hold, so a
		// tolerance window over it would suppress nothing.
		switch assertion {
		case runstore.AssertionMin, runstore.AssertionMax, runstore.AssertionDeltaFromBaseline, runstore.AssertionMaxLag, runstore.AssertionMissing:
		default:
			return nil, fmt.Errorf("--tolerate: %q is not an assertion kind (min, max, deltaFromBaseline, maxLag, missing)", assertion)
		}
		d, err := time.ParseDuration(duration)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("--tolerate %s: duration must be positive (e.g. 24h)", assertion)
		}
		if _, exists := result[assertion]; exists {
			return nil, fmt.Errorf("--tolerate repeats assertion %s", assertion)
		}
		result[assertion] = duration
	}
	return result, nil
}
