package verify

import (
	"strings"
	"testing"

	"github.com/runlog-org/runlog-verifier/internal/verify/runner"
)

// TestMatchOutcomeCollectionPropertyTrueMatch — happy path: spec requires
// duplicates, repr has duplicate lines → 0 reasons.
func TestMatchOutcomeCollectionPropertyTrueMatch(t *testing.T) {
	got := runner.ExecResult{
		TypeName: "str",
		Repr:     "a\nb\nc\nb\n",
	}
	diff := map[string]any{
		"failed_branch_collection_has_duplicates": true,
	}
	reasons := matchOutcome(branchFailed, got, diff)
	if len(reasons) != 0 {
		t.Fatalf("expected no reasons, got %v", reasons)
	}
}

// TestMatchOutcomeCollectionPropertyFalseMatch — happy path: spec requires
// no duplicates, repr has all-unique lines → 0 reasons.
func TestMatchOutcomeCollectionPropertyFalseMatch(t *testing.T) {
	got := runner.ExecResult{
		TypeName: "str",
		Repr:     "x\ny\nz\n",
	}
	diff := map[string]any{
		"working_branch_collection_has_duplicates": false,
	}
	reasons := matchOutcome(branchWorking, got, diff)
	if len(reasons) != 0 {
		t.Fatalf("expected no reasons, got %v", reasons)
	}
}

// TestMatchOutcomeCollectionPropertyTrueMismatch — spec requires duplicates
// but repr has all-unique lines → 1 reason differential_collection_property_mismatch.
// Message must contain has_duplicates=false and spec required has_duplicates=true.
func TestMatchOutcomeCollectionPropertyTrueMismatch(t *testing.T) {
	got := runner.ExecResult{
		TypeName: "str",
		Repr:     "a\nb\nc\n",
	}
	diff := map[string]any{
		"failed_branch_collection_has_duplicates": true,
	}
	reasons := matchOutcome(branchFailed, got, diff)
	if len(reasons) != 1 {
		t.Fatalf("expected 1 reason, got %v", reasons)
	}
	if reasons[0].Code != "differential_collection_property_mismatch" {
		t.Fatalf("expected differential_collection_property_mismatch, got %v", reasons)
	}
	if !strings.Contains(reasons[0].Message, "has_duplicates=false") {
		t.Fatalf("expected message to contain has_duplicates=false, got %q", reasons[0].Message)
	}
	if !strings.Contains(reasons[0].Message, "spec required has_duplicates=true") {
		t.Fatalf("expected message to contain 'spec required has_duplicates=true', got %q", reasons[0].Message)
	}
}

// TestMatchOutcomeCollectionPropertyFalseMismatch — spec requires no
// duplicates but repr has duplicates → 1 reason
// differential_collection_property_mismatch.
func TestMatchOutcomeCollectionPropertyFalseMismatch(t *testing.T) {
	got := runner.ExecResult{
		TypeName: "str",
		Repr:     "x\ny\nx\n",
	}
	diff := map[string]any{
		"working_branch_collection_has_duplicates": false,
	}
	reasons := matchOutcome(branchWorking, got, diff)
	if len(reasons) != 1 {
		t.Fatalf("expected 1 reason, got %v", reasons)
	}
	if reasons[0].Code != "differential_collection_property_mismatch" {
		t.Fatalf("expected differential_collection_property_mismatch, got %v", reasons)
	}
}

// TestMatchOutcomeCollectionPropertyEmptyReprWithFalse — empty repr, spec
// requires no duplicates → 0 reasons (empty list has no duplicates,
// satisfies false).
func TestMatchOutcomeCollectionPropertyEmptyReprWithFalse(t *testing.T) {
	got := runner.ExecResult{
		TypeName: "str",
		Repr:     "",
	}
	diff := map[string]any{
		"failed_branch_collection_has_duplicates": false,
	}
	reasons := matchOutcome(branchFailed, got, diff)
	if len(reasons) != 0 {
		t.Fatalf("expected no reasons, got %v", reasons)
	}
}

