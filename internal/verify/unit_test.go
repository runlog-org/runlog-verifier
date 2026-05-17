package verify

import (
	"os/exec"
	"strings"
	"testing"
)

// F36 lifted the five universal falsifiability shape checks to a
// SHAPE-FIRST pre-flight for unit + integration tiers. Every entry that
// reaches Run() for those tiers must now structurally satisfy schema
// submission rules §1-3 + discrimination — the same bar runlog_submit
// already enforces server-side. The unit-tier fixtures below therefore
// carry a falsifiability tail: a mutate_fixture mutation (§1), a
// fail-expecting one (§2), an unchanged-expecting one (§3), at least one
// breaking the working_approach assertion (discrimination).
//
// Two shapes of tail appear here:
//
//   - greenTail / litTail / returnTail: the working branch genuinely
//     reads the mutated token, so the "runlog_f36_break" string-break
//     makes the working assertion fail for real (these feed verified-case
//     or targeted-reject tests whose own mutation must still drive the
//     asserted outcome — appended AFTER, so reason ordering is unaffected).
//   - structTail: the entry never reaches mutation execution (rejected by
//     differential / runtime-missing, or tier_unsupported), so only the
//     §1-3+discrimination STRUCTURE matters; the target token need not
//     resolve.
//
// To let the string-break actually break a literal-int return without
// changing the differential, working bodies that returned a bare literal
// are rebound through a differential input in a value-preserving way
// (`$F36G + 0` still returns the same int; a string `$F36G` raises
// TypeError → the working spec fails → outcome fail). The no-op restores
// the original value → outcome unchanged.

// unitGreenYAML is a minimal unit-tier entry that runs end-to-end through
// the Python runner. The working branch returns $F36G (= 22, bound via
// the differential inputs) so the F36 falsifiability tail can break it;
// `+ 0` keeps the return an int so the differential is unchanged.
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
    - { type: code, lang: python, body: "$RESULT = $F36G + 0" }
  assertion: { type: returns, expect: success }
verification:
  type: unit
  isolation: function
  differential:
    inputs:
      $F36G: 22
    failed_branch_must_return: { type: int, value_equals: 18 }
    working_branch_must_return: { type: int, value_equals: 22 }
  mutations:` + greenTail + `
  timeout_seconds: 5
`

// greenTail is the F36 falsifiability tail for unitGreenYAML's $F36G
// input (working branch returns $F36G + 0). The string break sets $F36G
// to a non-int → `$F36G + 0` raises TypeError → working spec fails (§1 +
// §2 + discrimination). The no-op restores 22 → unchanged (§3).
const greenTail = `
    - strategy: mutate_fixture
      target: $F36G
      new_value: "runlog_f36_break"
      branch: working_approach
      expected_result: fail
    - strategy: mutate_fixture
      target: $F36G
      new_value: 22
      branch: working_approach
      expected_result: unchanged`

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
	skipIfNoPython3(t)
	// Empty isolation defaults to "function" — the dispatcher resolves to
	// the Python driver and the entry runs as if isolation were declared.
	// (The schema's allOf branch makes isolation required for type: unit,
	// so this is the CLI-path defensive fallback only.)
	// Strip the leading two-space indent so the surrounding mapping stays
	// at a uniform indentation level (yaml.v3 rejects mid-block dedents).
	yaml := strings.Replace(unitGreenYAML, "  isolation: function\n", "", 1)
	res, err := Run([]byte(yaml))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "verified" {
		t.Fatalf("status=%q (reasons=%v), want verified — empty isolation must default to function",
			res.Status, res.Reasons)
	}
}

func TestRunUnitSubprocessMissingRuntime(t *testing.T) {
	// `isolation: subprocess` requires a `verification.runtime.tool`
	// declaration to know which CLI to drive. Without it, the entry is
	// well-formed YAML but unverifiable — surface rejected with
	// verification_runtime_missing so the submitter fixes the entry
	// rather than thinking the verifier build is incomplete.
	yaml := strings.Replace(unitGreenYAML, "isolation: function", "isolation: subprocess", 1)
	res, err := Run([]byte(yaml))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "rejected" {
		t.Fatalf("status=%q, want rejected (reasons=%v)", res.Status, res.Reasons)
	}
	if !hasReason(res.Reasons, "verification_runtime_missing") {
		t.Fatalf("expected verification_runtime_missing, got %v", res.Reasons)
	}
}

// unitSubprocessShellYAML is a minimal unit-tier entry that runs end-to-end
// through the SubprocessDriver with tool=shell. Each branch echoes a
// distinct stdout string; the differential block matches on those exact
// strings (SubprocessDriver returns last-step stdout as a string-typed
// $RESULT). This is the path Godot/Node/Ruby/etc. take — no language-
// specific driver, just the host CLI.
const unitSubprocessShellYAML = `
unit_id: unit-subprocess-shell-greenpath
domain: [test]
version_constraints: { spec: { name: test } }
failed_approach:
  description: echoes failure
  setup: []
  action:
    - { type: code, lang: shell, body: "echo failed-output" }
  assertion: { type: returns, expect: fail }
