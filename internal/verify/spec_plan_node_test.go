package verify

import (
	"strings"
	"testing"

	"github.com/runlog-org/runlog-verifier/internal/verify/runner"
)

// reasonCodes flattens the list of Reason.Code values for compact assertion.
func reasonCodes(rs []Reason) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Code
	}
	return out
}

// TestMatchOutcomePlanNodeSingleStringMatch covers the happy path: the
// substring is present in got.Repr — matchOutcome returns no reasons.
func TestMatchOutcomePlanNodeSingleStringMatch(t *testing.T) {
	got := runner.ExecResult{
		TypeName: "str",
		Repr:     "Seq Scan on tenant_idx (cost=0.00..1234.56)",
	}
	diff := map[string]any{
		"failed_branch_must_contain_plan_node": "Seq Scan",
	}
	reasons := matchOutcome(branchFailed, got, diff)
	if len(reasons) != 0 {
		t.Fatalf("expected no reasons, got %v", reasons)
	}
}

// TestMatchOutcomePlanNodeSingleStringMismatch surfaces the typed
// differential_plan_node_mismatch reason when the substring is absent.
func TestMatchOutcomePlanNodeSingleStringMismatch(t *testing.T) {
	got := runner.ExecResult{
		TypeName: "str",
		Repr:     "Index Scan using idx_pk on users",
	}
	diff := map[string]any{
		"failed_branch_must_contain_plan_node": "Seq Scan",
	}
	reasons := matchOutcome(branchFailed, got, diff)
	if len(reasons) != 1 {
		t.Fatalf("expected 1 reason, got %v", reasons)
	}
	if reasons[0].Code != "differential_plan_node_mismatch" {
		t.Fatalf("expected differential_plan_node_mismatch, got %v", reasons)
	}
	if !strings.Contains(reasons[0].Message, "Seq Scan") {
		t.Fatalf("expected message to name the missing needle, got %q", reasons[0].Message)
	}
}

// TestMatchOutcomePlanNodeAnyMatch covers the any-of happy path: at least
// one needle in the list is present in got.Repr.
func TestMatchOutcomePlanNodeAnyMatch(t *testing.T) {
	got := runner.ExecResult{
		TypeName: "str",
		Repr:     "Bitmap Index Scan on idx_gin (cost=0.00..12.34)",
	}
	diff := map[string]any{
		"working_branch_must_contain_plan_node_any": []any{
			"Bitmap Index Scan",
			"Index Scan",
		},
	}
	reasons := matchOutcome(branchWorking, got, diff)
	if len(reasons) != 0 {
		t.Fatalf("expected no reasons, got %v", reasons)
	}
}

// TestMatchOutcomePlanNodeAnyMismatch — none of the listed needles appear
// in got.Repr, so the typed mismatch reason fires.
func TestMatchOutcomePlanNodeAnyMismatch(t *testing.T) {
	got := runner.ExecResult{
		TypeName: "str",
		Repr:     "Seq Scan on tenant_idx",
	}
	diff := map[string]any{
		"working_branch_must_contain_plan_node_any": []any{
			"Bitmap Index Scan",
			"Index Scan",
		},
	}
	reasons := matchOutcome(branchWorking, got, diff)
	if len(reasons) != 1 {
		t.Fatalf("expected 1 reason, got %v", reasons)
	}
	if reasons[0].Code != "differential_plan_node_mismatch" {
		t.Fatalf("expected differential_plan_node_mismatch, got %v", reasons)
	}
}

// TestMatchOutcomePlanNodeBothKeysAdditive verifies that both the scalar
// and _any keys are evaluated when both are present (additive semantics):
// a mismatch on either fires its own reason.
func TestMatchOutcomePlanNodeBothKeysAdditive(t *testing.T) {
	got := runner.ExecResult{
		TypeName: "str",
		Repr:     "Seq Scan on tenant_idx",
	}
	diff := map[string]any{
		"failed_branch_must_contain_plan_node":     "Seq Scan",
		"failed_branch_must_contain_plan_node_any": []any{"Hash Join", "Merge Join"},
	}
	reasons := matchOutcome(branchFailed, got, diff)
	// Scalar matches; _any does not — exactly one mismatch.
	if len(reasons) != 1 {
		t.Fatalf("expected 1 reason (only _any mismatched), got %v", reasons)
	}
	if reasons[0].Code != "differential_plan_node_mismatch" {
		t.Fatalf("expected differential_plan_node_mismatch, got %v", reasons)
	}
}

