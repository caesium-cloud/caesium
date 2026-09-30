package system

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/caesium-cloud/caesium/cmd/cliutil"
	"github.com/spf13/cobra"
)

// node mirrors the GET /v1/system/nodes entries this command prints.
type node struct {
	ID           string `json:"id"`
	Address      string `json:"address"`
	Role         string `json:"role"`
	Leader       bool   `json:"leader"`
	Reachability string `json:"reachability"`
}

// member mirrors a member in a removal answer.
type member struct {
	ID      string `json:"id"`
	Address string `json:"address"`
	Role    string `json:"role"`
}

type removal struct {
	Status  string   `json:"status"`
	Removed member   `json:"removed"`
	Members []member `json:"members"`
}

type refusal struct {
	Status    string `json:"status"`
	Reason    string `json:"reason"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

func newListCommand() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List cluster members with their dqlite node IDs, roles and observed reachability",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			resp, err := request(cmd, http.MethodGet, "/v1/system/nodes")
			if err != nil {
				return err
			}
			if resp.status != http.StatusOK {
				return fmt.Errorf("listing nodes failed (HTTP %d): %s", resp.status, serverMessage(resp.body))
			}
			if jsonOut {
				return cliutil.WritePrettyJSON(cmd, resp.body, "system nodes")
			}
			var nodes []node
			if err := json.Unmarshal(resp.body, &nodes); err != nil {
				return fmt.Errorf("system nodes response was not valid JSON: %w", err)
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			_, _ = fmt.Fprintln(w, "ID\tADDRESS\tROLE\tREACHABILITY\tLEADER")
			for _, n := range nodes {
				id := n.ID
				if id == "" {
					id = "-"
				}
				_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%t\n", id, n.Address, n.Role, n.Reachability, n.Leader)
			}
			return w.Flush()
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Print the server's JSON answer")
	return cmd
}

func newRemoveCommand() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "remove <node-id>",
		Short: "Remove a stale (non-voting, unreachable) dqlite member from the cluster configuration",
		Long: `Remove the raft entry a lost member leaves behind after its replacement joined
with an empty volume. The server refuses, and changes nothing, unless the member
is a spare or standby, is neither the serving node nor the leader, does not
answer at its address, and every one of at least three voters answers.

A refusal exits non-zero with the reason on stderr. With --json, stdout carries
the server's answer either way: {"status":"removed",...} or
{"status":"refused","reason":...,"retryable":...}. A retryable refusal
(configuration_change_in_progress, no_leader, or a lost voter not yet demoted)
may succeed if the same command is run again later.`,
		Example: `  caesium system nodes list
  caesium system nodes remove 3297041220608546238 --json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			raw := strings.TrimSpace(args[0])
			id, err := strconv.ParseUint(raw, 10, 64)
			if err != nil || id == 0 {
				return fmt.Errorf("%q is not a dqlite node ID; use an ID from `caesium system nodes list`", raw)
			}
			resp, err := request(cmd, http.MethodDelete, "/v1/system/nodes/"+strconv.FormatUint(id, 10))
			if err != nil {
				return err
			}
			if jsonOut && json.Valid(resp.body) {
				if err := cliutil.WritePrettyJSON(cmd, resp.body, "member removal"); err != nil {
					return err
				}
			}
			if resp.status == http.StatusOK {
				if jsonOut {
					return nil
				}
				var done removal
				if err := json.Unmarshal(resp.body, &done); err != nil {
					return fmt.Errorf("member removal response was not valid JSON: %w", err)
				}
				_, err := fmt.Fprintf(cmd.OutOrStdout(), "Removed dqlite member %s (%s, was %s); the configuration now has %d members.\n",
					done.Removed.ID, done.Removed.Address, done.Removed.Role, len(done.Members))
				return err
			}
			var refused refusal
			if err := json.Unmarshal(resp.body, &refused); err == nil && refused.Reason != "" {
				msg := fmt.Sprintf("refused to remove dqlite member %s (HTTP %d, %s): %s", raw, resp.status, refused.Reason, refused.Message)
				if refused.Retryable {
					msg += " (retryable: run the command again later)"
				}
				return errors.New(msg)
			}
			return fmt.Errorf("member removal failed (HTTP %d): %s", resp.status, serverMessage(resp.body))
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Print the server's JSON answer on stdout, including a refusal")
	return cmd
}
