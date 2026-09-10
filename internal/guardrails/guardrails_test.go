package guardrails_test

import (
	"bufio"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/caesium-cloud/caesium/internal/jobdef/report"
)

// pinnedImages defines the single canonical tag for each base image used in
// the repo. The test below scans every source file and asserts that all
// occurrences of an image name use exactly this tag. Add a new entry here
// whenever a new pinned image is introduced; remove the old entry and add the
// new tag here when upgrading.
//
// Intentionally excluded from enforcement:
//   - caesiumcloud/* product images — tagged dynamically at build time
//   - Images that appear only in .devcontainer/ (developer convenience only)
//   - Placeholder/fixture images used in unit tests (e.g. "example", "etl:latest")
//   - The deliberate bad-image used for error-handling tests
var pinnedImages = map[string]string{
	"alpine":          "alpine:3.23",
	"busybox":         "busybox:1.36.1",
	"curlimages/curl": "curlimages/curl:8.12.1",
}

// scanDirs is the set of subtrees checked for image version consistency.
// .devcontainer and vendor are excluded intentionally.
var scanDirs = []string{
	"api", "build", "cmd", "docs", "helm",
	"internal", "pkg", "test", "ui",
	".github",
}

// exemptPatterns matches lines that should not be checked:
//   - The deliberate bad image in the error-handling test
//   - Dynamic image tags built at CI time
//   - Lines that reference alpine only as part of a compound image tag (e.g. golang:1.25-alpine3.23, docker:29-alpine3.23)
var exemptPatterns = []*regexp.Regexp{
	regexp.MustCompile(`this-image-definitely-does-not-exist`),
	regexp.MustCompile(`caesiumcloud/`),
	regexp.MustCompile(`golang:`), // golang:X.Y-alpineZ
	regexp.MustCompile(`docker:`), // docker:X.Y.Z-alpineZ
	regexp.MustCompile(`mcr\.microsoft`),
	regexp.MustCompile(`BUILDER_TAG`),
	regexp.MustCompile(`VARIANT`),
}

func TestPinnedContainerImageVersionsAreConsistent(t *testing.T) {
	root := repoRoot(t)

	// imageRef matches any occurrence of a pinned image name followed by an
	// optional tag, e.g. `alpine`, `alpine:3.23`, `busybox:1.36.1`.
	imageRef := regexp.MustCompile(`(alpine|busybox|curlimages/curl)(?::[\w.\-]+)?`)

	type violation struct {
		file    string
		line    int
		content string
		want    string
		got     string
	}
	var violations []violation

	for _, subdir := range scanDirs {
		base := filepath.Join(root, subdir)
		if _, err := os.Stat(base); os.IsNotExist(err) {
			continue
		}

		err := filepath.WalkDir(base, func(path string, d os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if d.IsDir() {
				name := d.Name()
				// skip hidden dirs, vendor, node_modules, dist
				if strings.HasPrefix(name, ".") || name == "vendor" || name == "node_modules" || name == "dist" {
					return filepath.SkipDir
				}
				return nil
			}

			// Skip this file itself — it defines the canonical tag map and the
			// imageRef regex, both of which contain bare image names that would
			// otherwise trigger false-positive violations.
			if rel, _ := filepath.Rel(root, path); filepath.ToSlash(rel) == "internal/guardrails/guardrails_test.go" {
				return nil
			}

			ext := strings.ToLower(filepath.Ext(path))
			switch ext {
			case ".go", ".yaml", ".yml", ".md", ".tsx", ".ts", ".dockerfile":
				// check these
			default:
				if !strings.HasSuffix(strings.ToLower(filepath.Base(path)), "dockerfile") {
					return nil
				}
			}

			f, err := os.Open(path)
			if err != nil {
				return err
			}
			defer f.Close()

			scanner := bufio.NewScanner(f)
			lineNum := 0
			for scanner.Scan() {
				lineNum++
				line := scanner.Text()

				// skip exempt lines
				exempt := false
				for _, pat := range exemptPatterns {
					if pat.MatchString(line) {
						exempt = true
						break
					}
				}
				if exempt {
					continue
				}

				matches := imageRef.FindAllString(line, -1)
				for _, match := range matches {
					// Identify which canonical image this is
					for imageName, canonical := range pinnedImages {
						// match must start with this image name
						if match != imageName && !strings.HasPrefix(match, imageName+":") {
							continue
						}
						if match != canonical {
							relPath, _ := filepath.Rel(root, path)
							violations = append(violations, violation{
								file:    relPath,
								line:    lineNum,
								content: strings.TrimSpace(line),
								want:    canonical,
								got:     match,
							})
						}
					}
				}
			}
			return scanner.Err()
		})
		if err != nil {
			t.Fatalf("walk %s: %v", subdir, err)
		}
	}

	if len(violations) > 0 {
		sort.Slice(violations, func(i, j int) bool {
			if violations[i].file != violations[j].file {
				return violations[i].file < violations[j].file
			}
			return violations[i].line < violations[j].line
		})

		t.Log("container image version inconsistencies found")
		t.Log("(to fix: update the image reference, or bump the canonical version in pinnedImages in internal/guardrails/guardrails_test.go)")
		for _, v := range violations {
			t.Errorf("%s:%d  want=%-22s  got=%-22s  %s",
				filepath.ToSlash(v.file), v.line, v.want, v.got, v.content)
		}
	}
}

