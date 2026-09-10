//go:build integration

package test

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/metrics"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

//go:embed statement_budget_testdata/baseline.json
var statementBudgetBaselineJSON []byte

//go:embed statement_budget_testdata/cases.json
var statementBudgetCasesJSON []byte

//go:embed statement_budget_testdata/metrics_prefix.prom
var statementBudgetPrefixMetrics []byte

const (
	dbWritesMetric     = "caesium_db_writes_total"
	dbStatementsMetric = "caesium_db_statements_total"
)

// statementBudgetCategories is the full labeled set from internal/metrics.
var statementBudgetCategories = []string{
	metrics.DBWriteCategoryTaskRunInsert,
	metrics.DBWriteCategoryTaskRunStatus,
	metrics.DBWriteCategoryEventInsert,
	metrics.DBWriteCategoryLeaseRenewal,
	metrics.DBWriteCategoryCallback,
	metrics.DBWriteCategoryCommand,
	metrics.DBWriteCategoryCheckpoint,
}

type statementBudgetDoc struct {
	SchemaVersion int                       `json:"schema_version"`
	Workload      statementBudgetWorkload   `json:"workload"`
	Categories    map[string]categoryBudget `json:"categories"`
}

type statementBudgetWorkload struct {
	Tasks               int    `json:"tasks"`
	ExpectedCompletions int    `json:"expected_completions"`
	Description         string `json:"description"`
}

type categoryBudget struct {
	Mode          string  `json:"mode"`
	Reason        string  `json:"reason,omitempty"`
	Writes        float64 `json:"writes,omitempty"`
	Statements    float64 `json:"statements,omitempty"`
	MinWrites     float64 `json:"min_writes,omitempty"`
	MinStatements float64 `json:"min_statements,omitempty"`
	WriteSlack    float64 `json:"write_slack,omitempty"`
	Justification string  `json:"justification,omitempty"`
}

type statementBudgetObservation struct {
	Completions *int               `json:"completions"`
	Writes      map[string]float64 `json:"writes"`
	Statements  map[string]float64 `json:"statements"`
}

type statementBudgetCaseFile struct {
	Cases []statementBudgetCase `json:"cases"`
}

type statementBudgetCase struct {
	Name      string                     `json:"name"`
	WantPass  bool                       `json:"want_pass"`
	MustMatch string                     `json:"must_match"`
	Observed  statementBudgetObservation `json:"observed"`
}

func loadStatementBudget(t *testing.T) statementBudgetDoc {
	t.Helper()
	var budget statementBudgetDoc
	require.NoError(t, json.Unmarshal(statementBudgetBaselineJSON, &budget))
	require.Equal(t, 1, budget.SchemaVersion, "baseline schema_version")
	require.Equal(t, 2, budget.Workload.Tasks)
	require.Equal(t, 1, budget.Workload.ExpectedCompletions)
	for _, cat := range statementBudgetCategories {
		_, ok := budget.Categories[cat]
		require.True(t, ok, "baseline missing category %s", cat)
	}
	return budget
}

// TestStatementBudgetComparator feeds recorded profiles through the checker.
// A good profile must pass; extra statements / lost batching with matching
// completions must fail; missing evidence must not pass.
func TestStatementBudgetComparator(t *testing.T) {
	runStatementBudgetComparator(t)
}

func runStatementBudgetComparator(t *testing.T) {
	t.Helper()
	budget := loadStatementBudget(t)
	var file statementBudgetCaseFile
	require.NoError(t, json.Unmarshal(statementBudgetCasesJSON, &file))
	require.NotEmpty(t, file.Cases)

	var sawPass, sawFail bool
	for _, tc := range file.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			findings := evaluateStatementBudget(budget, tc.Observed)
			if tc.WantPass {
				sawPass = true
				require.Empty(t, findings, "good profile must pass, got:\n%s", strings.Join(findings, "\n"))
				return
			}
			sawFail = true
			require.NotEmpty(t, findings, "mutated or incomplete profile must fail")
			if tc.MustMatch != "" {
				joined := strings.Join(findings, "\n")
				require.Contains(t, joined, tc.MustMatch, "findings:\n%s", joined)
			}
		})
	}
	require.True(t, sawPass, "testdata must include a passing profile")
	require.True(t, sawFail, "testdata must include a failing mutation")
}

