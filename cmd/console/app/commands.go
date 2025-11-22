package app

import (
	"context"
	"net/url"

	"github.com/caesium-cloud/caesium/cmd/console/api"
	tea "github.com/charmbracelet/bubbletea"
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

func fetchData(client *api.Client) tea.Cmd {
	return func() tea.Msg {
		params := url.Values{}
		params.Set("order_by", "created_at desc")

		jobs, err := client.Jobs().List(context.Background(), params)
		if err != nil {
			return errMsg(err)
		}

		triggers, err := client.Triggers().List(context.Background(), url.Values{})
		if err != nil {
			return errMsg(err)
		}

		atoms, err := client.Atoms().List(context.Background(), url.Values{})
		if err != nil {
			return errMsg(err)
		}

		return dataLoadedMsg{
			jobs:     jobs,
			triggers: triggers,
			atoms:    atoms,
		}
	}
}

type jobDetailLoadedMsg struct {
	detail *api.JobDetail
}

type jobDetailErrMsg struct {
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

type dataLoadedMsg struct {
	jobs     []api.Job
	triggers []api.Trigger
	atoms    []api.Atom
}

type errMsg error
