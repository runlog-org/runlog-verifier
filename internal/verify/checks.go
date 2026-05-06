package verify

import (
	"fmt"
	"regexp"
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
//  1. At least one mutation with strategy: mutate_fixture
//  2. At least one mutation expecting result: fail
//  3. At least one mutation expecting result: unchanged
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
		if mutationExpectsBreak(m) {
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
			if isBreakLikeOutcome(m.ExpectedResult) {
				return nil
			}
		}
		// Or via per-branch outcomes.
		if outcome, ok := m.ExpectedBranchOutcome["working_approach"]; ok {
			if isBreakLikeOutcome(outcome) {
				return nil
			}
		}
		// Mutations without an explicit branch and with expected_result: fail
		// also count: if the mutation acts on a working_approach token by
		// inspection of the target string, the discriminator is implicit.
		if m.Branch == "" && m.ExpectedResult == "fail" {
			// Conservative: only count when the target path mentions the
			// working branch, otherwise we cannot prove it discriminates.
			if workingApproachToken.MatchString(m.Target) {
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
// timeout_seconds is within the allowed range.
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
	if r := validateTimeoutSeconds(e); r != nil {
		out = append(out, *r)
	}
	return out
}

// validateTimeoutSeconds checks that e.Verification.TimeoutSeconds is in the
// schema-mandated range. Per runlog-schema/entry.schema.yaml (exclusiveMinimum:
// 0, maximum: 300), timeout_seconds must be in (0, 300]. Zero (unset) is NOT
// accepted here: accepting 0 would codify the runner-side fallback as a
// contract, which diverges from the schema. The runner-side defaults in
// runner/python.go and runner/subprocess.go remain as a safety net for code
// paths that bypass this helper, but no schema-valid entry can reach 0.
//
// Reason code: invalid_timeout.
func validateTimeoutSeconds(e *Entry) *Reason {
	ts := e.Verification.TimeoutSeconds
	if ts <= 0 || ts > 300 {
		r := Reason{
			Code: "invalid_timeout",
			Message: fmt.Sprintf(
				"timeout_seconds=%v is outside the schema range (0, 300]",
				ts,
			),
		}
		return &r
	}
	return nil
}

// isBreakLikeOutcome reports whether the raw outcome string signals that a
// mutation is expected to break the branch — covers both "fail" and the
// assertion-only alias "assertion_does_not_match". Use this for
// ExpectedBranchOutcome values; for ExpectedResult, use isBreakLikeResult
// instead — the schema rejects assertion_does_not_match in expected_result.
func isBreakLikeOutcome(s string) bool {
	return s == "fail" || s == "assertion_does_not_match"
}

// isBreakLikeResult reports whether the raw outcome string signals a break
// for the expected_result field, which uses the narrower 4-value enum that
// does NOT include "assertion_does_not_match". Only "fail" counts here.
func isBreakLikeResult(s string) bool {
	return s == "fail"
}

// mutationExpectsBreak reports whether mutation m expects a break-like
// outcome on any branch (either via expected_result or expected_branch_outcome).
// Uses the enum-appropriate helper for each field: isBreakLikeResult for
// expected_result (4-value enum) and isBreakLikeOutcome for
// expected_branch_outcome values (5-value enum, includes assertion_does_not_match).
func mutationExpectsBreak(m Mutation) bool {
	if isBreakLikeResult(m.ExpectedResult) {
		return true
	}
	for _, v := range m.ExpectedBranchOutcome {
		if isBreakLikeOutcome(v) {
			return true
		}
	}
	return false
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

// workingApproachToken matches the literal "working_approach" surrounded by
// word boundaries — used for conservative discrimination matching when no
// explicit branch is set on a mutation. \b anchors on [A-Za-z0-9_], which
// matches the prior hand-rolled isIdent definition byte-for-byte.
var workingApproachToken = regexp.MustCompile(`\bworking_approach\b`)
