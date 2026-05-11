package verify

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/runlog-org/runlog-verifier/internal/verify/runner"
)

// mutationBaseYAML is a unit-tier entry where the working branch returns
// the input $X, the failed branch returns 0, and inputs $X=5 are shared.
// Tests below substitute the differential spec and the mutations block to
// exercise each branch of the mutation framework.
const mutationBaseYAML = `
unit_id: unit-mutation-test
domain: [test]
version_constraints: { spec: { name: test } }
failed_approach:
  description: returns 0 always
  setup: []
  action:
    - { type: code, lang: python, body: "$RESULT = 0" }
  assertion: { type: returns, expect: fail }
working_approach:
  description: returns $X
  setup: []
  action:
    - { type: code, lang: python, body: "$RESULT = $X" }
  assertion: { type: returns, expect: success }
verification:
  type: unit
  isolation: function
  differential:
    inputs:
      $X: 5
    failed_branch_must_return: { type: int, value_equals: 0 }
    working_branch_must_return: __WORKING_SPEC__
  mutations: __MUTATIONS__
  timeout_seconds: 5
`

// buildMutationYAML interpolates the working-branch return spec and the
// mutations YAML block into mutationBaseYAML. workingSpec is rendered as
// inline YAML; mutations is the raw `mutations:` value (list block).
func buildMutationYAML(workingSpec, mutations string) string {
	y := strings.Replace(mutationBaseYAML, "__WORKING_SPEC__", workingSpec, 1)
	return strings.Replace(y, "__MUTATIONS__", mutations, 1)
}

func TestMutationFixtureExpectedFail(t *testing.T) {
	skipIfNoPython3(t)
	mutations := `
    - strategy: mutate_fixture
      target: $X
      new_value: 99
      branch: working_approach
      expected_result: fail
    - strategy: mutate_fixture
      target: $X
      new_value: 5
      branch: working_approach
      expected_result: unchanged
`
	yaml := buildMutationYAML("{ type: int, value_equals: 5 }", mutations)
	res, err := Run([]byte(yaml))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "verified" {
		t.Fatalf("status=%q, want verified (reasons=%v)", res.Status, res.Reasons)
	}
}

func TestMutationFixtureExpectedPass(t *testing.T) {
	skipIfNoPython3(t)
	// Spec requires only type: int (no value_equals). Mutating $X from 5
	// to 99 still satisfies the spec but the value changed → outcomePass.
	mutations := `
    - strategy: mutate_fixture
      target: $X
      new_value: 99
      branch: working_approach
      expected_result: pass
`
	yaml := buildMutationYAML("{ type: int }", mutations)
	res, err := Run([]byte(yaml))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "verified" {
		t.Fatalf("status=%q, want verified (reasons=%v)", res.Status, res.Reasons)
	}
}

func TestMutationFixtureExpectedUnchanged(t *testing.T) {
	skipIfNoPython3(t)
	// Mutating $X to its current value 5 is a no-op → outcomeUnchanged.
	mutations := `
    - strategy: mutate_fixture
      target: $X
      new_value: 5
      branch: working_approach
      expected_result: unchanged
`
	yaml := buildMutationYAML("{ type: int, value_equals: 5 }", mutations)
	res, err := Run([]byte(yaml))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "verified" {
		t.Fatalf("status=%q, want verified (reasons=%v)", res.Status, res.Reasons)
	}
}

func TestMutationOutcomeMismatch(t *testing.T) {
	skipIfNoPython3(t)
	// Declares expected_result: fail but actually nothing changes
	// (new_value equals current value) → outcomeUnchanged → mismatch.
	mutations := `
    - strategy: mutate_fixture
      target: $X
      new_value: 5
      branch: working_approach
      expected_result: fail
`
	yaml := buildMutationYAML("{ type: int, value_equals: 5 }", mutations)
	res, err := Run([]byte(yaml))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "rejected" {
		t.Fatalf("status=%q, want rejected (reasons=%v)", res.Status, res.Reasons)
	}
	if !hasReason(res.Reasons, "mutation_outcome_mismatch") {
		t.Fatalf("expected mutation_outcome_mismatch, got %v", res.Reasons)
	}
	// Message must name strategy, branch, expected, got.
	var msg string
	for _, r := range res.Reasons {
		if r.Code == "mutation_outcome_mismatch" {
			msg = r.Message
			break
		}
	}
	for _, want := range []string{"mutate_fixture", "working_approach", "expected fail", "got unchanged"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q missing %q", msg, want)
		}
	}
}

func TestMutationSetLiteralValue(t *testing.T) {
	skipIfNoPython3(t)
	// Use $LITERAL_1 as the input key and the target. Same shape as
	// fixture but exercises the set_literal_value strategy branch.
	yaml := `
unit_id: unit-mutation-set-literal
domain: [test]
version_constraints: { spec: { name: test } }
failed_approach:
  description: returns 0
  setup: []
  action:
    - { type: code, lang: python, body: "$RESULT = 0" }
  assertion: { type: returns, expect: fail }
working_approach:
  description: returns $LITERAL_1
  setup: []
  action:
    - { type: code, lang: python, body: "$RESULT = $LITERAL_1" }
  assertion: { type: returns, expect: success }
verification:
  type: unit
  isolation: function
  differential:
    inputs:
      $LITERAL_1: 7
    failed_branch_must_return: { type: int, value_equals: 0 }
    working_branch_must_return: { type: int, value_equals: 7 }
  mutations:
    - strategy: set_literal_value
      target: $LITERAL_1
      new_value: 42
      branch: working_approach
      expected_result: fail
  timeout_seconds: 5
`
	res, err := Run([]byte(yaml))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "verified" {
		t.Fatalf("status=%q, want verified (reasons=%v)", res.Status, res.Reasons)
	}
}

func TestMutationStrategyUnsupported(t *testing.T) {
	skipIfNoPython3(t)
	// custom is not implemented in this build; entry must degrade to
	// tier_unsupported with a message naming the strategy.
	mutations := `
    - strategy: custom
      target: working_approach.action
      token: foo
      new_value: bar
      branch: working_approach
      expected_result: fail
`
	yaml := buildMutationYAML("{ type: int, value_equals: 5 }", mutations)
	res, err := Run([]byte(yaml))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "tier_unsupported" {
		t.Fatalf("status=%q, want tier_unsupported (reasons=%v)", res.Status, res.Reasons)
	}
	if !hasReason(res.Reasons, "mutation_strategy_unsupported") {
		t.Fatalf("expected mutation_strategy_unsupported, got %v", res.Reasons)
	}
	var msg string
	for _, r := range res.Reasons {
		if r.Code == "mutation_strategy_unsupported" {
			msg = r.Message
			break
		}
	}
	if !strings.Contains(msg, "custom") {
		t.Errorf("message %q does not name custom", msg)
	}
}

func TestMutationExpectedBranchOutcome(t *testing.T) {
	skipIfNoPython3(t)
	// Mutation with branch: both and per-branch expectations. The failed
	// branch action does not reference $X so mutating $X is a no-op there
	// (unchanged). The working branch returns $X, so mutating breaks the
	// value_equals spec (fail).
	mutations := `
    - strategy: mutate_fixture
      target: $X
      new_value: 99
      branch: both
      expected_branch_outcome:
        failed_approach: unchanged
        working_approach: fail
`
	yaml := buildMutationYAML("{ type: int, value_equals: 5 }", mutations)
	res, err := Run([]byte(yaml))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "verified" {
		t.Fatalf("status=%q, want verified (reasons=%v)", res.Status, res.Reasons)
	}
}

