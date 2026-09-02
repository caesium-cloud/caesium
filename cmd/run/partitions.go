package run

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/caesium-cloud/caesium/cmd/cliutil"
	"github.com/spf13/cobra"
)

var (
	partitionsJobID  string
	partitionsTask   string
	partitionsStatus string
	partitionsJSON   bool
	partitionsLimit  int
	partitionsOffset int
	partitionsServer string
	partitionsAPIKey string

	partitionsHTTPClient = &http.Client{Timeout: cliutil.DefaultHTTPTimeout}
)

// partitionsPageSize is the per-request page size used when the CLI pages a
// group to completion. The server caps `limit` at 1000; 500 keeps each response
// well inside that while halving the round trips of the server's own default.
const partitionsPageSize = 500

// partitionsMaxRows bounds an unattended full-page walk. A fan-out group is
// capped at 10k instances, so a walk that has collected more than that is
// following a server that never terminates its `next_offset` chain rather than
// reading a real group — stop and say so instead of looping forever.
const partitionsMaxRows = 10_000

// partitionRow mirrors the server's partitionRow JSON
// (api/rest/controller/job/run/partitions.go) so the CLI can render a table.
// --json prints the server's rows VERBATIM (as json.RawMessage, re-indented),
// so fields this struct does not model — task_run_id, started_at, completed_at,
// anything added server-side later — still round-trip losslessly.
type partitionRow struct {
	Value       string   `json:"value"`
	Index       int      `json:"index"`
	Status      string   `json:"status"`
	Attempt     int      `json:"attempt"`
	CacheHit    bool     `json:"cache_hit"`
	Duration    string   `json:"duration,omitempty"`
	Error       string   `json:"error,omitempty"`
	Fingerprint string   `json:"fingerprint,omitempty"`
	DependsOn   []string `json:"depends_on,omitempty"`
}

// partitionsPage is one response from the paginated list endpoint.
//
// NextOffset is a *int on purpose: the server sends `next_offset: null` for the
// LAST page, and a plain int would decode that as 0 — indistinguishable from
// "start again at the beginning", which is an infinite loop. Limit/Offset are
// echoed by the server; the CLI reads them only to report what it asked for.
type partitionsPage struct {
	Partitions   []json.RawMessage `json:"partitions"`
	Total        int               `json:"total"`
	Limit        int               `json:"limit"`
	Offset       int               `json:"offset"`
	NextOffset   *int              `json:"next_offset"`
	StatusCounts map[string]int    `json:"status_counts,omitempty"`
}

// partitionsResult is the accumulated view the command renders, whether it came
// from one explicit page or from walking every page.
type partitionsResult struct {
	Rows         []json.RawMessage
	Total        int
	StatusCounts map[string]int
	// NextOffset is set only in explicit --limit/--offset mode, where the caller
	// owns the paging and needs the cursor to continue.
	NextOffset *int
}

// partitionsJSONOutput is the --json document. It is assembled by the CLI rather
// than echoed from one response because the default mode concatenates every
// page: printing page one's envelope would claim a complete list it does not
// contain.
type partitionsJSONOutput struct {
	Partitions   []json.RawMessage `json:"partitions"`
	Total        int               `json:"total"`
	StatusCounts map[string]int    `json:"status_counts,omitempty"`
	NextOffset   *int              `json:"next_offset,omitempty"`
}

var partitionsCmd = &cobra.Command{
	Use:   "partitions <run-id> --job-id <job-id> --task <name>",
	Short: "List fan-out partition instances for a task in a run",
	Long: "List the materialized TaskRun instances of a fanned step. Prints a " +
		"human-readable table by default (value, index, status, attempt, " +
		"cache-hit, duration, error, plus fingerprint/depends_on columns when " +
		"any instance carries them), or the server's machine-readable JSON " +
		"rows on stdout with --json.\n\n" +
		"The listing endpoint is paginated. By default every page is walked so " +
		"the table and --json always describe the WHOLE group; pass --limit " +
		"(and optionally --offset) to fetch exactly one page, in which case a " +
		"truncation note is written to stderr and --json carries next_offset.",
	Args: cobra.ExactArgs(1),
	RunE: runPartitions,
}

