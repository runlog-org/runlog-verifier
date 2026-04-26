package verify

import (
	"os/exec"
	"strings"
	"testing"
)

// unitGreenYAML is a minimal unit-tier entry that runs end-to-end through
// the Python runner. Both branches assign $RESULT to a literal int and
// the differential block expects exactly those values.
const unitGreenYAML = `
unit_id: unit-runner-greenpath
domain: [test]
version_constraints: { spec: { name: test } }
failed_approach:
  description: returns 18 always
  setup: []
  action:
    - { type: code, lang: python, body: "$RESULT = 18" }
  assertion: { type: returns, expect: fail }
working_approach:
  description: returns 22
  setup: []
  action:
    - { type: code, lang: python, body: "$RESULT = 22" }
  assertion: { type: returns, expect: success }
verification:
  type: unit
  isolation: function
  differential:
    failed_branch_must_return: { type: int, value_equals: 18 }
    working_branch_must_return: { type: int, value_equals: 22 }
  timeout_seconds: 5
`

func skipIfNoPython3(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
}

func TestRunUnitVerifiedReturn(t *testing.T) {
	skipIfNoPython3(t)
	res, err := Run([]byte(unitGreenYAML))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "verified" {
		t.Fatalf("status=%q, reasons=%v", res.Status, res.Reasons)
	}
	if res.Tier != "unit" {
		t.Fatalf("tier=%q, want unit", res.Tier)
	}
}

func TestRunUnitWrongReturnValue(t *testing.T) {
	skipIfNoPython3(t)
	yaml := strings.Replace(
		unitGreenYAML,
		"failed_branch_must_return: { type: int, value_equals: 18 }",
		"failed_branch_must_return: { type: int, value_equals: 99 }",
		1,
	)
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
}

func TestRunUnitWrongReturnType(t *testing.T) {
	skipIfNoPython3(t)
	yaml := strings.Replace(
		unitGreenYAML,
		"failed_branch_must_return: { type: int, value_equals: 18 }",
		`failed_branch_must_return: { type: str, value_equals: "18" }`,
		1,
	)
	res, err := Run([]byte(yaml))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "rejected" {
		t.Fatalf("status=%q, want rejected (reasons=%v)", res.Status, res.Reasons)
	}
	if !hasReason(res.Reasons, "wrong_return_type") {
		t.Fatalf("expected wrong_return_type, got %v", res.Reasons)
	}
}

func TestRunUnitMissingIsolation(t *testing.T) {
	// Empty isolation hits the "isolation_not_yet_implemented" tier-unsupported
	// path without needing to spawn python — runs even on python-less hosts.
	yaml := strings.Replace(unitGreenYAML, "isolation: function\n", "", 1)
	res, err := Run([]byte(yaml))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "tier_unsupported" {
		t.Fatalf("status=%q, want tier_unsupported (reasons=%v)", res.Status, res.Reasons)
	}
	if !hasReason(res.Reasons, "isolation_not_yet_implemented") {
		t.Fatalf("expected isolation_not_yet_implemented, got %v", res.Reasons)
	}
}

func TestRunUnitNonFunctionIsolation(t *testing.T) {
	yaml := strings.Replace(unitGreenYAML, "isolation: function", "isolation: subprocess", 1)
	res, err := Run([]byte(yaml))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "tier_unsupported" {
		t.Fatalf("status=%q, want tier_unsupported (reasons=%v)", res.Status, res.Reasons)
	}
	if !hasReason(res.Reasons, "isolation_not_yet_implemented") {
		t.Fatalf("expected isolation_not_yet_implemented, got %v", res.Reasons)
	}
}

func TestRunUnitNonPythonLang(t *testing.T) {
	skipIfNoPython3(t)
	yaml := strings.Replace(
		unitGreenYAML,
		`{ type: code, lang: python, body: "$RESULT = 18" }`,
		`{ type: code, lang: ruby, body: "RESULT = 18" }`,
		1,
	)
	res, err := Run([]byte(yaml))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "tier_unsupported" {
		t.Fatalf("status=%q, want tier_unsupported (reasons=%v)", res.Status, res.Reasons)
	}
	if !hasReason(res.Reasons, "language_not_yet_implemented") {
		t.Fatalf("expected language_not_yet_implemented, got %v", res.Reasons)
	}
}
