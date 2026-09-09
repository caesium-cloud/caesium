package incident

import (
	"slices"
	"strings"
)

// playbook_class.go implements per-failure-class autonomy narrowing:
// `metadata.remediation.autonomy.perClass.<class>` (pkg/jobdef) and the same
// key in an AgentProfile playbook document.
//
// The whole point of the feature is a job that scopes autonomy differently per
// class — "auto-retry a transient_infra failure, but always ask a human about a
// schema_violation". Until this existed the block decoded to nothing, so such a
// job was enforced as if it had named no classes at all: fail-OPEN, in a
// security-relevant path (issue #416).
//
// # The merge rule
//
// A per-class block is a CONSTRAINT, never a grant. Merging it over the
// resolved base policy may only remove permissions, relative to BOTH the
// job-level block and the operator-level AgentProfile playbook the job resolved
// over. Per field:
//
//   - allow — INTERSECTION. An action must be permitted by the base AND named
//     by the class block to stay autonomous. If the base allow-list is nil
//     (unconfigured, so the tier default governs), the class block is
//     intersected against that default instead: only tier 0/1 actions survive,
//     because a class block that could name a tier-2 action there would WIDEN
//     the very policy it is narrowing.
//   - requireApproval — UNION (logical OR). Approval gates are additive, so a
//     class block can force more actions through a human but can never drop a
//     gate the base imposes.
//   - paramOverrides — MOST RESTRICTIVE. Only keys present in both survive, and
//     only the values both permit. A nil base whitelist denies every key, so it
//     stays nil rather than adopting the class block's keys. A key whose value
//     intersection comes out empty is DROPPED, not kept with an empty list: an
//     empty list reads as "any value for this key" (validateParamOverrides), so
//     keeping it would reopen exactly what the narrowing meant to close.
//
// Everything the class block does not mention is inherited unchanged.

// ForClass folds the constraint declared for one failure class into the policy
// and returns the result with PerClass CONSUMED (nil).
//
// Consuming it makes the call idempotent: ResolvePlaybook narrows once at
// resolution, Executor.Execute narrows again defensively before the tier
// decision, and the second call is a no-op. It also keeps decide() honest —
// what it reads is a single, already-narrowed policy, never a policy plus a
// pending constraint it might forget to apply.
//
// A class with no entry (including the empty class an incident opened before
// classification would carry) inherits the base policy untouched.
func (pb Playbook) ForClass(class string) Playbook {
	base := pb
	base.PerClass = nil
	if len(pb.PerClass) == 0 {
		return base
	}
	constraint, ok := pb.PerClass[strings.TrimSpace(class)]
	if !ok {
		return base
	}
	return base.narrowedBy(constraint)
}

// narrowedBy applies one class constraint to a base policy per the merge rule
// documented at the top of this file. It can only ever reduce what the base
// permits.
func (pb Playbook) narrowedBy(c Playbook) Playbook {
	out := pb
	out.PerClass = nil

	if c.Allow != nil {
		out.Allow = narrowAllow(pb.Allow, c.Allow)
	}
	if c.RequireApproval != nil {
		out.RequireApproval = unionSets(pb.RequireApproval, c.RequireApproval)
	}
	if c.ParamOverrides != nil && pb.ParamOverrides != nil {
		out.ParamOverrides = intersectParamWhitelists(pb.ParamOverrides, c.ParamOverrides)
	}
	return out
}

// narrowAllow intersects a class allow-list with the base's EFFECTIVE
// permissions.
//
// The nil base is the subtle case: nil means unconfigured, which permits tier
// 0/1 autonomously and denies tier 2 (Playbook.allowsAutonomous). Taking the
// class list wholesale there would grant any tier-2 action it named — a class
// block widening its own job. So an unconfigured base is intersected against
// the tier default, and an action whose type is not in the catalog at all is
// dropped rather than trusted.
func narrowAllow(base, class map[string]bool) map[string]bool {
	out := make(map[string]bool, len(class))
	for action, allowed := range class {
		if !allowed {
			continue
		}
		if base == nil {
			if tier, known := ActionTier(action); known && tier <= TierAutonomous {
				out[action] = true
			}
			continue
		}
		if base[action] {
			out[action] = true
		}
	}
	return out
}

// unionSets ORs two enforcement sets, returning nil only when both are nil so
// the unconfigured/empty distinction survives.
func unionSets(a, b map[string]bool) map[string]bool {
	if a == nil && b == nil {
		return nil
	}
	out := make(map[string]bool, len(a)+len(b))
	for k, v := range a {
		if v {
			out[k] = true
		}
	}
	for k, v := range b {
		if v {
			out[k] = true
		}
	}
	return out
}