func runPartitions(cmd *cobra.Command, args []string) error {
	if partitionsJobID == "" || partitionsTask == "" {
		return fmt.Errorf("--job-id and --task are required")
	}
	runID := strings.TrimSpace(args[0])

	// Past argument validation: everything below is I/O against the server, and
	// a transport failure or a non-2xx response is not a usage error. Cobra
	// prints the full usage block after ANY error returned from RunE, which
	// buries the one line that is actually actionable (e.g. a 409 explaining
	// that only a failed partition can be retried). It also writes that
	// block through OutOrStderr(), so a caller that has set the command's out
	// writer gets usage on STDOUT — corrupting --json.
	//
	// Same convention as cmd/verify: flipped here rather than declared on the
	// command, so a missing or malformed flag above still gets its usage block.
	cmd.SilenceUsage = true

	// An explicit --limit/--offset hands paging to the caller; anything else
	// means "give me the group", which is a walk.
	singlePage := cmd.Flags().Changed("limit") || cmd.Flags().Changed("offset")

	result, err := collectPartitions(cmd, partitionsJobID, runID, partitionsTask, partitionsStatus, singlePage)
	if err != nil {
		return err
	}

	if result.NextOffset != nil {
		// stderr, never stdout: --json output has to stay machine-parseable.
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
			"note: showing %d of %d partitions; continue with --offset %d\n",
			len(result.Rows), result.Total, *result.NextOffset)
	}

	if partitionsJSON {
		doc := partitionsJSONOutput{
			Partitions:   result.Rows,
			Total:        result.Total,
			StatusCounts: result.StatusCounts,
			NextOffset:   result.NextOffset,
		}
		encoded, err := json.Marshal(doc)
		if err != nil {
			return fmt.Errorf("encoding partitions response: %w", err)
		}
		return cliutil.WritePrettyJSON(cmd, encoded, "partitions response")
	}

	rows := make([]partitionRow, 0, len(result.Rows))
	for _, raw := range result.Rows {
		var row partitionRow
		if err := json.Unmarshal(raw, &row); err != nil {
			return fmt.Errorf("partitions response was not valid JSON: %w", err)
		}
		rows = append(rows, row)
	}
	renderPartitionsTable(cmd, rows, result.Total)
	return nil
}

// collectPartitions reads the listing endpoint. With singlePage it issues one
// request honoring --limit/--offset and reports the server's next_offset;
// otherwise it follows next_offset until the server reports the end, so a group
// larger than one page is never silently truncated to its first page (the
// defect this replaced: the CLI read page one and called it the group).
func collectPartitions(cmd *cobra.Command, jobID, runID, task, status string, singlePage bool) (*partitionsResult, error) {
	result := &partitionsResult{Rows: make([]json.RawMessage, 0, partitionsPageSize)}

	limit := partitionsPageSize
	offset := 0
	if singlePage {
		limit = partitionsLimit
		offset = partitionsOffset
	}

	for {
		body, err := fetchPartitions(cmd, jobID, runID, task, status, limit, offset)
		if err != nil {
			return nil, err
		}
		var page partitionsPage
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, fmt.Errorf("partitions response was not valid JSON: %w", err)
		}

		result.Rows = append(result.Rows, page.Partitions...)
		result.Total = page.Total
		if page.StatusCounts != nil {
			result.StatusCounts = page.StatusCounts
		}

		if singlePage {
			result.NextOffset = page.NextOffset
			return result, nil
		}

		// A nil next_offset is the end of the list. It is also what a server
		// that predates pagination sends (no such key at all), which correctly
		// degrades to the single-response behavior this command used to have.
		if page.NextOffset == nil {
			return result, nil
		}
		// A cursor that does not advance would spin forever; treat it as the end
		// and let the total-vs-collected mismatch be visible in the output.
		if *page.NextOffset <= offset {
			return result, nil
		}
		offset = *page.NextOffset

		if len(result.Rows) >= partitionsMaxRows {
			return nil, fmt.Errorf(
				"partitions listing did not terminate after %d rows (server keeps advancing next_offset); "+
					"re-run with --limit/--offset to page manually", len(result.Rows))
		}
	}
}

