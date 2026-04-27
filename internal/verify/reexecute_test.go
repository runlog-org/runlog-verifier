package verify

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// skipIfNoSh skips when /bin/sh isn't on PATH. The reexecute orchestrator's
// shell-tool path needs `sh` in the same way the unit-tier needs `python3`;
// CI ubuntu-latest has it but sandboxed test runners might not.
func skipIfNoSh(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not on PATH")
	}
}

// TestPOCReexecuteShapeEndToEnd is the integration smoke test for the F23
// reexecute slice: schema delta (cassette.runtime + setup_script), per-branch
// tmpdir sandbox, SubprocessDriver dispatch, mutation testing across both
// input-substitution (mutate_fixture) and source-rewriting (swap_function_call)
// strategies. Mirrors TestPOCUnitShapeEndToEnd's role for the unit tier.
//
// The POC is shell-flavored (no sqlite/postgres/redis dependency) so it runs
// on any host with sh + cat + tr — every supported CI / dev box.
func TestPOCReexecuteShapeEndToEnd(t *testing.T) {
	skipIfNoSh(t)
	skipIfNoBinV(t, "tr")
	skipIfNoBinV(t, "cat")
	data, err := os.ReadFile("testdata/reexecute-shape-poc.yaml")
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	res, err := Run(data)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "verified" {
		t.Fatalf("status=%q, reasons=%v", res.Status, res.Reasons)
	}
	if res.Tier != "integration" {
		t.Fatalf("tier=%q, want integration", res.Tier)
	}
	if res.UnitID != "reexecute-shape-poc-shell-uppercase" {
		t.Fatalf("unit_id=%q", res.UnitID)
	}
}

// TestReexecuteRuntimeToolNotImplemented covers the tier_unsupported diagnostic
// path for runtime tools the verifier recognises but doesn't drive yet
// (postgres, redis, git, docker). Authors of canonical seeds for those tools
// should see a precise reason naming the tool, not a generic rejection.
func TestReexecuteRuntimeToolNotImplemented(t *testing.T) {
	yaml := `
unit_id: reexecute-postgres-stub
domain: [test]
failed_approach:
  description: stub
  setup: []
  action: [{type: code, lang: shell, body: "true"}]
  assertion: { type: returns, expect: fail }
working_approach:
  description: stub
  setup: []
  action: [{type: code, lang: shell, body: "true"}]
  assertion: { type: returns, expect: success }
verification:
  type: integration
  isolation: database
  cassette:
    mode: reexecute
    artifact: pg-stub.cassette.yaml
    runtime: { tool: postgres }
    captures: [stub]
    strips: [stub]
    replay_targets: [stub]
  differential:
    failed_branch_must_return: { type: string, value_equals: "" }
    working_branch_must_return: { type: string, value_equals: "" }
  mutations:
    - { strategy: mutate_fixture, target: $X, new_value: 1, expected_result: unchanged }
    - { strategy: mutate_fixture, target: $X, new_value: 2, expected_result: unchanged }
  timeout_seconds: 5
`
	res, err := Run([]byte(yaml))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "tier_unsupported" {
		t.Fatalf("status=%q, want tier_unsupported (reasons=%v)", res.Status, res.Reasons)
	}
	if len(res.Reasons) != 1 || res.Reasons[0].Code != "runtime_tool_not_yet_implemented" {
		t.Fatalf("reasons=%v, want runtime_tool_not_yet_implemented", res.Reasons)
	}
	if !strings.Contains(res.Reasons[0].Message, "postgres") {
		t.Fatalf("message=%q, expected to name postgres", res.Reasons[0].Message)
	}
}

// TestReexecuteRuntimeMissing covers the case where a hand-crafted entry sets
// cassette.mode: reexecute without declaring runtime. The schema's allOf gate
// would catch this upstream; the CLI path has no such gate so the orchestrator
// must surface a precise reason.
func TestReexecuteRuntimeMissing(t *testing.T) {
	yaml := `
unit_id: reexecute-no-runtime
domain: [test]
failed_approach:
  description: stub
  setup: []
  action: [{type: code, lang: shell, body: "true"}]
  assertion: { type: returns, expect: fail }
working_approach:
  description: stub
  setup: []
  action: [{type: code, lang: shell, body: "true"}]
  assertion: { type: returns, expect: success }
verification:
  type: integration
  isolation: subprocess
  cassette:
    mode: reexecute
    artifact: no-runtime.cassette.yaml
    captures: [stub]
    strips: [stub]
    replay_targets: [stub]
  differential:
    failed_branch_must_return: { type: string, value_equals: "" }
    working_branch_must_return: { type: string, value_equals: "" }
  mutations:
    - { strategy: mutate_fixture, target: $X, new_value: 1, expected_result: unchanged }
    - { strategy: mutate_fixture, target: $X, new_value: 2, expected_result: unchanged }
  timeout_seconds: 5
`
	res, err := Run([]byte(yaml))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "rejected" {
		t.Fatalf("status=%q, want rejected", res.Status)
	}
	if len(res.Reasons) != 1 || res.Reasons[0].Code != "cassette_runtime_missing" {
		t.Fatalf("reasons=%v, want cassette_runtime_missing", res.Reasons)
	}
}

