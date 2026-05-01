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
// `runtime_unsupported`. The slice deliberately keeps
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
	"path/filepath"

	"github.com/runlog-org/runlog-verifier/internal/verify/cassette"
	"github.com/runlog-org/runlog-verifier/internal/verify/runner"
)

// reexecuteSupportedTools enumerates the cassette.runtime.tool values this
// build can drive. Entries declaring a tool outside this set surface
// `runtime_unsupported` so authors know the feature is recognised
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
		return tierUnsupported(res, "isolation_unsupported", fmt.Sprintf(
			"reexecute-mode isolation %q is not implemented in this verifier "+
				"build — subprocess + database (sqlite) ship first; "+
				"compiler / docker_daemon land in follow-up commits",
			e.Verification.Isolation,
		))
	}

	if cas.Runtime == nil {
		// Defensive: schema's allOf gate enforces this, but the CLI path has
		// no upstream JSON Schema validator, so a hand-crafted entry that
		// omits `runtime` must be caught here rather than silently degrading.
		return rejected(res, "cassette_runtime_missing",
			"cassette.mode: reexecute requires cassette.runtime.tool")
	}
	if !reexecuteSupportedTools[cas.Runtime.Tool] {
		return tierUnsupported(res, "runtime_unsupported", fmt.Sprintf(
			"cassette.runtime.tool %q is not implemented in this verifier "+
				"build — shell + sqlite ship first; postgres / redis / git / "+
				"docker land in follow-up commits",
			cas.Runtime.Tool,
		))
	}

	// ── Branch step shape + inputs ────────────────────────────────────
	// reexecute mode skips path-extract appending: setup/action steps run as
	// shell or sql, and the captured $RESULT is stdout (already a string), so
	// dotted-key dict path extraction does not apply.
	prep, prepReason := prepareBranches(e, false)
	if prepReason != nil {
		return rejectedReasons(res, []Reason{*prepReason})
	}
	failedSetup, failedAction := prep.FailedSetup, prep.FailedAction
	workingSetup, workingAction := prep.WorkingSetup, prep.WorkingAction
	failedInputs, workingInputs := prep.FailedInputs, prep.WorkingInputs

	if r := validateTimeoutSeconds(e); r != nil {
		return rejectedReasons(res, []Reason{*r})
	}

	timeout := e.Verification.TimeoutSeconds

	// ── Failed branch run ─────────────────────────────────────────────
	failedRes, failedDriver, failedReason := runReexecuteBranch(
		"failed_approach", cas, failedSetup, failedAction, failedInputs, timeout)
	if failedReason != nil {
		return rejectedReasons(res, []Reason{*failedReason})
	}
	defer cleanupReexecuteSandbox(failedDriver, cas, failedInputs, timeout)

	// ── Working branch run ────────────────────────────────────────────
	workingRes, workingDriver, workingReason := runReexecuteBranch(
		"working_approach", cas, workingSetup, workingAction, workingInputs, timeout)
	if workingReason != nil {
		return rejectedReasons(res, []Reason{*workingReason})
	}
	defer cleanupReexecuteSandbox(workingDriver, cas, workingInputs, timeout)

	// ── Outcome matching ──────────────────────────────────────────────
	var reasons []Reason
	reasons = append(reasons, matchOutcome(branchFailed, failedRes, e.Verification.Differential)...)
	reasons = append(reasons, matchOutcome(branchWorking, workingRes, e.Verification.Differential)...)
	if len(reasons) > 0 {
		return rejectedReasons(res, reasons)
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
		return tierUnsupportedReasons(res, mutReasons)
	}
	if len(mutReasons) > 0 {
		return rejectedReasons(res, mutReasons)
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
// The sandbox is *not* cleaned up here on success — the caller defers
// cleanupReexecuteSandbox after both branches succeed so that a panic
// mid-run still triggers teardown. On every error return,
// cleanupReexecuteSandbox is called immediately so the teardown sequence
// stays in one place rather than being duplicated at each error site.
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
		cleanupReexecuteSandbox(driver, cas, inputs, timeout)

		code := "setup_script_failed"
		switch {
		case errors.Is(err, runner.ErrInputInvalidName):
			code = "cassette_input_invalid_name"
		case errors.Is(err, runner.ErrInterpreterMissing):
			code = "runtime_unavailable"
		case errors.Is(err, runner.ErrTimeout):
			code = "branch_timeout"
		}
		r := Reason{Code: code, Message: fmt.Sprintf("%s: %v", branchName, err)}
		return runner.ExecResult{}, runner.SubprocessDriver{}, &r
	}

	// db.sqlite Lstat guard: if the cassette's setup_script created a
	// symlink at <workdir>/db.sqlite, subsequent sqlite3 invocations would
	// follow it and write outside the sandbox. Refuse to proceed.
	if cas.Runtime.Tool == "sqlite" {
		dbPath := filepath.Join(workdir, "db.sqlite")
		if info, err := os.Lstat(dbPath); err == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				cleanupReexecuteSandbox(driver, cas, inputs, timeout)
				r := Reason{
					Code: "sandbox_symlink_rejected",
					Message: fmt.Sprintf("%s: db.sqlite is a symlink; refusing to follow it",
						branchName),
				}
				return runner.ExecResult{}, runner.SubprocessDriver{}, &r
			}
		}
	}

	res, err := driver.Run(setup, action, inputs, timeout)
	if err != nil {
		cleanupReexecuteSandbox(driver, cas, inputs, timeout)

		code := "branch_runner_error"
		switch {
		case errors.Is(err, runner.ErrInputInvalidName):
			code = "cassette_input_invalid_name"
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
// them into a non-HTTP cassette. The shared aggregation loop lives in
// iterateMutations (mutate.go); this wrapper just closes over cas.
func runReexecuteMutations(e *Entry, b mutationBaseline, cas *cassette.Cassette) ([]Reason, bool) {
	return iterateMutations(e, func(m Mutation, i int) ([]Reason, bool) {
		return runOneReexecuteMutation(e, b, m, i, cas)
	})
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
		return strategyUnsupportedReason(idx, m.Strategy), false
	}

	reasons := forEachMutationBranch(m, idx, b, func(branch branchKind, baseline branchBaseline, expected mutationOutcome) []Reason {
		mutInputs, mutAction, err := strat.apply(baseline, m)
		if err != nil {
			return []Reason{{
				Code: "mutation_target_invalid",
				Message: fmt.Sprintf("mutation #%d (%s) on %s: %v",
					idx+1, m.Strategy, branch, err),
			}}
		}

		// ── Per-mutation fresh sandbox ────────────────────────────────
		// Wrap the run + classify in a closure so the per-iteration cleanup
		// (cassette teardown_script + os.RemoveAll) runs deterministically at
		// the end of *this* mutation. A function-scope `defer` would
		// accumulate across iterations and keep N tmpdirs + teardown_script
		// processes live until the whole mutation loop returned.
		return func() []Reason {
			got, mutDriver, runReason := runReexecuteBranch(
				fmt.Sprintf("mutation #%d (%s) on %s", idx+1, m.Strategy, branch),
				cas, baseline.Setup, mutAction, mutInputs, b.Timeout)
			defer cleanupReexecuteSandbox(mutDriver, cas, mutInputs, b.Timeout)

			if runReason != nil {
				// Environmental failures (timeout, missing tool) stay as-is;
				// in-band step crashes have already been folded into the
				// ExecResult.Raised path by the driver and won't reach here.
				if runReason.Code == "branch_timeout" || runReason.Code == "runtime_unavailable" {
					return []Reason{{
						Code:    "mutation_runner_error",
						Message: runReason.Message,
					}}
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
			if actual == expected {
				return nil
			}
			if expected == outcomeFail &&
				actual == outcomeUnchanged &&
				discriminatingStrategies[m.Strategy] &&
				!stepBodiesEqual(baseline.Action, mutAction) {
				token, _ := resolveSwapToken(m)
				return []Reason{{
					Code: "mutation_did_not_discriminate",
					Message: fmt.Sprintf(
						"mutation #%d (%s) on %s: rewrote source but produced no behavioural change. "+
							"The token %q was substituted in the action source but the program's observable "+
							"output was byte-identical to the baseline. Pick a token that actually discriminates.",
						idx+1, m.Strategy, branch, token),
				}}
			}
			return []Reason{{
				Code: "mutation_outcome_mismatch",
				Message: fmt.Sprintf("mutation #%d (%s) on %s: expected %s, got %s",
					idx+1, m.Strategy, branch, expected, actual),
			}}
		}()
	})
	return reasons, true
}