working_approach:
  description: echoes success
  setup: []
  action:
    - { type: code, lang: shell, body: "echo $F36S" }
  assertion: { type: returns, expect: success }
verification:
  type: unit
  isolation: subprocess
  runtime: { tool: shell }
  differential:
    inputs:
      $F36S: working-output
    failed_branch_must_return: { type: string, value_equals: "failed-output\n" }
    working_branch_must_return: { type: string, value_equals: "working-output\n" }
  mutations:` + shellTail + `
  timeout_seconds: 5
`

// shellTail is the F36 falsifiability tail for unitSubprocessShellYAML's
// $F36S input (working branch echoes $F36S). The string break changes the
// echoed line so it no longer matches working_branch_must_return → fail
// (§1 + §2 + discrimination); the no-op restores it → unchanged (§3).
const shellTail = `
    - strategy: mutate_fixture
      target: $F36S
      new_value: "runlog_f36_break"
      branch: working_approach
      expected_result: fail
    - strategy: mutate_fixture
      target: $F36S
      new_value: working-output
      branch: working_approach
      expected_result: unchanged`

func skipIfNoShell(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not on PATH")
	}
}

func TestRunUnitSubprocessShellVerified(t *testing.T) {
	skipIfNoShell(t)
	res, err := Run([]byte(unitSubprocessShellYAML))
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

// unitSubprocessMutationsYAML proves the per-mutation fresh-tmpdir lifecycle
// works for unit-tier subprocess. Two mutations on the failed branch:
//   - swap a comment-only token  → output unchanged → expected unchanged
//   - swap the literal output    → branch becomes indistinguishable from
//     working → expected fail
//
// Mirrors the "discriminating + non-discriminating mutation" pattern the
// schema enforces at submission time, but exercises the per-mutation
// sandbox isolation that's the load-bearing piece for shell/subprocess.
const unitSubprocessMutationsYAML = `
unit_id: unit-subprocess-shell-mutations
domain: [test]
version_constraints: { spec: { name: test } }
failed_approach:
  description: echoes failure with a trailing comment
  setup: []
  action:
    - { type: code, lang: shell, body: "echo failed-output # commentmark" }
  assertion: { type: returns, expect: fail }
working_approach:
  description: echoes success
  setup: []
  action:
    - { type: code, lang: shell, body: "echo $F36S" }
  assertion: { type: returns, expect: success }
verification:
  type: unit
  isolation: subprocess
  runtime: { tool: shell }
  differential:
    inputs:
      $F36S: working-output
    failed_branch_must_return: { type: string, value_equals: "failed-output\n" }
    working_branch_must_return: { type: string, value_equals: "working-output\n" }
  mutations:
    - strategy: swap_identifier
      target: action
      branch: failed_approach
      token: commentmark
      new_value: remark
      expected_branch_outcome: { failed_approach: unchanged }
    - strategy: swap_identifier
      target: action
      branch: failed_approach
      token: failed-output
      new_value: working-output
      expected_branch_outcome: { failed_approach: fail }` + shellTail + `
  timeout_seconds: 5
