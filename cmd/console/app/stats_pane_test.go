package app

import (
	"strings"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/cmd/console/api"
	"github.com/stretchr/testify/suite"
)

type StatsPaneSuite struct {
	suite.Suite
}

func TestStatsPaneSuite(t *testing.T) {
	suite.Run(t, new(StatsPaneSuite))
}

func (s *StatsPaneSuite) TestRenderStatsViewNilShowsPlaceholder() {
	out := renderStatsView(nil, 80)
	s.Contains(out, "No statistics available")
}

func (s *StatsPaneSuite) TestRenderStatsViewShowsOverview() {
	stats := &api.StatsResponse{
		Jobs: api.JobStats{
			Total:              10,
			RecentRuns:         25,
			SuccessRate:        0.92,
			AvgDurationSeconds: 45.5,
		},
	}

	out := renderStatsView(stats, 100)
	s.Contains(out, "Overview")
	s.Contains(out, "10")
	s.Contains(out, "25")
	s.Contains(out, "92%")
}

func (s *StatsPaneSuite) TestRenderStatsViewShowsTopFailing() {
	lastFail := time.Date(2024, 6, 1, 10, 0, 0, 0, time.UTC)
	stats := &api.StatsResponse{
		TopFailing: []api.FailingJob{
			{JobID: "job-1", Alias: "etl-pipeline", FailureCount: 5, LastFailure: &lastFail},
		},
	}

	out := renderStatsView(stats, 100)
	s.Contains(out, "Top Failing Jobs")
	s.Contains(out, "etl-pipeline")
	s.Contains(out, "5")
}

func (s *StatsPaneSuite) TestRenderStatsViewShowsSlowestJobs() {
	stats := &api.StatsResponse{
		SlowestJobs: []api.SlowestJob{
			{JobID: "job-2", Alias: "data-export", AvgDurationSeconds: 312.5},
		},
	}

	out := renderStatsView(stats, 100)
	s.Contains(out, "Slowest Jobs")
	s.Contains(out, "data-export")
}

func (s *StatsPaneSuite) TestRenderStatsViewUsesShortIDWhenNoAlias() {
	stats := &api.StatsResponse{
		TopFailing: []api.FailingJob{
			{JobID: "abcdefgh-1234-5678-abcd-1234567890ab", FailureCount: 1},
		},
	}

	out := renderStatsView(stats, 100)
	s.Contains(out, "abcdefgh")
}

func (s *StatsPaneSuite) TestRenderProgressBarFull() {
	bar := renderProgressBar(1.0, 10)
	s.Equal(strings.Repeat("█", 10), bar)
}

func (s *StatsPaneSuite) TestRenderProgressBarEmpty() {
	bar := renderProgressBar(0.0, 10)
	s.Equal(strings.Repeat("░", 10), bar)
}

func (s *StatsPaneSuite) TestRenderProgressBarHalf() {
	bar := renderProgressBar(0.5, 10)
	s.Contains(bar, "█")
	s.Contains(bar, "░")
	s.Equal(10, len([]rune(bar)))
}

func (s *StatsPaneSuite) TestFormatDurationVariousRanges() {
	s.Equal("-", formatDuration(0))
	s.Equal("-", formatDuration(-1))
	s.Contains(formatDuration(0.5), "ms")
	s.Contains(formatDuration(5), "s")
	s.Contains(formatDuration(90), "m")
	s.Contains(formatDuration(7200), "h")
}

func (s *StatsPaneSuite) TestRenderStatsViewEmptyLists() {
	stats := &api.StatsResponse{
		Jobs:        api.JobStats{Total: 0, RecentRuns: 0, SuccessRate: 0},
		TopFailing:  nil,
		SlowestJobs: nil,
	}

	out := renderStatsView(stats, 80)
	s.Contains(out, "Overview")
	s.NotContains(out, "Top Failing Jobs")
	s.NotContains(out, "Slowest Jobs")
}

func (s *StatsPaneSuite) TestRenderStatsViewTruncatesLongAlias() {
	stats := &api.StatsResponse{
		TopFailing: []api.FailingJob{
			{JobID: "j1", Alias: "this-is-a-very-long-job-alias-name", FailureCount: 1},
		},
	}

	out := renderStatsView(stats, 100)
	s.Contains(out, "...")
}