// TestStatementBudgetParseCounterRejectsPrefixOnlyName pins the labeled scrape
// against the prefix-only match scrapeCounter uses. A colliding
// caesium_db_statements_total_extra series listed first must not be read as
// caesium_db_statements_total.
func TestStatementBudgetParseCounterRejectsPrefixOnlyName(t *testing.T) {
	text := string(statementBudgetPrefixMetrics)

	insert, found, err := parsePromLabeledCounter(text, dbStatementsMetric, map[string]string{"category": metrics.DBWriteCategoryTaskRunInsert})
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, 2.0, insert)

	_, found, err = parsePromLabeledCounter(text, "caesium_db_statements", map[string]string{"category": metrics.DBWriteCategoryTaskRunInsert})
	require.NoError(t, err)
	require.False(t, found, "prefix-only metric name matches must be rejected")

	_, found, err = parsePromLabeledCounter(text, dbStatementsMetric+"_extra", map[string]string{"category": metrics.DBWriteCategoryTaskRunInsert})
	require.NoError(t, err)
	require.True(t, found, "the colliding extra series is itself a distinct name")

	writes, found, err := parsePromLabeledCounter(text, dbWritesMetric, map[string]string{"category": metrics.DBWriteCategoryEventInsert})
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, 6.0, writes)

	_, found, err = parsePromLabeledCounter(text, dbWritesMetric, map[string]string{"category": "task_run"})
	require.NoError(t, err)
	require.False(t, found, "partial category values must not match")
}

// TestStatementBudgetFixedWorkload drives a 2-step sequential DAG through the
// real CLI apply/start surface and HTTP run/metrics endpoints, then asserts
// per-category SQL-work deltas against the recorded baseline.
func (s *IntegrationTestSuite) TestStatementBudgetFixedWorkload() {
	budget := loadStatementBudget(s.T())
	alias := fmt.Sprintf("statement-budget-%d", time.Now().UnixNano())
	manifest := fmt.Sprintf(`
apiVersion: v1
kind: Job
metadata:
  alias: %s
trigger:
  type: http
  configuration:
    path: %s
steps:
  - name: extract
    image: alpine:3.23
    cache: false
    command: ["sh", "-c", "echo extract-%s"]
  - name: load
    image: alpine:3.23
    cache: false
    command: ["sh", "-c", "echo load-%s"]
`, alias, alias, alias, alias)

	dir := s.writeJobManifest(manifest)
	defer os.RemoveAll(dir)

	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)
	s.Require().NotNil(job)

	s.awaitQuietDBBudget(time.Second, 20*time.Second)

	before := s.scrapeDBBudget()
	stdout, err := s.runCLIStdout("run", "start", "--job-id", job.ID, "--server", s.caesiumURL)
	s.Require().NoError(err, "run start stdout=%q", stdout)
	runID, err := uuid.Parse(strings.TrimSpace(stdout))
	s.Require().NoError(err, "run start stdout must be a run id on its own stream, got %q", stdout)

	completed := s.awaitRun(job.ID, runID.String(), runTimeout)
	s.Require().Equal("succeeded", completed.Status, "workload must complete: %s", completed.Error)
	s.Require().Len(completed.Tasks, budget.Workload.Tasks, "expected %d task completions", budget.Workload.Tasks)
	for _, task := range completed.Tasks {
		s.Require().Equal("succeeded", task.Status, "task %s: %s", task.ID, task.Error)
	}

	after := s.scrapeDBBudget()
	delta := dbBudgetDelta(before, after)
	observedCompletions := 1
	delta.Completions = &observedCompletions

	for cat, spec := range budget.Categories {
		if spec.Mode != "skip" {
			continue
		}
		s.T().Logf("skipping %s (delta writes=%.0f statements=%.0f): %s",
			cat, delta.Writes[cat], delta.Statements[cat], spec.Reason)
	}

	findings := evaluateStatementBudget(budget, delta)
	s.Require().Empty(findings, "SQL-work budget failed after a successful run:\n%s", strings.Join(findings, "\n"))
}

