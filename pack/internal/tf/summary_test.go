package tf

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-exec/tfexec"
	tfjson "github.com/hashicorp/terraform-json"
)

func change(address string, actions ...tfjson.Action) *tfjson.ResourceChange {
	return &tfjson.ResourceChange{
		Address: address,
		Type:    strings.SplitN(address, ".", 2)[0],
		Change:  &tfjson.Change{Actions: actions},
	}
}

func TestSummarizePlanCountsEachActionKind(t *testing.T) {
	plan := &tfjson.Plan{ResourceChanges: []*tfjson.ResourceChange{
		change("null_resource.a", tfjson.ActionCreate),
		change("null_resource.b", tfjson.ActionUpdate),
		change("null_resource.c", tfjson.ActionDelete),
		change("null_resource.d", tfjson.ActionDelete, tfjson.ActionCreate),
		change("null_resource.e", tfjson.ActionCreate, tfjson.ActionDelete),
		// A no-op and a data-source read are not changes to anything. Counting
		// them would make every plan non-empty forever, which silently disables
		// the empty-plan short circuit the whole design leans on.
		change("null_resource.f", tfjson.ActionNoop),
		change("data.null_data_source.g", tfjson.ActionRead),
	}}

	got := SummarizePlan(plan)
	if got.Add != 1 || got.Change != 1 || got.Destroy != 1 || got.Replace != 2 {
		t.Fatalf("counts = %+v", got)
	}
	if len(got.Resources) != 5 {
		t.Fatalf("resource list = %+v", got.Resources)
	}
	if !got.HasChanges() {
		t.Fatal("HasChanges is false for a plan with changes")
	}
}

func TestSummarizePlanOfAnEmptyPlanHasChangesFalse(t *testing.T) {
	got := SummarizePlan(&tfjson.Plan{ResourceChanges: []*tfjson.ResourceChange{
		change("null_resource.a", tfjson.ActionNoop),
	}})
	if got.HasChanges() {
		t.Fatalf("a no-op plan reports changes: %+v", got)
	}
	encoded, err := got.Encode()
	if err != nil {
		t.Fatal(err)
	}
	// Every count present at zero, and an empty (not null) resource list: a
	// renderer that has to tell "zero" from "key absent" eventually shows an
	// empty panel for a real plan.
	for _, want := range []string{`"add":0`, `"change":0`, `"destroy":0`, `"replace":0`, `"import":0`, `"resources":[]`} {
		if !strings.Contains(encoded, want) {
			t.Fatalf("encoded summary is missing %s: %s", want, encoded)
		}
	}
	if SummarizePlan(nil).HasChanges() {
		t.Fatal("a nil plan reports changes")
	}
}

func TestSummarizePlanCountsImportsAndCapsTheResourceList(t *testing.T) {
	importing := change("null_resource.imported", tfjson.ActionNoop)
	importing.Change.Importing = &tfjson.Importing{ID: "i-0123456789"}

	changes := []*tfjson.ResourceChange{importing}
	for i := range MaxProposalResources + 25 {
		changes = append(changes, change("null_resource.bulk"+string(rune('a'+i%26)), tfjson.ActionCreate))
	}

	got := SummarizePlan(&tfjson.Plan{ResourceChanges: changes})
	if got.Import != 1 {
		t.Fatalf("import count = %d", got.Import)
	}
	if len(got.Resources) != MaxProposalResources {
		t.Fatalf("resource list is not capped: %d entries", len(got.Resources))
	}
	if got.ResourcesTruncated != 25 {
		t.Fatalf("truncation count = %d, want 25", got.ResourcesTruncated)
	}
	// The counts are exact even when the list is truncated: an operator reading
	// "100 to add" for a 125-resource plan would approve the wrong thing.
	if got.Add != MaxProposalResources+25 {
		t.Fatalf("add count = %d, want the exact total", got.Add)
	}
	encoded, err := got.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > 65536 {
		t.Fatalf("a capped summary still exceeds the per-task output limit: %d bytes", len(encoded))
	}
}

