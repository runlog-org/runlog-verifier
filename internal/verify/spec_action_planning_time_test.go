package verify

import (
	"strings"
	"testing"

	"github.com/runlog-org/runlog-verifier/internal/verify/runner"
)

// TestMatchActionPlanNodeTimingGtMatch — happy path for the per-branch gt
// field: got.Repr's Planning Time exceeds the threshold, no reasons fire.
func TestMatchActionPlanNodeTimingGtMatch(t *testing.T) {
	got := runner.ExecResult{
		TypeName: "str",
		Repr:     `[{"Plan":{"Node Type":"Update"},"Planning Time":12345.6}]`,
	}
	a := Assertion{Type: "plan_node", PlanningTimeSecondsGt: 5}
	reasons := matchActionPlanNodeTiming("failed_approach", a, got)
	if len(reasons) != 0 {
		t.Fatalf("expected no reasons, got %v", reasons)
	}
}

// TestMatchActionPlanNodeTimingGtMismatch — gt threshold not exceeded
// surfaces exactly one action_planning_time_mismatch reason.
func TestMatchActionPlanNodeTimingGtMismatch(t *testing.T) {
	got := runner.ExecResult{
		TypeName: "str",
		Repr:     `[{"Plan":{},"Planning Time":1234.5}]`,
	}
	a := Assertion{Type: "plan_node", PlanningTimeSecondsGt: 5}
	reasons := matchActionPlanNodeTiming("failed_approach", a, got)
	if len(reasons) != 1 {
		t.Fatalf("expected 1 reason, got %v", reasons)
	}
	if reasons[0].Code != "action_planning_time_mismatch" {
		t.Fatalf("expected action_planning_time_mismatch, got %v", reasons)
	}
	if !strings.Contains(reasons[0].Message, "1.23") {
		t.Fatalf("expected message to name parsed value ~1.23, got %q", reasons[0].Message)
	}
}

// TestMatchActionPlanNodeTimingLtMatch — happy path for the per-branch lt
// field.
func TestMatchActionPlanNodeTimingLtMatch(t *testing.T) {
	got := runner.ExecResult{
		TypeName: "str",
		Repr:     `[{"Plan":{},"Planning Time":234.5}]`,
	}
	a := Assertion{Type: "plan_node", PlanningTimeSecondsLt: 1}
	reasons := matchActionPlanNodeTiming("working_approach", a, got)
	if len(reasons) != 0 {
		t.Fatalf("expected no reasons, got %v", reasons)
	}
}

// TestMatchActionPlanNodeTimingLtMismatch — lt threshold not satisfied,
// one mismatch reason fires.
func TestMatchActionPlanNodeTimingLtMismatch(t *testing.T) {
	got := runner.ExecResult{
		TypeName: "str",
		Repr:     `[{"Plan":{},"Planning Time":1234.5}]`,
	}
	a := Assertion{Type: "plan_node", PlanningTimeSecondsLt: 0.5}
	reasons := matchActionPlanNodeTiming("working_approach", a, got)
	if len(reasons) != 1 {
		t.Fatalf("expected 1 reason, got %v", reasons)
	}
	if reasons[0].Code != "action_planning_time_mismatch" {
		t.Fatalf("expected action_planning_time_mismatch, got %v", reasons)
	}
}

// TestMatchActionPlanNodeTimingNegativeThreshold — negative threshold is
// rejected as a malformed spec and the parse step is skipped.
func TestMatchActionPlanNodeTimingNegativeThreshold(t *testing.T) {
	got := runner.ExecResult{
		TypeName: "str",
		Repr:     `[{"Plan":{},"Planning Time":1234.5}]`,
	}
	a := Assertion{Type: "plan_node", PlanningTimeSecondsGt: -1}
	reasons := matchActionPlanNodeTiming("failed_approach", a, got)
	if len(reasons) != 1 {
		t.Fatalf("expected 1 reason, got %v", reasons)
	}
	if reasons[0].Code != "malformed_action_planning_time_spec" {
		t.Fatalf("expected malformed_action_planning_time_spec, got %v", reasons)
	}
	if !strings.Contains(reasons[0].Message, "negative threshold") {
		t.Fatalf("expected message to mention 'negative threshold', got %q", reasons[0].Message)
	}
}

// TestMatchActionPlanNodeTimingPlanningTimeAbsent — the parsed JSON has
// no "Planning Time" field; surfaces malformed_action_planning_time_spec.
func TestMatchActionPlanNodeTimingPlanningTimeAbsent(t *testing.T) {
	got := runner.ExecResult{
		TypeName: "str",
		Repr:     `[{"Plan":{"Node Type":"Seq Scan"}}]`,
	}
	a := Assertion{Type: "plan_node", PlanningTimeSecondsGt: 5}
	reasons := matchActionPlanNodeTiming("failed_approach", a, got)
	if len(reasons) != 1 {
		t.Fatalf("expected 1 reason, got %v", reasons)
	}
	if reasons[0].Code != "malformed_action_planning_time_spec" {
		t.Fatalf("expected malformed_action_planning_time_spec, got %v", reasons)
	}
	if !strings.Contains(reasons[0].Message, "Planning Time") {
		t.Fatalf("expected message to mention 'Planning Time', got %q", reasons[0].Message)
	}
}

// TestMatchActionPlanNodeTimingMalformedJSON — got.Repr is not JSON at
// all; surfaces malformed_action_planning_time_spec.
func TestMatchActionPlanNodeTimingMalformedJSON(t *testing.T) {
	got := runner.ExecResult{
		TypeName: "str",
		Repr:     "not json at all",
	}
	a := Assertion{Type: "plan_node", PlanningTimeSecondsGt: 5}
	reasons := matchActionPlanNodeTiming("failed_approach", a, got)
	if len(reasons) != 1 {
		t.Fatalf("expected 1 reason, got %v", reasons)
	}
	if reasons[0].Code != "malformed_action_planning_time_spec" {
		t.Fatalf("expected malformed_action_planning_time_spec, got %v", reasons)
	}
}

// TestMatchActionPlanNodeTimingBothFieldsAdditive documents the canonical-
// seed range pattern: gt AND lt set on the same branch must both pass.
func TestMatchActionPlanNodeTimingBothFieldsAdditive(t *testing.T) {
	got := runner.ExecResult{
		TypeName: "str",
		Repr:     `[{"Plan":{},"Planning Time":2345.6}]`, // 2.34s
	}
	a := Assertion{Type: "plan_node", PlanningTimeSecondsGt: 1, PlanningTimeSecondsLt: 10}
	reasons := matchActionPlanNodeTiming("failed_approach", a, got)
	if len(reasons) != 0 {
		t.Fatalf("expected no reasons, got %v", reasons)
	}
}

// TestMatchActionPlanNodeTimingTypeNotPlanNodeIgnored — defensive type-
// gate: when a.Type != "plan_node" the timing fields are inert and the
// helper does not even attempt to parse got.Repr (so nonsense Repr is fine).
func TestMatchActionPlanNodeTimingTypeNotPlanNodeIgnored(t *testing.T) {
	got := runner.ExecResult{
		TypeName: "str",
		Repr:     "x",
	}
	a := Assertion{Type: "returns", PlanningTimeSecondsGt: 5}
	reasons := matchActionPlanNodeTiming("failed_approach", a, got)
	if len(reasons) != 0 {
		t.Fatalf("expected no reasons (type-gate suppresses check), got %v", reasons)
	}
}