func fetchPartitions(cmd *cobra.Command, jobID, runID, task, status string, limit, offset int) ([]byte, error) {
	server := strings.TrimSuffix(partitionsServer, "/")
	endpoint := fmt.Sprintf("%s/v1/jobs/%s/runs/%s/tasks/%s/partitions",
		server, url.PathEscape(jobID), url.PathEscape(runID), url.PathEscape(task))

	query := url.Values{}
	if status != "" {
		query.Set("status", status)
	}
	if limit > 0 {
		query.Set("limit", strconv.Itoa(limit))
	}
	if offset > 0 {
		query.Set("offset", strconv.Itoa(offset))
	}
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}

	req, err := http.NewRequestWithContext(cmd.Context(), http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	if apiKey := resolveRunDiffAPIKey(cmd, partitionsAPIKey); apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := partitionsHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading partitions response: %w", err)
	}
	if resp.StatusCode >= http.StatusBadRequest {
		return nil, fmt.Errorf("partitions failed (%d): %s", resp.StatusCode, replayErrorMessage(body))
	}
	return body, nil
}

func renderPartitionsTable(cmd *cobra.Command, rows []partitionRow, total int) {
	out := cmd.OutOrStdout()
	if len(rows) == 0 {
		_, _ = fmt.Fprintln(out, "No partitions found.")
		return
	}

	var showFingerprint, showDependsOn bool
	for _, r := range rows {
		if r.Fingerprint != "" {
			showFingerprint = true
		}
		if len(r.DependsOn) > 0 {
			showDependsOn = true
		}
	}

	// The count line makes a paged listing self-describing: "5 of 12" says the
	// table is a window, "5 partitions" says it is the group.
	if total > len(rows) {
		_, _ = fmt.Fprintf(out, "Showing %d of %d partitions\n", len(rows), total)
	} else {
		_, _ = fmt.Fprintf(out, "%d partitions\n", len(rows))
	}

	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	header := "VALUE\tINDEX\tSTATUS\tATTEMPT\tCACHE HIT\tDURATION\tERROR"
	if showFingerprint {
		header += "\tFINGERPRINT"
	}
	if showDependsOn {
		header += "\tDEPENDS ON"
	}
	_, _ = fmt.Fprintln(tw, header)

	for _, r := range rows {
		line := fmt.Sprintf("%s\t%d\t%s\t%d\t%s\t%s\t%s",
			r.Value,
			r.Index,
			r.Status,
			r.Attempt,
			strconv.FormatBool(r.CacheHit),
			dashIfEmptyPartitions(r.Duration),
			dashIfEmptyPartitions(r.Error),
		)
		if showFingerprint {
			line += "\t" + dashIfEmptyPartitions(r.Fingerprint)
		}
		if showDependsOn {
			line += "\t" + dashIfEmptyPartitions(strings.Join(r.DependsOn, ","))
		}
		_, _ = fmt.Fprintln(tw, line)
	}
	_ = tw.Flush()
}

func dashIfEmptyPartitions(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func init() {
	partitionsCmd.Flags().StringVar(&partitionsJobID, "job-id", "", "Job ID (required)")
	partitionsCmd.Flags().StringVar(&partitionsTask, "task", "", "Task ID or name (required)")
	partitionsCmd.Flags().StringVar(&partitionsStatus, "status", "", "Filter by instance status")
	partitionsCmd.Flags().BoolVar(&partitionsJSON, "json", false, "Write the server's machine-readable JSON to stdout")
	partitionsCmd.Flags().IntVar(&partitionsLimit, "limit", 0, "Fetch a single page of at most N instances instead of the whole group (max 1000)")
	partitionsCmd.Flags().IntVar(&partitionsOffset, "offset", 0, "Start a single page at this instance offset (implies single-page mode)")
	partitionsCmd.Flags().StringVar(&partitionsServer, "server", "http://localhost:8080", "Caesium server base URL")
	partitionsCmd.Flags().StringVar(&partitionsAPIKey, "api-key", "", "API key for authentication (prefer "+runDiffAPIKeyEnvVar+"; --api-key is visible in process listings)")
	Cmd.AddCommand(partitionsCmd)
}