// A refresh-only plan reports its findings in resource_drift, and its
// resource_changes are all no-ops by construction. Summarizing the wrong list
// would report drift=true with a zero-count summary — the shape an operator
// reads as "false alarm".
func TestSummarizeDriftReadsResourceDriftNotResourceChanges(t *testing.T) {
	plan := &tfjson.Plan{
		ResourceChanges: []*tfjson.ResourceChange{change("null_resource.a", tfjson.ActionNoop)},
		ResourceDrift:   []*tfjson.ResourceChange{change("null_resource.a", tfjson.ActionDelete)},
	}
	drift := SummarizeDrift(plan)
	if drift.Destroy != 1 || !drift.HasChanges() {
		t.Fatalf("drift summary = %+v", drift)
	}
	if SummarizePlan(plan).HasChanges() {
		t.Fatal("the plan half of a refresh-only plan should be empty")
	}
}

func TestEncodeDecodeSummaryRoundTrips(t *testing.T) {
	original := Summary{Add: 2, Change: 1, Destroy: 3, Replace: 1, Import: 1,
		Resources: []ResourceChange{{Address: "null_resource.a", Action: "add"}}}
	encoded, err := original.Encode()
	if err != nil {
		t.Fatal(err)
	}
	// The Console's renderer JSON.parses the value, so it must be a string that
	// contains an object — not an object.
	var asObject map[string]any
	if err := json.Unmarshal([]byte(encoded), &asObject); err != nil {
		t.Fatalf("the encoded summary is not a JSON object: %v", err)
	}
	decoded, err := DecodeSummary(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Add != 2 || decoded.Destroy != 3 || len(decoded.Resources) != 1 {
		t.Fatalf("round trip lost data: %+v", decoded)
	}

	// A summary shape this binary does not recognise must not be silently
	// reinterpreted: tf-apply decides whether to touch infrastructure from it.
	if _, err := DecodeSummary(`{"add":1,"unexpected":true}`); err == nil {
		t.Fatal("DecodeSummary accepted an unknown field")
	}
	if _, err := DecodeSummary("not json"); err == nil {
		t.Fatal("DecodeSummary accepted a non-JSON value")
	}
}

// The proposal must survive the round trip through pkg/task's scalar-only
// output map. This pins the two halves of that contract the E1 renderer depends
// on: the value is a string, and it parses to an object with numeric counts and
// a resources array.
func TestEncodedSummaryMatchesTheConsoleContract(t *testing.T) {
	encoded, err := Summary{Add: 2, Change: 1, Resources: []ResourceChange{
		{Address: "aws_instance.web", Action: "add"},
	}}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Add       float64 `json:"add"`
		Change    float64 `json:"change"`
		Destroy   float64 `json:"destroy"`
		Resources []struct {
			Address string `json:"address"`
			Action  string `json:"action"`
		} `json:"resources"`
	}
	if err := json.Unmarshal([]byte(encoded), &parsed); err != nil {
		t.Fatalf("the renderer's shape does not parse: %v", err)
	}
	if parsed.Add != 2 || parsed.Change != 1 || parsed.Destroy != 0 {
		t.Fatalf("counts = %+v", parsed)
	}
	if len(parsed.Resources) != 1 || parsed.Resources[0].Address != "aws_instance.web" {
		t.Fatalf("resources = %+v", parsed.Resources)
	}
}

