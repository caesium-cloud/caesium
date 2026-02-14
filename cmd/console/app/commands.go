package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/cmd/console/api"
	tea "github.com/charmbracelet/bubbletea"
)

const (
	maxFetchAttempts = 3
	retryBackoffStep = 150 * time.Millisecond
)

func fetchAtomDetail(client *api.Client, atomID string) tea.Cmd {
	return func() tea.Msg {
		atom, err := client.Atoms().Get(context.Background(), atomID)
		if err != nil {
			return atomDetailErrMsg{id: atomID, err: err}
		}
		return atomDetailLoadedMsg{id: atomID, atom: atom}
	}
}

func triggerJob(client *api.Client, jobID string) tea.Cmd {
	return func() tea.Msg {
		run, err := client.Runs().Trigger(context.Background(), jobID)
		if err != nil {
			return jobTriggerErrMsg{jobID: jobID, err: err}
		}
		return jobTriggeredMsg{jobID: jobID, run: run}
	}
}

func rerunJob(client *api.Client, jobID, runID string) tea.Cmd {
	return func() tea.Msg {
		// A dedicated rerun endpoint is not available yet; start a new run for the same job.
		run, err := client.Runs().Trigger(context.Background(), jobID)
		if err != nil {
			return jobTriggerErrMsg{jobID: jobID, err: err}
		}
		return jobTriggeredMsg{jobID: jobID, run: run}
	}
}

func fetchJobDetail(client *api.Client, jobID string, includeDAG bool) tea.Cmd {
	return func() tea.Msg {
		var opts *api.JobDetailOptions
		if includeDAG {
			opts = &api.JobDetailOptions{IncludeDAG: true}
		}

		detail, err := client.Jobs().Detail(context.Background(), jobID, opts)
		if err != nil {
			return jobDetailErrMsg{err: err}
		}

		return jobDetailLoadedMsg{detail: detail}
	}
}

func fetchLatestRun(client *api.Client, jobID string) tea.Cmd {
	return func() tea.Msg {
		detail, err := client.Jobs().Detail(context.Background(), jobID, nil)
		if err != nil {
			return jobStatusErrMsg{jobID: jobID, err: err}
		}
		return jobStatusLoadedMsg{jobID: jobID, run: detail.LatestRun}
	}
}

func fetchJobStatuses(client *api.Client, jobIDs []string) tea.Cmd {
	return func() tea.Msg {
		results := make(map[string]*api.Run, len(jobIDs))
		for _, id := range jobIDs {
			detail, err := client.Jobs().Detail(context.Background(), id, nil)
			if err != nil {
				continue
			}
			results[id] = detail.LatestRun
		}
		return jobStatusBatchMsg{statuses: results}
	}
}

func fetchRuns(client *api.Client, jobID string) tea.Cmd {
	return func() tea.Msg {
		runs, err := client.Runs().List(context.Background(), jobID, url.Values{})
		if err != nil {
			return runsErrMsg{jobID: jobID, err: err}
		}
		return runsLoadedMsg{jobID: jobID, runs: runs}
	}
}

func fetchData(client *api.Client) tea.Cmd {
	return func() tea.Msg {
		if client == nil {
			return dataLoadErrMsg{err: fmt.Errorf("api client not configured")}
		}
		startedAt := time.Now()
		healthCheckedAt := time.Now()
		healthStarted := time.Now()
		healthErr := client.Ping(context.Background())
		healthLatency := time.Since(healthStarted)
		healthOK := healthErr == nil

		params := url.Values{}
		params.Set("order_by", "created_at desc")

		attempts := 0
		for attempts < maxFetchAttempts {
			attempts++
			jobs, err := client.Jobs().List(context.Background(), params)
			if err != nil {
				err = fmt.Errorf("fetch jobs: %w", err)
				if attempts >= maxFetchAttempts {
					return dataLoadErrMsg{
						err:             err,
						attempts:        attempts,
						fetchDuration:   time.Since(startedAt),
						healthCheckedAt: healthCheckedAt,
						healthLatency:   healthLatency,
						healthOK:        healthOK,
						healthErr:       healthErr,
					}
				}
				time.Sleep(time.Duration(attempts) * retryBackoffStep)
				continue
			}

			triggers, err := client.Triggers().List(context.Background(), url.Values{})
			if err != nil {
				err = fmt.Errorf("fetch triggers: %w", err)
				if attempts >= maxFetchAttempts {
					return dataLoadErrMsg{
						err:             err,
						attempts:        attempts,
						fetchDuration:   time.Since(startedAt),
						healthCheckedAt: healthCheckedAt,
						healthLatency:   healthLatency,
						healthOK:        healthOK,
						healthErr:       healthErr,
					}
				}
				time.Sleep(time.Duration(attempts) * retryBackoffStep)
				continue
			}

			atoms, err := client.Atoms().List(context.Background(), url.Values{})
			if err != nil {
				err = fmt.Errorf("fetch atoms: %w", err)
				if attempts >= maxFetchAttempts {
					return dataLoadErrMsg{
						err:             err,
						attempts:        attempts,
						fetchDuration:   time.Since(startedAt),
						healthCheckedAt: healthCheckedAt,
						healthLatency:   healthLatency,
						healthOK:        healthOK,
						healthErr:       healthErr,
					}
				}
				time.Sleep(time.Duration(attempts) * retryBackoffStep)
				continue
			}

			return dataLoadedMsg{
				jobs:            jobs,
				triggers:        triggers,
				atoms:           atoms,
				attempts:        attempts,
				fetchDuration:   time.Since(startedAt),
				healthCheckedAt: healthCheckedAt,
				healthLatency:   healthLatency,
				healthOK:        healthOK,
				healthErr:       healthErr,
			}
		}

		return dataLoadErrMsg{
			err:             fmt.Errorf("failed to fetch data"),
			attempts:        attempts,
			fetchDuration:   time.Since(startedAt),
			healthCheckedAt: healthCheckedAt,
			healthLatency:   healthLatency,
			healthOK:        healthOK,
			healthErr:       healthErr,
		}
	}
}

