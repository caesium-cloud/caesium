package app

import (
	"strings"
	"testing"

	"github.com/caesium-cloud/caesium/cmd/console/api"
	"github.com/charmbracelet/bubbles/table"
	"github.com/stretchr/testify/suite"
)

type TableSuite struct {
	suite.Suite
}

func TestTableSuite(t *testing.T) {
	suite.Run(t, new(TableSuite))
}

func (s *TableSuite) TestJobsToRowsIncludesMetadata() {
	jobs := []api.Job{{
		Alias: "nightly",
		ID:    "job-12345678-abcd",
		Labels: map[string]string{
			"env":  "prod",
			"team": "data",
		},
		Annotations: map[string]string{
			"owner": "ops",
		},
	}}

	rows := jobsToRows(jobs, nil, "")
	s.Require().Len(rows, 1)
	row := rows[0]
	s.Equal("nightly", row[0])
	s.Equal("-", row[1])                   // status (no run)
	s.Equal("-", row[2])                   // last run
	s.Equal("-", row[3])                   // duration
	s.Equal("env=prod, team=data", row[4]) // labels
	s.Equal("job-1234", row[5])            // short ID
}

func (s *TableSuite) TestFormatStringMapEmpty() {
	s.Equal("-", formatStringMap(nil))
	s.Equal("-", formatStringMap(map[string]string{}))
}

func (s *TableSuite) TestTriggersAndAtomsToRows() {
	triggers := []api.Trigger{{Alias: "cron", Type: "cron", ID: "t1"}}
	atoms := []api.Atom{{Image: "busybox", Engine: "docker", ID: "a1"}}
	triggerRows := triggersToRows(triggers)
	atomRows := atomsToRows(atoms)
	s.Require().Len(triggerRows, 1)
	s.Require().Len(atomRows, 1)
	s.Equal(table.Row{"cron", "cron", "t1"}, triggerRows[0])
	s.Equal(table.Row{"busybox", "docker", "a1"}, atomRows[0])
}

func (s *TableSuite) TestDistributeWidthsRespectsMinimums() {
	widths := distributeWidths(30, []int{1, 1, 1})
	s.Len(widths, 3)
	s.GreaterOrEqual(widths[0], 10)
	s.GreaterOrEqual(widths[1], 10)
	s.GreaterOrEqual(widths[2], 10)
	widths = distributeWidths(0, []int{1})
	s.Equal([]int{12}, widths)
}

func (s *TableSuite) TestFormatRunStatusUsesPlainTextWithoutANSI() {
	run := &api.Run{Status: "succeeded"}
	s.Equal("✅ Succeeded", formatRunStatus(run, ""))
	s.False(strings.Contains(formatRunStatus(run, ""), "\x1b"))

	run = &api.Run{Status: "failed"}
	s.Equal("❌ Failed", formatRunStatus(run, ""))
	s.False(strings.Contains(formatRunStatus(run, ""), "\x1b"))

	run = &api.Run{Status: "running"}
	s.Equal("⠋ Running", formatRunStatus(run, "⠋"))
	s.False(strings.Contains(formatRunStatus(run, "⠋"), "\x1b"))

	run = &api.Run{Status: "pending"}
	s.Equal("⠋ Pending", formatRunStatus(run, "⠋"))
	s.False(strings.Contains(formatRunStatus(run, "⠋"), "\x1b"))
}

func (s *TableSuite) TestAdjustColumnsToWidthFitsTarget() {
	cols := []table.Column{
		{Title: "A", Width: 40},
		{Title: "B", Width: 40},
		{Title: "C", Width: 40},
	}
	adjusted := adjustColumnsToWidth(cols, 80)
	total := 0
	for _, col := range adjusted {
		total += col.Width
	}
	total += len(adjusted) * 2 // table cell/header horizontal padding frame
	s.LessOrEqual(total, 80)
}