`

func TestRunUnitSubprocessShellWithMutations(t *testing.T) {
	skipIfNoShell(t)
	res, err := Run([]byte(unitSubprocessMutationsYAML))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "verified" {
		t.Fatalf("status=%q, reasons=%v", res.Status, res.Reasons)
	}
}

func TestRunUnitSubprocessMutationOutcomeMismatch(t *testing.T) {
	// Sanity: if the mutation expectation is wrong, we surface
	// mutation_outcome_mismatch (proves the per-mutation sandbox actually
	// runs, isn't a no-op that returns "verified" regardless).
	skipIfNoShell(t)
	yaml := strings.Replace(unitSubprocessMutationsYAML,
		"expected_branch_outcome: { failed_approach: fail }",
		"expected_branch_outcome: { failed_approach: unchanged }",
		1)
	res, err := Run([]byte(yaml))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "rejected" {
		t.Fatalf("status=%q, want rejected (reasons=%v)", res.Status, res.Reasons)
	}
	if !hasReason(res.Reasons, "mutation_outcome_mismatch") {
		t.Fatalf("expected mutation_outcome_mismatch, got %v", res.Reasons)
	}
}

func TestRunUnitSubprocessUnsupportedTool(t *testing.T) {
	// tool=postgres is in the schema enum but not implemented in this
	// build — surface tier_unsupported with runtime_unsupported so the
	// submitter knows the entry is well-formed but waiting on driver
	// work. Mirrors integration-reexecute's runtime_unsupported behavior.
	yaml := strings.Replace(unitSubprocessShellYAML,
		"runtime: { tool: shell }",
		"runtime: { tool: postgres }",
		1)
	res, err := Run([]byte(yaml))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "tier_unsupported" {
		t.Fatalf("status=%q, want tier_unsupported (reasons=%v)", res.Status, res.Reasons)
	}
	if !hasReason(res.Reasons, "runtime_unsupported") {
		t.Fatalf("expected runtime_unsupported, got %v", res.Reasons)
	}
}

func TestRunUnitCompilerIsolation(t *testing.T) {
	// A second schema-recognised-but-unimplemented value, to confirm the
	// dispatcher names whichever isolation was requested rather than
	// hard-coding "subprocess".
	yaml := strings.Replace(unitGreenYAML, "isolation: function", "isolation: compiler", 1)
	res, err := Run([]byte(yaml))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "tier_unsupported" {
		t.Fatalf("status=%q, want tier_unsupported (reasons=%v)", res.Status, res.Reasons)
	}
	if !hasReason(res.Reasons, "isolation_unsupported") {
		t.Fatalf("expected isolation_unsupported, got %v", res.Reasons)
	}
	found := false
	for _, r := range res.Reasons {
		if r.Code == "isolation_unsupported" && strings.Contains(r.Message, `"compiler"`) {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected isolation_unsupported message naming \"compiler\", got %v", res.Reasons)
	}
}

func TestRunUnitUnknownIsolation(t *testing.T) {
	// A value outside the schema enum entirely — the dispatcher distinguishes
	// "schema-recognised but unimplemented" (isolation_unsupported) from
	// "not in the schema at all" (isolation_unknown) so the submitter gets
	// the right diagnostic. The schema-side validator typically rejects
	// these upstream of the verifier; this is the CLI defensive path.
	yaml := strings.Replace(unitGreenYAML, "isolation: function", "isolation: bogus_value", 1)
	res, err := Run([]byte(yaml))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "tier_unsupported" {
		t.Fatalf("status=%q, want tier_unsupported (reasons=%v)", res.Status, res.Reasons)
	}
	if !hasReason(res.Reasons, "isolation_unknown") {
		t.Fatalf("expected isolation_unknown, got %v", res.Reasons)
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
    - { type: code, lang: python, body: "$RESULT = $F36G + 0" }
  assertion: { type: returns, expect: success }
verification:
  type: unit
  isolation: function
  differential:
    inputs:
      $F36G: 22
    failed_branch_must_raise: FAILED_RAISE_SPEC
    working_branch_must_return: { type: int, value_equals: 22 }
  mutations:` + greenTail + `
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
    - { type: code, lang: python, body: "WORKING_BODY; $RESULT = $RESULT if isinstance($F36G, int) else $F36G + 0" }
  assertion: { type: returns, expect: success }
verification:
  type: unit
  isolation: function
  differential:
    inputs:
      $F36G: 0
    failed_branch_must_return: { type: int, value_equals: 0 }
    working_branch_must_return: WORKING_RETURN_SPEC
  mutations:` + returnTail + `
  timeout_seconds: 5
`

