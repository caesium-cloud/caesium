package tf

import (
	"encoding/json"
	"fmt"
	"strings"

	tfjson "github.com/hashicorp/terraform-json"
)

// ProposalKind is the renderer selector the Console keys its registry on
// (design §5.6). The version suffix is part of the contract: a summary shape
// change is a new kind, never a silent reinterpretation of this one.
const ProposalKind = "terraform.plan.v1"

// MaxProposalResources caps the per-resource list carried in the summary.
//
// The summary is an ordinary step output, so it lands in dqlite and counts
// against the 64 KB per-task output cap. A thousand-resource plan would blow
// that cap and fail the task — which would turn a large but perfectly healthy
// plan into a red run. The counts are always exact; only the address list is
// truncated, and the summary says by how much.
const MaxProposalResources = 100

// ResourceChange is one line of the proposal's per-resource list. Only the
// address and the action: no attribute values ever appear in a proposal, which
// is what keeps a sensitive value out of the output row (design §6.4).
type ResourceChange struct {
	Address string `json:"address"`
	Action  string `json:"action"`
}

// Summary is the `proposal_summary` payload. Its JSON is what the Console's
// terraform.plan.v1 renderer parses, and what tf-apply reads to decide whether
// there is anything to apply at all.
//
// Every count is present even at zero. A renderer that has to distinguish
// "no changes" from "this key was omitted" is a renderer that will eventually
// show an empty panel for a real plan.
type Summary struct {
	// Changes is Terraform's OWN answer to "would this plan do anything",
	// carried verbatim from `plan -detailed-exitcode`.
	//
	// Without it the question is decided twice by two mechanisms: tf-plan gates
	// the artifact and the branch marker on Terraform's boolean, while tf-apply
	// gates whether to invoke Terraform on the counts below. The counts are
	// derived by actionLabel, which deliberately does not count an action set it
	// does not recognise — so the day Terraform introduces one, a plan Terraform
	// called non-empty arrives at an apply that reads all zeros, logs "proposed
	// no changes", never invokes Terraform, and the run is GREEN. That is the
	// cardinal failure (design §8): a green run that deployed nothing.
	//
	// A pointer so a proposal written before this field existed decodes as nil
	// and falls back to the counts, rather than decoding as false and skipping
	// an apply that should run.
	Changes *bool `json:"changes,omitempty"`
	Add     int   `json:"add"`
	Change  int   `json:"change"`
	Destroy int   `json:"destroy"`
	Replace int   `json:"replace"`
	Import  int   `json:"import"`
	// Outputs counts root-module output changes.
	//
	// It is a first-class count, not a detail. A plan whose only change is an
	// output value still has to be applied: `terraform output` reads the STATE,
	// so an unapplied output change leaves every consuming stack reading a
	// stale value — a green run that deployed nothing, one stack over. It also
	// keeps a refresh-only plan from reporting drift with an all-zero summary,
	// which reads as a false alarm: `plan -refresh-only -detailed-exitcode`
	// returns 2 for an output change as readily as for resource drift.
	Outputs int `json:"outputs"`
	// Resources is capped; ResourcesTruncated says how many were dropped.
	Resources          []ResourceChange `json:"resources"`
	ResourcesTruncated int              `json:"resources_truncated,omitempty"`
}

// HasChanges reports whether the plan would do anything.
//
// It is deliberately count-based rather than "was an artifact emitted": tf-apply
// decides whether to invoke Terraform from this, and a decision derived from
// the presence of a file would be a decision derived from the filesystem rather
// than from the proposal that was actually reviewed.
func (s Summary) HasChanges() bool {
	// Terraform's own answer wins whenever the producer recorded it. The counts
	// are a summary FOR PEOPLE; they must never be the thing that decides
	// whether infrastructure is touched.
	if s.Changes != nil {
		return *s.Changes
	}
	return s.Add+s.Change+s.Destroy+s.Replace+s.Import+s.Outputs > 0
}

