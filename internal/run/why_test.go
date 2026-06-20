package run

import (
	"strings"
	"testing"

	"github.com/caesium-cloud/caesium/internal/models"
)

func TestClassifyVerdict(t *testing.T) {
	cases := []struct {
		name   string
		status TaskStatus
		cache  bool
		want   WhyVerdict
	}{
		{"cached", TaskStatusCached, true, VerdictCacheHit},
		{"succeeded-cache-on", TaskStatusSucceeded, true, VerdictCacheMiss},
		{"succeeded-cache-off", TaskStatusSucceeded, false, VerdictCacheOff},
		{"failed-cache-on", TaskStatusFailed, true, VerdictCacheMiss},
		{"running", TaskStatusRunning, true, VerdictUnknown},
		{"pending", TaskStatusPending, false, VerdictUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := &models.TaskRun{Status: string(tc.status), CacheEnabled: tc.cache}
			if got := classifyVerdict(tr); got != tc.want {
				t.Errorf("classifyVerdict(%s, cache=%v) = %s, want %s", tc.status, tc.cache, got, tc.want)
			}
		})
	}
}

func TestSummarize_CacheMissNamesHeadlineField(t *testing.T) {
	exp := &WhyExplanation{
		TaskName: "load",
		Verdict:  VerdictCacheMiss,
		Baseline: WhyBaseline{Kind: "prior_run"},
		Diff: &BlobDiff{
			Changes: []FieldChange{
				{Field: "predecessorOutputs.extract.row_count", Kind: fieldMapEntry, Before: "1200000", After: "1400000"},
				{Field: "image", Kind: fieldScalar, Before: "alpine:3.23", After: "busybox:1.36.1"},
			},
		},
	}
	got := summarize(exp)
	if !strings.HasPrefix(got, "CACHE MISS") {
		t.Errorf("expected CACHE MISS prefix, got %q", got)
	}
	if !strings.Contains(got, "extract.row_count") || !strings.Contains(got, "1200000") || !strings.Contains(got, "1400000") {
		t.Errorf("expected headline field + before/after in summary, got %q", got)
	}
	if !strings.Contains(got, "and 1 other field") {
		t.Errorf("expected secondary-change count, got %q", got)
	}
}

func TestSummarize_CacheHitIdentical(t *testing.T) {
	exp := &WhyExplanation{
		TaskName: "transform",
		Verdict:  VerdictCacheHit,
		Diff:     &BlobDiff{HashEqual: true},
	}
	got := summarize(exp)
	if !strings.HasPrefix(got, "CACHE HIT") {
		t.Errorf("expected CACHE HIT prefix, got %q", got)
	}
	if !strings.Contains(got, "identical") {
		t.Errorf("expected identical-inputs proof, got %q", got)
	}
}

func TestSummarize_CacheMissNoPriorRun(t *testing.T) {
	exp := &WhyExplanation{
		TaskName: "extract",
		Verdict:  VerdictCacheMiss,
		Baseline: WhyBaseline{Kind: "none"},
		Diff:     &BlobDiff{},
	}
	got := summarize(exp)
	if !strings.Contains(got, "first run") {
		t.Errorf("expected first-run explanation, got %q", got)
	}
}

func TestSummarize_Degraded(t *testing.T) {
	exp := &WhyExplanation{
		TaskName: "x",
		Verdict:  VerdictCacheMiss,
		Baseline: WhyBaseline{Kind: "prior_run"},
		Diff:     &BlobDiff{Degraded: "one or both blobs were stored oversized"},
	}
	got := summarize(exp)
	if !strings.Contains(got, "oversized") {
		t.Errorf("expected degraded reason surfaced, got %q", got)
	}
}

func TestDescribeChange_RedactedNeverShowsPlaintext(t *testing.T) {
	c := FieldChange{Field: "env.SECRET", Kind: fieldMapEntry, Before: "sha256:aaa", After: "sha256:bbb", Redacted: true}
	got := describeChange(c)
	if !strings.Contains(got, "redacted") {
		t.Errorf("expected redacted label, got %q", got)
	}
	if !strings.Contains(got, "sha256:aaa") || !strings.Contains(got, "sha256:bbb") {
		t.Errorf("expected digests in redacted description, got %q", got)
	}
}