func TestMutationInapplicableSkipped(t *testing.T) {
	skipIfNoPython3(t)
	// One inapplicable mutation (skipped) plus one valid one (verified).
	mutations := `
    - strategy: mutate_fixture
      target: $X
      new_value: 99
      branch: working_approach
      expected_result: inapplicable
    - strategy: mutate_fixture
      target: $X
      new_value: 5
      branch: working_approach
      expected_result: unchanged
`
	yaml := buildMutationYAML("{ type: int, value_equals: 5 }", mutations)
	res, err := Run([]byte(yaml))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "verified" {
		t.Fatalf("status=%q, want verified (reasons=%v)", res.Status, res.Reasons)
	}
}

// swapBaseYAML is a unit-tier entry whose working branch action invokes a
// function on a list literal. The function name is interpolated via __FN__
// and the working-branch return spec via __WORKING_SPEC__ so each swap test
// can pin a different baseline + spec without rewriting the surrounding
// scaffolding. The failed branch always returns 0; inputs are empty.
const swapBaseYAML = `
unit_id: unit-mutation-swap-test
domain: [test]
version_constraints: { spec: { name: test } }
failed_approach:
  description: returns 0
  setup: []
  action:
    - { type: code, lang: python, body: "$RESULT = 0" }
  assertion: { type: returns, expect: fail }
working_approach:
  description: invokes a function on [1,2,3]
  setup: []
  action:
    - { type: code, lang: python, body: "$RESULT = __FN__([1,2,3])" }
  assertion: { type: returns, expect: success }
verification:
  type: unit
  isolation: function
  differential:
    failed_branch_must_return: { type: int, value_equals: 0 }
    working_branch_must_return: __WORKING_SPEC__
  mutations: __MUTATIONS__
  timeout_seconds: 5
`

// buildSwapYAML interpolates the function name, working-branch return spec,
// and the mutations YAML block into swapBaseYAML.
func buildSwapYAML(fn, workingSpec, mutations string) string {
	y := strings.Replace(swapBaseYAML, "__FN__", fn, 1)
	y = strings.Replace(y, "__WORKING_SPEC__", workingSpec, 1)
	return strings.Replace(y, "__MUTATIONS__", mutations, 1)
}

func TestMutationSwapFunctionCall(t *testing.T) {
	skipIfNoPython3(t)
	// Baseline working action: $RESULT = sum([1,2,3]) → 6. Spec accepts any
	// int (no value_equals). Swap sum→max yields max([1,2,3]) → 3, which
	// still satisfies the spec but differs from the baseline → outcomePass.
	mutations := `
    - strategy: swap_function_call
      target: working_approach.action
      token: sum
      new_value: max
      branch: working_approach
      expected_result: pass
`
	yaml := buildSwapYAML("sum", "{ type: int }", mutations)
	res, err := Run([]byte(yaml))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "verified" {
		t.Fatalf("status=%q, want verified (reasons=%v)", res.Status, res.Reasons)
	}
}

func TestMutationSwapFunctionCallExpectedFail(t *testing.T) {
	skipIfNoPython3(t)
	// Baseline: sum([1,2,3]) → 6, spec: value_equals: 6. Swap to max yields
	// 3, which violates value_equals → outcomeFail. Mutation declares
	// expected_result: fail → verified.
	mutations := `
    - strategy: swap_function_call
      target: working_approach.action
      token: sum
      new_value: max
      branch: working_approach
      expected_result: fail
`
	yaml := buildSwapYAML("sum", "{ type: int, value_equals: 6 }", mutations)
	res, err := Run([]byte(yaml))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "verified" {
		t.Fatalf("status=%q, want verified (reasons=%v)", res.Status, res.Reasons)
	}
}

func TestMutationSwapFunctionCallTargetIsName(t *testing.T) {
	skipIfNoPython3(t)
	// Shape B: no token field; target is the bare identifier.
	mutations := `
    - strategy: swap_function_call
      target: sum
      new_value: max
      branch: working_approach
      expected_result: pass
`
	yaml := buildSwapYAML("sum", "{ type: int }", mutations)
	res, err := Run([]byte(yaml))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "verified" {
		t.Fatalf("status=%q, want verified (reasons=%v)", res.Status, res.Reasons)
	}
}

func TestMutationSwapIdentifier(t *testing.T) {
	skipIfNoPython3(t)
	// swap_identifier routes through the same helper. Same shape as the
	// swap_function_call tests; verifies the strategy is supported.
	mutations := `
    - strategy: swap_identifier
      target: working_approach.action
      token: sum
      new_value: max
      branch: working_approach
      expected_result: pass
`
	yaml := buildSwapYAML("sum", "{ type: int }", mutations)
	res, err := Run([]byte(yaml))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "verified" {
		t.Fatalf("status=%q, want verified (reasons=%v)", res.Status, res.Reasons)
	}
}

func TestMutationSwapWordBoundary(t *testing.T) {
	skipIfNoPython3(t)
	// Baseline action references sum_check (which contains "sum"); swap
	// targets sum→max. Word boundaries prevent the substring match — there
	// is no \b between "m" and "_" since "_" is a word character. After the
	// Bug 1 zero-match guard landed, this is no longer a silent
	// outcome=unchanged path: applySourceMutation surfaces a typed error,
	// runOneMutation rejects the mutation as mutation_target_invalid, and
	// the seed author sees the offending token in the message.
	// Setup defines sum_check so the working baseline runs successfully.
	yaml := `
unit_id: unit-mutation-swap-boundary
domain: [test]
version_constraints: { spec: { name: test } }
failed_approach:
  description: returns 0
  setup: []
  action:
    - { type: code, lang: python, body: "$RESULT = 0" }
  assertion: { type: returns, expect: fail }
working_approach:
  description: invokes sum_check
  setup:
    - { type: code, lang: python, body: "def sum_check(xs): return sum(xs)" }
  action:
    - { type: code, lang: python, body: "$RESULT = sum_check([1,2,3])" }
  assertion: { type: returns, expect: success }
verification:
  type: unit
  isolation: function
  differential:
    failed_branch_must_return: { type: int, value_equals: 0 }
    working_branch_must_return: { type: int, value_equals: 6 }
  mutations:
    - strategy: swap_function_call
      target: working_approach.action
      token: sum
      new_value: max
      branch: working_approach
      expected_result: unchanged
  timeout_seconds: 5
`
	res, err := Run([]byte(yaml))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "rejected" {
		t.Fatalf("status=%q, want rejected (reasons=%v)", res.Status, res.Reasons)
	}
	if !hasReason(res.Reasons, "mutation_target_invalid") {
		t.Fatalf("expected mutation_target_invalid, got %v", res.Reasons)
	}
	var msg string
	for _, r := range res.Reasons {
		if r.Code == "mutation_target_invalid" {
			msg = r.Message
			break
		}
	}
	for _, want := range []string{`"sum"`, "did not match anywhere"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q missing %q", msg, want)
		}
	}
}

