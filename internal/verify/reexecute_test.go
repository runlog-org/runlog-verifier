package verify

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/runlog-org/runlog-verifier/internal/verify/cassette"
	"github.com/runlog-org/runlog-verifier/internal/verify/runner"
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
	if len(res.Reasons) != 1 || res.Reasons[0].Code != "runtime_unsupported" {
		t.Fatalf("reasons=%v, want runtime_unsupported", res.Reasons)
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
	if len(res.Reasons) != 1 || res.Reasons[0].Code != "isolation_unsupported" {
		t.Fatalf("reasons=%v, want isolation_unsupported", res.Reasons)
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

// TestRunReexecuteBranchRejectsDbSqliteSymlink covers the Lstat guard at
// reexecute.go:225-238. A hostile sqlite cassette whose setup_script creates a
// symlink at $WORKDIR/db.sqlite would otherwise let subsequent sqlite3
// invocations write through the link and out of the sandbox. The guard refuses
// to proceed and surfaces sandbox_symlink_rejected.
func TestRunReexecuteBranchRejectsDbSqliteSymlink(t *testing.T) {
	skipIfNoSh(t)
	skipIfNoBinV(t, "ln")

	// Pre-create a target file outside the sandbox so the assertion can later
	// confirm its content was NOT exposed (proving the link was not followed).
	targetDir := t.TempDir()
	target := filepath.Join(targetDir, "secret.txt")
	if err := os.WriteFile(target, []byte("secret-payload"), 0o600); err != nil {
		t.Fatalf("WriteFile target: %v", err)
	}

	cas := &cassette.Cassette{
		Mode:    "reexecute",
		Runtime: &cassette.Runtime{Tool: "sqlite"},
		// setup_script creates a symlink at $WORKDIR/db.sqlite pointing at the
		// out-of-sandbox secret. The Lstat guard must catch this before the
		// driver opens it via sqlite3.
		SetupScript: []string{"ln -sf " + target + " $WORKDIR/db.sqlite"},
	}

	// A no-op action — we never get past the symlink check.
	action := []runner.Step{{Type: "code", Lang: "sql", Body: "SELECT 1;"}}

	res, driver, runErr, reason := runReexecuteBranch(
		"failed_approach", cas, nil, action, nil, 5,
	)

	if reason == nil {
		t.Fatalf("expected non-nil Reason, got res=%+v driver=%+v err=%v", res, driver, runErr)
	}
	if reason.Code != "sandbox_symlink_rejected" {
		t.Fatalf("reason.Code=%q, want sandbox_symlink_rejected (msg=%q)", reason.Code, reason.Message)
	}
	if runErr != nil {
		// Per the doc comment on runReexecuteBranch (lines 195-196), the
		// verifier-internal early return should set the third return to nil.
		t.Errorf("runErr=%v, want nil for verifier-internal early return", runErr)
	}
	// The guard's cleanupReexecuteSandbox call must have removed the workdir.
	// driver returned to caller is the zero value here per the source's
	// `return runner.ExecResult{}, runner.SubprocessDriver{}, nil, &r`. So we
	// can't assert a specific path is gone, but the guard's path uses the
	// local `driver` variable (not the zero value); covered by the integration
	// path below.

	// Defense in depth: the `secret-payload` content must not leak via
	// res.Repr — we never ran the action, so res should be the zero value.
	if res.Repr != "" || res.JSONValue != nil {
		t.Errorf("ExecResult unexpectedly non-zero: %+v", res)
	}
}

// TestRunReexecuteBranchAllocFailedReturnsTypedReason covers the MkdirTemp
// failure path at reexecute.go:206-211. Setting TMPDIR to a non-existent
// directory forces MkdirTemp to fail; the function must return the
// sandbox_alloc_failed Reason without leaking partial state and with a nil
// runErr per the doc comment (the failure is verifier-internal, not a child
// process error).
func TestRunReexecuteBranchAllocFailedReturnsTypedReason(t *testing.T) {
	// Use a path under a real but unwritable parent so MkdirTemp fails
	// portably. /nonexistent-runlog-T17-... isn't created and isn't writable.
	t.Setenv("TMPDIR", "/nonexistent-runlog-T17-alloc-fail-xyz")

	cas := &cassette.Cassette{
		Mode:    "reexecute",
		Runtime: &cassette.Runtime{Tool: "shell"},
	}
	action := []runner.Step{{Type: "code", Lang: "shell", Body: "true"}}

	res, driver, runErr, reason := runReexecuteBranch(
		"failed_approach", cas, nil, action, nil, 5,
	)

	if reason == nil {
		t.Fatalf("expected non-nil Reason, got res=%+v driver=%+v err=%v", res, driver, runErr)
	}
	if reason.Code != "sandbox_alloc_failed" {
		t.Fatalf("reason.Code=%q, want sandbox_alloc_failed (msg=%q)", reason.Code, reason.Message)
	}
	if !strings.Contains(reason.Message, "failed_approach") {
		t.Errorf("message=%q, expected to name the branch", reason.Message)
	}
	if runErr != nil {
		t.Errorf("runErr=%v, want nil for verifier-internal early return", runErr)
	}
	// Driver must be the zero value — no partial state leaked.
	if driver.Tool != "" || driver.Workdir != "" {
		t.Errorf("driver=%+v, want zero value (no partial state)", driver)
	}
	// ExecResult must be the zero value too.
	if res.Repr != "" || res.JSONValue != nil || res.Raised {
		t.Errorf("ExecResult unexpectedly non-zero: %+v", res)
	}
}

// TestCleanupReexecuteSandbox pins the three contracts of
// cleanupReexecuteSandbox (reexecute.go:253-259):
//
//  1. teardown_script runs and the workdir is RemoveAll'd.
//  2. teardown_script step errors do not stop the workdir RemoveAll.
//  3. a zero-value SubprocessDriver is a safe no-op.
func TestCleanupReexecuteSandbox(t *testing.T) {
	t.Run("runs_teardown_and_removes_workdir", func(t *testing.T) {
		skipIfNoSh(t)
		dir, err := os.MkdirTemp("", "runlog-T17-cleanup-")
		if err != nil {
			t.Fatalf("MkdirTemp: %v", err)
		}
		// In case the test fails before cleanup runs, ensure tmpdir is removed.
		t.Cleanup(func() { _ = os.RemoveAll(dir) })

		// Marker file lives outside the sandbox so we can observe teardown
		// running even after the workdir is gone.
		marker := filepath.Join(t.TempDir(), "teardown-marker")
		cas := &cassette.Cassette{
			Mode:           "reexecute",
			Runtime:        &cassette.Runtime{Tool: "shell"},
			TeardownScript: []string{"touch " + marker},
		}
		driver := runner.SubprocessDriver{Tool: "shell", Workdir: dir}

		cleanupReexecuteSandbox(driver, cas, nil, 5)

		if _, err := os.Stat(marker); err != nil {
			t.Errorf("teardown marker not created: %v", err)
		}
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("workdir still exists (err=%v); RemoveAll should have removed it", err)
		}
	})

	t.Run("tolerates_teardown_script_nonzero_exit", func(t *testing.T) {
		skipIfNoSh(t)
		dir, err := os.MkdirTemp("", "runlog-T17-cleanup-nonzero-")
		if err != nil {
			t.Fatalf("MkdirTemp: %v", err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(dir) })

		// teardown_script with a non-zero-exiting line. RunTeardownScript runs
		// each line via execShell; non-zero exits are silently ignored
		// (subprocess.go:262-277). cleanupReexecuteSandbox must not panic and
		// must still RemoveAll the workdir.
		cas := &cassette.Cassette{
			Mode:           "reexecute",
			Runtime:        &cassette.Runtime{Tool: "shell"},
			TeardownScript: []string{"false"},
		}
		driver := runner.SubprocessDriver{Tool: "shell", Workdir: dir}

		// Must not panic.
		cleanupReexecuteSandbox(driver, cas, nil, 5)

		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("workdir still exists (err=%v); cleanup should have continued past the non-zero teardown line", err)
		}
	})

	t.Run("zero_value_driver_no_op", func(t *testing.T) {
		// Locks the contract documented at reexecute.go:251-252: "Safe to call
		// with a zero-value SubprocessDriver — the os.RemoveAll on an empty
		// path is a no-op." A regression that drops the early return would
		// either run teardown against a missing shell context or RemoveAll an
		// empty path; both are observable as a panic or process spawn.
		cas := &cassette.Cassette{
			Mode:           "reexecute",
			Runtime:        &cassette.Runtime{Tool: "shell"},
			TeardownScript: []string{"echo should-not-run"},
		}
		// Must not panic, must not spawn a subprocess.
		cleanupReexecuteSandbox(runner.SubprocessDriver{}, cas, nil, 5)
	})
}