// TestMatchOutcomeCollectionPropertyEmptyReprWithTrue — empty repr, spec
// requires duplicates → 1 reason differential_collection_property_mismatch.
func TestMatchOutcomeCollectionPropertyEmptyReprWithTrue(t *testing.T) {
	got := runner.ExecResult{
		TypeName: "str",
		Repr:     "",
	}
	diff := map[string]any{
		"failed_branch_collection_has_duplicates": true,
	}
	reasons := matchOutcome(branchFailed, got, diff)
	if len(reasons) != 1 {
		t.Fatalf("expected 1 reason, got %v", reasons)
	}
	if reasons[0].Code != "differential_collection_property_mismatch" {
		t.Fatalf("expected differential_collection_property_mismatch, got %v", reasons)
	}
}

// TestMatchOutcomeCollectionPropertyWhitespaceTrimming — lines with leading
// and trailing whitespace are trimmed before dedup: "  a\n a\nb\n" trims to
// ["a", "a", "b"] which has duplicates. Spec requires has_duplicates: true →
// 0 reasons.
func TestMatchOutcomeCollectionPropertyWhitespaceTrimming(t *testing.T) {
	got := runner.ExecResult{
		TypeName: "str",
		Repr:     "  a\n a\nb\n",
	}
	diff := map[string]any{
		"failed_branch_collection_has_duplicates": true,
	}
	reasons := matchOutcome(branchFailed, got, diff)
	if len(reasons) != 0 {
		t.Fatalf("expected no reasons (trimmed duplicates satisfy true), got %v", reasons)
	}
}

// TestMatchOutcomeCollectionPropertyEmptyLinesIgnored — blank lines between
// items are filtered out after trim: "a\n\n\nb\n" → ["a", "b"] → no
// duplicates. Spec requires has_duplicates: false → 0 reasons.
func TestMatchOutcomeCollectionPropertyEmptyLinesIgnored(t *testing.T) {
	got := runner.ExecResult{
		TypeName: "str",
		Repr:     "a\n\n\nb\n",
	}
	diff := map[string]any{
		"working_branch_collection_has_duplicates": false,
	}
	reasons := matchOutcome(branchWorking, got, diff)
	if len(reasons) != 0 {
		t.Fatalf("expected no reasons (empty lines ignored, no duplicates), got %v", reasons)
	}
}

// TestMatchOutcomeCollectionPropertyMalformedNonBool — spec value is an int,
// not a bool → 1 reason malformed_collection_property_spec. Message names
// the branch and the actual type.
func TestMatchOutcomeCollectionPropertyMalformedNonBool(t *testing.T) {
	got := runner.ExecResult{TypeName: "str", Repr: "a\nb\n"}
	diff := map[string]any{
		"failed_branch_collection_has_duplicates": 42,
	}
	reasons := matchOutcome(branchFailed, got, diff)
	if len(reasons) != 1 {
		t.Fatalf("expected 1 reason, got %v", reasons)
	}
	if reasons[0].Code != "malformed_collection_property_spec" {
		t.Fatalf("expected malformed_collection_property_spec, got %v", reasons)
	}
	if !strings.Contains(reasons[0].Message, "failed") {
		t.Fatalf("expected message to name the branch, got %q", reasons[0].Message)
	}
	if !strings.Contains(reasons[0].Message, "int") {
		t.Fatalf("expected message to name the actual type (int), got %q", reasons[0].Message)
	}
}

// TestMatchOutcomeCollectionPropertyMalformedString — spec value is a string
// "yes" (not a bool) → 1 reason malformed_collection_property_spec.
func TestMatchOutcomeCollectionPropertyMalformedString(t *testing.T) {
	got := runner.ExecResult{TypeName: "str", Repr: "a\nb\n"}
	diff := map[string]any{
		"working_branch_collection_has_duplicates": "yes",
	}
	reasons := matchOutcome(branchWorking, got, diff)
	if len(reasons) != 1 {
		t.Fatalf("expected 1 reason, got %v", reasons)
	}
	if reasons[0].Code != "malformed_collection_property_spec" {
		t.Fatalf("expected malformed_collection_property_spec, got %v", reasons)
	}
}

