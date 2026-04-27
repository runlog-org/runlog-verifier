package verify

// Integration tier — reexecute mode. Companion to integration.go (replay).
//
// =============================================================================
// Design summary (Phase 2 slice F23 in the verifier slice cadence)
// =============================================================================
//
// Reexecute mode covers integration entries whose state is local rather than
// network-traversed: filesystems, embedded databases, shells. Where replay
// mode rebinds $ENDPOINT to a stub HTTP server seeded from cassette.steps,
// reexecute mode allocates a per-branch tmpdir sandbox, executes the
// cassette's declared setup_script to provision it, and dispatches the
// branch's typed setup+action steps through SubprocessDriver.
//
// v0.1 of this slice supports the two simplest runtime tools:
//
//   tool: shell  → step.Lang must be "shell"; bodies run via `sh -c`.
//   tool: sqlite → step.Lang ∈ {"shell", "sql"}; sql bodies run via
//                  `sqlite3 $DB_PATH` with body on stdin.
//
// Other declared tools (postgres, redis, git, docker) surface as
// `runtime_tool_not_yet_implemented`. The slice deliberately keeps
// tool-specific drivers out of scope per CLAUDE.md invariant #10 ("cheap and
// simple first") — adding a postgres/redis driver is a one-tool follow-up.
//
// Per-branch sandbox lifecycle:
//
//   1. os.MkdirTemp("", "runlog-reexec-...")
//   2. SubprocessDriver{Tool, Workdir}.RunSetupScript(cassette.setup_script)
//   3. SubprocessDriver{Tool, Workdir}.Run(branch.setup, branch.action, inputs)
//   4. SubprocessDriver{Tool, Workdir}.RunTeardownScript(cassette.teardown_script) (best-effort)
//   5. os.RemoveAll(Workdir)
//
// Per-mutation isolation invariant: each mutation re-runs its target branch
// in a fresh tmpdir with the cassette's setup_script re-applied (mirrors
// F19's per-mutation stub re-creation). Setup-script runs use the
// per-mutation inputs so that mutate_fixture / set_literal_value
// perturbations propagate into the sandbox seed (e.g. a perturbed $TABLE
// name lands in the schema-creation SQL).
//
// Cassette-response mutations are NOT supported at reexecute tier — there's
// no HTTP response to perturb. They surface as mutation_strategy_unsupported
// so a seed author who tries to mix them gets a precise diagnostic.
//
// Deliberately out of scope for this slice (follow-ups):
//   - postgres / redis / git / docker runtime tools
//   - cassette.runtime.version enforcement (advisory in v0.1)
//   - regex-based stdout matching (failed_branch_must_return: {pattern: ...})
//   - reexecute-mode replay_sequence semantics (per-branch sandboxes are
//     each fully isolated; sequences would only matter if a single sandbox
//     were shared, which v0.1 doesn't do)
//
// =============================================================================

import (
	"errors"
	"fmt"
	"os"

	"github.com/runlog-org/runlog-verifier/internal/verify/cassette"
	"github.com/runlog-org/runlog-verifier/internal/verify/runner"
)

// reexecuteSupportedTools enumerates the cassette.runtime.tool values this
// build can drive. Entries declaring a tool outside this set surface
// `runtime_tool_not_yet_implemented` so authors know the feature is recognised
// but unimplemented (rather than malformed).
var reexecuteSupportedTools = map[string]bool{
	"shell":  true,
	"sqlite": true,
}

// reexecuteSupportedIsolations enumerates the verification.isolation values
// reexecute mode dispatches under. v0.1 covers `subprocess` (shell-flavored)
// and `database` (sqlite-flavored). Other isolations declared by the schema
// (compiler, docker_daemon) stay tier_unsupported until their drivers land.
var reexecuteSupportedIsolations = map[string]bool{
	"subprocess": true,
	"database":   true,
}