func TestMutationSwapInvalidShape(t *testing.T) {
	skipIfNoPython3(t)
	// target is a branch path and token is empty → resolveSwapToken errors.
	mutations := `
    - strategy: swap_function_call
      target: working_approach.action
      new_value: max
      branch: working_approach
      expected_result: fail
`
	yaml := buildSwapYAML("sum", "{ type: int, value_equals: 6 }", mutations)
	res, err := Run([]byte(yaml))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "rejected" {
		t.Fatalf("status=%q, want rejected (reasons=%v)", res.Status, res.Reasons)
	}
	if !hasReason(res.Reasons, "mutation_target_invalid") {
		t.Fatalf("expected mutation_target_invalid, got %v", res.Reasons)
	}
	var msg string
	for _, r := range res.Reasons {
		if r.Code == "mutation_target_invalid" {
			msg = r.Message
			break
		}
	}
	if !strings.Contains(msg, "needs a token field or a non-path target") {
		t.Errorf("message %q missing the resolution-rule hint", msg)
	}
}

func TestMutationSwapNonStringNewValue(t *testing.T) {
	skipIfNoPython3(t)
	// new_value is an int, not a string → applySourceMutation errors.
	mutations := `
    - strategy: swap_function_call
      target: sum
      new_value: 42
      branch: working_approach
      expected_result: fail
`
	yaml := buildSwapYAML("sum", "{ type: int, value_equals: 6 }", mutations)
	res, err := Run([]byte(yaml))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "rejected" {
		t.Fatalf("status=%q, want rejected (reasons=%v)", res.Status, res.Reasons)
	}
	if !hasReason(res.Reasons, "mutation_target_invalid") {
		t.Fatalf("expected mutation_target_invalid, got %v", res.Reasons)
	}
	var msg string
	for _, r := range res.Reasons {
		if r.Code == "mutation_target_invalid" {
			msg = r.Message
			break
		}
	}
	if !strings.Contains(msg, "new_value must be a string") {
		t.Errorf("message %q missing non-string-new_value hint", msg)
	}
}

func TestMutationDifferentialFailureBlocks(t *testing.T) {
	skipIfNoPython3(t)
	// working spec says value_equals: 18, but action returns 5 →
	// differential rejects before the mutation pass runs.
	mutations := `
    - strategy: remove_kwarg
      target: working_approach.action
      token: foo
      new_value: bar
      branch: working_approach
      expected_result: fail
`
	yaml := buildMutationYAML("{ type: int, value_equals: 18 }", mutations)
	res, err := Run([]byte(yaml))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "rejected" {
		t.Fatalf("status=%q, want rejected (reasons=%v)", res.Status, res.Reasons)
	}
	if !hasReason(res.Reasons, "wrong_return_value") {
		t.Fatalf("expected wrong_return_value, got %v", res.Reasons)
	}
	// Confirm we did not surface mutation reasons — differential failure
	// must short-circuit before mutation testing.
	if hasReason(res.Reasons, "mutation_strategy_unsupported") {
		t.Fatalf("mutation reasons leaked despite differential failure: %v", res.Reasons)
	}
	if hasReason(res.Reasons, "mutation_outcome_mismatch") {
		t.Fatalf("mutation reasons leaked despite differential failure: %v", res.Reasons)
	}
}

// removeBaseYAML is a unit-tier entry whose working branch action calls
// sorted([3, 1, 2], reverse=False) returning [1, 2, 3]. Failed branch
// returns []. Tests below interpolate the working-branch return spec and
// the mutations YAML block to exercise the remove strategies.
const removeBaseYAML = `
unit_id: unit-mutation-remove-test
domain: [test]
version_constraints: { spec: { name: test } }
failed_approach:
  description: returns empty list
  setup: []
  action:
    - { type: code, lang: python, body: "$RESULT = []" }
  assertion: { type: returns, expect: fail }
working_approach:
  description: sorts with reverse=False
  setup: []
  action:
    - { type: code, lang: python, body: "$RESULT = sorted([3, 1, 2], reverse=False)" }
  assertion: { type: returns, expect: success }
verification:
  type: unit
  isolation: function
  differential:
    failed_branch_must_return: { type: list, value_equals: [] }
    working_branch_must_return: __WORKING_SPEC__
  mutations: __MUTATIONS__
  timeout_seconds: 5
`

// buildRemoveYAML interpolates the working-branch return spec and the
// mutations YAML block into removeBaseYAML.
func buildRemoveYAML(workingSpec, mutations string) string {
	y := strings.Replace(removeBaseYAML, "__WORKING_SPEC__", workingSpec, 1)
	return strings.Replace(y, "__MUTATIONS__", mutations, 1)
}

func TestMutationRemoveKwargCleanRemoval(t *testing.T) {
	skipIfNoPython3(t)
	// Removing ", reverse=False" (with leading comma + space) leaves
	// sorted([3, 1, 2]) which still returns [1, 2, 3]. Outcome: unchanged.
	mutations := `
    - strategy: remove_kwarg
      target: working_approach.action
      token: ", reverse=False"
      branch: working_approach
      expected_result: unchanged
`
	yaml := buildRemoveYAML("{ type: list, value_equals: [1, 2, 3] }", mutations)
	res, err := Run([]byte(yaml))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "verified" {
		t.Fatalf("status=%q, want verified (reasons=%v)", res.Status, res.Reasons)
	}
}

func TestMutationRemoveKwargBreakingTextLeavesSyntaxError(t *testing.T) {
	skipIfNoPython3(t)
	// Removing just "reverse" (without the comma or value) leaves
	// sorted([3, 1, 2], =False) which is a Python SyntaxError. The
	// crash-as-fail classifier synthesizes a raised ExecResult so the
	// outcome classifier produces outcomeFail → matches expected fail.
	// Load-bearing test for the crash-as-fail change.
	mutations := `
    - strategy: remove_kwarg
      target: working_approach.action
      token: "reverse"
      branch: working_approach
      expected_result: fail
`
	yaml := buildRemoveYAML("{ type: list, value_equals: [1, 2, 3] }", mutations)
	res, err := Run([]byte(yaml))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "verified" {
		t.Fatalf("status=%q, want verified (reasons=%v)", res.Status, res.Reasons)
	}
}

func TestMutationDropFlag(t *testing.T) {
	skipIfNoPython3(t)
	// drop_flag routes through the same applyRemoveMutation helper.
	// Same shape as the remove_kwarg clean-removal test.
	mutations := `
    - strategy: drop_flag
      target: working_approach.action
      token: ", reverse=False"
      branch: working_approach
      expected_result: unchanged
`
	yaml := buildRemoveYAML("{ type: list, value_equals: [1, 2, 3] }", mutations)
	res, err := Run([]byte(yaml))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "verified" {
		t.Fatalf("status=%q, want verified (reasons=%v)", res.Status, res.Reasons)
	}
}

// TestDropFlagSetupScope covers B20: a drop_flag mutation whose target
// names a setup-step type (`working_approach.setup.dockerfile`) must scan
// and rewrite b.Setup, not b.Action. The token lives in the Dockerfile
// body, so the historical action-only scan surfaced a zero-match
// mutation_target_invalid even though the token was clearly present in
// the setup body the target pointed at.
func TestDropFlagSetupScope(t *testing.T) {
	baseline := branchBaseline{
		Setup: []runner.Step{
			{Type: "dockerfile", Body: "FROM alpine\nCOPY --link src dst\n"},
		},
		Action: []runner.Step{
			{Type: "shell", Body: "echo hi"},
		},
		Inputs: map[string]any{"$X": 1},
	}
	m := Mutation{
		Strategy: "drop_flag",
		Target:   "working_approach.setup.dockerfile",
		Token:    "--link",
	}
	gotInputs, gotSetup, gotAction, err := sourceRemoveStrategy{}.apply(baseline, m)
	if err != nil {
		t.Fatalf("apply: unexpected error %v", err)
	}
	if len(gotSetup) != 1 || gotSetup[0].Body != "FROM alpine\nCOPY  src dst\n" {
		t.Errorf("setup body = %q, want %q", gotSetup[0].Body, "FROM alpine\nCOPY  src dst\n")
	}
	if len(gotAction) != 1 || gotAction[0].Body != "echo hi" {
		t.Errorf("action body = %q, want unchanged %q", gotAction[0].Body, "echo hi")
	}
	if gotInputs["$X"] != 1 {
		t.Errorf("inputs[$X] = %v, want 1 (passthrough)", gotInputs["$X"])
	}
}

