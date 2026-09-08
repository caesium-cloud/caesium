//go:build integration

package test

import (
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Milliseconds, using the maximum observed across Docker amd64/arm64, Podman,
// and Kubernetes in https://github.com/caesium-cloud/caesium/actions/runs/34236365746.
// These are scheduling hints, never an allowlist. New scenarios get a positive
// default cost; deleted scenarios are ignored. See docs/ci.md for refreshing.
//
//go:embed shard_timings.json
var integrationShardTimings []byte

// Longest-processing-time first, with stable name and shard-index tie breaks.
// Each runner still runs serially against its own independent server.
func balancedIntegrationShards(methods []string, count int, timings map[string]int) [][]string {
	cost := func(name string) int {
		if timings[name] > 0 {
			return timings[name]
		}
		return 1000
	}
	methods = append([]string(nil), methods...)
	sort.Slice(methods, func(i, j int) bool {
		if cost(methods[i]) != cost(methods[j]) {
			return cost(methods[i]) > cost(methods[j])
		}
		return methods[i] < methods[j]
	})
	shards := make([][]string, count)
	totals := make([]int, count)
	for _, name := range methods {
		lightest := 0
		for i := 1; i < count; i++ {
			if totals[i] < totals[lightest] {
				lightest = i
			}
		}
		shards[lightest] = append(shards[lightest], name)
		totals[lightest] += cost(name)
	}
	return shards
}

// Reflect the same Test* methods that testify discovers. Every new scenario
// automatically belongs to exactly one shard, regardless of the timing data.
func integrationShardFilter(s any, index, count string) (string, error) {
	if index == "" && count == "" {
		return "", nil
	}
	i, indexErr := strconv.Atoi(index)
	n, countErr := strconv.Atoi(count)
	if indexErr != nil || countErr != nil || n < 1 || i < 1 || i > n {
		return "", fmt.Errorf("invalid integration shard %q/%q (want 1 <= index <= count)", index, count)
	}
	typ := reflect.TypeOf(s)
	var methods, selected []string
	for m := 0; m < typ.NumMethod(); m++ {
		name := typ.Method(m).Name
		if strings.HasPrefix(name, "Test") {
			methods = append(methods, name)
		}
	}
	if n > len(methods) {
		return "", fmt.Errorf("%d shards exceed %d integration scenarios", n, len(methods))
	}
	var timings map[string]int
	if err := json.Unmarshal(integrationShardTimings, &timings); err != nil {
		return "", fmt.Errorf("decode integration shard timings: %w", err)
	}
	for _, name := range balancedIntegrationShards(methods, n, timings)[i-1] {
		selected = append(selected, regexp.QuoteMeta(name))
	}
	return "^(" + strings.Join(selected, "|") + ")$", nil
}

func TestIntegrationShardBalancesDurations(t *testing.T) {
	// Alphabetical round robin puts both slow scenarios on shard zero (18s).
	methods := []string{"TestA", "TestB", "TestC", "TestD"}
	timings := map[string]int{"TestA": 9000, "TestB": 1000, "TestC": 9000, "TestD": 1000}
	shards := balancedIntegrationShards(methods, 2, timings)
	for _, shard := range shards {
		total := 0
		for _, name := range shard {
			total += timings[name]
		}
		require.Equal(t, 10000, total)
	}
	require.Equal(t, shards, balancedIntegrationShards([]string{"TestD", "TestC", "TestB", "TestA"}, 2, timings))
	// Stale entries cannot add tests; absent/invalid hints cannot drop new ones.
	timings["TestDeleted"] = 99999
	timings["TestE"] = -1
	methods = append(methods, "TestE", "TestNew")
	var assigned []string
	for _, shard := range balancedIntegrationShards(methods, 3, timings) {
		assigned = append(assigned, shard...)
	}
	require.ElementsMatch(t, methods, assigned)
}

func configureIntegrationShard(t *testing.T, s any) {
	t.Helper()
	filter, err := integrationShardFilter(s, os.Getenv("CAESIUM_TEST_SHARD_INDEX"), os.Getenv("CAESIUM_TEST_SHARD_COUNT"))
	require.NoError(t, err)
	if filter == "" {
		return
	}
	previous := flag.Lookup("testify.m").Value.String()
	require.Empty(t, previous, "sharding cannot be combined with a testify method filter")
	require.Empty(t, flag.Lookup("test.run").Value.String(), "sharding requires the full suite, without -run")
	require.NoError(t, flag.Set("testify.m", filter))
	t.Cleanup(func() { require.NoError(t, flag.Set("testify.m", previous)) })
	t.Logf("integration shard %s/%s: %s", os.Getenv("CAESIUM_TEST_SHARD_INDEX"), os.Getenv("CAESIUM_TEST_SHARD_COUNT"), filter)
}

func TestIntegrationShardPartition(t *testing.T) {
	s := new(IntegrationTestSuite)
	typ := reflect.TypeOf(s)
	for _, count := range []int{1, 2, 3, 4} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			seen := map[string]int{}
			for index := 1; index <= count; index++ {
				filter, err := integrationShardFilter(s, strconv.Itoa(index), strconv.Itoa(count))
				require.NoError(t, err)
				re := regexp.MustCompile(filter)
				matches := 0
				for m := 0; m < typ.NumMethod(); m++ {
					name := typ.Method(m).Name
					if re.MatchString(name) {
						require.True(t, strings.HasPrefix(name, "Test"))
						seen[name]++
						matches++
					}
				}
				require.Positive(t, matches)
			}
			for m := 0; m < typ.NumMethod(); m++ {
				name := typ.Method(m).Name
				if strings.HasPrefix(name, "Test") {
					require.Equal(t, 1, seen[name], "scenario %s must run exactly once", name)
				}
			}
		})
	}
}

func TestIntegrationShardInvalidConfiguration(t *testing.T) {
	for _, pair := range [][2]string{{"", "3"}, {"1", ""}, {"0", "3"}, {"4", "3"}, {"1", "0"}, {"1", "-1"}, {"x", "3"}, {"1", "99999"}} {
		_, err := integrationShardFilter(new(IntegrationTestSuite), pair[0], pair[1])
		require.Error(t, err, "shard %v must fail closed", pair)
	}
	filter, err := integrationShardFilter(new(IntegrationTestSuite), "", "")
	require.NoError(t, err)
	require.Empty(t, filter, "local runs remain unsharded")
}