const (
	modulePrefix = "github.com/caesium-cloud/caesium"
	schemaHeader = "<!-- Generated by `caesium job schema --doc` -->"
)

func TestGeneratedSchemaReferenceIsCurrent(t *testing.T) {
	root := repoRoot(t)

	actual, err := os.ReadFile(filepath.Join(root, "docs", "job-schema-reference.md"))
	if err != nil {
		t.Fatalf("read schema reference: %v", err)
	}

	expected := schemaHeader + "\n\n" + report.Markdown()
	if normalizeText(string(actual)) != normalizeText(expected) {
		t.Fatalf("docs/job-schema-reference.md is stale; regenerate it with `caesium job schema --doc`")
	}
}

func TestJobDefinitionsDocReferencesEveryExampleManifest(t *testing.T) {
	root := repoRoot(t)

	entries, err := filepath.Glob(filepath.Join(root, "docs", "examples", "*.job.yaml"))
	if err != nil {
		t.Fatalf("glob examples: %v", err)
	}

	expected := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		expected[filepath.Base(entry)] = struct{}{}
	}

	docPath := filepath.Join(root, "docs", "job-definitions.md")
	docBytes, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatalf("read job definitions doc: %v", err)
	}

	section := exampleSection(t, string(docBytes))
	re := regexp.MustCompile(`[A-Za-z0-9._-]+\.job\.yaml`)
	actual := make(map[string]struct{})
	for _, match := range re.FindAllString(section, -1) {
		actual[match] = struct{}{}
	}

	missing := setDifference(expected, actual)
	extra := setDifference(actual, expected)
	if len(missing) > 0 || len(extra) > 0 {
		t.Fatalf(
			"docs/job-definitions.md example list drifted; missing=%v extra=%v",
			missing,
			extra,
		)
	}
}

func TestRetryDocumentationDisclosesLiveTopology(t *testing.T) {
	root := repoRoot(t)
	docBytes, err := os.ReadFile(filepath.Join(root, "docs", "job-definitions.md"))
	if err != nil {
		t.Fatalf("read job definitions doc: %v", err)
	}
	doc := string(docBytes)

	for _, required := range []string{
		"A retry preserves the execution fields frozen for tasks that were already registered.",
		"Task membership and DAG wiring are loaded from the current catalog",
		"a newly applied step can be registered onto the older run",
		"removing a step can leave a failed registered task without a live atom to execute",
	} {
		if !strings.Contains(doc, required) {
			t.Errorf("docs/job-definitions.md must disclose retry topology behavior; missing %q", required)
		}
	}

	for _, overclaim := range []string{
		"A retry reproduces the run as it was registered.",
		"To reproduce exactly what a past run did, retry it.",
	} {
		if strings.Contains(doc, overclaim) {
			t.Errorf("docs/job-definitions.md overclaims whole-run retry reproducibility: %q", overclaim)
		}
	}
}