// TestMatchOutcomePlanNodeBothKeysBothMismatch confirms two reasons fire
// when both scalar and _any miss.
func TestMatchOutcomePlanNodeBothKeysBothMismatch(t *testing.T) {
	got := runner.ExecResult{
		TypeName: "str",
		Repr:     "Index Only Scan",
	}
	diff := map[string]any{
		"failed_branch_must_contain_plan_node":     "Seq Scan",
		"failed_branch_must_contain_plan_node_any": []any{"Hash Join", "Merge Join"},
	}
	reasons := matchOutcome(branchFailed, got, diff)
	if len(reasons) != 2 {
		t.Fatalf("expected 2 reasons, got %v", reasons)
	}
	for _, r := range reasons {
		if r.Code != "differential_plan_node_mismatch" {
			t.Fatalf("expected all reasons to be differential_plan_node_mismatch, got %v", reasons)
		}
	}
}

// TestMatchOutcomePlanNodeLayeredWithReturn confirms plan_node layers onto
// the existing return-spec check rather than replacing it.
func TestMatchOutcomePlanNodeLayeredWithReturn(t *testing.T) {
	// got.JSONValue must be set so canonicalizeJSON succeeds; the type and
	// value match the return spec, but the plan_node check fails.
	got := runner.ExecResult{
		TypeName:     "str",
		Repr:         "Seq Scan on tenant_idx",
		Serializable: true,
		JSONValue:    []byte(`"Seq Scan on tenant_idx"`),
	}
	diff := map[string]any{
		"failed_branch_must_return": map[string]any{
			"type":         "str",
			"value_equals": "Seq Scan on tenant_idx",
		},
		"failed_branch_must_contain_plan_node": "Bitmap Index Scan",
	}
	reasons := matchOutcome(branchFailed, got, diff)
	if len(reasons) != 1 {
		t.Fatalf("expected 1 reason (plan_node mismatch only), got %v", reasons)
	}
	if reasons[0].Code != "differential_plan_node_mismatch" {
		t.Fatalf("expected differential_plan_node_mismatch, got %v", reasons)
	}
}

// TestMatchOutcomePlanNodeLayeredWithReturnBothFail — when both return-spec
// and plan_node fail, both reasons are emitted (proves additive layering).
func TestMatchOutcomePlanNodeLayeredWithReturnBothFail(t *testing.T) {
	got := runner.ExecResult{
		TypeName:     "str",
		Repr:         "Index Scan",
		Serializable: true,
		JSONValue:    []byte(`"Index Scan"`),
	}
	diff := map[string]any{
		"failed_branch_must_return": map[string]any{
			"type":         "str",
			"value_equals": "different value",
		},
		"failed_branch_must_contain_plan_node": "Seq Scan",
	}
	reasons := matchOutcome(branchFailed, got, diff)
	if len(reasons) != 2 {
		t.Fatalf("expected 2 reasons (return + plan_node), got %v", reasons)
	}
	codes := reasonCodes(reasons)
	wantCodes := map[string]bool{
		"wrong_return_value":              true,
		"differential_plan_node_mismatch": true,
	}
	for _, c := range codes {
		if !wantCodes[c] {
			t.Fatalf("unexpected reason code %q in %v", c, codes)
		}
	}
}

// TestMatchOutcomePlanNodeMalformedScalarNonString — non-string scalar spec
// surfaces malformed_plan_node_spec.
func TestMatchOutcomePlanNodeMalformedScalarNonString(t *testing.T) {
	got := runner.ExecResult{TypeName: "str", Repr: "anything"}
	diff := map[string]any{
		"failed_branch_must_contain_plan_node": 42,
	}
	reasons := matchOutcome(branchFailed, got, diff)
	if len(reasons) != 1 {
		t.Fatalf("expected 1 reason, got %v", reasons)
	}
	if reasons[0].Code != "malformed_plan_node_spec" {
		t.Fatalf("expected malformed_plan_node_spec, got %v", reasons)
	}
}