// runReexecute handles tier == "integration" with cassette.mode == "reexecute".
// Mirrors runIntegration's replay path: shape checks, parse cassette, drive
// both branches through a per-branch SubprocessDriver, run mutations with
// per-mutation fresh sandboxes, sign on success.
func runReexecute(e *Entry, cas *cassette.Cassette) Result {
	res := Result{UnitID: e.UnitID, Tier: "integration"}

	if !reexecuteSupportedIsolations[e.Verification.Isolation] {
		res.Status = "tier_unsupported"
		res.Reasons = []Reason{{
			Code: "isolation_not_yet_implemented",
			Message: fmt.Sprintf(
				"reexecute-mode isolation %q is not implemented in this verifier "+
					"build — subprocess + database (sqlite) ship first; "+
					"compiler / docker_daemon land in follow-up commits",
				e.Verification.Isolation,
			),
		}}
		return res
	}

	if cas.Runtime == nil {
		// Defensive: schema's allOf gate enforces this, but the CLI path has
		// no upstream JSON Schema validator, so a hand-crafted entry that
		// omits `runtime` must be caught here rather than silently degrading.
		return rejected(res, "cassette_runtime_missing",
			"cassette.mode: reexecute requires cassette.runtime.tool")
	}
	if !reexecuteSupportedTools[cas.Runtime.Tool] {
		res.Status = "tier_unsupported"
		res.Reasons = []Reason{{
			Code: "runtime_tool_not_yet_implemented",
			Message: fmt.Sprintf(
				"cassette.runtime.tool %q is not implemented in this verifier "+
					"build — shell + sqlite ship first; postgres / redis / git / "+
					"docker land in follow-up commits",
				cas.Runtime.Tool,
			),
		}}
		return res
	}

	// ── Branch step shape ─────────────────────────────────────────────
	failedSetup, err := stepsFromAny(e.FailedApproach.Setup)
	if err != nil {
		return rejected(res, "malformed_failed_setup", err.Error())
	}
	failedAction, err := stepsFromAny(e.FailedApproach.Action)
	if err != nil {
		return rejected(res, "malformed_failed_action", err.Error())
	}
	workingSetup, err := stepsFromAny(e.WorkingApproach.Setup)
	if err != nil {
		return rejected(res, "malformed_working_setup", err.Error())
	}
	workingAction, err := stepsFromAny(e.WorkingApproach.Action)
	if err != nil {
		return rejected(res, "malformed_working_action", err.Error())
	}

	failedInputs, workingInputs, err := splitInputs(e.Verification.Differential)
	if err != nil {
		return rejected(res, "malformed_inputs", err.Error())
	}
	failedInputs = mergeLiterals(e.Literals, failedInputs)
	workingInputs = mergeLiterals(e.Literals, workingInputs)

	timeout := e.Verification.TimeoutSeconds

	// ── Failed branch run ─────────────────────────────────────────────
	failedRes, failedDriver, failedReason := runReexecuteBranch(
		"failed_approach", cas, failedSetup, failedAction, failedInputs, timeout)
	if failedReason != nil {
		res.Status = "rejected"
		res.Reasons = []Reason{*failedReason}
		return res
	}
	defer cleanupReexecuteSandbox(failedDriver, cas, failedInputs, timeout)

	// ── Working branch run ────────────────────────────────────────────
	workingRes, workingDriver, workingReason := runReexecuteBranch(
		"working_approach", cas, workingSetup, workingAction, workingInputs, timeout)
	if workingReason != nil {
		res.Status = "rejected"
		res.Reasons = []Reason{*workingReason}
		return res
	}
	defer cleanupReexecuteSandbox(workingDriver, cas, workingInputs, timeout)

	// ── Outcome matching ──────────────────────────────────────────────
	var reasons []Reason
	reasons = append(reasons, matchOutcome("failed_approach", failedRes,
		e.Verification.Differential, "failed_branch_must_return", "failed_branch_must_raise")...)
	reasons = append(reasons, matchOutcome("working_approach", workingRes,
		e.Verification.Differential, "working_branch_must_return", "working_branch_must_raise")...)
	if len(reasons) > 0 {
		res.Status = "rejected"
		res.Reasons = reasons
		return res
	}

	// ── Mutation testing ──────────────────────────────────────────────
	baseline := mutationBaseline{
		Failed: branchBaseline{
			Setup:  failedSetup,
			Action: failedAction,
			Inputs: failedInputs,
			Result: failedRes,
			Driver: failedDriver,
		},
		Working: branchBaseline{
			Setup:  workingSetup,
			Action: workingAction,
			Inputs: workingInputs,
			Result: workingRes,
			Driver: workingDriver,
		},
		Diff:    e.Verification.Differential,
		Timeout: timeout,
	}

	mutReasons, supported := runReexecuteMutations(e, baseline, cas)
	if !supported {
		res.Status = "tier_unsupported"
		res.Reasons = mutReasons
		return res
	}
	if len(mutReasons) > 0 {
		res.Status = "rejected"
		res.Reasons = mutReasons
		return res
	}

	res.Status = "verified"
	return res
}

