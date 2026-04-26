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
	// Strip the leading two-space indent so the surrounding mapping stays
	// at a uniform indentation level (yaml.v3 rejects mid-block dedents).
	yaml := strings.Replace(unitGreenYAML, "  isolation: function\n", "", 1)
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

// unitRaiseFixture is a unit-tier entry where the failed branch raises an
// exception (configurable via the body and *_must_raise spec) and the
// working branch returns 22. Tests use strings.Replace against the marker
// strings to swap in the desired raise body / spec.
const unitRaiseFixture = `
unit_id: unit-raise-fixture
domain: [test]
version_constraints: { spec: { name: test } }
failed_approach:
  description: raises on failed branch
  setup: []
  action:
    - { type: code, lang: python, body: "FAILED_BODY" }
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
    failed_branch_must_raise: FAILED_RAISE_SPEC
    working_branch_must_return: { type: int, value_equals: 22 }
  timeout_seconds: 5
`

// applyRaiseFixture substitutes the failed-branch body and must-raise spec
// into unitRaiseFixture. body is the raw Python body assigned to the failed
// branch's action; raiseSpec is the YAML inline value for failed_branch_must_raise.
func applyRaiseFixture(body, raiseSpec string) string {
	out := strings.Replace(unitRaiseFixture, "FAILED_BODY", body, 1)
	out = strings.Replace(out, "FAILED_RAISE_SPEC", raiseSpec, 1)
	return out
}

func TestRunUnitRaiseExceptionAlias(t *testing.T) {
	skipIfNoPython3(t)
	yaml := applyRaiseFixture(`raise KeyError('k')`, `{ exception: KeyError }`)
	res, err := Run([]byte(yaml))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "verified" {
		t.Fatalf("status=%q, reasons=%v", res.Status, res.Reasons)
	}
}

func TestRunUnitRaiseExceptionAny(t *testing.T) {
	skipIfNoPython3(t)

	// Verified: raises ValueError, spec accepts TypeError or ValueError.
	yamlOK := applyRaiseFixture(`raise ValueError('boom')`, `{ exception_any: [TypeError, ValueError] }`)
	res, err := Run([]byte(yamlOK))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "verified" {
		t.Fatalf("verified-case status=%q, reasons=%v", res.Status, res.Reasons)
	}

	// Rejected: raises KeyError but spec lists only TypeError/ValueError.
	yamlBad := applyRaiseFixture(`raise KeyError('k')`, `{ exception_any: [TypeError, ValueError] }`)
	res, err = Run([]byte(yamlBad))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "rejected" {
		t.Fatalf("rejected-case status=%q, reasons=%v", res.Status, res.Reasons)
	}
	if !hasReason(res.Reasons, "wrong_exception") {
		t.Fatalf("expected wrong_exception, got %v", res.Reasons)
	}
	found := false
	for _, r := range res.Reasons {
		if r.Code == "wrong_exception" && strings.Contains(r.Message, "any of") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected wrong_exception message containing 'any of', got %v", res.Reasons)
	}
}

func TestRunUnitRaiseMessagePattern(t *testing.T) {
	skipIfNoPython3(t)

	// Verified: message matches pattern.
	yamlOK := applyRaiseFixture(
		`raise ValueError('Timestamp outside the tolerance zone')`,
		`{ exception: ValueError, message_pattern: "Timestamp outside" }`,
	)
	res, err := Run([]byte(yamlOK))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "verified" {
		t.Fatalf("verified-case status=%q, reasons=%v", res.Status, res.Reasons)
	}

	// Rejected: message doesn't match pattern.
	yamlBad := applyRaiseFixture(
		`raise ValueError('something else entirely')`,
		`{ exception: ValueError, message_pattern: "Timestamp outside" }`,
	)
	res, err = Run([]byte(yamlBad))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "rejected" {
		t.Fatalf("rejected-case status=%q, reasons=%v", res.Status, res.Reasons)
	}
	if !hasReason(res.Reasons, "wrong_exception_message") {
		t.Fatalf("expected wrong_exception_message, got %v", res.Reasons)
	}
}