// TestMatchOutcomePlanNodeMalformedScalarEmptyString — empty needle is not a
// valid constraint.
func TestMatchOutcomePlanNodeMalformedScalarEmptyString(t *testing.T) {
	got := runner.ExecResult{TypeName: "str", Repr: "anything"}
	diff := map[string]any{
		"failed_branch_must_contain_plan_node": "",
	}
	reasons := matchOutcome(branchFailed, got, diff)
	if len(reasons) != 1 {
		t.Fatalf("expected 1 reason, got %v", reasons)
	}
	if reasons[0].Code != "malformed_plan_node_spec" {
		t.Fatalf("expected malformed_plan_node_spec, got %v", reasons)
	}
}

// TestMatchOutcomePlanNodeMalformedAnyNonList — non-list _any spec.
func TestMatchOutcomePlanNodeMalformedAnyNonList(t *testing.T) {
	got := runner.ExecResult{TypeName: "str", Repr: "anything"}
	diff := map[string]any{
		"failed_branch_must_contain_plan_node_any": "not a list",
	}
	reasons := matchOutcome(branchFailed, got, diff)
	if len(reasons) != 1 {
		t.Fatalf("expected 1 reason, got %v", reasons)
	}
	if reasons[0].Code != "malformed_plan_node_spec" {
		t.Fatalf("expected malformed_plan_node_spec, got %v", reasons)
	}
}

// TestMatchOutcomePlanNodeMalformedAnyNonStringItem — a non-string item in
// the _any list surfaces malformed_plan_node_spec.
func TestMatchOutcomePlanNodeMalformedAnyNonStringItem(t *testing.T) {
	got := runner.ExecResult{TypeName: "str", Repr: "anything"}
	diff := map[string]any{
		"failed_branch_must_contain_plan_node_any": []any{"Seq Scan", 7},
	}
	reasons := matchOutcome(branchFailed, got, diff)
	if len(reasons) != 1 {
		t.Fatalf("expected 1 reason, got %v", reasons)
	}
	if reasons[0].Code != "malformed_plan_node_spec" {
		t.Fatalf("expected malformed_plan_node_spec, got %v", reasons)
	}
}

// TestMatchOutcomePlanNodeMalformedAnyEmptyList — empty list is not a valid
// any-of constraint.
func TestMatchOutcomePlanNodeMalformedAnyEmptyList(t *testing.T) {
	got := runner.ExecResult{TypeName: "str", Repr: "anything"}
	diff := map[string]any{
		"failed_branch_must_contain_plan_node_any": []any{},
	}
	reasons := matchOutcome(branchFailed, got, diff)
	if len(reasons) != 1 {
		t.Fatalf("expected 1 reason, got %v", reasons)
	}
	if reasons[0].Code != "malformed_plan_node_spec" {
		t.Fatalf("expected malformed_plan_node_spec, got %v", reasons)
	}
}

// TestMatchOutcomePlanNodeMalformedAnyEmptyStringItem — empty string item.
func TestMatchOutcomePlanNodeMalformedAnyEmptyStringItem(t *testing.T) {
	got := runner.ExecResult{TypeName: "str", Repr: "anything"}
	diff := map[string]any{
		"failed_branch_must_contain_plan_node_any": []any{""},
	}
	reasons := matchOutcome(branchFailed, got, diff)
	if len(reasons) != 1 {
		t.Fatalf("expected 1 reason, got %v", reasons)
	}
	if reasons[0].Code != "malformed_plan_node_spec" {
		t.Fatalf("expected malformed_plan_node_spec, got %v", reasons)
	}
}

// TestMatchOutcomePlanNodeRaisedWhenOnlyPlanDeclared — the branch raised
// but only plan_node was declared (no must_raise). Surface
// unexpected_exception (branch was supposed to return for plan_node to be
// inspectable).
func TestMatchOutcomePlanNodeRaisedWhenOnlyPlanDeclared(t *testing.T) {
	got := runner.ExecResult{
		Raised:    true,
		Exception: "ValueError",
		Message:   "boom",
	}
	diff := map[string]any{
		"failed_branch_must_contain_plan_node": "Seq Scan",
	}
	reasons := matchOutcome(branchFailed, got, diff)
	if len(reasons) != 1 {
		t.Fatalf("expected 1 reason, got %v", reasons)
	}
	if reasons[0].Code != "unexpected_exception" {
		t.Fatalf("expected unexpected_exception, got %v", reasons)
	}
}

