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
// (git, docker). Authors of canonical seeds for those tools
// should see a precise reason naming the tool, not a generic rejection.
func TestReexecuteRuntimeToolNotImplemented(t *testing.T) {
	yaml := `
unit_id: reexecute-git-stub
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
    artifact: git-stub.cassette.yaml
    runtime: { tool: git }
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
	if !strings.Contains(res.Reasons[0].Message, "git") {
		t.Fatalf("message=%q, expected to name git", res.Reasons[0].Message)
	}
}

// TestReexecuteRuntimePostgresProvisionFailure covers the new
// runtime_provision_failed reason: tool=postgres is recognised, but a
// deliberately unreachable RUNLOG_VERIFY_PGURL forces ProvisionPostgresDB
// to fail. The reason code must be "runtime_provision_failed", not
// "runtime_unsupported" (which would mean the runner declined to run at
// all) and not "branch_runner_error" (the catch-all for unmapped errors).
//
// Skips when psql is not on PATH — the helper shells out to it.
func TestReexecuteRuntimePostgresProvisionFailure(t *testing.T) {
	if _, err := exec.LookPath("psql"); err != nil {
		t.Skip("psql not on PATH")
	}
	// Port 1 is the TCPMUX well-known port; reliably refuses the
	// postgres handshake with a fast connect-failure on every host.
	t.Setenv("RUNLOG_VERIFY_PGURL", "postgres://localhost:1/postgres")

	cas := &cassette.Cassette{
		Mode:           "reexecute",
		Runtime:        &cassette.Runtime{Tool: "postgres"},
		SetupScript:    nil,
		TeardownScript: nil,
	}
	setup := []runner.Step(nil)
	action := []runner.Step{{Type: "code", Lang: "sql", Body: "SELECT 1;"}}

	_, _, reason, runErr := runReexecuteBranch(
		"failed_approach", cas, setup, action, nil, 5)

	if reason == nil {
		t.Fatalf("expected non-nil Reason; got nil (runErr=%v)", runErr)
	}
	if reason.Code != "runtime_provision_failed" {
		t.Fatalf("reason.Code=%q, want runtime_provision_failed; reason=%+v", reason.Code, reason)
	}
	if !strings.Contains(reason.Message, "failed_approach") {
		t.Fatalf("reason.Message=%q, want to contain branch name 'failed_approach'", reason.Message)
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

	res, driver, reason, runErr := runReexecuteBranch(
		"failed_approach", cas, nil, action, nil, 5,
	)

	if reason == nil {
		t.Fatalf("expected non-nil Reason, got res=%+v driver=%+v err=%v", res, driver, runErr)
	}
	if reason.Code != "sandbox_symlink_rejected" {
		t.Fatalf("reason.Code=%q, want sandbox_symlink_rejected (msg=%q)", reason.Code, reason.Message)
	}
	if runErr != nil {
		// Per the doc comment on runReexecuteBranch, the verifier-internal
		// early return should set the trailing error to nil.
		t.Errorf("runErr=%v, want nil for verifier-internal early return", runErr)
	}
	// The guard's cleanupReexecuteSandbox call must have removed the workdir.
	// driver returned to caller is the zero value here per the source's
	// `return runner.ExecResult{}, runner.SubprocessDriver{}, &r, nil`. So we
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

	res, driver, reason, runErr := runReexecuteBranch(
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

// TestRunReexecuteShareStateRejectedForNonDocker covers the F87 v0.1 gate:
// cassette.runtime.share_state_across_mutations=true is only honored for
// tool: docker. Other tools that set the flag get a typed
// share_state_unsupported_for_tool rejection so the seed-author mistake
// surfaces precisely.
// TestReexecuteMutationSharedPathSkipsProvision covers the F87 dispatcher in
// runReexecuteMutations: when cassette.runtime.share_state_across_mutations
// is true, mutations run via runOneReexecuteMutationShared which re-uses the
// baseline driver and does NOT re-provision a sandbox per mutation. We assert
// the structural wiring by counting Driver.Run invocations against a stub
// driver: with share=true and N mutations each targeting working_approach only,
// the stub should record exactly N Run calls (one per mutation × one branch).
//
// driverFunc (defined at file scope below) adapts a plain function to
// runner.Driver — no host-tool dependency; pure in-process.
func TestReexecuteMutationSharedPathSkipsProvision(t *testing.T) {
	// calls counts invocations of the stub working-branch driver.
	calls := 0

	// fixedResult is what the stub driver always returns. Because the stub
	// returns this for every Run call, execResultsEqual(got, baseline) is
	// true → classifyOutcome returns outcomeUnchanged, matching
	// ExpectedResult: "unchanged" on each mutation → no reasons emitted.
	fixedResult := runner.ExecResult{
		Raised:       false,
		TypeName:     "int",
		Serializable: true,
		JSONValue:    []byte("42"),
	}

	workingDriver := driverFunc(func(_, _ []runner.Step, _ map[string]any, _ float64) (runner.ExecResult, error) {
		calls++
		return fixedResult, nil
	})
	failedDriver := driverFunc(func(_, _ []runner.Step, _ map[string]any, _ float64) (runner.ExecResult, error) {
		t.Error("Failed branch Driver.Run called unexpectedly in share=true path")
		return runner.ExecResult{}, nil
	})

	b := mutationBaseline{
		Working: branchBaseline{
			Setup:  nil,
			Action: []runner.Step{{Type: "code", Lang: "shell", Body: "echo 42"}},
			Inputs: map[string]any{"$x": "hello"},
			Result: fixedResult,
			Driver: workingDriver,
		},
		// Failed branch is not targeted by these mutations (branch defaults to
		// working_approach), so failedDriver.Run must never be called.
		Failed: branchBaseline{
			Setup:  nil,
			Action: []runner.Step{{Type: "code", Lang: "shell", Body: "echo bad"}},
			Inputs: map[string]any{"$x": "hello"},
			Result: runner.ExecResult{Raised: true, Exception: "RuntimeError", Message: "fail"},
			Driver: failedDriver,
		},
		Diff:    nil,
		Timeout: 5,
	}

	// 2 mutations, each targeting working_approach (the default branch),
	// strategy set_literal_value. The stub always returns fixedResult, so
	// classifyOutcome → outcomeUnchanged == expected → no reasons.
	e := &Entry{
		UnitID: "stub-unit",
		Verification: Verification{
			TimeoutSeconds: 5,
			Mutations: []Mutation{
				{
					Strategy:       "set_literal_value",
					Target:         "$x",
					NewValue:       "mutated-1",
					ExpectedResult: "unchanged",
				},
				{
					Strategy:       "set_literal_value",
					Target:         "$x",
					NewValue:       "mutated-2",
					ExpectedResult: "unchanged",
				},
			},
		},
		WorkingApproach: Branch{
			Action: []runner.Step{{Type: "code", Lang: "shell", Body: "echo 42"}},
		},
	}

	cas := &cassette.Cassette{
		Mode: "reexecute",
		Runtime: &cassette.Runtime{
			Tool:                      "docker",
			ShareStateAcrossMutations: true,
		},
	}

	reasons, supported := runReexecuteMutations(e, b, cas)

	if !supported {
		t.Fatalf("share=true path returned supported=false; reasons: %v", reasons)
	}
	if len(reasons) != 0 {
		t.Errorf("expected no reasons for matching outcomes, got: %v", reasons)
	}
	// 2 mutations × 1 branch (working_approach) = 2 Driver.Run calls.
	if calls != 2 {
		t.Errorf("Driver.Run call count = %d, want 2 (one per mutation on working_approach)", calls)
	}
}

// driverFunc is an adapter that turns a Run-shaped function into a
// runner.Driver. Defined at file scope so it satisfies the interface without
// needing a test-local type assertion workaround.
// Only used by TestReexecuteMutationSharedPathSkipsProvision.
type driverFunc func(setup, action []runner.Step, inputs map[string]any, timeout float64) (runner.ExecResult, error)

func (f driverFunc) Run(setup, action []runner.Step, inputs map[string]any, timeout float64) (runner.ExecResult, error) {
	return f(setup, action, inputs, timeout)
}

func TestRunReexecuteShareStateRejectedForNonDocker(t *testing.T) {
	e := &Entry{
		UnitID: "test-unit",
		Verification: Verification{
			Isolation:      "database",
			TimeoutSeconds: 5,
		},
		FailedApproach: Branch{
			Action: []runner.Step{{Type: "code", Lang: "sql", Body: "SELECT 1;"}},
		},
		WorkingApproach: Branch{
			Action: []runner.Step{{Type: "code", Lang: "sql", Body: "SELECT 1;"}},
		},
	}
	cas := &cassette.Cassette{
		Mode: "reexecute",
		Runtime: &cassette.Runtime{
			Tool:                      "postgres",
			ShareStateAcrossMutations: true,
		},
	}

	res := runReexecute(e, cas)

	if res.Status == "verified" {
		t.Fatalf("expected rejection, got status=verified")
	}
	if len(res.Reasons) == 0 {
		t.Fatalf("expected reasons, got none")
	}
	if res.Reasons[0].Code != "share_state_unsupported_for_tool" {
		t.Errorf("reason.Code=%q, want share_state_unsupported_for_tool (msg=%q)",
			res.Reasons[0].Code, res.Reasons[0].Message)
	}
	if !strings.Contains(res.Reasons[0].Message, "postgres") {
		t.Errorf("reason.Message should name the offending tool; got %q", res.Reasons[0].Message)
	}
}

// TestMaterializeDirectoryFixtures_WritesAndBindsInputs is the focused
// unit test for the F94 slice 2 helper. It avoids any external-tool
// dependency by calling materializeDirectoryFixtures directly with a
// constructed cassette + tmpdir + inputs map. Asserts:
//
//   - the declared file tree lands on disk under <workdir>/<NAME>/
//     with parent directories created and file bodies intact;
//   - inputs[$NAME] is bound to the workdir-relative subdir path
//     ("./SOURCE_PATH") so subsequent setup_script / action steps see
//     it after $-token substitution.
func TestMaterializeDirectoryFixtures_WritesAndBindsInputs(t *testing.T) {
	workdir := t.TempDir()
	cas := &cassette.Cassette{
		Mode:    "reexecute",
		Runtime: &cassette.Runtime{Tool: "shell"},
		DirectoryFixtures: []cassette.DirectoryFixture{
			{
				Name: "$SOURCE_PATH",
				Kind: "directory",
				Files: map[string]string{
					"hello.txt":       "world",
					"src/nested/x.go": "package nested",
				},
			},
		},
	}
	var inputs map[string]any // intentionally nil — exercise the nil-guard.

	reason := materializeDirectoryFixtures(cas, workdir, &inputs)
	if reason != nil {
		t.Fatalf("unexpected reason: %+v", reason)
	}

	// File bodies on disk.
	body, err := os.ReadFile(filepath.Join(workdir, "SOURCE_PATH", "hello.txt"))
	if err != nil {
		t.Fatalf("read hello.txt: %v", err)
	}
	if string(body) != "world" {
		t.Errorf("hello.txt body=%q, want world", body)
	}
	body, err = os.ReadFile(filepath.Join(workdir, "SOURCE_PATH", "src", "nested", "x.go"))
	if err != nil {
		t.Fatalf("read src/nested/x.go: %v", err)
	}
	if string(body) != "package nested" {
		t.Errorf("nested body=%q, want %q", body, "package nested")
	}

	// inputs binding: workdir-relative path with leading "./".
	if inputs == nil {
		t.Fatalf("inputs still nil after materialize; nil-guard failed")
	}
	if got := inputs["$SOURCE_PATH"]; got != "./SOURCE_PATH" {
		t.Errorf("inputs[$SOURCE_PATH]=%v, want ./SOURCE_PATH", got)
	}
}

// TestReexecuteDirectoryFixtureEndToEnd is the integration sanity gate
// for the F94 slice 2 lifecycle. A shell-tool cassette declares a
// kind: directory fixture; the setup_script and action read the
// materialized file via $SOURCE_PATH. Asserts verify.Run reports
// status: "verified" — proves materialization happened before
// setup_script, $SOURCE_PATH was bound in inputs, and the
// workdir-relative path resolved correctly inside the per-branch
// shell.
func TestReexecuteDirectoryFixtureEndToEnd(t *testing.T) {
	skipIfNoSh(t)
	skipIfNoBinV(t, "cat")
	skipIfNoBinV(t, "tr")

	yaml := `unit_id: synthetic-directory-fixture-reexecute-smoke
domain: ['shell']
failed_approach:
  description: cats the materialized file verbatim (lowercase content)
  setup: []
  action:
    - type: code
      lang: shell
      body: |
        cat $SOURCE_PATH/hello.txt
  assertion:
    type: value_equals
    expect: fail
working_approach:
  description: cats then upper-cases the materialized file
  setup: []
  action:
    - type: code
      lang: shell
      body: |
        cat $SOURCE_PATH/hello.txt | tr a-z A-Z
  assertion:
    type: value_equals
    expect: success
verification:
  type: integration
  isolation: subprocess
  cassette:
    mode: reexecute
    artifact: synthetic-directory.cassette.yaml
    runtime:
      tool: shell
    fixtures:
      $SOURCE_PATH:
        kind: directory
        files:
          hello.txt: "world"
  differential:
    failed_branch_must_return:
      type: string
      value_equals: "world"
    working_branch_must_return:
      type: string
      value_equals: "WORLD"
  timeout_seconds: 10
`

	res, err := Run([]byte(yaml))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Defense in depth: any fixture_materialize_failed surfaces a
	// hook-ordering or sandbox bug.
	for _, r := range res.Reasons {
		if strings.HasPrefix(r.Code, "fixture_") {
			t.Fatalf("fixture-related rejection: code=%q msg=%q", r.Code, r.Message)
		}
	}
	if res.Status != "verified" {
		t.Fatalf("status=%q, want verified (reasons=%v)", res.Status, res.Reasons)
	}
}