// TestMatchOutcomeCollectionPropertyWorkingBranchKeyRouting verifies the
// working branch's key is not accidentally evaluated on the failed branch.
// With only working_* key present, evaluating against branchFailed must
// return no reasons (no expectation). Evaluating against branchWorking with
// a mismatching spec fires 1 reason.
func TestMatchOutcomeCollectionPropertyWorkingBranchKeyRouting(t *testing.T) {
	got := runner.ExecResult{
		TypeName: "str",
		Repr:     "x\ny\nz\n",
	}
	diff := map[string]any{
		// unique lines but spec says true (expects duplicates) → mismatch on working branch
		"working_branch_collection_has_duplicates": true,
	}
	// Failed branch has no keys → silent accept.
	if reasons := matchOutcome(branchFailed, got, diff); len(reasons) != 0 {
		t.Fatalf("failed branch with only working key should be silent, got %v", reasons)
	}
	// Working branch — has_duplicates=false but spec requires true → mismatch.
	reasons := matchOutcome(branchWorking, got, diff)
	if len(reasons) != 1 || reasons[0].Code != "differential_collection_property_mismatch" {
		t.Fatalf("expected single differential_collection_property_mismatch on working branch, got %v", reasons)
	}
}

// TestMatchOutcomeCollectionPropertyFailedBranchKeyRouting — inverse:
// only failed_* key present, working branch evaluates silently.
func TestMatchOutcomeCollectionPropertyFailedBranchKeyRouting(t *testing.T) {
	got := runner.ExecResult{
		TypeName: "str",
		Repr:     "a\nb\na\n",
	}
	diff := map[string]any{
		// repr has duplicates but spec says false → mismatch on failed branch
		"failed_branch_collection_has_duplicates": false,
	}
	if reasons := matchOutcome(branchWorking, got, diff); len(reasons) != 0 {
		t.Fatalf("working branch with only failed key should be silent, got %v", reasons)
	}
	reasons := matchOutcome(branchFailed, got, diff)
	if len(reasons) != 1 || reasons[0].Code != "differential_collection_property_mismatch" {
		t.Fatalf("expected single differential_collection_property_mismatch on failed branch, got %v", reasons)
	}
}

// TestMatchOutcomeCollectionPropertyLayeredWithReturn — both
// failed_branch_must_return AND failed_branch_collection_has_duplicates
// declared. Return passes, collection check fails → exactly 1 reason of
// differential_collection_property_mismatch (proves additive layering).
func TestMatchOutcomeCollectionPropertyLayeredWithReturn(t *testing.T) {
	got := runner.ExecResult{
		TypeName:     "str",
		Repr:         "a\nb\nc\n",
		Serializable: true,
		JSONValue:    []byte(`"a\nb\nc\n"`),
	}
	diff := map[string]any{
		"failed_branch_must_return": map[string]any{
			"type":         "str",
			"value_equals": "a\nb\nc\n",
		},
		// unique lines but spec says true (expects duplicates) → mismatch
		"failed_branch_collection_has_duplicates": true,
	}
	reasons := matchOutcome(branchFailed, got, diff)
	if len(reasons) != 1 {
		t.Fatalf("expected 1 reason (collection mismatch only), got %v", reasons)
	}
	if reasons[0].Code != "differential_collection_property_mismatch" {
		t.Fatalf("expected differential_collection_property_mismatch, got %v", reasons)
	}
}

// TestMatchOutcomeCollectionPropertyLayeredBothFail — return fails AND
// collection check fails → 2 reasons: wrong_return_value +
// differential_collection_property_mismatch (additive layering, both fire).
func TestMatchOutcomeCollectionPropertyLayeredBothFail(t *testing.T) {
	got := runner.ExecResult{
		TypeName:     "str",
		Repr:         "a\nb\nc\n",
		Serializable: true,
		JSONValue:    []byte(`"a\nb\nc\n"`),
	}
	diff := map[string]any{
		"failed_branch_must_return": map[string]any{
			"type":         "str",
			"value_equals": "different value",
		},
		// unique lines but spec says true → mismatch
		"failed_branch_collection_has_duplicates": true,
	}
	reasons := matchOutcome(branchFailed, got, diff)
	if len(reasons) != 2 {
		t.Fatalf("expected 2 reasons (return + collection), got %v", reasons)
	}
	codes := reasonCodes(reasons)
	wantCodes := map[string]bool{
		"wrong_return_value":                         true,
		"differential_collection_property_mismatch": true,
	}
	for _, c := range codes {
		if !wantCodes[c] {
			t.Fatalf("unexpected reason code %q in %v", c, codes)
		}
	}
}