func dbBudgetDelta(before, after statementBudgetObservation) statementBudgetObservation {
	delta := statementBudgetObservation{
		Writes:     make(map[string]float64, len(statementBudgetCategories)),
		Statements: make(map[string]float64, len(statementBudgetCategories)),
	}
	for _, cat := range statementBudgetCategories {
		delta.Writes[cat] = after.Writes[cat] - before.Writes[cat]
		delta.Statements[cat] = after.Statements[cat] - before.Statements[cat]
	}
	return delta
}

func (s *IntegrationTestSuite) awaitQuietDBBudget(quiet, timeout time.Duration) {
	s.T().Helper()
	watched := []string{
		metrics.DBWriteCategoryTaskRunInsert,
		metrics.DBWriteCategoryTaskRunStatus,
		metrics.DBWriteCategoryEventInsert,
	}
	deadline := time.Now().Add(timeout)
	for {
		before := s.scrapeDBBudget()
		time.Sleep(quiet)
		after := s.scrapeDBBudget()
		stable := true
		for _, cat := range watched {
			if after.Writes[cat] != before.Writes[cat] || after.Statements[cat] != before.Statements[cat] {
				stable = false
				break
			}
		}
		if stable {
			return
		}
		if time.Now().After(deadline) {
			s.T().Logf("DB write counters never went quiet within %s; leftover traffic may consume slack", timeout)
			return
		}
	}
}