// §6.4's plan-side security requirement. The values are dropped outright rather
// than redacted: Terraform marks what it knows to be sensitive, and cannot know
// that an unmarked attribute holds a credential someone pasted into a variable.
func TestStripSensitiveRemovesEveryValueBearingField(t *testing.T) {
	const secret = "hunter2-do-not-leak"
	plan := &tfjson.Plan{
		FormatVersion:    "1.2",
		TerraformVersion: "1.15.9",
		Variables:        map[string]*tfjson.PlanVariable{"password": {Value: secret}},
		PlannedValues: &tfjson.StateValues{
			Outputs: map[string]*tfjson.StateOutput{"token": {Sensitive: true, Value: secret}},
		},
		OutputChanges: map[string]*tfjson.Change{"token": {
			Actions: tfjson.Actions{tfjson.ActionCreate}, After: secret, AfterSensitive: true,
		}},
		ResourceChanges: []*tfjson.ResourceChange{{
			Address:      "random_password.admin",
			Type:         "random_password",
			Name:         "admin",
			ProviderName: "registry.terraform.io/hashicorp/random",
			Change: &tfjson.Change{
				Actions:         tfjson.Actions{tfjson.ActionCreate},
				After:           map[string]any{"result": secret},
				AfterSensitive:  map[string]any{"result": true},
				BeforeSensitive: map[string]any{"result": true},
				AfterUnknown:    map[string]any{"id": true},
				GeneratedConfig: "resource \"x\" { password = \"" + secret + "\" }",
				Importing:       &tfjson.Importing{ID: secret},
			},
		}},
	}

	stripped := StripSensitive(plan)
	encoded, err := json.Marshal(stripped)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), secret) {
		t.Fatalf("a value survived the strip:\n%s", encoded)
	}
	for _, name := range []string{"sensitive_values", "after_sensitive", "before_sensitive", "planned_values", "configuration", "prior_state", "variables", "output_changes"} {
		if strings.Contains(string(encoded), name) {
			t.Fatalf("%q survived the strip:\n%s", name, encoded)
		}
	}

	// The facts a proposal actually needs are still there.
	if len(stripped.ResourceChanges) != 1 {
		t.Fatalf("the strip dropped the resource change: %+v", stripped)
	}
	rc := stripped.ResourceChanges[0]
	if rc.Address != "random_password.admin" || !rc.Change.Actions.Create() {
		t.Fatalf("the strip lost the address or the action: %+v", rc)
	}
	if rc.Change.Importing == nil {
		t.Fatal("the strip lost the fact of the import")
	}
	if rc.Change.Importing.ID != "" {
		t.Fatal("the import ID survived; it is a real-world identifier and can be a secret itself")
	}

	// The caller's plan must be untouched: a half-sanitised object alive in the
	// same process is one debug log away from being printed.
	if plan.ResourceChanges[0].Change.After == nil {
		t.Fatal("StripSensitive mutated its argument")
	}
	if StripSensitive(nil) != nil {
		t.Fatal("StripSensitive(nil) should be nil")
	}
}

func TestOutputValueRendersScalarsVerbatimAndStructuresAsJSON(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"string", `"vpc-123"`, "vpc-123"},
		{"string with quotes", `"a \"b\" c"`, `a "b" c`},
		{"number", `42`, "42"},
		{"bool", `true`, "true"},
		{"object", "{\n  \"a\": 1,\n  \"b\": \"two\"\n}", `{"a":1,"b":"two"}`},
		{"list", "[\n  1,\n  2\n]", `[1,2]`},
		// A null output decodes cleanly into the empty string, which is what a
		// downstream TF_VAR_ should receive for "this resolved to nothing".
		{"null", `null`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := OutputValue(tfexec.OutputMeta{Value: json.RawMessage(c.raw)})
			if err != nil {
				t.Fatalf("OutputValue: %v", err)
			}
			if got != c.want {
				t.Fatalf("OutputValue = %q, want %q", got, c.want)
			}
		})
	}
}

// §6.4's apply-side security requirement, at the type level: the Sensitive flag
// comes from terraform-json's typed OutputMeta, not from parsing CLI text.
func TestPublishableOutputsWithholdsEverySensitiveOutput(t *testing.T) {
	values, withheld, err := PublishableOutputs(map[string]tfexec.OutputMeta{
		"vpc_id":      {Value: json.RawMessage(`"vpc-1"`)},
		"admin_token": {Sensitive: true, Value: json.RawMessage(`"s3cr3t"`)},
		"tags":        {Value: json.RawMessage(`{"env":"test"}`)},
		"db_password": {Sensitive: true, Value: json.RawMessage(`"another"`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 2 || values["vpc_id"] != "vpc-1" || values["tags"] != `{"env":"test"}` {
		t.Fatalf("published values = %v", values)
	}
	if strings.Join(withheld, ",") != "admin_token,db_password" {
		t.Fatalf("withheld = %v (want the sensitive outputs, in a stable order)", withheld)
	}
	for _, v := range values {
		if strings.Contains(v, "s3cr3t") || strings.Contains(v, "another") {
			t.Fatalf("a sensitive value reached the published map: %v", values)
		}
	}
}

func TestExportVariableRejectsAnythingThatIsNotAVariableName(t *testing.T) {
	for _, bad := range []string{"", "1leading", "has space", "has=equals", "has\nnewline", "-flag"} {
		if err := ExportVariable(bad, "x"); err == nil {
			t.Fatalf("ExportVariable accepted %q", bad)
		}
	}
	t.Setenv("TF_VAR_ok_name", "")
	if err := ExportVariable("ok_name", "value"); err != nil {
		t.Fatalf("ExportVariable: %v", err)
	}
}
