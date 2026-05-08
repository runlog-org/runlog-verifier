package verify

import (
	"strings"
	"testing"

	"github.com/runlog-org/runlog-verifier/internal/verify/runner"
)

// TestMatchOutcomePlanNodeTimingGtMatch — happy path for the gt key:
// got.Repr's Planning Time exceeds the threshold, no reasons fire.
func TestMatchOutcomePlanNodeTimingGtMatch(t *testing.T) {
	got := runner.ExecResult{
		TypeName: "str",
		Repr:     `[{"Plan":{"Node Type":"Update"},"Planning Time":12345.6}]`,
	}
	diff := map[string]any{
		"failed_branch_planning_time_seconds_gt": 5,
	}
	reasons := matchOutcome(branchFailed, got, diff)
	if len(reasons) != 0 {
		t.Fatalf("expected no reasons, got %v", reasons)
	}
}

// TestMatchOutcomePlanNodeTimingGtMismatch — gt threshold not exceeded
// surfaces exactly one differential_planning_time_mismatch reason whose
// message names the parsed value.
func TestMatchOutcomePlanNodeTimingGtMismatch(t *testing.T) {
	got := runner.ExecResult{
		TypeName: "str",
		Repr:     `[{"Plan":{},"Planning Time":1234.5}]`,
	}
	diff := map[string]any{
		"failed_branch_planning_time_seconds_gt": 5,
	}
	reasons := matchOutcome(branchFailed, got, diff)
	if len(reasons) != 1 {
		t.Fatalf("expected 1 reason, got %v", reasons)
	}
	if reasons[0].Code != "differential_planning_time_mismatch" {
		t.Fatalf("expected differential_planning_time_mismatch, got %v", reasons)
	}
	if !strings.Contains(reasons[0].Message, "1.23") {
		t.Fatalf("expected message to name parsed value ~1.23, got %q", reasons[0].Message)
	}
}

// TestMatchOutcomePlanNodeTimingLtMatch — happy path for the lt key.
func TestMatchOutcomePlanNodeTimingLtMatch(t *testing.T) {
	got := runner.ExecResult{
		TypeName: "str",
		Repr:     `[{"Plan":{},"Planning Time":234.5}]`,
	}
	diff := map[string]any{
		"working_branch_planning_time_seconds_lt": 1,
	}
	reasons := matchOutcome(branchWorking, got, diff)
	if len(reasons) != 0 {
		t.Fatalf("expected no reasons, got %v", reasons)
	}
}

// TestMatchOutcomePlanNodeTimingLtMismatch — lt threshold not below it,
// one mismatch reason fires.
func TestMatchOutcomePlanNodeTimingLtMismatch(t *testing.T) {
	got := runner.ExecResult{
		TypeName: "str",
		Repr:     `[{"Plan":{},"Planning Time":1234.5}]`,
	}
	diff := map[string]any{
		"working_branch_planning_time_seconds_lt": 0.5,
	}
	reasons := matchOutcome(branchWorking, got, diff)
	if len(reasons) != 1 {
		t.Fatalf("expected 1 reason, got %v", reasons)
	}
	if reasons[0].Code != "differential_planning_time_mismatch" {
		t.Fatalf("expected differential_planning_time_mismatch, got %v", reasons)
	}
}

// TestMatchOutcomePlanNodeTimingMalformedNonNumber — string spec value
// surfaces malformed_planning_time_spec.
func TestMatchOutcomePlanNodeTimingMalformedNonNumber(t *testing.T) {
	got := runner.ExecResult{
		TypeName: "str",
		Repr:     `[{"Plan":{},"Planning Time":1234.5}]`,
	}
	diff := map[string]any{
		"failed_branch_planning_time_seconds_gt": "ten",
	}
	reasons := matchOutcome(branchFailed, got, diff)
	if len(reasons) != 1 {
		t.Fatalf("expected 1 reason, got %v", reasons)
	}
	if reasons[0].Code != "malformed_planning_time_spec" {
		t.Fatalf("expected malformed_planning_time_spec, got %v", reasons)
	}
	if !strings.Contains(reasons[0].Message, "expected number") {
		t.Fatalf("expected message to mention 'expected number', got %q", reasons[0].Message)
	}
}