// runReexecuteBranch allocates a fresh tmpdir, runs the cassette's
// setup_script in it, then runs the branch's setup+action through a
// SubprocessDriver pointed at the same tmpdir. Returns the action's
// ExecResult, the driver instance (for teardown + mutation re-runs), and a
// non-nil Reason if any pre-action step failed.
//
// The sandbox is *not* cleaned up here — the caller defers cleanupReexecuteSandbox
// after both branches succeed so that a panic mid-run still triggers
// teardown. Call sites that bail early (e.g. on a Reason) MUST cleanupReexecuteSandbox
// explicitly first; that's the case the leftover defer in runReexecute
// covers when both branches succeed.
func runReexecuteBranch(branchName string, cas *cassette.Cassette, setup, action []runner.Step, inputs map[string]any, timeout float64) (runner.ExecResult, runner.SubprocessDriver, *Reason) {
	workdir, err := os.MkdirTemp("", "runlog-reexec-")
	if err != nil {
		r := Reason{
			Code:    "sandbox_alloc_failed",
			Message: fmt.Sprintf("%s: %v", branchName, err),
		}
		return runner.ExecResult{}, runner.SubprocessDriver{}, &r
	}
	driver := runner.SubprocessDriver{Tool: cas.Runtime.Tool, Workdir: workdir}

	if err := driver.RunSetupScript(cas.SetupScript, inputs, timeout); err != nil {
		// Best-effort teardown + remove on setup failure so we don't leak
		// half-provisioned sandboxes when authoring goes wrong.
		_ = driver.RunTeardownScript(cas.TeardownScript, inputs, timeout)
		_ = os.RemoveAll(workdir)

		code := "setup_script_failed"
		if errors.Is(err, runner.ErrInterpreterMissing) {
			code = "runtime_unavailable"
		} else if errors.Is(err, runner.ErrTimeout) {
			code = "branch_timeout"
		}
		r := Reason{Code: code, Message: fmt.Sprintf("%s: %v", branchName, err)}
		return runner.ExecResult{}, runner.SubprocessDriver{}, &r
	}

	res, err := driver.Run(setup, action, inputs, timeout)
	if err != nil {
		_ = driver.RunTeardownScript(cas.TeardownScript, inputs, timeout)
		_ = os.RemoveAll(workdir)

		code := "branch_runner_error"
		switch {
		case errors.Is(err, runner.ErrInterpreterMissing):
			code = "runtime_unavailable"
		case errors.Is(err, runner.ErrTimeout):
			code = "branch_timeout"
		case errors.Is(err, runner.ErrLanguageUnsupported), errors.Is(err, runner.ErrSubprocessTool):
			code = "language_not_yet_implemented"
		}
		r := Reason{Code: code, Message: fmt.Sprintf("%s: %v", branchName, err)}
		return runner.ExecResult{}, driver, &r
	}
	return res, driver, nil
}

// cleanupReexecuteSandbox runs the cassette's teardown_script (best-effort,
// non-zero exits ignored) and os.RemoveAll's the workdir. Safe to call with a
// zero-value SubprocessDriver — the os.RemoveAll on an empty path is a no-op.
func cleanupReexecuteSandbox(driver runner.SubprocessDriver, cas *cassette.Cassette, inputs map[string]any, timeout float64) {
	if driver.Workdir == "" {
		return
	}
	_ = driver.RunTeardownScript(cas.TeardownScript, inputs, timeout)
	_ = os.RemoveAll(driver.Workdir)
}

// runReexecuteMutations applies each declared mutation in a fresh per-mutation
// sandbox and verifies the outcome matches the declared expectation. Cassette-
// response mutations (mutate_cassette_response) are unsupported at reexecute
// tier — they surface as mutation_strategy_unsupported so authors don't mix
// them into a non-HTTP cassette.
func runReexecuteMutations(e *Entry, b mutationBaseline, cas *cassette.Cassette) ([]Reason, bool) {
	var reasons []Reason
	supported := true

	for i, m := range e.Verification.Mutations {
		mr, ok := runOneReexecuteMutation(e, b, m, i, cas)
		if !ok {
			supported = false
		}
		reasons = append(reasons, mr...)
	}
	return reasons, supported
}

