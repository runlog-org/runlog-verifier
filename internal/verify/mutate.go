package verify

import (
	"bytes"
	"errors"
	"fmt"
	"strings"

	"github.com/runlog/verifier/internal/verify/runner"
)

// mutationBaseline captures the un-mutated execution context for a unit/function
// run so mutations can be applied as deltas. Each branch's setup, action,
// inputs, and baseline ExecResult are kept side-by-side; runOneMutation pulls
// the per-branch slice based on the mutation's branch field.
type mutationBaseline struct {
	failedSetup, failedAction   []runner.Step
	workingSetup, workingAction []runner.Step
	failedInputs, workingInputs map[string]any
	failedRes, workingRes       runner.ExecResult
	diff                        map[string]any
	timeout                     float64
}

// mutationOutcome is the classified result of applying a mutation to one
// branch. Mirrors the schema's expected_result enum (minus inapplicable,
// which is surfaced separately as a skip signal).
type mutationOutcome string

const (
	outcomeFail      mutationOutcome = "fail"
	outcomePass      mutationOutcome = "pass"
	outcomeUnchanged mutationOutcome = "unchanged"
)

// supportedStrategies lists the strategies this verifier slice can actually
// apply. Anything else degrades the entry to tier_unsupported.
var supportedStrategies = map[string]struct{}{
	"set_literal_value": {},
	"mutate_fixture":    {},
}

// runMutations applies each declared mutation and verifies the actual outcome
// matches the declared expectation. Returns (reasons, supported).
//
//   - supported=false → at least one mutation strategy is not yet implemented;
//     caller must surface tier_unsupported. reasons will name the strategy.
//   - supported=true, len(reasons)==0 → all mutations matched their expectations.
//   - supported=true, len(reasons)>0 → at least one mutation produced an
//     unexpected outcome; caller must surface rejected.
func runMutations(e *Entry, b mutationBaseline) ([]Reason, bool) {
	var reasons []Reason
	supported := true

	for i, m := range e.Verification.Mutations {
		mr, ok := runOneMutation(e, b, m, i)
		if !ok {
			supported = false
		}
		reasons = append(reasons, mr...)
	}
	return reasons, supported
}

// runOneMutation applies mutation m (1-indexed via idx) to each target
// branch, re-runs that branch, classifies the outcome, and compares to the
// declared expectation. The bool return is false when the strategy is not
// implemented in this build, signalling tier_unsupported to the caller.
func runOneMutation(e *Entry, b mutationBaseline, m Mutation, idx int) ([]Reason, bool) {
	if _, ok := supportedStrategies[m.Strategy]; !ok {
		return []Reason{{
			Code: "mutation_strategy_unsupported",
			Message: fmt.Sprintf(
				"mutation #%d strategy %q is not yet implemented; supported in this build: set_literal_value, mutate_fixture",
				idx+1, m.Strategy,
			),
		}}, false
	}

	branches := branchesFor(m)
	var reasons []Reason

	for _, branch := range branches {
		expected, inapplicable, hasExp := expectedOutcomeFor(m, branch)
		if !hasExp {
			// No expectation declared for this branch — schema would have
			// flagged it; here we silently skip, which is the safe default.
			continue
		}
		if inapplicable {
			continue
		}

		setup, action, inputs, baselineRes, ok := selectBranch(b, branch)
		if !ok {
			reasons = append(reasons, Reason{
				Code: "mutation_unknown_branch",
				Message: fmt.Sprintf("mutation #%d targets unknown branch %q",
					idx+1, branch),
			})
			continue
		}

		mutInputs, err := applyInputSubstitution(inputs, m.Target, m.NewValue)
		if err != nil {
			reasons = append(reasons, Reason{
				Code: "mutation_target_invalid",
				Message: fmt.Sprintf("mutation #%d (%s) on %s: %v",
					idx+1, m.Strategy, branch, err),
			})
			continue
		}

		got, err := runner.RunPython(setup, action, mutInputs, b.timeout)
		if err != nil {
			reasons = append(reasons, Reason{
				Code: "mutation_runner_error",
				Message: fmt.Sprintf("mutation #%d (%s) on %s: %v",
					idx+1, m.Strategy, branch, err),
			})
			continue
		}

		actual := classifyOutcome(branch, got, baselineRes, b.diff)
		if actual != expected {
			reasons = append(reasons, Reason{
				Code: "mutation_outcome_mismatch",
				Message: fmt.Sprintf("mutation #%d (%s) on %s: expected %s, got %s",
					idx+1, m.Strategy, branch, expected, actual),
			})
		}
	}
	return reasons, true
}

// branchesFor returns the branch list a mutation should be applied to. An
// explicit "both" expands to both branches; an explicit single branch is
// returned as-is; an empty branch defaults to working_approach (matches the
// implicit-discrimination rule in checkMutationDiscriminating).
func branchesFor(m Mutation) []string {
	switch m.Branch {
	case "both":
		return []string{"failed_approach", "working_approach"}
	case "failed_approach", "working_approach":
		return []string{m.Branch}
	default:
		return []string{"working_approach"}
	}
}