func (s *IntegrationTestSuite) scrapeDBBudget() statementBudgetObservation {
	s.T().Helper()
	resp, err := s.doRequest(http.MethodGet, s.caesiumURL+"/metrics", nil)
	s.Require().NoError(err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	s.Require().NoError(err)
	s.Require().Equal(http.StatusOK, resp.StatusCode, string(body))

	text := string(body)
	obs := statementBudgetObservation{
		Writes:     make(map[string]float64, len(statementBudgetCategories)),
		Statements: make(map[string]float64, len(statementBudgetCategories)),
	}
	for _, cat := range statementBudgetCategories {
		labels := map[string]string{"category": cat}
		writes, _, err := parsePromLabeledCounter(text, dbWritesMetric, labels)
		s.Require().NoError(err, "parse %s category=%s", dbWritesMetric, cat)
		stmts, _, err := parsePromLabeledCounter(text, dbStatementsMetric, labels)
		s.Require().NoError(err, "parse %s category=%s", dbStatementsMetric, cat)
		obs.Writes[cat] = writes
		obs.Statements[cat] = stmts
	}
	return obs
}

func evaluateStatementBudget(budget statementBudgetDoc, obs statementBudgetObservation) []string {
	var findings []string
	if budget.SchemaVersion != 1 {
		findings = append(findings, fmt.Sprintf("unsupported baseline schema_version %d", budget.SchemaVersion))
	}
	if obs.Completions == nil {
		findings = append(findings, "missing evidence: completions")
	} else if *obs.Completions != budget.Workload.ExpectedCompletions {
		findings = append(findings, fmt.Sprintf("completions: got %d want %d", *obs.Completions, budget.Workload.ExpectedCompletions))
	}
	if obs.Writes == nil {
		findings = append(findings, "missing evidence: writes")
	}
	if obs.Statements == nil {
		findings = append(findings, "missing evidence: statements")
	}
	if obs.Writes == nil || obs.Statements == nil {
		return findings
	}

	for _, cat := range statementBudgetCategories {
		spec, ok := budget.Categories[cat]
		if !ok {
			findings = append(findings, fmt.Sprintf("missing evidence: baseline category %s", cat))
			continue
		}
		switch spec.Mode {
		case "skip":
			continue
		case "zero":
			findings = append(findings, evaluateZeroCategory(cat, spec, obs)...)
		case "bound":
			findings = append(findings, evaluateBoundCategory(cat, spec, obs)...)
		default:
			findings = append(findings, fmt.Sprintf("%s: unknown mode %q", cat, spec.Mode))
		}
	}
	return findings
}

func evaluateZeroCategory(cat string, spec categoryBudget, obs statementBudgetObservation) []string {
	writes, haveWrites := obs.Writes[cat]
	stmts, haveStmts := obs.Statements[cat]
	var findings []string
	if !haveWrites {
		findings = append(findings, fmt.Sprintf("missing evidence: writes.%s", cat))
	}
	if !haveStmts {
		findings = append(findings, fmt.Sprintf("missing evidence: statements.%s", cat))
	}
	if !haveWrites || !haveStmts {
		return findings
	}
	if err := invalidCounter(writes); err != "" {
		findings = append(findings, fmt.Sprintf("writes.%s: %s", cat, err))
	}
	if err := invalidCounter(stmts); err != "" {
		findings = append(findings, fmt.Sprintf("statements.%s: %s", cat, err))
	}
	if writes != 0 || stmts != 0 {
		reason := spec.Reason
		if reason == "" {
			reason = "reserved category must stay at zero for this workload"
		}
		findings = append(findings, fmt.Sprintf("%s: got writes=%.0f statements=%.0f want 0 (%s)", cat, writes, stmts, reason))
	}
	return findings
}

func evaluateBoundCategory(cat string, spec categoryBudget, obs statementBudgetObservation) []string {
	writes, haveWrites := obs.Writes[cat]
	stmts, haveStmts := obs.Statements[cat]
	var findings []string
	if !haveWrites {
		findings = append(findings, fmt.Sprintf("missing evidence: writes.%s", cat))
	}
	if !haveStmts {
		findings = append(findings, fmt.Sprintf("missing evidence: statements.%s", cat))
	}
	if !haveWrites || !haveStmts {
		return findings
	}
	if err := invalidCounter(writes); err != "" {
		findings = append(findings, fmt.Sprintf("writes.%s: %s", cat, err))
	}
	if err := invalidCounter(stmts); err != "" {
		findings = append(findings, fmt.Sprintf("statements.%s: %s", cat, err))
	}
	if len(findings) > 0 {
		return findings
	}

	minWrites := spec.MinWrites
	if minWrites == 0 {
		minWrites = spec.Writes
	}
	minStmts := spec.MinStatements
	if minStmts == 0 {
		minStmts = spec.Statements
	}
	if writes < minWrites {
		findings = append(findings, fmt.Sprintf("%s: writes %.0f below minimum %.0f (missing evidence of workload SQL)", cat, writes, minWrites))
	}
	if stmts < minStmts {
		findings = append(findings, fmt.Sprintf("%s: statements %.0f below minimum %.0f (missing evidence of workload SQL)", cat, stmts, minStmts))
	}
	maxWrites := spec.Writes + spec.WriteSlack
	if writes > maxWrites {
		findings = append(findings, fmt.Sprintf("%s: writes %.0f over budget %.0f (baseline %.0f + slack %.0f); matching completions do not waive SQL work", cat, writes, maxWrites, spec.Writes, spec.WriteSlack))
	}
	if stmts > writes {
		findings = append(findings, fmt.Sprintf("%s: statements %.0f exceed writes %.0f", cat, stmts, writes))
	}
	maxStmts := spec.Statements
	if writes > spec.Writes {
		extraWrites := writes - spec.Writes
		if spec.Writes > 0 && spec.Statements > 0 {
			maxStmts = spec.Statements + math.Ceil(extraWrites*spec.Statements/spec.Writes)
		} else {
			maxStmts = spec.Statements + extraWrites
		}
	}
	if stmts > maxStmts {
		findings = append(findings, fmt.Sprintf("%s: lost batching: %.0f statements for %.0f writes; baseline is %.0f statement(s) for %.0f writes (same completions do not waive SQL work)", cat, stmts, writes, spec.Statements, spec.Writes))
	}
	return findings
}

func invalidCounter(v float64) string {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return "invalid evidence (NaN/Inf)"
	}
	if v < 0 {
		return fmt.Sprintf("negative delta %.0f", v)
	}
	return ""
}

