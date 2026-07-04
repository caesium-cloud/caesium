package dataset

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/caesium-cloud/caesium/cmd/cliutil"
	"github.com/spf13/cobra"
)

var advanceWatermark string

var advanceCmd = &cobra.Command{
	Use:   "advance <namespace.name> --watermark <value>",
	Short: "Manually advance a freshness dataset watermark",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		namespace, name, err := splitDatasetRef(args[0])
		if err != nil {
			return err
		}
		watermark := strings.TrimSpace(advanceWatermark)
		if watermark == "" {
			return fmt.Errorf("--watermark is required")
		}

		payload, err := json.Marshal(map[string]string{"watermark": watermark})
		if err != nil {
			return err
		}
		body, err := request(cmd, http.MethodPost, serverBase()+datasetPath(namespace, name)+"/advance", bytes.NewReader(payload))
		if err != nil {
			return err
		}
		return cliutil.WritePrettyJSON(cmd, body, "dataset advance")
	},
}

func init() {
	advanceCmd.Flags().StringVar(&advanceWatermark, "watermark", "", "Watermark value to record")
}