// WithChanges records Terraform's own -detailed-exitcode answer on the summary.
func (s Summary) WithChanges(changes bool) Summary {
	s.Changes = &changes
	return s
}

// Encode renders the summary as the JSON string the marker carries.
//
// It is a STRING, not a nested object, and that is load-bearing:
// pkg/task.ParseOutput keeps only scalar values and silently DROPS a key whose
// value is an object or an array, so a summary emitted as a nested object never
// reaches dqlite, the Console, or tf-apply at all.
func (s Summary) Encode() (string, error) {
	if s.Resources == nil {
		s.Resources = []ResourceChange{}
	}
	data, err := json.Marshal(s)
	if err != nil {
		return "", fmt.Errorf("encode proposal summary: %w", err)
	}
	return string(data), nil
}

// DecodeSummary parses the string form back. tf-apply uses it to read the
// proposal its plan step produced.
func DecodeSummary(encoded string) (Summary, error) {
	var s Summary
	dec := json.NewDecoder(strings.NewReader(encoded))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return Summary{}, fmt.Errorf("decode proposal summary: %w", err)
	}
	return s, nil
}

// SummarizePlan counts a plan's resource changes and lists the first
// MaxProposalResources of them.
//
// It reads ONLY Address and Change.Actions through terraform-json's typed
// fields. That is the §6.4 security requirement realised as a type, not as a
// convention: there is no code path here that could reach an attribute value,
// so no sensitive value can reach the output row by accident. StripSensitive
// removes those fields outright before this ever runs, so the guarantee is
// enforced twice.
func SummarizePlan(plan *tfjson.Plan) Summary {
	if plan == nil {
		return Summary{Resources: []ResourceChange{}}
	}
	summary := summarizeChanges(plan.ResourceChanges)
	summary.Outputs = countOutputChanges(plan.OutputChanges)
	return summary
}

// SummarizeDrift counts the differences a refresh-only plan found between state
// and the real world.
//
// Terraform reports those in `resource_drift`, not `resource_changes`: a
// refresh-only plan's resource_changes are all no-ops by construction, because
// refresh-only deliberately ignores what the configuration wants. Summarizing
// the wrong list would report drift=true with a zero-count summary, which is
// the shape an operator would read as "false alarm" (design §6.6).
func SummarizeDrift(plan *tfjson.Plan) Summary {
	if plan == nil {
		return Summary{Resources: []ResourceChange{}}
	}
	summary := summarizeChanges(plan.ResourceDrift)
	summary.Outputs = countOutputChanges(plan.OutputChanges)
	return summary
}

// countOutputChanges counts root-module outputs the plan would move. Only the
// action set is read — StripSensitive has already reduced each Change to it.
func countOutputChanges(changes map[string]*tfjson.Change) int {
	count := 0
	for _, change := range changes {
		if change == nil || change.Actions.NoOp() {
			continue
		}
		count++
	}
	return count
}

func summarizeChanges(changes []*tfjson.ResourceChange) Summary {
	summary := Summary{Resources: []ResourceChange{}}
	for _, rc := range changes {
		if rc == nil || rc.Change == nil {
			continue
		}
		// An import with no other action still changes the world's bookkeeping,
		// so it is counted; it just has no create/update/delete to report.
		if rc.Change.Importing != nil {
			summary.Import++
		}
		action := actionLabel(rc.Change.Actions)
		if action == "" {
			continue
		}
		switch action {
		case "add":
			summary.Add++
		case "change":
			summary.Change++
		case "destroy":
			summary.Destroy++
		case "replace":
			summary.Replace++
		}
		if len(summary.Resources) < MaxProposalResources {
			summary.Resources = append(summary.Resources, ResourceChange{Address: rc.Address, Action: action})
		} else {
			summary.ResourcesTruncated++
		}
	}
	return summary
}