// parsePromLabeledCounter reads one Prometheus counter sample. The metric name
// must match exactly (prefix-only matches are rejected). Required labels are
// an exact value match on those keys; extra labels on the series are ignored.
// An absent series is (0, false, nil) so a "did this move?" delta can start at
// zero, but the budget checker still treats missing bound keys as failure.
func parsePromLabeledCounter(text, name string, labels map[string]string) (float64, bool, error) {
	var (
		found bool
		value float64
	)
	for _, line := range strings.Split(text, "\n") {
		series, raw, ok := splitPromSample(line)
		if !ok {
			continue
		}
		gotName, gotLabels, ok := parsePromSeries(series)
		if !ok || gotName != name {
			continue
		}
		matched := true
		for k, v := range labels {
			if gotLabels[k] != v {
				matched = false
				break
			}
		}
		if !matched {
			continue
		}
		parsed, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return 0, false, fmt.Errorf("parse metric sample %q: %w", line, err)
		}
		if math.IsNaN(parsed) || math.IsInf(parsed, 0) || parsed < 0 {
			return 0, false, fmt.Errorf("invalid counter %s labels %v: %v", name, labels, parsed)
		}
		if found {
			return 0, false, fmt.Errorf("ambiguous counter %s labels %v", name, labels)
		}
		found = true
		value = parsed
	}
	if !found {
		return 0, false, nil
	}
	return value, true, nil
}

func splitPromSample(line string) (series, value string, ok bool) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", false
	}
	if i := strings.IndexByte(line, '{'); i >= 0 {
		j := strings.LastIndexByte(line, '}')
		if j <= i {
			return "", "", false
		}
		rest := strings.TrimSpace(line[j+1:])
		if rest == "" {
			return "", "", false
		}
		if sp := strings.IndexByte(rest, ' '); sp >= 0 {
			rest = rest[:sp]
		}
		return line[:j+1], rest, true
	}
	name, rest, cut := strings.Cut(line, " ")
	if !cut || name == "" {
		return "", "", false
	}
	rest = strings.TrimSpace(rest)
	if sp := strings.IndexByte(rest, ' '); sp >= 0 {
		rest = rest[:sp]
	}
	return name, rest, true
}

func parsePromSeries(series string) (name string, labels map[string]string, ok bool) {
	labels = map[string]string{}
	name, body, hasLabels := strings.Cut(series, "{")
	if name == "" {
		return "", nil, false
	}
	if !hasLabels {
		return name, labels, true
	}
	if !strings.HasSuffix(body, "}") {
		return "", nil, false
	}
	body = strings.TrimSuffix(body, "}")
	if body == "" {
		return name, labels, true
	}
	for body != "" {
		key, rest, cut := strings.Cut(body, "=")
		if !cut || key == "" {
			return "", nil, false
		}
		rest = strings.TrimSpace(rest)
		if !strings.HasPrefix(rest, `"`) {
			return "", nil, false
		}
		val, next, err := readPromQuoted(rest)
		if err != nil {
			return "", nil, false
		}
		labels[strings.TrimSpace(key)] = val
		next = strings.TrimSpace(next)
		if next == "" {
			break
		}
		if !strings.HasPrefix(next, ",") {
			return "", nil, false
		}
		body = strings.TrimSpace(strings.TrimPrefix(next, ","))
	}
	return name, labels, true
}

func readPromQuoted(s string) (value, rest string, err error) {
	if !strings.HasPrefix(s, `"`) {
		return "", "", fmt.Errorf("missing quote")
	}
	s = s[1:]
	var b strings.Builder
	escaped := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if escaped {
			b.WriteByte(c)
			escaped = false
			continue
		}
		if c == '\\' {
			escaped = true
			continue
		}
		if c == '"' {
			return b.String(), s[i+1:], nil
		}
		b.WriteByte(c)
	}
	return "", "", fmt.Errorf("unterminated quote")
}