func TestArchitectureBoundaries(t *testing.T) {
	root := repoRoot(t)
	var violations []string

	for _, dir := range []string{"cmd", "api", "internal", "pkg"} {
		base := filepath.Join(root, dir)
		err := filepath.WalkDir(base, func(path string, d os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if d.IsDir() {
				if strings.HasPrefix(d.Name(), ".") {
					return filepath.SkipDir
				}
				return nil
			}
			if filepath.Ext(path) != ".go" {
				return nil
			}

			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)

			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
			if err != nil {
				return err
			}

			for _, imp := range file.Imports {
				importPath := strings.Trim(imp.Path.Value, `"`)
				switch {
				case importPath == "github.com/spf13/cobra" && !strings.HasPrefix(rel, "cmd/"):
					violations = append(violations, rel+" imports cobra outside cmd/")
				case strings.HasPrefix(rel, "pkg/") &&
					(strings.HasPrefix(importPath, modulePrefix+"/api/") || strings.HasPrefix(importPath, modulePrefix+"/cmd/")):
					violations = append(violations, rel+" imports app-layer package "+importPath)
				case strings.HasPrefix(rel, "internal/") &&
					(strings.HasPrefix(importPath, modulePrefix+"/cmd/") || strings.HasPrefix(importPath, modulePrefix+"/api/rest/controller/")):
					violations = append(violations, rel+" imports edge-layer package "+importPath)
				case strings.HasPrefix(rel, "api/rest/controller/") &&
					strings.HasPrefix(importPath, modulePrefix+"/api/rest/controller/"):
					violations = append(violations, rel+" imports another controller package "+importPath)
				}
			}

			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}

	if len(violations) > 0 {
		sort.Strings(violations)
		t.Fatalf("architecture boundary violations:\n%s", strings.Join(violations, "\n"))
	}
}

func TestDocsREADMEIndexesEveryTopLevelDoc(t *testing.T) {
	root := repoRoot(t)

	entries, err := filepath.Glob(filepath.Join(root, "docs", "*.md"))
	if err != nil {
		t.Fatalf("glob docs: %v", err)
	}

	expected := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		base := filepath.Base(entry)
		if base == "README.md" {
			continue
		}
		expected[base] = struct{}{}
	}

	readmePath := filepath.Join(root, "docs", "README.md")
	readmeBytes, err := os.ReadFile(readmePath)
	if err != nil {
		t.Fatalf("read docs README: %v", err)
	}

	re := regexp.MustCompile(`\(([^)]+\.md)\)`)
	actual := make(map[string]struct{})
	for _, match := range re.FindAllStringSubmatch(string(readmeBytes), -1) {
		base := filepath.Base(match[1])
		if base == "README.md" {
			continue
		}
		actual[base] = struct{}{}
	}

	missing := setDifference(expected, actual)
	extra := setDifference(actual, expected)
	if len(missing) > 0 || len(extra) > 0 {
		t.Fatalf("docs/README.md index drifted; missing=%v extra=%v", missing, extra)
	}
}

func TestPlanningAndHistoricalDocsCarryStatusBanner(t *testing.T) {
	root := repoRoot(t)

	files := []string{
		"docs/design-agent-in-the-loop.md",
		"docs/design-airflow-parity.md",
		"docs/design-backtesting.md",
		"docs/design-concurrency-priority.md",
		"docs/design-contract-enforcement.md",
		"docs/design-data-circuit-breaker.md",
		"docs/design-data-plane-memory.md",
		"docs/design-database-locking-fix.md",
		"docs/design-dynamic-fanout.md",
		"docs/design-event-triggers.md",
		"docs/design-freshness-scheduling.md",
		"docs/design-incremental-execution.md",
		"docs/design-quarantined-replay.md",
		"docs/design-reproduce.md",
		"docs/design-resource-right-sizing.md",
		"docs/design-scaling-job-execution.md",
		"docs/design-window-scheduling.md",
		"docs/differentiation-strategy.md",
	}

	for _, rel := range files {
		docBytes, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		if !hasStatusBanner(string(docBytes)) {
			t.Fatalf("%s must include a top-level '> Status:' banner", rel)
		}
	}
}

// integrationUpBaselineRecipe is the justfile recipe every other
// integration-up* recipe below must track for CAESIUM_* env parity (docs/ci.md
// §5, "Parity rule and its guardrail"). Each of those recipes starts its own
// server rather than inheriting from this one, so a feature-gate var added
// only here silently stops being exercised on the others (issue #425).
const integrationUpBaselineRecipe = "integration-up"

// integrationUpTrackingRecipes are the justfile recipes checked against
// integrationUpBaselineRecipe.
var integrationUpTrackingRecipes = []string{
	"integration-up-distributed",
	"integration-up-owner-memory",
	"integration-up-infra",
	"integration-up-agent",
}

// integrationUpEnvAllowlist names, per tracking recipe, baseline env vars
// that recipe is intentionally allowed to omit. Empty today: every recipe in
// integrationUpTrackingRecipes carries full CAESIUM_* parity with
// integration-up (plus whatever lane-specific vars it legitimately needs on
// top). Add an entry here only alongside a comment explaining why the var
// does not apply to that lane — never to silence a real drift.
var integrationUpEnvAllowlist = map[string][]string{}