func TestRunUnitRaiseMessagePatternInvalidRegex(t *testing.T) {
	skipIfNoPython3(t)
	yaml := applyRaiseFixture(
		`raise ValueError('whatever')`,
		`{ exception: ValueError, message_pattern: "[unclosed" }`,
	)
	res, err := Run([]byte(yaml))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "rejected" {
		t.Fatalf("status=%q, want rejected (reasons=%v)", res.Status, res.Reasons)
	}
	if !hasReason(res.Reasons, "malformed_raise_spec") {
		t.Fatalf("expected malformed_raise_spec, got %v", res.Reasons)
	}
	found := false
	for _, r := range res.Reasons {
		if r.Code == "malformed_raise_spec" && strings.Contains(r.Message, "[unclosed") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected malformed_raise_spec message naming the bad regex, got %v", res.Reasons)
	}
}

// unitReturnFixture is a unit-tier entry where the failed branch returns 0
// (matched by a fixed spec) and the working branch's action body and
// must_return spec are configurable via marker substitution.
const unitReturnFixture = `
unit_id: unit-return-fixture
domain: [test]
version_constraints: { spec: { name: test } }
failed_approach:
  description: returns 0
  setup: []
  action:
    - { type: code, lang: python, body: "$RESULT = 0" }
  assertion: { type: returns, expect: fail }
working_approach:
  description: configurable working body
  setup: []
  action:
    - { type: code, lang: python, body: "WORKING_BODY" }
  assertion: { type: returns, expect: success }
verification:
  type: unit
  isolation: function
  differential:
    failed_branch_must_return: { type: int, value_equals: 0 }
    working_branch_must_return: WORKING_RETURN_SPEC
  timeout_seconds: 5
`

func applyReturnFixture(body, returnSpec string) string {
	out := strings.Replace(unitReturnFixture, "WORKING_BODY", body, 1)
	out = strings.Replace(out, "WORKING_RETURN_SPEC", returnSpec, 1)
	return out
}

func TestRunUnitReturnLength(t *testing.T) {
	skipIfNoPython3(t)

	// Verified: length matches.
	yamlOK := applyReturnFixture(`$RESULT = [1, 2, 3]`, `{ length: 3 }`)
	res, err := Run([]byte(yamlOK))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "verified" {
		t.Fatalf("verified-case status=%q, reasons=%v", res.Status, res.Reasons)
	}

	// Rejected: length mismatch.
	yamlBad := applyReturnFixture(`$RESULT = [1, 2, 3]`, `{ length: 4 }`)
	res, err = Run([]byte(yamlBad))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "rejected" {
		t.Fatalf("rejected-case status=%q, reasons=%v", res.Status, res.Reasons)
	}
	if !hasReason(res.Reasons, "wrong_return_length") {
		t.Fatalf("expected wrong_return_length, got %v", res.Reasons)
	}
}

func TestRunUnitReturnLengthOnNonSized(t *testing.T) {
	skipIfNoPython3(t)
	yaml := applyReturnFixture(`$RESULT = 42`, `{ length: 1 }`)
	res, err := Run([]byte(yaml))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "rejected" {
		t.Fatalf("status=%q, want rejected (reasons=%v)", res.Status, res.Reasons)
	}
	if !hasReason(res.Reasons, "non_sized_return") {
		t.Fatalf("expected non_sized_return, got %v", res.Reasons)
	}
}

func TestRunUnitReturnContainsExceptionType(t *testing.T) {
	skipIfNoPython3(t)

	// Verified: list contains a ValueError instance.
	yamlOK := applyReturnFixture(
		`$RESULT = [ValueError('a'), TypeError('b')]`,
		`{ contains_exception_type: ValueError }`,
	)
	res, err := Run([]byte(yamlOK))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "verified" {
		t.Fatalf("verified-case status=%q, reasons=%v", res.Status, res.Reasons)
	}

	// Rejected: list of ints, no ValueError element.
	yamlBad := applyReturnFixture(
		`$RESULT = [1, 2, 3]`,
		`{ contains_exception_type: ValueError }`,
	)
	res, err = Run([]byte(yamlBad))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "rejected" {
		t.Fatalf("rejected-case status=%q, reasons=%v", res.Status, res.Reasons)
	}
	if !hasReason(res.Reasons, "missing_exception_element") {
		t.Fatalf("expected missing_exception_element, got %v", res.Reasons)
	}
}

func TestRunUnitReturnLengthAndContainsCombined(t *testing.T) {
	skipIfNoPython3(t)
	yaml := applyReturnFixture(
		`$RESULT = [1, 2, ValueError('x')]`,
		`{ length: 3, contains_exception_type: ValueError }`,
	)
	res, err := Run([]byte(yaml))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "verified" {
		t.Fatalf("status=%q, reasons=%v", res.Status, res.Reasons)
	}
}

