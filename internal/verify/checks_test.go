package verify

import "testing"

// TestMutationExpectsBreakRejectsAssertionDoesNotMatchOnExpectedResult asserts
// that a Mutation whose expected_result is "assertion_does_not_match" is NOT
// treated as break-like. The schema's expected_result enum has only 4 values
// (fail, unchanged, pass, inapplicable) and excludes assertion_does_not_match;
// accepting it here would disagree with parseMutationOutcome(allowAssertionMismatch=false).
func TestMutationExpectsBreakRejectsAssertionDoesNotMatchOnExpectedResult(t *testing.T) {
	m := Mutation{
		Strategy:       "mutate_fixture",
		ExpectedResult: "assertion_does_not_match",
	}
	if mutationExpectsBreak(m) {
		t.Errorf("mutationExpectsBreak returned true for expected_result: assertion_does_not_match — schema rejects this value in expected_result")
	}
}

// TestMutationExpectsBreakAcceptsAssertionDoesNotMatchOnBranchOutcome asserts
// that a Mutation whose expected_branch_outcome contains "assertion_does_not_match"
// IS treated as break-like. The schema's expected_branch_outcome enum has 5 values
// and includes assertion_does_not_match as a valid alias for fail.
func TestMutationExpectsBreakAcceptsAssertionDoesNotMatchOnBranchOutcome(t *testing.T) {
	m := Mutation{
		Strategy: "mutate_fixture",
		ExpectedBranchOutcome: map[string]string{
			"failed_approach": "assertion_does_not_match",
		},
	}
	if !mutationExpectsBreak(m) {
		t.Errorf("mutationExpectsBreak returned false for expected_branch_outcome.failed_approach: assertion_does_not_match — should be accepted in branch-outcome context")
	}
}