// TestMatchOutcomeCollectionPropertyRaisedWhenOnlyCollectionDeclared — the
// branch raised an exception but only failed_branch_collection_has_duplicates
// was declared (no must_raise). Surface unexpected_exception — mirrors the
// plan_node test of the same shape.
func TestMatchOutcomeCollectionPropertyRaisedWhenOnlyCollectionDeclared(t *testing.T) {
	got := runner.ExecResult{
		Raised:    true,
		Exception: "RuntimeError",
		Message:   "something went wrong",
	}
	diff := map[string]any{
		"failed_branch_collection_has_duplicates": true,
	}
	reasons := matchOutcome(branchFailed, got, diff)
	if len(reasons) != 1 {
		t.Fatalf("expected 1 reason, got %v", reasons)
	}
	if reasons[0].Code != "unexpected_exception" {
		t.Fatalf("expected unexpected_exception, got %v", reasons)
	}
}

// unitCollectionPropertyYAML — synthetic e2e fixture exercising the
// collection-property check through verify.Run on the Python unit tier.
//
// The PythonDriver surfaces got.Repr as repr(_v_RESULT). For
// parseCollectionLines to see real newlines in the repr, each action uses
// a minimal custom class whose __repr__ returns a literal newline-
// separated string. The YAML body uses double-quote escapes so \n in the
// Python source becomes a real newline, which then propagates through
// json.dumps and Go's json.Unmarshal as a real newline in got.Repr.
//
//   failed approach: repr → "a\nb\na\nc" (4 lines, has duplicates → true)
//   working approach: repr → "x\ny\nz"   (3 lines, no duplicates → false)
const unitCollectionPropertyYAML = `
unit_id: unit-collection-property-greenpath
domain: [test]
version_constraints: { spec: { name: test } }
failed_approach:
  description: returns a list with duplicates
  setup: []
  action:
    - { type: code, lang: python, body: "class _L:\n    def __repr__(self): return 'a\\nb\\na\\nc'\n$RESULT = _L()" }
  assertion: { type: returns, expect: fail }
working_approach:
  description: returns a list with no duplicates
  setup: []
  action:
    - { type: code, lang: python, body: "class _L:\n    def __repr__(self): return 'x\\ny\\nz'\n$RESULT = _L()" }
  assertion: { type: returns, expect: success }
verification:
  type: unit
  isolation: function
  differential:
    failed_branch_collection_has_duplicates: true
    working_branch_collection_has_duplicates: false
  timeout_seconds: 5
`

// TestRunUnitCollectionPropertyVerified is the e2e green-path gate: a
// synthetic unit-tier entry with collection-property specs verifies green
// through verify.Run end-to-end. Skips when python3 isn't on PATH.
func TestRunUnitCollectionPropertyVerified(t *testing.T) {
	skipIfNoPython3(t)
	res, err := Run([]byte(unitCollectionPropertyYAML))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "verified" {
		t.Fatalf("status=%q, reasons=%v", res.Status, res.Reasons)
	}
}

// TestRunUnitCollectionPropertyRejected confirms the rejected case end-to-end:
// when the working branch's spec is flipped to true (but the action produces
// no duplicates), Run surfaces differential_collection_property_mismatch.
func TestRunUnitCollectionPropertyRejected(t *testing.T) {
	skipIfNoPython3(t)
	yaml := strings.Replace(
		unitCollectionPropertyYAML,
		"working_branch_collection_has_duplicates: false",
		"working_branch_collection_has_duplicates: true",
		1,
	)
	res, err := Run([]byte(yaml))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "rejected" {
		t.Fatalf("status=%q, want rejected (reasons=%v)", res.Status, res.Reasons)
	}
	if !hasReason(res.Reasons, "differential_collection_property_mismatch") {
		t.Fatalf("expected differential_collection_property_mismatch, got %v", res.Reasons)
	}
}
