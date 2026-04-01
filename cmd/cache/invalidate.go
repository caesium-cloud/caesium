package cache

import (
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/spf13/cobra"
)

var (
	invalidateJobID  string
	invalidateTask   string
	invalidateServer string
)

var invalidateCmd = &cobra.Command{
	Use:   "invalidate",
	Short: "Invalidate cache entries for a job or task",
	RunE: func(cmd *cobra.Command, args []string) error {
		if invalidateJobID == "" {
			return fmt.Errorf("--job-id is required")
		}

		server := strings.TrimSuffix(invalidateServer, "/")
		url := fmt.Sprintf("%s/v1/jobs/%s/cache", server, invalidateJobID)
		if invalidateTask != "" {
			url = fmt.Sprintf("%s/%s", url, invalidateTask)
		}

		req, err := http.NewRequestWithContext(cmd.Context(), http.MethodDelete, url, nil)
		if err != nil {
			return err
		}

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()

		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode >= http.StatusBadRequest {
			return fmt.Errorf("cache invalidate failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
		}

		if invalidateTask != "" {
			cmd.Printf("Cache invalidated for task %q in job %s\n", invalidateTask, invalidateJobID)
		} else {
			cmd.Printf("Cache invalidated for job %s\n", invalidateJobID)
		}
		return nil
	},
}

func init() {
	invalidateCmd.Flags().StringVar(&invalidateJobID, "job-id", "", "Job ID to invalidate cache for (required)")
	invalidateCmd.Flags().StringVar(&invalidateTask, "task", "", "Task name to invalidate (optional, omit to invalidate all)")
	invalidateCmd.Flags().StringVar(&invalidateServer, "server", "http://localhost:8080", "Caesium server base URL")

	Cmd.AddCommand(invalidateCmd)
}