// intersectSets ANDs two enforcement sets. Callers handle the nil cases, since
// nil means "unconfigured" for a base policy and "no constraint" for a class
// constraint — opposite defaults that must not be decided here.
func intersectSets(a, b map[string]bool) map[string]bool {
	out := make(map[string]bool, len(a))
	for k, v := range a {
		if v && b[k] {
			out[k] = true
		}
	}
	return out
}

// intersectParamWhitelists keeps only the keys present in BOTH whitelists, and
// for a shared key only the values both permit. An empty value list means "any
// value for this key" and therefore contributes no value constraint; a key whose
// value intersection is empty is dropped, because keeping it with an empty list
// would read as "any value" and widen.
func intersectParamWhitelists(a, b map[string][]string) map[string][]string {
	out := make(map[string][]string, len(a))
	for key, av := range a {
		bv, ok := b[key]
		if !ok {
			continue
		}
		switch {
		case len(av) == 0:
			out[key] = slices.Clone(bv)
		case len(bv) == 0:
			out[key] = slices.Clone(av)
		default:
			var shared []string
			for _, v := range av {
				if slices.Contains(bv, v) {
					shared = append(shared, v)
				}
			}
			if len(shared) == 0 {
				continue
			}
			out[key] = shared
		}
	}
	return out
}

// decodeClassPolicies turns a document's `perClass` map into constraints. A nil
// map stays nil so an absent key is distinguishable from `perClass: {}`.
func decodeClassPolicies(doc map[string]playbookClassDocument) map[string]Playbook {
	if doc == nil {
		return nil
	}
	out := make(map[string]Playbook, len(doc))
	for class, policy := range doc {
		out[strings.TrimSpace(class)] = Playbook{
			Allow:           actionSet(policy.Allow),
			ParamOverrides:  policy.ParamOverrides,
			RequireApproval: actionSet(policy.RequireApproval),
		}
	}
	return out
}

// combinePerClass merges the per-class constraints of two policies being
// resolved over each other (an AgentProfile's and a job's).
//
// Both sides are constraints, so the result must satisfy both: applying the
// combined constraint is equivalent to applying them in sequence. Allow-lists
// intersect (nil on one side is "no constraint", so the other wins),
// requireApproval unions, and paramOverrides take the most restrictive of the
// two. A job may widen its base allow-list at the JOB level — that is an
// authored policy the design permits — but it may not use a per-class block to
// erase a narrowing the operator's profile declared.
func combinePerClass(profile, job map[string]Playbook) map[string]Playbook {
	if profile == nil && job == nil {
		return nil
	}
	out := make(map[string]Playbook, len(profile)+len(job))
	for class, c := range profile {
		out[class] = c
	}
	for class, jobConstraint := range job {
		existing, ok := out[class]
		if !ok {
			out[class] = jobConstraint
			continue
		}
		out[class] = combineConstraints(existing, jobConstraint)
	}
	return out
}

// combineConstraints ANDs two class constraints into one. Unlike narrowedBy,
// neither side is a base policy: a nil field here means "this side constrains
// nothing", not "unconfigured".
func combineConstraints(a, b Playbook) Playbook {
	out := Playbook{RequireApproval: unionSets(a.RequireApproval, b.RequireApproval)}

	switch {
	case a.Allow == nil:
		out.Allow = b.Allow
	case b.Allow == nil:
		out.Allow = a.Allow
	default:
		out.Allow = intersectSets(a.Allow, b.Allow)
	}

	switch {
	case a.ParamOverrides == nil:
		out.ParamOverrides = b.ParamOverrides
	case b.ParamOverrides == nil:
		out.ParamOverrides = a.ParamOverrides
	default:
		out.ParamOverrides = intersectParamWhitelists(a.ParamOverrides, b.ParamOverrides)
	}

	return out
}

// classDocument re-encodes one class constraint into the stored document shape.
func (pb Playbook) classDocument() map[string]any {
	doc := map[string]any{}
	if pb.Allow != nil {
		doc["allow"] = sortedTrueKeys(pb.Allow)
	}
	if pb.RequireApproval != nil {
		doc["requireApproval"] = sortedTrueKeys(pb.RequireApproval)
	}
	if pb.ParamOverrides != nil {
		doc["paramOverrides"] = pb.ParamOverrides
	}
	return doc
}