func TestRunUnitLiteralBound(t *testing.T) {
	skipIfNoPython3(t)
	yaml := `
unit_id: unit-literal-bound
domain: [test]
version_constraints: { spec: { name: test } }
literals:
  $LITERAL_1:
    value: 5
    reason: test
failed_approach:
  description: subtracts one
  setup: []
  action:
    - { type: code, lang: python, body: "$RESULT = $LITERAL_1 - 1" }
  assertion: { type: returns, expect: fail }
working_approach:
  description: returns the literal
  setup: []
  action:
    - { type: code, lang: python, body: "$RESULT = $LITERAL_1" }
  assertion: { type: returns, expect: success }
verification:
  type: unit
  isolation: function
  differential:
    failed_branch_must_return: { type: int, value_equals: 4 }
    working_branch_must_return: { type: int, value_equals: 5 }
  timeout_seconds: 5
`
	res, err := Run([]byte(yaml))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "verified" {
		t.Fatalf("status=%q, reasons=%v", res.Status, res.Reasons)
	}
}

func TestRunUnitLiteralStringValueBound(t *testing.T) {
	skipIfNoPython3(t)
	yaml := `
unit_id: unit-literal-string-bound
domain: [test]
version_constraints: { spec: { name: test } }
literals:
  $LITERAL_1:
    value: "fixtures/probe-object"
    reason: test
failed_approach:
  description: returns a different string
  setup: []
  action:
    - { type: code, lang: python, body: "$RESULT = 'other'" }
  assertion: { type: returns, expect: fail }
working_approach:
  description: returns the literal string
  setup: []
  action:
    - { type: code, lang: python, body: "$RESULT = $LITERAL_1" }
  assertion: { type: returns, expect: success }
verification:
  type: unit
  isolation: function
  differential:
    failed_branch_must_return: { type: str, value_equals: "other" }
    working_branch_must_return: { type: str, value_equals: "fixtures/probe-object" }
  timeout_seconds: 5
`
	res, err := Run([]byte(yaml))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "verified" {
		t.Fatalf("status=%q, reasons=%v", res.Status, res.Reasons)
	}
}

func TestRunUnitInputOverridesLiteral(t *testing.T) {
	skipIfNoPython3(t)
	yaml := `
unit_id: unit-input-overrides-literal
domain: [test]
version_constraints: { spec: { name: test } }
literals:
  $LITERAL_1:
    value: 5
    reason: test
failed_approach:
  description: returns a different value
  setup: []
  action:
    - { type: code, lang: python, body: "$RESULT = 0" }
  assertion: { type: returns, expect: fail }
working_approach:
  description: returns the bound literal name
  setup: []
  action:
    - { type: code, lang: python, body: "$RESULT = $LITERAL_1" }
  assertion: { type: returns, expect: success }
verification:
  type: unit
  isolation: function
  differential:
    inputs:
      $LITERAL_1: 99
    failed_branch_must_return: { type: int, value_equals: 0 }
    working_branch_must_return: { type: int, value_equals: 99 }
  timeout_seconds: 5
`
	res, err := Run([]byte(yaml))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "verified" {
		t.Fatalf("status=%q, reasons=%v", res.Status, res.Reasons)
	}
}

func TestRunUnitMalformedLiteralSkipped(t *testing.T) {
	skipIfNoPython3(t)
	yaml := `
unit_id: unit-malformed-literal-skipped
domain: [test]
version_constraints: { spec: { name: test } }
literals:
  $LITERAL_1:
    value: 5
    reason: test
  $LITERAL_2: "not a map"
failed_approach:
  description: returns a different value
  setup: []
  action:
    - { type: code, lang: python, body: "$RESULT = 0" }
  assertion: { type: returns, expect: fail }
working_approach:
  description: returns the well-formed literal
  setup: []
  action:
    - { type: code, lang: python, body: "$RESULT = $LITERAL_1" }
  assertion: { type: returns, expect: success }
verification:
  type: unit
  isolation: function
  differential:
    failed_branch_must_return: { type: int, value_equals: 0 }
    working_branch_must_return: { type: int, value_equals: 5 }
  timeout_seconds: 5
`
	res, err := Run([]byte(yaml))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "verified" {
		t.Fatalf("status=%q, reasons=%v", res.Status, res.Reasons)
	}
}