// TestMatchOutcomePlanNodeTimingMalformedNegative — negative threshold is
// rejected as a malformed spec, not silently accepted.
func TestMatchOutcomePlanNodeTimingMalformedNegative(t *testing.T) {
	got := runner.ExecResult{
		TypeName: "str",
		Repr:     `[{"Plan":{},"Planning Time":1234.5}]`,
	}
	diff := map[string]any{
		"failed_branch_planning_time_seconds_gt": -1,
	}
	reasons := matchOutcome(branchFailed, got, diff)
	if len(reasons) != 1 {
		t.Fatalf("expected 1 reason, got %v", reasons)
	}
	if reasons[0].Code != "malformed_planning_time_spec" {
		t.Fatalf("expected malformed_planning_time_spec, got %v", reasons)
	}
	if !strings.Contains(reasons[0].Message, "negative threshold") {
		t.Fatalf("expected message to mention 'negative threshold', got %q", reasons[0].Message)
	}
}

// TestMatchOutcomePlanNodeTimingPlanningTimeAbsent — the parsed JSON has
// no "Planning Time" field; surfaces malformed_planning_time_spec.
func TestMatchOutcomePlanNodeTimingPlanningTimeAbsent(t *testing.T) {
	got := runner.ExecResult{
		TypeName: "str",
		Repr:     `[{"Plan":{"Node Type":"Seq Scan"}}]`,
	}
	diff := map[string]any{
		"failed_branch_planning_time_seconds_gt": 5,
	}
	reasons := matchOutcome(branchFailed, got, diff)
	if len(reasons) != 1 {
		t.Fatalf("expected 1 reason, got %v", reasons)
	}
	if reasons[0].Code != "malformed_planning_time_spec" {
		t.Fatalf("expected malformed_planning_time_spec, got %v", reasons)
	}
	if !strings.Contains(reasons[0].Message, "Planning Time") {
		t.Fatalf("expected message to mention 'Planning Time', got %q", reasons[0].Message)
	}
}

// TestMatchOutcomePlanNodeTimingMalformedJSON — got.Repr is not JSON at
// all; surfaces malformed_planning_time_spec.
func TestMatchOutcomePlanNodeTimingMalformedJSON(t *testing.T) {
	got := runner.ExecResult{
		TypeName: "str",
		Repr:     "not json at all",
	}
	diff := map[string]any{
		"failed_branch_planning_time_seconds_gt": 5,
	}
	reasons := matchOutcome(branchFailed, got, diff)
	if len(reasons) != 1 {
		t.Fatalf("expected 1 reason, got %v", reasons)
	}
	if reasons[0].Code != "malformed_planning_time_spec" {
		t.Fatalf("expected malformed_planning_time_spec, got %v", reasons)
	}
}

// TestMatchOutcomePlanNodeTimingBothBranchesAdditive documents the
// canonical-seed pattern: failed_branch_gt AND working_branch_lt evaluated
// in the same Run. With separate got values per branch satisfying both
// thresholds, neither emits a reason.
func TestMatchOutcomePlanNodeTimingBothBranchesAdditive(t *testing.T) {
	failedGot := runner.ExecResult{
		TypeName: "str",
		Repr:     `[{"Plan":{},"Planning Time":12345.6}]`, // 12.3s > 10s
	}
	workingGot := runner.ExecResult{
		TypeName: "str",
		Repr:     `[{"Plan":{},"Planning Time":234.5}]`, // 0.23s < 1s
	}
	diff := map[string]any{
		"failed_branch_planning_time_seconds_gt":  10,
		"working_branch_planning_time_seconds_lt": 1,
	}
	if reasons := matchOutcome(branchFailed, failedGot, diff); len(reasons) != 0 {
		t.Fatalf("failed branch: expected no reasons, got %v", reasons)
	}
	if reasons := matchOutcome(branchWorking, workingGot, diff); len(reasons) != 0 {
		t.Fatalf("working branch: expected no reasons, got %v", reasons)
	}
}