// actionLabel maps terraform-json's action set to the proposal vocabulary, or
// "" for a change that does nothing to infrastructure.
//
// A no-op is skipped because Terraform emits one for every unchanged resource
// under -refresh=false and for every resource in a refresh-only plan; counting
// them would make an empty plan look enormous. A read (a data source) is
// skipped for the same reason: it is not a change to anything.
func actionLabel(actions tfjson.Actions) string {
	switch {
	case actions.Replace():
		return "replace"
	case actions.Create():
		return "add"
	case actions.Update():
		return "change"
	case actions.Delete(), actions.Forget():
		return "destroy"
	default:
		// NoOp, Read, and any action set a future Terraform introduces. An
		// unrecognised set is deliberately NOT counted as a change: over-
		// counting would make every plan non-empty forever, which silently
		// disables the whole empty-plan short circuit.
		return ""
	}
}

// StripSensitive returns a copy of the plan carrying only the structural facts
// a proposal needs: the resource address, its type/name/module, its provider,
// and the action set.
//
// This is the second of §6.4's two security requirements. Step outputs land in
// dqlite and flow onward as environment variables, so `sensitive_values` — and
// in fact every attribute value, marked sensitive or not — must not survive
// into anything the runner emits. Terraform marks what it knows to be
// sensitive; it cannot know that an unmarked attribute holds a credential
// someone pasted into a variable. Dropping the values outright rather than
// redacting the marked ones is therefore strictly stronger, and it costs
// nothing: a proposal is a list of addresses and actions.
//
// Being a copy matters. Mutating the caller's plan in place would leave a
// half-sanitised object alive in the same process, and the next person to add a
// debug log would print it.
func StripSensitive(plan *tfjson.Plan) *tfjson.Plan {
	if plan == nil {
		return nil
	}
	out := &tfjson.Plan{
		FormatVersion:    plan.FormatVersion,
		TerraformVersion: plan.TerraformVersion,
		Complete:         plan.Complete,
		Timestamp:        plan.Timestamp,
	}
	out.ResourceChanges = stripResourceChanges(plan.ResourceChanges)
	out.ResourceDrift = stripResourceChanges(plan.ResourceDrift)
	// Output changes are kept as ACTION SETS ONLY, keyed by output name. The
	// name is declared in the configuration and is not a secret; the value very
	// much can be, so Before/After and the sensitivity maps are dropped like
	// every other value. The count matters (see Summary.Outputs), so dropping
	// the whole map would be dropping a signal, not a risk.
	if len(plan.OutputChanges) > 0 {
		out.OutputChanges = make(map[string]*tfjson.Change, len(plan.OutputChanges))
		for name, change := range plan.OutputChanges {
			if change == nil {
				continue
			}
			out.OutputChanges[name] = &tfjson.Change{Actions: change.Actions}
		}
	}
	// Everything else is dropped by omission: Variables, PlannedValues,
	// PriorState, Config, RelevantAttributes, Checks and DeferredChanges all
	// carry values. Constructing the copy field by field (rather than copying
	// the struct and blanking fields) means a field added by a future
	// terraform-json is dropped by DEFAULT rather than leaked until someone
	// notices.
	return out
}

func stripResourceChanges(in []*tfjson.ResourceChange) []*tfjson.ResourceChange {
	if len(in) == 0 {
		return nil
	}
	out := make([]*tfjson.ResourceChange, 0, len(in))
	for _, rc := range in {
		if rc == nil {
			continue
		}
		stripped := &tfjson.ResourceChange{
			Address:         rc.Address,
			PreviousAddress: rc.PreviousAddress,
			ModuleAddress:   rc.ModuleAddress,
			Mode:            rc.Mode,
			Type:            rc.Type,
			Name:            rc.Name,
			ProviderName:    rc.ProviderName,
		}
		if rc.Change != nil {
			stripped.Change = &tfjson.Change{Actions: rc.Change.Actions}
			if rc.Change.Importing != nil {
				// The import ID is a real-world resource identifier and can be
				// a secret in its own right (a connection string, an ARN with
				// an account in it). Only the fact of the import survives.
				stripped.Change.Importing = &tfjson.Importing{Unknown: rc.Change.Importing.Unknown}
			}
		}
		out = append(out, stripped)
	}
	return out
}