// returnTail is the F36 falsifiability tail for unitReturnFixture. The
// working body carries a value-preserving guard:
// `$RESULT = $RESULT if isinstance($F36G, int) else $F36G + 0`. With the
// no-op ($F36G=0) the guard is the identity (§3 unchanged). The string
// break makes isinstance() false, so `$F36G + 0` raises TypeError → the
// working spec fails regardless of which WORKING_RETURN_SPEC the test
// substituted (§1 + §2 + discrimination).
const returnTail = `
    - strategy: mutate_fixture
      target: $F36G
      new_value: "runlog_f36_break"
      branch: working_approach
      expected_result: fail
    - strategy: mutate_fixture
      target: $F36G
      new_value: 0
      branch: working_approach
      expected_result: unchanged`

// litTailN / litTailStr / litTailInput are F36 falsifiability tails for
// the inline literal-binding tests, whose working branch is
// `$RESULT = $LITERAL_1`. mutate_fixture rebinds $LITERAL_1 (an input
// rebind always overrides a literal of the same name — the very property
// TestRunUnitInputOverridesLiteral pins). The string break makes the
// working spec fail (§1 + §2 + discrimination); the no-op restores the
// test's effective working value so the outcome is unchanged (§3). Three
// variants because the no-op must match each test's effective value:
//   - litTailN     : numeric literal value 5
//   - litTailStr   : string literal value "fixtures/probe-object"
//   - litTailInput : differential input overrides $LITERAL_1 to 99
const litTailN = `
  mutations:
    - strategy: mutate_fixture
      target: $LITERAL_1
      new_value: "runlog_f36_break"
      branch: working_approach
      expected_result: fail
    - strategy: mutate_fixture
      target: $LITERAL_1
      new_value: 5
      branch: working_approach
      expected_result: unchanged`

const litTailStr = `
  mutations:
    - strategy: mutate_fixture
      target: $LITERAL_1
      new_value: 12345
      branch: working_approach
      expected_result: fail
    - strategy: mutate_fixture
      target: $LITERAL_1
      new_value: "fixtures/probe-object"
      branch: working_approach
      expected_result: unchanged`

const litTailInput = `
  mutations:
    - strategy: mutate_fixture
      target: $LITERAL_1
      new_value: "runlog_f36_break"
      branch: working_approach
      expected_result: fail
    - strategy: mutate_fixture
      target: $LITERAL_1
      new_value: 99
      branch: working_approach
      expected_result: unchanged`

// pathGuard is appended to a path test's working body; pathTail is the
// matching mutations block. The guard is value-preserving when $F36G is
// the no-op int 0 (identity) and raises TypeError when $F36G is the
// string break — so the working spec fails regardless of which dict
// shape / path the test returns (§1 + §2 + §3 + discrimination). The
// guard keeps $RESULT intact so the spec's `path:` resolution is
// unaffected on the unmutated run.
const pathGuard = `; $RESULT = $RESULT if isinstance($F36G, int) else $F36G + 0`

const pathTail = `
  mutations:
    - strategy: mutate_fixture
      target: $F36G
      new_value: "runlog_f36_break"
      branch: working_approach
      expected_result: fail
    - strategy: mutate_fixture
      target: $F36G
      new_value: 0
      branch: working_approach
      expected_result: unchanged`

// structTail satisfies §1-3 + discrimination structurally only. It is for
// entries rejected (by the differential, before the mutation baseline at
// runUnit's matchBranchOutcome gate) or tier_unsupported — the mutations
// never execute, so the target token need not resolve and no body rebind
// is required. Used by TestRunUnitPathMissingKey (rejected at the failed
// branch's missing-key path resolution).
const structTail = `
  mutations:
    - strategy: mutate_fixture
      target: $F36G
      new_value: "runlog_f36_break"
      branch: working_approach
      expected_result: fail
    - strategy: mutate_fixture
      target: $F36G
      new_value: 0
      branch: working_approach
      expected_result: unchanged`

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
    working_branch_must_return: { type: int, value_equals: 5 }` + litTailN + `
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
    working_branch_must_return: { type: str, value_equals: "fixtures/probe-object" }` + litTailStr + `
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
    working_branch_must_return: { type: int, value_equals: 99 }` + litTailInput + `
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
    working_branch_must_return: { type: int, value_equals: 5 }` + litTailN + `
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