// selectBranch returns the per-branch baseline tuple for the given branch.
// The bool return is false for unknown branch names.
func selectBranch(b mutationBaseline, branch string) ([]runner.Step, []runner.Step, map[string]any, runner.ExecResult, bool) {
	switch branch {
	case "failed_approach":
		return b.failedSetup, b.failedAction, b.failedInputs, b.failedRes, true
	case "working_approach":
		return b.workingSetup, b.workingAction, b.workingInputs, b.workingRes, true
	}
	return nil, nil, nil, runner.ExecResult{}, false
}

// expectedOutcomeFor resolves the expected outcome for one branch. Returns
// (expected, isInapplicable, hasExpectation). expected_branch_outcome takes
// precedence over expected_result when both are set on a single mutation.
func expectedOutcomeFor(m Mutation, branch string) (mutationOutcome, bool, bool) {
	if v, ok := m.ExpectedBranchOutcome[branch]; ok {
		return canonicalizeOutcome(v)
	}
	if m.ExpectedResult != "" {
		return canonicalizeOutcome(m.ExpectedResult)
	}
	return "", false, false
}

// canonicalizeOutcome maps the schema's enum to the internal outcome enum.
// "assertion_does_not_match" folds into outcomeFail (consistent with
// checkMutationStructure); "inapplicable" returns isInapplicable=true so
// the caller skips the mutation.
func canonicalizeOutcome(s string) (mutationOutcome, bool, bool) {
	switch s {
	case "fail", "assertion_does_not_match":
		return outcomeFail, false, true
	case "pass":
		return outcomePass, false, true
	case "unchanged":
		return outcomeUnchanged, false, true
	case "inapplicable":
		return "", true, true
	}
	return "", false, false
}

// classifyOutcome compares a re-run ExecResult against the un-mutated baseline
// and the differential spec for the same branch. Returns:
//   - outcomeUnchanged when the re-run is byte-equivalent to the baseline.
//   - outcomeFail when the re-run violates the branch's spec (raised when
//     should return, returned when should raise, wrong type/value/exception).
//   - outcomePass otherwise (re-run still satisfies the spec but produced a
//     different concrete value than the baseline).
func classifyOutcome(branch string, got, baseline runner.ExecResult, diff map[string]any) mutationOutcome {
	if execResultsEqual(got, baseline) {
		return outcomeUnchanged
	}
	var retKey, raiseKey string
	if branch == "failed_approach" {
		retKey, raiseKey = "failed_branch_must_return", "failed_branch_must_raise"
	} else {
		retKey, raiseKey = "working_branch_must_return", "working_branch_must_raise"
	}
	if reasons := matchOutcome(branch, got, diff, retKey, raiseKey); len(reasons) > 0 {
		return outcomeFail
	}
	return outcomePass
}

// execResultsEqual returns true when two ExecResults are observationally
// identical at the granularity the verifier compares against — both must be
// raised-or-not, exception fields equal if raised, type+canonical-JSON or
// repr equal if returned.
func execResultsEqual(a, b runner.ExecResult) bool {
	if a.Raised != b.Raised {
		return false
	}
	if a.Raised {
		return a.Exception == b.Exception && a.Message == b.Message
	}
	if a.TypeName != b.TypeName || a.Serializable != b.Serializable {
		return false
	}
	if !a.Serializable {
		return a.Repr == b.Repr
	}
	aBytes, errA := canonicalizeJSON(a.JSONValue)
	bBytes, errB := canonicalizeJSON(b.JSONValue)
	if errA != nil || errB != nil {
		return false
	}
	return bytes.Equal(aBytes, bBytes)
}

// normalizeInputTarget reduces target paths down to the bare $TOKEN.
// Accepts shapes seen in seeds: "$LITERAL_2", "$ITEMS",
// "differential.inputs.$PAYLOAD", "failed_approach.$INPUT".
// Returns the bare $TOKEN, or the original string if no $-prefixed token is
// found.
func normalizeInputTarget(target string) string {
	if i := strings.LastIndex(target, "$"); i >= 0 {
		return target[i:]
	}
	return target
}

// applyInputSubstitution returns a new inputs map with the placeholder set
// to newValue. The map is copied so concurrent re-runs see independent
// inputs. Both bare ($KEY) and stripped-prefix (KEY) are checked; whichever
// exists in inputs is overwritten. If neither exists, the bare $-prefixed
// key is added so the runner's TrimPrefix logic can still bind it.
func applyInputSubstitution(inputs map[string]any, target string, newValue any) (map[string]any, error) {
	if target == "" {
		return nil, errors.New("mutation target is empty")
	}
	key := normalizeInputTarget(target)
	if !strings.HasPrefix(key, "$") {
		return nil, fmt.Errorf("input target must include a $-prefixed placeholder, got %q", target)
	}
	out := make(map[string]any, len(inputs)+1)
	for k, v := range inputs {
		out[k] = v
	}
	bare := strings.TrimPrefix(key, "$")
	switch {
	case mapHas(out, key):
		out[key] = newValue
	case mapHas(out, bare):
		out[bare] = newValue
	default:
		out[key] = newValue
	}
	return out, nil
}

func mapHas(m map[string]any, k string) bool {
	_, ok := m[k]
	return ok
}