// TestDropFlagActionScope covers the default scope: an action-prefixed
// target (or any non-`*.setup.*` target) continues to scan b.Action — the
// historical behaviour, preserved by mutationTargetScope's fallthrough.
func TestDropFlagActionScope(t *testing.T) {
	baseline := branchBaseline{
		Setup: []runner.Step{
			{Type: "dockerfile", Body: "FROM alpine\nCOPY --link src dst\n"},
		},
		Action: []runner.Step{
			{Type: "shell", Body: "docker build --no-cache ."},
		},
		Inputs: map[string]any{"$X": 1},
	}
	m := Mutation{
		Strategy: "drop_flag",
		Target:   "working_approach.action.shell",
		Token:    "--no-cache",
	}
	_, gotSetup, gotAction, err := sourceRemoveStrategy{}.apply(baseline, m)
	if err != nil {
		t.Fatalf("apply: unexpected error %v", err)
	}
	if len(gotSetup) != 1 || gotSetup[0].Body != "FROM alpine\nCOPY --link src dst\n" {
		t.Errorf("setup body = %q, want unchanged", gotSetup[0].Body)
	}
	if len(gotAction) != 1 || gotAction[0].Body != "docker build  ." {
		t.Errorf("action body = %q, want %q", gotAction[0].Body, "docker build  .")
	}
}

// TestRemoveKwargSetupScope proves the sibling remove_kwarg strategy
// honours the same setup-scope routing — both names dispatch through
// sourceRemoveStrategy, so the scope rule must work uniformly across them.
func TestRemoveKwargSetupScope(t *testing.T) {
	baseline := branchBaseline{
		Setup: []runner.Step{
			{Type: "code", Lang: "python", Body: "configure(name='x', cache=True, verbose=False)"},
		},
		Action: []runner.Step{
			{Type: "code", Lang: "python", Body: "$RESULT = run()"},
		},
		Inputs: map[string]any{},
	}
	m := Mutation{
		Strategy: "remove_kwarg",
		Target:   "working_approach.setup.code",
		Token:    "cache=True, ",
	}
	_, gotSetup, gotAction, err := sourceRemoveStrategy{}.apply(baseline, m)
	if err != nil {
		t.Fatalf("apply: unexpected error %v", err)
	}
	if len(gotSetup) != 1 || gotSetup[0].Body != "configure(name='x', verbose=False)" {
		t.Errorf("setup body = %q, want %q", gotSetup[0].Body, "configure(name='x', verbose=False)")
	}
	if len(gotAction) != 1 || gotAction[0].Body != "$RESULT = run()" {
		t.Errorf("action body = %q, want unchanged", gotAction[0].Body)
	}
}

// TestDropFlagSetupZeroMatchSurfaces proves the zero-match guard fires
// against the correct slice once scope routing is in play: a setup-scoped
// target whose token is absent from setup must surface
// mutation_target_invalid even when the token happens to appear in the
// action body (the historical scan would have falsely matched it there).
func TestDropFlagSetupZeroMatchSurfaces(t *testing.T) {
	baseline := branchBaseline{
		Setup: []runner.Step{
			{Type: "dockerfile", Body: "FROM alpine\nCOPY src dst\n"},
		},
		Action: []runner.Step{
			// Action contains --link, but scope routing must ignore it.
			{Type: "shell", Body: "docker build --link ."},
		},
		Inputs: map[string]any{},
	}
	m := Mutation{
		Strategy: "drop_flag",
		Target:   "working_approach.setup.dockerfile",
		Token:    "--link",
	}
	_, _, _, err := sourceRemoveStrategy{}.apply(baseline, m)
	if err == nil {
		t.Fatal("apply: expected zero-match error, got nil")
	}
	if !strings.Contains(err.Error(), "did not appear anywhere") {
		t.Errorf("error %q missing zero-match phrasing", err.Error())
	}
}

// TestSourceSubstStrategySetupScope mirrors the drop_flag setup-scope test
// for swap_function_call / swap_identifier: a setup-scoped target rewrites
// b.Setup, the sibling slice is untouched. Future seeds may want to swap a
// builder identifier inside a Dockerfile or shell setup — B20 makes that
// path consistent across all source-modifying strategies.
func TestSourceSubstStrategySetupScope(t *testing.T) {
	baseline := branchBaseline{
		Setup: []runner.Step{
			{Type: "dockerfile", Body: "FROM alpine\nRUN apk add curl\n"},
		},
		Action: []runner.Step{
			{Type: "shell", Body: "echo hi"},
		},
		Inputs: map[string]any{},
	}
	m := Mutation{
		Strategy: "swap_identifier",
		Target:   "working_approach.setup.dockerfile",
		Token:    "alpine",
		NewValue: "debian",
	}
	_, gotSetup, gotAction, err := sourceSubstStrategy{}.apply(baseline, m)
	if err != nil {
		t.Fatalf("apply: unexpected error %v", err)
	}
	if len(gotSetup) != 1 || gotSetup[0].Body != "FROM debian\nRUN apk add curl\n" {
		t.Errorf("setup body = %q, want %q", gotSetup[0].Body, "FROM debian\nRUN apk add curl\n")
	}
	if len(gotAction) != 1 || gotAction[0].Body != "echo hi" {
		t.Errorf("action body = %q, want unchanged", gotAction[0].Body)
	}
}

// TestInputSubstStrategyPassesSetupThrough confirms inputSubstStrategy's
// widened signature still returns b.Setup unchanged — its job is to rebind
// an input, not perturb source.
func TestInputSubstStrategyPassesSetupThrough(t *testing.T) {
	baseline := branchBaseline{
		Setup: []runner.Step{
			{Type: "dockerfile", Body: "FROM alpine\n"},
		},
		Action: []runner.Step{
			{Type: "code", Lang: "python", Body: "$RESULT = $X"},
		},
		Inputs: map[string]any{"$X": 5},
	}
	m := Mutation{
		Strategy: "set_literal_value",
		Target:   "$X",
		NewValue: 99,
	}
	gotInputs, gotSetup, gotAction, err := inputSubstStrategy{}.apply(baseline, m)
	if err != nil {
		t.Fatalf("apply: unexpected error %v", err)
	}
	if gotInputs["$X"] != 99 {
		t.Errorf("inputs[$X] = %v, want 99", gotInputs["$X"])
	}
	if len(gotSetup) != 1 || gotSetup[0].Body != "FROM alpine\n" {
		t.Errorf("setup body = %q, want unchanged", gotSetup[0].Body)
	}
	if len(gotAction) != 1 || gotAction[0].Body != "$RESULT = $X" {
		t.Errorf("action body = %q, want unchanged", gotAction[0].Body)
	}
}