// TestIntegrationUpRecipesTrackBaselineEnv guards against the class of bug
// fixed in #425: integration-up-distributed, integration-up-owner-memory, and
// integration-up-agent each start their own Caesium server (rather than
// inheriting integration-up's), so a feature-gate env var added only to
// integration-up silently stopped being exercised on those lanes (e.g.
// CAESIUM_CACHE_ENABLED, CAESIUM_RUN_QUEUE_ENABLED, and siblings). This test
// parses justfile and fails if any integration-up-* recipe in
// integrationUpTrackingRecipes is missing a CAESIUM_* var that
// integration-up sets, unless it's named in integrationUpEnvAllowlist.
func TestIntegrationUpRecipesTrackBaselineEnv(t *testing.T) {
	root := repoRoot(t)

	justfileBytes, err := os.ReadFile(filepath.Join(root, "justfile"))
	if err != nil {
		t.Fatalf("read justfile: %v", err)
	}

	recipes := parseJustfileRecipeEnvVars(string(justfileBytes))

	baseline, ok := recipes[integrationUpBaselineRecipe]
	if !ok || len(baseline) == 0 {
		t.Fatalf("could not locate %s recipe's CAESIUM_* env vars in justfile; recipe parsing may be broken", integrationUpBaselineRecipe)
	}

	for _, name := range integrationUpTrackingRecipes {
		vars, ok := recipes[name]
		if !ok || len(vars) == 0 {
			t.Fatalf("could not locate %s recipe's CAESIUM_* env vars in justfile; recipe parsing may be broken", name)
		}

		allowed := make(map[string]struct{}, len(integrationUpEnvAllowlist[name]))
		for _, v := range integrationUpEnvAllowlist[name] {
			allowed[v] = struct{}{}
		}

		missing := make([]string, 0)
		for v := range baseline {
			if _, present := vars[v]; present {
				continue
			}
			if _, exempt := allowed[v]; exempt {
				continue
			}
			missing = append(missing, v)
		}
		sort.Strings(missing)

		if len(missing) > 0 {
			t.Errorf("%s is missing CAESIUM_* env var(s) that %s sets: %v (add them to the recipe, or a justified entry to integrationUpEnvAllowlist)",
				name, integrationUpBaselineRecipe, missing)
		}
	}
}

// parseJustfileRecipeEnvVars extracts, for every top-level justfile recipe
// (a line matching `^name:` with no leading whitespace), the set of
// `CAESIUM_*` env var names referenced anywhere in its indented body (e.g. on
// `-e CAESIUM_X=...` lines). A blank line or a new top-level line (recipe,
// comment, or variable assignment) ends the current recipe's body.
func parseJustfileRecipeEnvVars(justfile string) map[string]map[string]struct{} {
	headerRe := regexp.MustCompile(`^([A-Za-z0-9_-]+):`)
	envRe := regexp.MustCompile(`CAESIUM_[A-Z0-9_]+`)

	recipes := make(map[string]map[string]struct{})
	current := ""

	for _, line := range strings.Split(justfile, "\n") {
		if line == "" {
			current = ""
			continue
		}
		if line[0] != ' ' && line[0] != '\t' {
			if m := headerRe.FindStringSubmatch(line); m != nil {
				current = m[1]
				if _, ok := recipes[current]; !ok {
					recipes[current] = make(map[string]struct{})
				}
			} else {
				current = ""
			}
			continue
		}
		if current == "" {
			continue
		}
		for _, v := range envRe.FindAllString(line, -1) {
			recipes[current][v] = struct{}{}
		}
	}

	return recipes
}

func repoRoot(t *testing.T) string {
	t.Helper()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}

	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not locate repo root")
		}
		dir = parent
	}
}

func exampleSection(t *testing.T, doc string) string {
	t.Helper()

	startMarker := "- Example scenarios under `docs/examples/` include:"
	endMarker := "\n\nThe CLI surfaces both `caesium job apply` and `caesium job lint`;"

	start := strings.Index(doc, startMarker)
	if start < 0 {
		t.Fatalf("could not find example section start marker in docs/job-definitions.md")
	}

	section := doc[start:]
	end := strings.Index(section, endMarker)
	if end < 0 {
		t.Fatalf("could not find example section end marker in docs/job-definitions.md")
	}

	return section[:end]
}

func normalizeText(value string) string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	return strings.TrimSpace(value)
}

func hasStatusBanner(doc string) bool {
	lines := strings.Split(strings.ReplaceAll(doc, "\r\n", "\n"), "\n")
	nonEmpty := 0
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		nonEmpty++
		if strings.HasPrefix(trimmed, "> Status:") {
			return true
		}
		if nonEmpty >= 8 {
			return false
		}
	}
	return false
}

func setDifference(left, right map[string]struct{}) []string {
	diff := make([]string, 0)
	for key := range left {
		if _, ok := right[key]; !ok {
			diff = append(diff, key)
		}
	}
	sort.Strings(diff)
	return diff
}
