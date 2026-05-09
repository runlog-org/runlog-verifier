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