func TestMutationRemoveKwargInvalidShape(t *testing.T) {
	skipIfNoPython3(t)
	// No token field, target is a branch path → resolveSwapToken errors.
	mutations := `
    - strategy: remove_kwarg
      target: working_approach.action
      branch: working_approach
      expected_result: fail
`
	yaml := buildRemoveYAML("{ type: list, value_equals: [1, 2, 3] }", mutations)
	res, err := Run([]byte(yaml))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "rejected" {
		t.Fatalf("status=%q, want rejected (reasons=%v)", res.Status, res.Reasons)
	}
	if !hasReason(res.Reasons, "mutation_target_invalid") {
		t.Fatalf("expected mutation_target_invalid, got %v", res.Reasons)
	}
	var msg string
	for _, r := range res.Reasons {
		if r.Code == "mutation_target_invalid" {
			msg = r.Message
			break
		}
	}
	if !strings.Contains(msg, "needs a token field or a non-path target") {
		t.Errorf("message %q missing the resolution-rule hint", msg)
	}
}

func TestMutationCrashAsFailNotTimeout(t *testing.T) {
	skipIfNoPython3(t)
	// A mutation that triggers a sleep exceeding timeout_seconds: 1
	// must surface as mutation_runner_error (real ErrTimeout), NOT as
	// outcomeFail. Confirms the timeout discrimination works.
	//
	// Baseline action sleeps for $SLEEP_SEC (0) → completes fast.
	// Mutation sets $SLEEP_SEC = 5 → sleeps past the 1s timeout.
	yaml := `
unit_id: unit-mutation-timeout
domain: [test]
version_constraints: { spec: { name: test } }
failed_approach:
  description: returns 0
  setup: []
  action:
    - { type: code, lang: python, body: "$RESULT = 0" }
  assertion: { type: returns, expect: fail }
working_approach:
  description: sleeps for $SLEEP_SEC then returns 1
  setup: []
  action:
    - { type: code, lang: python, body: "import time\ntime.sleep($SLEEP_SEC)\n$RESULT = 1" }
  assertion: { type: returns, expect: success }
verification:
  type: unit
  isolation: function
  differential:
    inputs:
      $SLEEP_SEC: 0
    failed_branch_must_return: { type: int, value_equals: 0 }
    working_branch_must_return: { type: int, value_equals: 1 }
  mutations:
    - strategy: mutate_fixture
      target: $SLEEP_SEC
      new_value: 5
      branch: working_approach
      expected_result: fail
  timeout_seconds: 1
`
	res, err := Run([]byte(yaml))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "rejected" {
		t.Fatalf("status=%q, want rejected (reasons=%v)", res.Status, res.Reasons)
	}
	if !hasReason(res.Reasons, "mutation_runner_error") {
		t.Fatalf("expected mutation_runner_error, got %v", res.Reasons)
	}
	// And specifically NOT mutation_outcome_mismatch — that would mean
	// the timeout was wrongly synthesized into outcomeFail.
	if hasReason(res.Reasons, "mutation_outcome_mismatch") {
		t.Fatalf("timeout was wrongly classified as outcome mismatch: %v", res.Reasons)
	}
}

func TestMutationStrategyUnsupportedRemainsCustom(t *testing.T) {
	skipIfNoPython3(t)
	// custom must remain in the unsupported set after F15. This locks
	// the contract that adding remove_kwarg / drop_flag did not also
	// silently graduate "custom".
	mutations := `
    - strategy: custom
      target: working_approach.action
      token: foo
      new_value: bar
      branch: working_approach
      expected_result: fail
`
	yaml := buildMutationYAML("{ type: int, value_equals: 5 }", mutations)
	res, err := Run([]byte(yaml))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "tier_unsupported" {
		t.Fatalf("status=%q, want tier_unsupported (reasons=%v)", res.Status, res.Reasons)
	}
	if !hasReason(res.Reasons, "mutation_strategy_unsupported") {
		t.Fatalf("expected mutation_strategy_unsupported, got %v", res.Reasons)
	}
}

// F17: python_expr-aware mutation new_value — the following tests verify that
// mutate_fixture and set_literal_value new_value accepts the {python_expr: "<expr>"}
// opt-in shape introduced by F12 on the input side. The value is evaluated in
// Python instead of being JSON-decoded as a literal string.

func TestMutationFixturePythonExprNewValue(t *testing.T) {
	skipIfNoPython3(t)
	// Working action returns the input list. Spec accepts any list (no value_equals).
	// Baseline: $ITEMS=[1] (literal) → working returns [1]. Mutation rewrites
	// $ITEMS to {python_expr: "list(range(3))"} → working returns [0,1,2].
	// Spec is type: list (no value_equals) → outcomePass is the only correct
	// result. If python_expr is misrouted as a literal string,
	// json.loads("list(range(3))") raises and the runner returns Raised=true,
	// which classifyOutcome treats as outcomeFail — mismatch surfaces.
	yml := `
unit_id: unit-mutation-python-expr-newvalue
domain: [test]
version_constraints: { spec: { name: test } }
failed_approach:
  description: returns []
  setup: []
  action:
    - { type: code, lang: python, body: "$RESULT = []" }
  assertion: { type: returns, expect: fail }
working_approach:
  description: returns the input list
  setup: []
  action:
    - { type: code, lang: python, body: "$RESULT = $ITEMS" }
  assertion: { type: returns, expect: success }
verification:
  type: unit
  isolation: function
  differential:
    inputs:
      $ITEMS: [1]
    failed_branch_must_return: { type: list, value_equals: [] }
    working_branch_must_return: { type: list }
  mutations:
    - strategy: mutate_fixture
      target: $ITEMS
      new_value:
        python_expr: "list(range(3))"
      branch: working_approach
      expected_result: pass
  timeout_seconds: 5
`
	res, err := Run([]byte(yml))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "verified" {
		t.Fatalf("status=%q, want verified (reasons=%v)", res.Status, res.Reasons)
	}
}

func TestMutationFixturePythonExprNewValueWithLength(t *testing.T) {
	skipIfNoPython3(t)
	// Baseline $ITEMS=[1] → working returns length=1. Mutation rewrites $ITEMS
	// to list(range(3)) → length=3. Spec uses length: 1 (matches baseline only)
	// → mutation produces length=3 which violates spec → outcomeFail. Mutation
	// declares expected_result: fail → verified. Tests integration with F11 length matcher.
	yml := `
unit_id: unit-mutation-python-expr-with-length
domain: [test]
version_constraints: { spec: { name: test } }
failed_approach:
  description: returns []
  setup: []
  action:
    - { type: code, lang: python, body: "$RESULT = []" }
  assertion: { type: returns, expect: fail }
working_approach:
  description: returns the input list
  setup: []
  action:
    - { type: code, lang: python, body: "$RESULT = $ITEMS" }
  assertion: { type: returns, expect: success }
verification:
  type: unit
  isolation: function
  differential:
    inputs:
      $ITEMS: [1]
    failed_branch_must_return: { type: list, length: 0 }
    working_branch_must_return: { type: list, length: 1 }
  mutations:
    - strategy: mutate_fixture
      target: $ITEMS
      new_value:
        python_expr: "list(range(3))"
      branch: working_approach
      expected_result: fail
  timeout_seconds: 5
`
	res, err := Run([]byte(yml))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "verified" {
		t.Fatalf("status=%q, want verified (reasons=%v)", res.Status, res.Reasons)
	}
}

