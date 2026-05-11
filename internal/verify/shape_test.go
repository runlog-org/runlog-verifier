package verify

import (
	"strings"
	"testing"
)

// TestSplitInputsRejectsMalformedFailedApproach asserts that a per-branch
// shape with a malformed failed_approach value (non-mapping) surfaces a
// typed error rather than silently dropping the inputs. Closes the
// silent-discard hole at shape.go:splitInputs that the schema-side gate
// would catch upstream but a CLI-path entry would otherwise sneak through.
func TestSplitInputsRejectsMalformedFailedApproach(t *testing.T) {
	diff := map[string]any{
		"inputs": map[string]any{
			"failed_approach":  "not-a-mapping",
			"working_approach": map[string]any{"$X": 1},
		},
	}
	_, _, err := splitInputs(diff)
	if err == nil {
		t.Fatal("splitInputs accepted malformed failed_approach (expected error)")
	}
	if !strings.Contains(err.Error(), "failed_approach is not a mapping") {
		t.Errorf("error message does not name the offending key: %v", err)
	}
}

// TestSplitInputsRejectsMalformedWorkingApproach is the symmetric case for
// working_approach.
func TestSplitInputsRejectsMalformedWorkingApproach(t *testing.T) {
	diff := map[string]any{
		"inputs": map[string]any{
			"failed_approach":  map[string]any{"$X": 1},
			"working_approach": []any{"unexpected", "list"},
		},
	}
	_, _, err := splitInputs(diff)
	if err == nil {
		t.Fatal("splitInputs accepted malformed working_approach (expected error)")
	}
	if !strings.Contains(err.Error(), "working_approach is not a mapping") {
		t.Errorf("error message does not name the offending key: %v", err)
	}
}

// TestSplitInputsAcceptsAbsentBranches asserts that the per-branch shape with
// only one branch declared (the other branch absent) still works — the test
// guards against a regression where the new error path mistakenly fires on
// missing keys instead of malformed ones.
func TestSplitInputsAcceptsAbsentBranches(t *testing.T) {
	diff := map[string]any{
		"inputs": map[string]any{
			"failed_approach": map[string]any{"$X": 1},
			// working_approach intentionally absent
		},
	}
	failed, working, err := splitInputs(diff)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if failed == nil || failed["$X"] != 1 {
		t.Errorf("failed branch lost inputs: %v", failed)
	}
	if working != nil {
		t.Errorf("working branch should be nil, got %v", working)
	}
}

// TestSplitInputsAcceptsSharedShape pins the legacy shared-shape behaviour
// (no failed_approach / working_approach key → both branches see the same
// map). Nothing should change here; included so the new per-branch
// validation doesn't accidentally regress the shared path.
func TestSplitInputsAcceptsSharedShape(t *testing.T) {
	diff := map[string]any{
		"inputs": map[string]any{
			"$X": 1,
			"$Y": "two",
		},
	}
	failed, working, err := splitInputs(diff)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if failed["$X"] != 1 || failed["$Y"] != "two" {
		t.Errorf("failed branch lost shared inputs: %v", failed)
	}
	if working["$X"] != 1 || working["$Y"] != "two" {
		t.Errorf("working branch lost shared inputs: %v", working)
	}
}

// TestMergeLiterals_ChainedLiteralResolution pins the F94 use case: a
// per-branch input value of form `$LITERAL_N` must be resolved through the
// literal block at merge time so downstream substituteVars (which is single
// pass, longest-key-first) sees the underlying value rather than the raw
// `$LITERAL_N` string. Without this resolution, `FROM $BASE_IMAGE` in a
// Dockerfile body would substitute to `FROM $LITERAL_1`, which docker then
// treats as an unset ARG and the build fails.
func TestMergeLiterals_ChainedLiteralResolution(t *testing.T) {
	literals := map[string]any{
		"$LITERAL_1": map[string]any{"value": "debian:12"},
		"$LITERAL_2": map[string]any{"value": "debian:12-slim"},
	}
	inputs := map[string]any{
		"$BASE_IMAGE": "$LITERAL_1",
	}
	merged := mergeLiterals(literals, inputs)
	if merged["$BASE_IMAGE"] != "debian:12" {
		t.Errorf("$BASE_IMAGE not resolved through $LITERAL_1: got %v (want %q)", merged["$BASE_IMAGE"], "debian:12")
	}
	if merged["$LITERAL_1"] != "debian:12" {
		t.Errorf("$LITERAL_1 lost its literal value: got %v", merged["$LITERAL_1"])
	}
	if merged["$LITERAL_2"] != "debian:12-slim" {
		t.Errorf("$LITERAL_2 lost its literal value: got %v", merged["$LITERAL_2"])
	}
}

