package verify

import (
	"fmt"
)

// Reason is a single rejection cause emitted by a failed check. Code is
// stable for tooling (e.g. "tautological_branches"); Message is the
// human-readable explanation included in the signed bundle.
type Reason struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// checkBranchesPresent enforces that both branches carry an assertion
// block — the verifier cannot reason about a missing assertion.
func checkBranchesPresent(e *Entry) []Reason {
	var out []Reason
	if e.FailedApproach.Assertion.Type == "" {
		out = append(out, Reason{
			Code:    "missing_failed_assertion",
			Message: "failed_approach.assertion.type is empty",
		})
	}
	if e.WorkingApproach.Assertion.Type == "" {
		out = append(out, Reason{
			Code:    "missing_working_assertion",
			Message: "working_approach.assertion.type is empty",
		})
	}
	return out
}

// checkBranchesDiscriminating enforces docs/03 §5.3 step 3: both branches
// must be meaningfully different. We treat two assertions as tautological
// when type, expect, path, and value all match exactly. Different
// `expect` values (fail vs success) are the most common discriminator.
func checkBranchesDiscriminating(e *Entry) []Reason {
	fa := e.FailedApproach.Assertion
	wa := e.WorkingApproach.Assertion
	if fa.Type == "" || wa.Type == "" {
		return nil // already reported by checkBranchesPresent
	}
	if fa.Type == wa.Type &&
		fa.Expect == wa.Expect &&
		fa.Path == wa.Path &&
		fmt.Sprintf("%v", fa.Value) == fmt.Sprintf("%v", wa.Value) &&
		fa.Exception == wa.Exception {
		return []Reason{{
			Code: "tautological_branches",
			Message: "failed_approach.assertion and working_approach.assertion " +
				"are identical — the test cannot discriminate between the two paths",
		}}
	}
	return nil
}

// checkMutationStructure enforces the schema's submission-time rules 1-3
// from schema/entry.schema.yaml lines 583-587:
//   1. At least one mutation with strategy: mutate_fixture
//   2. At least one mutation expecting result: fail
//   3. At least one mutation expecting result: unchanged
//
// Rules 2-3 accept either expected_result or per-branch outcomes
// containing the relevant value — the schema's oneOf permits both shapes.
func checkMutationStructure(e *Entry) []Reason {
	var out []Reason
	muts := e.Verification.Mutations

	hasFixture := false
	hasFail := false
	hasUnchanged := false

	for _, m := range muts {
		if m.Strategy == "mutate_fixture" {
			hasFixture = true
		}
		if mutationExpects(m, "fail") {
			hasFail = true
		}
		// "assertion_does_not_match" is the assertion-only equivalent of
		// "fail" — the test signals that the mutation broke the claim.
		if mutationExpects(m, "assertion_does_not_match") {
			hasFail = true
		}
		if mutationExpects(m, "unchanged") {
			hasUnchanged = true
		}
	}

	if !hasFixture {
		out = append(out, Reason{
			Code:    "missing_mutate_fixture",
			Message: "no mutation with strategy: mutate_fixture (schema rule §1)",
		})
	}
	if !hasFail {
		out = append(out, Reason{
			Code: "missing_breaking_mutation",
			Message: "no mutation expects result fail / assertion_does_not_match " +
				"(schema rule §2 — the test must be falsifiable)",
		})
	}
	if !hasUnchanged {
		out = append(out, Reason{
			Code: "missing_unchanged_mutation",
			Message: "no mutation expects result unchanged (schema rule §3 — " +
				"prevents over-broad mutations from masking the discriminator)",
		})
	}
	return out
}

// checkMutationDiscriminating enforces docs/03 §5.3 step 4: at least one
// mutation must, when applied to the working branch, cause the assertion
// to fail. Otherwise the test is not discriminating — it would still
// pass even after the fix is broken.
func checkMutationDiscriminating(e *Entry) []Reason {
	for _, m := range e.Verification.Mutations {
		// Targets the working branch explicitly.
		if m.Branch == "working_approach" || m.Branch == "both" {
			if m.ExpectedResult == "fail" || m.ExpectedResult == "assertion_does_not_match" {
				return nil
			}
		}
		// Or via per-branch outcomes.
		if outcome, ok := m.ExpectedBranchOutcome["working_approach"]; ok {
			if outcome == "fail" || outcome == "assertion_does_not_match" {
				return nil
			}
		}
		// Mutations without an explicit branch and with expected_result: fail
		// also count: if the mutation acts on a working_approach token by
		// inspection of the target string, the discriminator is implicit.
		if m.Branch == "" && m.ExpectedResult == "fail" {
			// Conservative: only count when the target path mentions the
			// working branch, otherwise we cannot prove it discriminates.
			if containsToken(m.Target, "working_approach") {
				return nil
			}
		}
	}
	return []Reason{{
		Code: "no_discriminating_mutation",
		Message: "no mutation breaks the working_approach assertion — " +
			"the test cannot prove the fix is what made the working branch pass",
	}}
}

// checkPrimitivesRegistered enforces the schema's submission-time rule 7:
// every primitives_required entry must be in the registered set. The
// registered set is mirrored from the schema in primitives.go.
func checkPrimitivesRegistered(e *Entry) []Reason {
	var unknown []string
	for _, p := range e.Verification.PrimitivesRequired {
		if !IsRegisteredPrimitive(p) {
			unknown = append(unknown, p)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	return []Reason{{
		Code:    "unregistered_primitives",
		Message: fmt.Sprintf("primitives_required contains unregistered values: %v", unknown),
	}}
}

// checkAssertionOnlyShape enforces the assertion_only-specific schema
// constraints: primitives_required is non-empty, no cassette, and the
// timeout_seconds is set within the schema's [0, 300] range.
func checkAssertionOnlyShape(e *Entry) []Reason {
	var out []Reason
	if len(e.Verification.PrimitivesRequired) == 0 {
		out = append(out, Reason{
			Code:    "missing_primitives_required",
			Message: "assertion_only entries must declare primitives_required (schema)",
		})
	}
	if e.Verification.Cassette != nil {
		out = append(out, Reason{
			Code:    "unexpected_cassette",
			Message: "assertion_only entries must not declare a cassette (schema)",
		})
	}
	if e.Verification.TimeoutSeconds <= 0 || e.Verification.TimeoutSeconds > 300 {
		out = append(out, Reason{
			Code: "invalid_timeout",
			Message: fmt.Sprintf(
				"timeout_seconds=%v is outside the schema range (0, 300]",
				e.Verification.TimeoutSeconds,
			),
		})
	}
	return out
}

// mutationExpects reports whether m is asking for the given outcome,
// either via expected_result or via any expected_branch_outcome value.
func mutationExpects(m Mutation, want string) bool {
	if m.ExpectedResult == want {
		return true
	}
	for _, v := range m.ExpectedBranchOutcome {
		if v == want {
			return true
		}
	}
	return false
}

// containsToken returns true when target contains the literal token
// surrounded by non-identifier boundaries — used for conservative
// discrimination matching when no explicit branch is set.
func containsToken(target, token string) bool {
	for i := 0; i+len(token) <= len(target); i++ {
		if target[i:i+len(token)] != token {
			continue
		}
		left := i == 0 || !isIdent(target[i-1])
		right := i+len(token) == len(target) || !isIdent(target[i+len(token)])
		if left && right {
			return true
		}
	}
	return false
}

func isIdent(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') || b == '_'
}