func TestMutationSetLiteralPythonExprNewValue(t *testing.T) {
	skipIfNoPython3(t)
	// set_literal_value shares the input-substitution path with mutate_fixture
	// (both go through applyInputSubstitution). Baseline $LITERAL_1=[1,2,3]
	// → working returns sum=6. Mutation rewrites $LITERAL_1 to list(range(10))
	// → sum=45 ≠ 6 → outcomeFail. Mutation declares expected_result: fail → verified.
	yml := `
unit_id: unit-mutation-set-literal-python-expr
domain: [test]
version_constraints: { spec: { name: test } }
failed_approach:
  description: returns 0
  setup: []
  action:
    - { type: code, lang: python, body: "$RESULT = 0" }
  assertion: { type: returns, expect: fail }
working_approach:
  description: returns sum of $LITERAL_1
  setup: []
  action:
    - { type: code, lang: python, body: "$RESULT = sum($LITERAL_1)" }
  assertion: { type: returns, expect: success }
verification:
  type: unit
  isolation: function
  differential:
    inputs:
      $LITERAL_1: [1, 2, 3]
    failed_branch_must_return: { type: int, value_equals: 0 }
    working_branch_must_return: { type: int, value_equals: 6 }
  mutations:
    - strategy: set_literal_value
      target: $LITERAL_1
      new_value:
        python_expr: "list(range(10))"
      branch: working_approach
      expected_result: fail
  timeout_seconds: 5
`
	res, err := Run([]byte(yml))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "verified" {
		t.Fatalf("status=%q, want verified (reasons=%v)", res.Status, res.Reasons)
	}
}

// --- Bug 1: zero-match diagnostic for source-mutating strategies ------------

func TestMutationSwapFunctionCallTokenWithNonWordCharsRejected(t *testing.T) {
	skipIfNoPython3(t)
	// Real-world regression from the F19 seed-migration session: a seed
	// author wrote swap_function_call with token "== 204" intending to break
	// a status-code predicate. The compiled regex `\b== 204\b` matches
	// nowhere because `=` and ` ` are non-word chars, so `\b=` never
	// anchors. The mutation silently no-opped, classified `unchanged`, and
	// gave no diagnostic. After Bug 1 the verifier rejects the mutation
	// with a typed reason naming the offending token.
	yaml := `
unit_id: unit-mutation-swap-nonword-token
domain: [test]
version_constraints: { spec: { name: test } }
failed_approach:
  description: returns 0
  setup: []
  action:
    - { type: code, lang: python, body: "$RESULT = 0" }
  assertion: { type: returns, expect: fail }
working_approach:
  description: returns the constant 204
  setup: []
  action:
    - { type: code, lang: python, body: "$RESULT = 204" }
  assertion: { type: returns, expect: success }
verification:
  type: unit
  isolation: function
  differential:
    failed_branch_must_return: { type: int, value_equals: 0 }
    working_branch_must_return: { type: int, value_equals: 204 }
  mutations:
    - strategy: swap_function_call
      target: working_approach.action
      token: "== 204"
      new_value: "== 999"
      branch: working_approach
      expected_result: fail
  timeout_seconds: 5
`
	res, err := Run([]byte(yaml))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "rejected" {
		t.Fatalf("status=%q, want rejected (reasons=%v)", res.Status, res.Reasons)
	}
	if !hasReason(res.Reasons, "mutation_target_invalid") {
		t.Fatalf("expected mutation_target_invalid, got %v", res.Reasons)
	}
	var msg string
	for _, r := range res.Reasons {
		if r.Code == "mutation_target_invalid" {
			msg = r.Message
			break
		}
	}
	for _, want := range []string{`"== 204"`, "did not match anywhere"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q missing %q", msg, want)
		}
	}
}

func TestMutationSwapIdentifierUnknownTokenRejected(t *testing.T) {
	skipIfNoPython3(t)
	// swap_identifier with a token that doesn't exist anywhere in the
	// action source. Pre-Bug-1 this silently no-opped to outcomeUnchanged;
	// now it surfaces as mutation_target_invalid.
	mutations := `
    - strategy: swap_identifier
      target: working_approach.action
      token: NoSuchSymbol
      new_value: max
      branch: working_approach
      expected_result: fail
`
	yaml := buildSwapYAML("sum", "{ type: int, value_equals: 6 }", mutations)
	res, err := Run([]byte(yaml))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "rejected" {
		t.Fatalf("status=%q, want rejected (reasons=%v)", res.Status, res.Reasons)
	}
	if !hasReason(res.Reasons, "mutation_target_invalid") {
		t.Fatalf("expected mutation_target_invalid, got %v", res.Reasons)
	}
	var msg string
	for _, r := range res.Reasons {
		if r.Code == "mutation_target_invalid" {
			msg = r.Message
			break
		}
	}
	for _, want := range []string{`"NoSuchSymbol"`, "did not match anywhere"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q missing %q", msg, want)
		}
	}
}

func TestMutationRemoveKwargUnknownTokenRejected(t *testing.T) {
	skipIfNoPython3(t)
	// remove_kwarg / drop_flag share applyRemoveMutation. A token that
	// doesn't appear anywhere in the source must error out with the same
	// shape as the swap variants — silent no-op was a parallel bug.
	mutations := `
    - strategy: remove_kwarg
      target: working_approach.action
      token: ", reverse=Nope"
      branch: working_approach
      expected_result: fail
`
	yaml := buildRemoveYAML("{ type: list, value_equals: [1, 2, 3] }", mutations)
	res, err := Run([]byte(yaml))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "rejected" {
		t.Fatalf("status=%q, want rejected (reasons=%v)", res.Status, res.Reasons)
	}
	if !hasReason(res.Reasons, "mutation_target_invalid") {
		t.Fatalf("expected mutation_target_invalid, got %v", res.Reasons)
	}
	var msg string
	for _, r := range res.Reasons {
		if r.Code == "mutation_target_invalid" {
			msg = r.Message
			break
		}
	}
	for _, want := range []string{`", reverse=Nope"`, "did not appear anywhere"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q missing %q", msg, want)
		}
	}
}

// --- Bug 2: non-discriminating source rewrite diagnostic --------------------

// discrimYAML uses a local-only identifier `result` inside the working
// approach, then assigns it to $RESULT. Renaming `result` consistently to
// `outcome` rewrites the source byte-for-byte but produces an identical
// observable output — Python doesn't care what locals are named.
const discrimYAML = `
unit_id: unit-mutation-discriminate-test
domain: [test]
version_constraints: { spec: { name: test } }
failed_approach:
  description: returns 0
  setup: []
  action:
    - { type: code, lang: python, body: "$RESULT = 0" }
  assertion: { type: returns, expect: fail }
working_approach:
  description: computes via a local named "result"
  setup: []
  action:
    - { type: code, lang: python, body: "result = sum([1,2,3])\n$RESULT = result" }
  assertion: { type: returns, expect: success }
verification:
  type: unit
  isolation: function
  differential:
    failed_branch_must_return: { type: int, value_equals: 0 }
    working_branch_must_return: { type: int, value_equals: 6 }
  mutations: __MUTATIONS__
  timeout_seconds: 5
`

func buildDiscrimYAML(mutations string) string {
	return strings.Replace(discrimYAML, "__MUTATIONS__", mutations, 1)
}