// TestMergeLiterals_NoResolution_WhenKeyMissing confirms that a $-prefixed
// value referencing a key NOT in the merged map passes through unchanged.
// substituteVars handles unset references at the bash/Dockerfile level
// (passes them through to shell or docker's ARG handling); a missing
// reference must not be an error here.
func TestMergeLiterals_NoResolution_WhenKeyMissing(t *testing.T) {
	literals := map[string]any{
		"$LITERAL_1": map[string]any{"value": "x"},
	}
	inputs := map[string]any{
		"$BASE_IMAGE": "$NOPE",
	}
	merged := mergeLiterals(literals, inputs)
	if merged["$BASE_IMAGE"] != "$NOPE" {
		t.Errorf("$BASE_IMAGE should pass through unresolved when $NOPE is missing: got %v", merged["$BASE_IMAGE"])
	}
	if merged["$LITERAL_1"] != "x" {
		t.Errorf("$LITERAL_1 lost its literal value: got %v", merged["$LITERAL_1"])
	}
}

// TestMergeLiterals_SelfReferenceIsNoOp pins the self-reference guard:
// `$X → $X` must be left alone rather than re-resolved into itself (a no-op
// in this case, but the guard also future-proofs against any panic/loop if
// the implementation grew).
func TestMergeLiterals_SelfReferenceIsNoOp(t *testing.T) {
	// literals must be non-nil for mergeLiterals to enter its full path
	// (the early `len(literals) == 0` return otherwise skips resolution).
	literals := map[string]any{
		"$ANCHOR": map[string]any{"value": "anchor"},
	}
	inputs := map[string]any{
		"$X": "$X",
	}
	merged := mergeLiterals(literals, inputs)
	if merged["$X"] != "$X" {
		t.Errorf("self-reference should be left alone: got %v (want %q)", merged["$X"], "$X")
	}
}

// TestMergeLiterals_ChainedResolutionRespectsPerBranchOverride confirms that
// the resolution pass is a value-not-key transformation: only string values
// starting with `$` that match another map key get rewritten. A per-branch
// override of $LITERAL_1's own value (to a plain string) stays put — the
// override has already won the collision before the resolution pass walks
// the map.
func TestMergeLiterals_ChainedResolutionRespectsPerBranchOverride(t *testing.T) {
	literals := map[string]any{
		"$LITERAL_1": map[string]any{"value": "v1"},
	}
	inputs := map[string]any{
		"$LITERAL_1": "overridden",
	}
	merged := mergeLiterals(literals, inputs)
	if merged["$LITERAL_1"] != "overridden" {
		t.Errorf("per-branch override lost: got %v (want %q)", merged["$LITERAL_1"], "overridden")
	}
}

// TestMergeLiterals_NonStringValueNotRewritten confirms numeric / bool /
// other-typed inputs pass through the resolution pass untouched (the
// `s, ok := v.(string); if !ok { continue }` guard).
func TestMergeLiterals_NonStringValueNotRewritten(t *testing.T) {
	literals := map[string]any{
		"$ANCHOR": map[string]any{"value": "anchor"},
	}
	inputs := map[string]any{
		"$COUNT": 42,
		"$FLAG":  true,
	}
	merged := mergeLiterals(literals, inputs)
	if merged["$COUNT"] != 42 {
		t.Errorf("int value rewritten: got %v (want 42)", merged["$COUNT"])
	}
	if merged["$FLAG"] != true {
		t.Errorf("bool value rewritten: got %v (want true)", merged["$FLAG"])
	}
}