func TestRunUnitPathSimpleKey(t *testing.T) {
	skipIfNoPython3(t)
	yaml := `
unit_id: unit-path-simple-key
domain: [test]
version_constraints: { spec: { name: test } }
failed_approach:
  description: returns dict with int permissions
  setup: []
  action:
    - { type: code, lang: python, body: "$RESULT = {'permissions': 18}" }
  assertion: { type: returns, expect: fail }
working_approach:
  description: returns dict with str permissions
  setup: []
  action:
    - { type: code, lang: python, body: "$RESULT = {'permissions': '022'}` + pathGuard + `" }
  assertion: { type: returns, expect: success }
verification:
  type: unit
  isolation: function
  differential:
    inputs:
      $F36G: 0
    failed_branch_must_return: { type: int, value_equals: 18, path: permissions }
    working_branch_must_return: { type: str, value_equals: '022', path: permissions }` + pathTail + `
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

func TestRunUnitPathNestedKeys(t *testing.T) {
	skipIfNoPython3(t)
	yaml := `
unit_id: unit-path-nested-keys
domain: [test]
version_constraints: { spec: { name: test } }
failed_approach:
  description: returns nested dict with int 0
  setup: []
  action:
    - { type: code, lang: python, body: "$RESULT = {'a': {'b': 0}}" }
  assertion: { type: returns, expect: fail }
working_approach:
  description: returns nested dict with int 42
  setup: []
  action:
    - { type: code, lang: python, body: "$RESULT = {'a': {'b': 42}}` + pathGuard + `" }
  assertion: { type: returns, expect: success }
verification:
  type: unit
  isolation: function
  differential:
    inputs:
      $F36G: 0
    failed_branch_must_return: { type: int, value_equals: 0, path: a.b }
    working_branch_must_return: { type: int, value_equals: 42, path: a.b }` + pathTail + `
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

func TestRunUnitPathMissingKey(t *testing.T) {
	skipIfNoPython3(t)
	yaml := `
unit_id: unit-path-missing-key
domain: [test]
version_constraints: { spec: { name: test } }
failed_approach:
  description: returns dict missing the requested key
  setup: []
  action:
    - { type: code, lang: python, body: "$RESULT = {'permissions': 18}" }
  assertion: { type: returns, expect: fail }
working_approach:
  description: returns 22 (irrelevant for this test)
  setup: []
  action:
    - { type: code, lang: python, body: "$RESULT = 22" }
  assertion: { type: returns, expect: success }
verification:
  type: unit
  isolation: function
  differential:
    failed_branch_must_return: { type: int, value_equals: 18, path: missing_key }
    working_branch_must_return: { type: int, value_equals: 22 }` + structTail + `
  timeout_seconds: 5
`
	res, err := Run([]byte(yaml))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "rejected" {
		t.Fatalf("status=%q, want rejected (reasons=%v)", res.Status, res.Reasons)
	}
	if !hasReason(res.Reasons, "unexpected_exception") {
		t.Fatalf("expected unexpected_exception, got %v", res.Reasons)
	}
}

func TestRunUnitPathWithLengthAndContains(t *testing.T) {
	skipIfNoPython3(t)
	yaml := `
unit_id: unit-path-length-contains
domain: [test]
version_constraints: { spec: { name: test } }
failed_approach:
  description: returns dict with shorter items list
  setup: []
  action:
    - { type: code, lang: python, body: "$RESULT = {'items': [1, 2]}" }
  assertion: { type: returns, expect: fail }
working_approach:
  description: returns dict with mixed items including ValueError
  setup: []
  action:
    - { type: code, lang: python, body: "$RESULT = {'items': [ValueError('a'), 1, 2]}` + pathGuard + `" }
  assertion: { type: returns, expect: success }
verification:
  type: unit
  isolation: function
  differential:
    inputs:
      $F36G: 0
    failed_branch_must_return: { length: 2, path: items }
    working_branch_must_return: { length: 3, contains_exception_type: ValueError, path: items }` + pathTail + `
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