func TestMutationConsistentRenameDoesNotDiscriminate(t *testing.T) {
	skipIfNoPython3(t)
	// swap_identifier rewrites both occurrences of `result` to `outcome`.
	// Source is byte-different from baseline; observable behaviour is
	// identical. Mutation declares expected_result: fail → must reject
	// with the new mutation_did_not_discriminate reason, not the generic
	// mutation_outcome_mismatch.
	mutations := `
    - strategy: swap_identifier
      target: working_approach.action
      token: result
      new_value: outcome
      branch: working_approach
      expected_result: fail
`
	yaml := buildDiscrimYAML(mutations)
	res, err := Run([]byte(yaml))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "rejected" {
		t.Fatalf("status=%q, want rejected (reasons=%v)", res.Status, res.Reasons)
	}
	if !hasReason(res.Reasons, "mutation_did_not_discriminate") {
		t.Fatalf("expected mutation_did_not_discriminate, got %v", res.Reasons)
	}
	if hasReason(res.Reasons, "mutation_outcome_mismatch") {
		t.Fatalf("did_not_discriminate should replace generic mismatch, got both: %v", res.Reasons)
	}
	var msg string
	for _, r := range res.Reasons {
		if r.Code == "mutation_did_not_discriminate" {
			msg = r.Message
			break
		}
	}
	for _, want := range []string{
		"rewrote source but produced no behavioural change",
		`"result"`,
		"swap_identifier",
		"working_approach",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q missing %q", msg, want)
		}
	}
}

func TestMutationConsistentRenameExpectedUnchangedIsLegitimate(t *testing.T) {
	skipIfNoPython3(t)
	// Same mutation shape as the previous test, but expected_result:
	// unchanged. This is the legitimate path — submitter knew the rename
	// is observationally inert and is asserting it. No diagnostic should
	// fire and the entry verifies green.
	mutations := `
    - strategy: swap_identifier
      target: working_approach.action
      token: result
      new_value: outcome
      branch: working_approach
      expected_result: unchanged
`
	yaml := buildDiscrimYAML(mutations)
	res, err := Run([]byte(yaml))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "verified" {
		t.Fatalf("status=%q, want verified (reasons=%v)", res.Status, res.Reasons)
	}
	if hasReason(res.Reasons, "mutation_did_not_discriminate") {
		t.Fatalf("did_not_discriminate must not fire on expected_result: unchanged: %v", res.Reasons)
	}
}

func TestMutationInputSubstUnchangedDoesNotTriggerDiscriminationHint(t *testing.T) {
	skipIfNoPython3(t)
	// Bug 2 only applies to source-mutating strategies. mutate_fixture and
	// set_literal_value rebind an input — an unchanged outcome there means
	// the new value happened to behave identically (e.g. assigning $X=5
	// when the baseline already had $X=5). That's a different problem and
	// must NOT trigger the source-rewrite hint. Here we set new_value back
	// to the same baseline value so outcome is `unchanged`, and declare
	// expected_result: fail. Result: generic mutation_outcome_mismatch
	// fires; mutation_did_not_discriminate does NOT.
	mutations := `
    - strategy: set_literal_value
      target: $X
      new_value: 5
      branch: working_approach
      expected_result: fail
`
	yaml := buildMutationYAML("{ type: int, value_equals: 5 }", mutations)
	res, err := Run([]byte(yaml))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "rejected" {
		t.Fatalf("status=%q, want rejected (reasons=%v)", res.Status, res.Reasons)
	}
	if !hasReason(res.Reasons, "mutation_outcome_mismatch") {
		t.Fatalf("expected mutation_outcome_mismatch (input-subst out of scope for Bug 2), got %v", res.Reasons)
	}
	if hasReason(res.Reasons, "mutation_did_not_discriminate") {
		t.Fatalf("did_not_discriminate must not fire for input-substitution strategies: %v", res.Reasons)
	}
}

// ── B21: action-discriminated mutate_fixture ──────────────────────────────
//
// These tests cover the new fixtureActionStrategy + resolveMutationStrategy
// dispatcher. A mutate_fixture mutation with a non-empty action: field routes
// to fixtureActionStrategy and performs the named action against the workdir-
// materialized fixture directory; a mutate_fixture mutation without an action:
// field continues to route through inputSubstStrategy (F82's sidecar enabled
// flip path). See B21 in .hv/bugs/B21.md for the canonical failure case.

// makeFixtureBaseline returns a branchBaseline with a workdir-materialized
// fixture directory under <workdir>/<name>/ containing one seed file, and
// inputs[$NAME] = "./<name>" — the same shape materializeDirectoryFixtures
// produces during reexecute. Used by the fixtureActionStrategy unit tests
// below to avoid spinning up a full reexecute pipeline.
func makeFixtureBaseline(t *testing.T, name string) (branchBaseline, string) {
	t.Helper()
	workdir := t.TempDir()
	fixtureDir := filepath.Join(workdir, name)
	if err := os.MkdirAll(fixtureDir, 0o755); err != nil {
		t.Fatalf("mkdir fixture: %v", err)
	}
	seedPath := filepath.Join(fixtureDir, "seed.txt")
	if err := os.WriteFile(seedPath, []byte("seed contents\n"), 0o644); err != nil {
		t.Fatalf("write seed: %v", err)
	}
	b := branchBaseline{
		Setup: []runner.Step{
			{Type: "shell", Body: "echo setup"},
		},
		Action: []runner.Step{
			{Type: "shell", Body: "ls ./" + name},
		},
		Inputs:  map[string]any{"$" + name: "./" + name},
		Workdir: workdir,
	}
	return b, fixtureDir
}

// TestMutateFixtureActionAddNewFile covers the happy path: a mutate_fixture
// mutation with action: add_new_file lands a new file in the workdir-
// materialized fixture directory, leaves the seed file alone, and passes
// inputs/setup/action through unchanged.
func TestMutateFixtureActionAddNewFile(t *testing.T) {
	baseline, fixtureDir := makeFixtureBaseline(t, "SOURCE_PATH")
	originalSeed := filepath.Join(fixtureDir, "seed.txt")
	originalSeedBefore, err := os.ReadFile(originalSeed)
	if err != nil {
		t.Fatalf("read seed before: %v", err)
	}

	m := Mutation{
		Strategy:     "mutate_fixture",
		Target:       "$SOURCE_PATH",
		ActionLegacy: "add_new_file",
	}
	gotInputs, gotSetup, gotAction, err := fixtureActionStrategy{}.apply(baseline, m)
	if err != nil {
		t.Fatalf("apply: unexpected error %v", err)
	}

	// 1. A new file appeared in the fixture directory.
	entries, err := os.ReadDir(fixtureDir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 2 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("fixture dir should have 2 files (seed + added), got %d: %v", len(entries), names)
	}
	foundAdded := false
	for _, e := range entries {
		if e.Name() != "seed.txt" {
			foundAdded = true
			// Derived name must include "add_new_file" so a future
			// reader can tell what mutated the tree.
			if !strings.Contains(e.Name(), "add_new_file") {
				t.Errorf("added filename %q should contain 'add_new_file' (derived from m.ActionLegacy)", e.Name())
			}
		}
	}
	if !foundAdded {
		t.Fatal("no new file appeared in the fixture directory")
	}

	// 2. The original seed file is untouched.
	originalSeedAfter, err := os.ReadFile(originalSeed)
	if err != nil {
		t.Fatalf("read seed after: %v", err)
	}
	if string(originalSeedBefore) != string(originalSeedAfter) {
		t.Errorf("seed file body changed: before=%q after=%q", originalSeedBefore, originalSeedAfter)
	}

	// 3. Inputs/setup/action are passed through unchanged. The strategy
	// operates on the on-disk tree, not the step bodies.
	if gotInputs["$SOURCE_PATH"] != "./SOURCE_PATH" {
		t.Errorf("inputs[$SOURCE_PATH] = %v, want unchanged ./SOURCE_PATH", gotInputs["$SOURCE_PATH"])
	}
	if len(gotSetup) != 1 || gotSetup[0].Body != "echo setup" {
		t.Errorf("setup body changed: %v", gotSetup)
	}
	if len(gotAction) != 1 || gotAction[0].Body != "ls ./SOURCE_PATH" {
		t.Errorf("action body changed: %v", gotAction)
	}
}