// TestReexecuteIsolationNotImplemented covers a reexecute-mode cassette paired
// with an isolation v0.1 doesn't dispatch under (compiler, docker_daemon).
func TestReexecuteIsolationNotImplemented(t *testing.T) {
	yaml := `
unit_id: reexecute-compiler-stub
domain: [test]
failed_approach:
  description: stub
  setup: []
  action: [{type: code, lang: shell, body: "true"}]
  assertion: { type: returns, expect: fail }
working_approach:
  description: stub
  setup: []
  action: [{type: code, lang: shell, body: "true"}]
  assertion: { type: returns, expect: success }
verification:
  type: integration
  isolation: compiler
  cassette:
    mode: reexecute
    artifact: compiler-stub.cassette.yaml
    runtime: { tool: shell }
    captures: [stub]
    strips: [stub]
    replay_targets: [stub]
  differential:
    failed_branch_must_return: { type: string, value_equals: "" }
    working_branch_must_return: { type: string, value_equals: "" }
  mutations:
    - { strategy: mutate_fixture, target: $X, new_value: 1, expected_result: unchanged }
    - { strategy: mutate_fixture, target: $X, new_value: 2, expected_result: unchanged }
  timeout_seconds: 5
`
	res, err := Run([]byte(yaml))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "tier_unsupported" {
		t.Fatalf("status=%q, want tier_unsupported (reasons=%v)", res.Status, res.Reasons)
	}
	if len(res.Reasons) != 1 || res.Reasons[0].Code != "isolation_not_yet_implemented" {
		t.Fatalf("reasons=%v, want isolation_not_yet_implemented", res.Reasons)
	}
}

// TestReexecuteSetupScriptFailed covers a setup_script line that exits non-zero.
// The branch should be rejected with `setup_script_failed`, naming the failing
// command so authors can fix it.
func TestReexecuteSetupScriptFailed(t *testing.T) {
	skipIfNoSh(t)
	yaml := `
unit_id: reexecute-bad-setup
domain: [test]
failed_approach:
  description: never reached
  setup: []
  action: [{type: code, lang: shell, body: "echo unreachable"}]
  assertion: { type: returns, expect: fail }
working_approach:
  description: never reached
  setup: []
  action: [{type: code, lang: shell, body: "echo unreachable"}]
  assertion: { type: returns, expect: success }
verification:
  type: integration
  isolation: subprocess
  cassette:
    mode: reexecute
    artifact: bad-setup.cassette.yaml
    runtime: { tool: shell }
    setup_script:
      - "exit 11"
    captures: [stub]
    strips: [stub]
    replay_targets: [stub]
  differential:
    failed_branch_must_return: { type: string, value_equals: "" }
    working_branch_must_return: { type: string, value_equals: "" }
  mutations:
    - { strategy: mutate_fixture, target: $X, new_value: 1, expected_result: unchanged }
    - { strategy: mutate_fixture, target: $X, new_value: 2, expected_result: unchanged }
  timeout_seconds: 5
`
	res, err := Run([]byte(yaml))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "rejected" {
		t.Fatalf("status=%q, want rejected (reasons=%v)", res.Status, res.Reasons)
	}
	if len(res.Reasons) != 1 || res.Reasons[0].Code != "setup_script_failed" {
		t.Fatalf("reasons=%v, want setup_script_failed", res.Reasons)
	}
}

// skipIfNoBinV is the verify-package version of skipIfNoBin from the runner
// package's subprocess_test.go. Duplicated to keep the test packages
// independent; trivially small.
func skipIfNoBinV(t *testing.T, name string) {
	t.Helper()
	if _, err := exec.LookPath(name); err != nil {
		t.Skipf("%s not on PATH", name)
	}
}