type jobDetailLoadedMsg struct {
	detail *api.JobDetail
}

type jobDetailErrMsg struct {
	err error
}

type jobStatusLoadedMsg struct {
	jobID string
	run   *api.Run
}

type jobStatusErrMsg struct {
	jobID string
	err   error
}

type jobStatusBatchMsg struct {
	statuses map[string]*api.Run
}

type logsOpenedMsg struct {
	ctx    context.Context
	reader io.ReadCloser
}

type logChunkMsg struct {
	ctx    context.Context
	reader io.ReadCloser
	data   string
}

type logsClosedMsg struct {
	err error
}

type logsExportedMsg struct {
	path string
}

type logsExportErrMsg struct {
	err error
}

type atomDetailLoadedMsg struct {
	id   string
	atom *api.Atom
}

type atomDetailErrMsg struct {
	id  string
	err error
}

type jobTriggeredMsg struct {
	jobID string
	run   *api.Run
}

type jobTriggerErrMsg struct {
	jobID string
	err   error
}

type runsLoadedMsg struct {
	jobID string
	runs  []api.Run
}

type runsErrMsg struct {
	jobID string
	err   error
}

type healthCheckedMsg struct {
	ok        bool
	latency   time.Duration
	checkedAt time.Time
	err       error
}

type dataLoadedMsg struct {
	jobs            []api.Job
	triggers        []api.Trigger
	atoms           []api.Atom
	attempts        int
	fetchDuration   time.Duration
	healthCheckedAt time.Time
	healthLatency   time.Duration
	healthOK        bool
	healthErr       error
}

type dataLoadErrMsg struct {
	err             error
	attempts        int
	fetchDuration   time.Duration
	healthCheckedAt time.Time
	healthLatency   time.Duration
	healthOK        bool
	healthErr       error
}

type statsLoadedMsg struct {
	stats *api.StatsResponse
}

type statsErrMsg struct {
	err error
}

type errMsg error

func fetchStats(client *api.Client) tea.Cmd {
	return func() tea.Msg {
		stats, err := client.Stats().Get(context.Background())
		if err != nil {
			return statsErrMsg{err: err}
		}
		return statsLoadedMsg{stats: stats}
	}
}

func pingHealth(client *api.Client) tea.Cmd {
	return func() tea.Msg {
		started := time.Now()
		err := client.Ping(context.Background())
		return healthCheckedMsg{
			ok:        err == nil,
			latency:   time.Since(started),
			checkedAt: time.Now(),
			err:       err,
		}
	}
}

func openLogStream(ctx context.Context, client *api.Client, jobID, runID, taskID string, since time.Time) tea.Cmd {
	return func() tea.Msg {
		reader, err := client.Runs().Logs(ctx, jobID, runID, taskID, since)
		if err != nil {
			return logsClosedMsg{err: err}
		}
		return logsOpenedMsg{ctx: ctx, reader: reader}
	}
}

func readLogChunk(ctx context.Context, reader io.ReadCloser) tea.Cmd {
	return func() tea.Msg {
		if reader == nil {
			return logsClosedMsg{}
		}

		select {
		case <-ctx.Done():
			return logsClosedMsg{}
		default:
		}

		buf := make([]byte, 2048)
		n, err := reader.Read(buf)

		if n > 0 {
			return logChunkMsg{ctx: ctx, reader: reader, data: string(buf[:n])}
		}

		if err != nil {
			if errors.Is(err, io.EOF) {
				return logsClosedMsg{}
			}
			return logsClosedMsg{err: err}
		}

		return logsClosedMsg{}
	}
}

func exportLogSnippet(content, runID, taskID, filter string) tea.Cmd {
	return func() tea.Msg {
		content = strings.TrimSpace(content)
		if content == "" {
			return logsExportErrMsg{err: fmt.Errorf("no log content to export")}
		}

		file, err := os.CreateTemp("", "caesium-log-*.txt")
		if err != nil {
			return logsExportErrMsg{err: err}
		}

		defer func() {
			_ = file.Close()
		}()

		header := []string{
			fmt.Sprintf("exported_at=%s", time.Now().Format(time.RFC3339Nano)),
			fmt.Sprintf("run_id=%s", strings.TrimSpace(runID)),
			fmt.Sprintf("task_id=%s", strings.TrimSpace(taskID)),
		}
		if q := strings.TrimSpace(filter); q != "" {
			header = append(header, fmt.Sprintf("filter=%q", q))
		}
		header = append(header, "")
		header = append(header, content)

		if _, err := file.WriteString(strings.Join(header, "\n") + "\n"); err != nil {
			return logsExportErrMsg{err: err}
		}

		return logsExportedMsg{path: file.Name()}
	}
}