// TestMutateFixtureRoutesByAction covers the dispatcher: the SAME
// mutate_fixture strategy name routes to two different strategies based on
// whether action: is empty. This is the core B21 contract.
func TestMutateFixtureRoutesByAction(t *testing.T) {
	// (a) Empty action + new_value → inputSubstStrategy (F82's path).
	m1 := Mutation{
		Strategy: "mutate_fixture",
		Target:   "$FLAG",
		NewValue: false,
	}
	strat1, ok := resolveMutationStrategy(m1)
	if !ok {
		t.Fatal("resolveMutationStrategy returned !ok for mutate_fixture without action")
	}
	if _, isInputSubst := strat1.(inputSubstStrategy); !isInputSubst {
		t.Errorf("mutate_fixture without action routed to %T, want inputSubstStrategy", strat1)
	}

	// (b) Non-empty action → fixtureActionStrategy (B21's path).
	m2 := Mutation{
		Strategy:     "mutate_fixture",
		Target:       "$SOURCE_PATH",
		ActionLegacy: "add_new_file",
	}
	strat2, ok := resolveMutationStrategy(m2)
	if !ok {
		t.Fatal("resolveMutationStrategy returned !ok for mutate_fixture with action")
	}
	if _, isFixtureAction := strat2.(fixtureActionStrategy); !isFixtureAction {
		t.Errorf("mutate_fixture with action routed to %T, want fixtureActionStrategy", strat2)
	}

	// (c) Whitespace-only action: routes as if empty (input-rebind path),
	// matching mutationField's TrimSpace contract.
	m3 := Mutation{
		Strategy:     "mutate_fixture",
		Target:       "$FLAG",
		NewValue:     false,
		ActionLegacy: "   ",
	}
	strat3, _ := resolveMutationStrategy(m3)
	if _, isInputSubst := strat3.(inputSubstStrategy); !isInputSubst {
		t.Errorf("mutate_fixture with whitespace-only action routed to %T, want inputSubstStrategy", strat3)
	}
}

// TestMutateFixtureUnknownActionSurfaces covers the v0.1 scope guard: actions
// not yet implemented surface a typed error rather than silently no-opping.
// F89 will widen this when remove_file / flip_permission / etc. land.
func TestMutateFixtureUnknownActionSurfaces(t *testing.T) {
	baseline, _ := makeFixtureBaseline(t, "SOURCE_PATH")
	m := Mutation{
		Strategy:     "mutate_fixture",
		Target:       "$SOURCE_PATH",
		ActionLegacy: "delete_existing",
	}
	_, _, _, err := fixtureActionStrategy{}.apply(baseline, m)
	if err == nil {
		t.Fatal("apply: expected unsupported-action error, got nil")
	}
	if !strings.Contains(err.Error(), "is not supported") {
		t.Errorf("error %q missing 'is not supported' phrasing", err.Error())
	}
	if !strings.Contains(err.Error(), "add_new_file") {
		t.Errorf("error %q should name the supported action(s)", err.Error())
	}
}

// TestMutateFixtureEmptyWorkdirSurfaces is a defensive guard: under normal
// flow Workdir is mirrored from the SubprocessDriver in reexecute.go, but a
// PythonDriver-tier baseline (or a hand-constructed test baseline) leaves
// Workdir empty. The strategy must surface this with a clear error rather
// than writing into the current working directory.
func TestMutateFixtureEmptyWorkdirSurfaces(t *testing.T) {
	baseline := branchBaseline{
		Setup:  []runner.Step{{Type: "shell", Body: "true"}},
		Action: []runner.Step{{Type: "shell", Body: "true"}},
		Inputs: map[string]any{"$SOURCE_PATH": "./SOURCE_PATH"},
		// Workdir intentionally empty.
	}
	m := Mutation{
		Strategy:     "mutate_fixture",
		Target:       "$SOURCE_PATH",
		ActionLegacy: "add_new_file",
	}
	_, _, _, err := fixtureActionStrategy{}.apply(baseline, m)
	if err == nil {
		t.Fatal("apply: expected empty-workdir error, got nil")
	}
	if !strings.Contains(err.Error(), "Workdir") {
		t.Errorf("error %q should name the empty Workdir field", err.Error())
	}
}

// TestMutateFixtureEmptyActionGuard is the defensive sibling: if a future
// dispatch bug routes a mutate_fixture-without-action mutation to
// fixtureActionStrategy, the apply method surfaces it rather than silently
// writing a file. Belt-and-braces — resolveMutationStrategy should keep us
// out of this branch in practice.
func TestMutateFixtureEmptyActionGuard(t *testing.T) {
	baseline, _ := makeFixtureBaseline(t, "SOURCE_PATH")
	m := Mutation{
		Strategy: "mutate_fixture",
		Target:   "$SOURCE_PATH",
		// ActionLegacy intentionally empty.
	}
	_, _, _, err := fixtureActionStrategy{}.apply(baseline, m)
	if err == nil {
		t.Fatal("apply: expected empty-action guard error, got nil")
	}
	if !strings.Contains(err.Error(), "empty action") {
		t.Errorf("error %q should mention 'empty action'", err.Error())
	}
}

// TestMutateFixtureInputRebindPathRegression confirms the F82 sidecar-
// enabled-flip path still works after B21: a mutate_fixture mutation with
// no action: field continues to rebind the input through inputSubstStrategy.
// If this test fails, B21 broke F82.
func TestMutateFixtureInputRebindPathRegression(t *testing.T) {
	baseline := branchBaseline{
		Setup: []runner.Step{
			{Type: "shell", Body: "true"},
		},
		Action: []runner.Step{
			{Type: "shell", Body: "echo $FIXTURE_ENABLED"},
		},
		Inputs: map[string]any{"$FIXTURE_ENABLED": true},
	}
	m := Mutation{
		Strategy: "mutate_fixture",
		Target:   "$FIXTURE_ENABLED",
		NewValue: false,
		// ActionLegacy is empty → must route through inputSubstStrategy.
	}
	strat, ok := resolveMutationStrategy(m)
	if !ok {
		t.Fatal("resolveMutationStrategy returned !ok")
	}
	gotInputs, _, _, err := strat.apply(baseline, m)
	if err != nil {
		t.Fatalf("apply: unexpected error %v", err)
	}
	if gotInputs["$FIXTURE_ENABLED"] != false {
		t.Errorf("inputs[$FIXTURE_ENABLED] = %v, want false (input-rebind path)", gotInputs["$FIXTURE_ENABLED"])
	}
}

// TestMutateFixtureFilenameSanitization confirms the derived filename is
// safe even when m.ActionLegacy contains characters that would otherwise
// embed slashes or shell metacharacters in the filename. The seed validator
// won't generally let pathological actions through, but defence-in-depth
// matters here because we're writing to disk based on those strings.
func TestMutateFixtureFilenameSanitization(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"add_new_file", "add_new_file"},
		{"a/b", "a_b"},
		{"a..b", "a__b"},
		{"", "x"},
		{"weird name!", "weird_name_"},
		{"already-safe_123", "already-safe_123"},
	}
	for _, c := range cases {
		got := sanitizeFixtureFilenamePart(c.in)
		if got != c.want {
			t.Errorf("sanitize(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
