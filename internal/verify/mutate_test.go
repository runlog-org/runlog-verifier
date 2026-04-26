package verify

import (
	"strings"
	"testing"
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
	// targets sum→max. Word boundaries should prevent the substring match,
	// leaving the action unchanged → outcomeUnchanged. Expected: unchanged.
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
	if res.Status != "verified" {
		t.Fatalf("status=%q, want verified (reasons=%v)", res.Status, res.Reasons)
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
