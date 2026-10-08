package cache

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/caesium-cloud/caesium/internal/clihttp"
	"github.com/spf13/cobra"
)

var (
	invalidateJobID  string
	invalidateTask   string
	invalidateServer string
)

var invalidateCmd = &cobra.Command{
	Use:   "invalidate",
	Args:  cobra.NoArgs,
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

		body, status, readErr := clihttp.Exchange(cmd.Context(), http.DefaultClient, http.MethodDelete, url, nil, make(http.Header))
		if err := clihttp.ResponseError("cache invalidate", status, body, readErr); err != nil {
			return err
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