// TestMatchOutcomePlanNodeWorkingBranchKeyRouting verifies the working
// branch's keys aren't accidentally read on the failed branch and vice
// versa. With only the working_* keys present, evaluating against
// branchFailed must return no reasons (no expectation on that branch).
func TestMatchOutcomePlanNodeWorkingBranchKeyRouting(t *testing.T) {
	got := runner.ExecResult{
		TypeName: "str",
		Repr:     "Index Scan only",
	}
	diff := map[string]any{
		"working_branch_must_contain_plan_node": "Bitmap Index Scan",
	}
	// Failed branch has no keys → silent accept.
	if reasons := matchOutcome(branchFailed, got, diff); len(reasons) != 0 {
		t.Fatalf("failed branch with only working keys should be silent, got %v", reasons)
	}
	// Working branch — needle "Bitmap Index Scan" is absent → mismatch.
	reasons := matchOutcome(branchWorking, got, diff)
	if len(reasons) != 1 || reasons[0].Code != "differential_plan_node_mismatch" {
		t.Fatalf("expected single differential_plan_node_mismatch on working branch, got %v", reasons)
	}
}

// TestMatchOutcomePlanNodeFailedBranchKeyRouting — the inverse: only
// failed_* keys present, working branch routes silently.
func TestMatchOutcomePlanNodeFailedBranchKeyRouting(t *testing.T) {
	got := runner.ExecResult{
		TypeName: "str",
		Repr:     "Seq Scan",
	}
	diff := map[string]any{
		"failed_branch_must_contain_plan_node": "Bitmap Heap Scan",
	}
	if reasons := matchOutcome(branchWorking, got, diff); len(reasons) != 0 {
		t.Fatalf("working branch with only failed keys should be silent, got %v", reasons)
	}
	reasons := matchOutcome(branchFailed, got, diff)
	if len(reasons) != 1 || reasons[0].Code != "differential_plan_node_mismatch" {
		t.Fatalf("expected single differential_plan_node_mismatch on failed branch, got %v", reasons)
	}
}

// unitPlanNodeYAML — synthetic e2e fixture exercising the plan_node check
// through verify.Run on the Python unit tier. Mirrors postgres EXPLAIN-output
// shape but uses Python repr() so no infrastructure is needed.
const unitPlanNodeYAML = `
unit_id: unit-plan-node-greenpath
domain: [test]
version_constraints: { spec: { name: test } }
failed_approach:
  description: returns simulated Seq Scan plan output
  setup: []
  action:
    - { type: code, lang: python, body: "$RESULT = 'Seq Scan on tenant_idx (cost=0.00..1234.56)'" }
  assertion: { type: returns, expect: fail }
working_approach:
  description: returns simulated Bitmap Index Scan plan output
  setup: []
  action:
    - { type: code, lang: python, body: "$RESULT = 'Bitmap Index Scan on idx_gin (cost=0.00..12.34)'" }
  assertion: { type: returns, expect: success }
verification:
  type: unit
  isolation: function
  differential:
    failed_branch_must_contain_plan_node: "Seq Scan"
    working_branch_must_contain_plan_node_any: ["Bitmap Index Scan", "Index Scan"]
  timeout_seconds: 5
`

// TestRunUnitPlanNodeContainmentVerified is the e2e gate: a synthetic
// unit-tier entry with plan_node containment specs verifies green through
// verify.Run end-to-end. Skips when python3 isn't on PATH.
func TestRunUnitPlanNodeContainmentVerified(t *testing.T) {
	skipIfNoPython3(t)
	res, err := Run([]byte(unitPlanNodeYAML))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "verified" {
		t.Fatalf("status=%q, reasons=%v", res.Status, res.Reasons)
	}
}

// TestRunUnitPlanNodeContainmentRejected confirms a rejected-case end-to-end:
// when the working branch's plan_node list doesn't include the actual output,
// Run surfaces differential_plan_node_mismatch.
func TestRunUnitPlanNodeContainmentRejected(t *testing.T) {
	skipIfNoPython3(t)
	yaml := strings.Replace(
		unitPlanNodeYAML,
		`working_branch_must_contain_plan_node_any: ["Bitmap Index Scan", "Index Scan"]`,
		`working_branch_must_contain_plan_node_any: ["Hash Join", "Merge Join"]`,
		1,
	)
	res, err := Run([]byte(yaml))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "rejected" {
		t.Fatalf("status=%q, want rejected (reasons=%v)", res.Status, res.Reasons)
	}
	if !hasReason(res.Reasons, "differential_plan_node_mismatch") {
		t.Fatalf("expected differential_plan_node_mismatch, got %v", res.Reasons)
	}
}