// runOneReexecuteMutation mirrors runOneIntegrationMutation but allocates a
// fresh tmpdir + replays the cassette's setup_script per mutation re-run, so
// state from the baseline + prior mutations doesn't leak.
func runOneReexecuteMutation(e *Entry, b mutationBaseline, m Mutation, idx int, cas *cassette.Cassette) ([]Reason, bool) {
	if isCassetteResponseStrategy(m.Strategy) {
		return []Reason{{
			Code: "mutation_strategy_unsupported",
			Message: fmt.Sprintf(
				"mutation #%d strategy %q targets a cassette response, but reexecute "+
					"mode has no HTTP responses to perturb. Use mutate_fixture / "+
					"set_literal_value / swap_* / remove_kwarg / drop_flag at "+
					"reexecute tier",
				idx+1, m.Strategy),
		}}, false
	}
	strat, ok := strategies[m.Strategy]
	if !ok {
		return []Reason{{
			Code: "mutation_strategy_unsupported",
			Message: fmt.Sprintf(
				"mutation #%d strategy %q is not yet implemented; supported in this build: %s",
				idx+1, m.Strategy, supportedStrategiesMessage(),
			),
		}}, false
	}

	var reasons []Reason

	for _, branch := range branchesFor(m) {
		expected, inapplicable, hasExp := expectedOutcomeFor(m, branch)
		if !hasExp {
			reasons = append(reasons, Reason{
				Code: "mutation_no_expectation",
				Message: fmt.Sprintf(
					"mutation #%d targets %s but declares no expected_result and no expected_branch_outcome.%s",
					idx+1, branch, branch),
			})
			continue
		}
		if inapplicable {
			continue
		}

		baseline, ok := b.byBranch(branch)
		if !ok {
			reasons = append(reasons, Reason{
				Code:    "mutation_unknown_branch",
				Message: fmt.Sprintf("mutation #%d targets unknown branch %q", idx+1, branch),
			})
			continue
		}

		mutInputs, mutAction, err := strat.apply(baseline, m)
		if err != nil {
			reasons = append(reasons, Reason{
				Code: "mutation_target_invalid",
				Message: fmt.Sprintf("mutation #%d (%s) on %s: %v",
					idx+1, m.Strategy, branch, err),
			})
			continue
		}

		// ── Per-mutation fresh sandbox ────────────────────────────────
		got, mutDriver, runReason := runReexecuteBranch(
			fmt.Sprintf("mutation #%d (%s) on %s", idx+1, m.Strategy, branch),
			cas, baseline.Setup, mutAction, mutInputs, b.Timeout)
		// Always tear down + remove, even on the runReason path.
		defer cleanupReexecuteSandbox(mutDriver, cas, mutInputs, b.Timeout)

		if runReason != nil {
			// Environmental failures (timeout, missing tool) stay as-is;
			// in-band step crashes have already been folded into the
			// ExecResult.Raised path by the driver and won't reach here.
			if runReason.Code == "branch_timeout" || runReason.Code == "runtime_unavailable" {
				reasons = append(reasons, Reason{
					Code:    "mutation_runner_error",
					Message: runReason.Message,
				})
				continue
			}
			// setup_script_failed under a mutation: synthesize a raised
			// ExecResult so the outcome classifier produces outcomeFail.
			// The setup script is part of the test surface — if a mutation
			// breaks it, that's a real fail, not a runner error.
			got = runner.ExecResult{
				Raised:    true,
				Exception: "SubprocessError",
				Message:   runReason.Message,
			}
		}

		actual := classifyOutcome(branch, got, baseline.Result, b.Diff)
		if actual != expected {
			if expected == outcomeFail &&
				actual == outcomeUnchanged &&
				discriminatingStrategies[m.Strategy] &&
				!stepBodiesEqual(baseline.Action, mutAction) {
				token, _ := resolveSwapToken(m)
				reasons = append(reasons, Reason{
					Code: "mutation_did_not_discriminate",
					Message: fmt.Sprintf(
						"mutation #%d (%s) on %s: rewrote source but produced no behavioural change. "+
							"The token %q was substituted in the action source but the program's observable "+
							"output was byte-identical to the baseline. Pick a token that actually discriminates.",
						idx+1, m.Strategy, branch, token),
				})
				continue
			}
			reasons = append(reasons, Reason{
				Code: "mutation_outcome_mismatch",
				Message: fmt.Sprintf("mutation #%d (%s) on %s: expected %s, got %s",
					idx+1, m.Strategy, branch, expected, actual),
			})
		}
	}
	return reasons, true
}
