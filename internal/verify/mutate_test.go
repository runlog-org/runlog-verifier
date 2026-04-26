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
	// swap_identifier is not implemented; entry must degrade to
	// tier_unsupported with a message naming the strategy.
	mutations := `
    - strategy: swap_identifier
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
	if !strings.Contains(msg, "swap_identifier") {
		t.Errorf("message %q does not name swap_identifier", msg)
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

func TestMutationDifferentialFailureBlocks(t *testing.T) {
	skipIfNoPython3(t)
	// working spec says value_equals: 18, but action returns 5 →
	// differential rejects before the mutation pass runs.
	mutations := `
    - strategy: swap_identifier
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
