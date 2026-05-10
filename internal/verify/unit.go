package verify

import (
	"errors"
	"fmt"
	"os"

	"github.com/runlog-org/runlog-verifier/internal/verify/runner"
)

// unitSubprocessSupportedTools enumerates the verification.runtime.tool values
// the unit-tier subprocess path can drive. Mirrors reexecuteSupportedTools but
// scoped to unit-tier so adding a tool at one tier doesn't silently flip it on
// at the other. Anything outside this set surfaces `runtime_unsupported` so
// authors know the tool is recognised but unimplemented.
var unitSubprocessSupportedTools = map[string]bool{
	"shell":  true,
	"sqlite": true,
}

// schemaIsolations is the full enum from schema/entry.schema.yaml's
// verification.isolation field. The dispatcher uses it to distinguish
// "schema-recognised but unimplemented in this build" (→ isolation_unsupported)
// from "not in the schema at all" (→ isolation_unknown). Keeping the list
// hard-coded next to the dispatcher means adding a schema value forces a
// touch here — the schema's enum definition stays the single source of
// truth, and this list is the dispatcher's cached view of it.
var schemaIsolations = map[string]bool{
	"function":      true,
	"subprocess":    true,
	"compiler":      true,
	"database":      true,
	"http_client":   true,
	"docker_daemon": true,
}

// runUnit handles tier == "unit". The schema's verification.isolation
// field selects the driver; today only "function" resolves to a real
// driver (Python-in-subprocess via runner.PythonDriver). Other values
// declared by the schema are recognised and degrade to tier_unsupported
// with isolation_unsupported so the submitter knows the entry is well-
// formed but waiting on driver work; values not in the schema enum at
// all degrade to tier_unsupported with isolation_unknown so the
// submitter knows it's an authoring bug.
func runUnit(e *Entry) Result {
	res := Result{UnitID: e.UnitID, Tier: "unit"}

	// Default to function when the field is empty — preserves the
	// pre-dispatcher behaviour for entries that omit the field. The
	// schema's allOf branch makes isolation required for type: unit,
	// so this fallback is mostly defensive (CLI-path entries without
	// upstream JSON-schema validation could still arrive empty).
	iso := e.Verification.Isolation
	if iso == "" {
		iso = "function"
	}

	// subprocess isolation is dispatched ad-hoc rather than through
	// runner.DriverFor: SubprocessDriver is stateful (per-branch
	// Workdir), so a singleton-style registry doesn't fit. Mirrors the
	// reexecute-tier pattern in reexecute.go.
	if iso == "subprocess" {
		return runUnitSubprocess(e)
	}

	driver, registered := runner.DriverFor(iso)
	if !registered {
		if schemaIsolations[iso] {
			return tierUnsupported(res, "isolation_unsupported", fmt.Sprintf(
				"isolation %q is recognised by the schema but not implemented "+
					"in this verifier build — function isolation ships first; "+
					"subprocess / compiler / database / http_client / "+
					"docker_daemon land in follow-up commits",
				iso,
			))
		}
		return tierUnsupported(res, "isolation_unknown", fmt.Sprintf(
			"isolation %q is not in the schema enum — accepted values are "+
				"function, subprocess, compiler, database, http_client, "+
				"docker_daemon",
			iso,
		))
	}

	prep, reason := prepareBranches(e, true)
	if reason != nil {
		return rejectedReasons(res, []Reason{*reason})
	}

	if r := validateTimeoutSeconds(e); r != nil {
		return rejectedReasons(res, []Reason{*r})
	}

	timeout := e.Verification.TimeoutSeconds

	failedRes, err := driver.Run(prep.FailedSetup, prep.FailedAction, prep.FailedInputs, timeout)
	if err != nil {
		return runnerError(res, "failed_approach", err)
	}
	workingRes, err := driver.Run(prep.WorkingSetup, prep.WorkingAction, prep.WorkingInputs, timeout)
	if err != nil {
		return runnerError(res, "working_approach", err)
	}

	var reasons []Reason
	reasons = append(reasons, matchOutcome(branchFailed, failedRes, e.Verification.Differential)...)
	reasons = append(reasons, matchOutcome(branchWorking, workingRes, e.Verification.Differential)...)
	reasons = append(reasons, matchActionPlanNodeTiming("failed_approach", e.FailedApproach.Assertion, failedRes)...)
	reasons = append(reasons, matchActionPlanNodeTiming("working_approach", e.WorkingApproach.Assertion, workingRes)...)
	reasons = append(reasons, matchActionOutputPattern("failed_approach", e.FailedApproach.Assertion, failedRes)...)
	reasons = append(reasons, matchActionOutputPattern("working_approach", e.WorkingApproach.Assertion, workingRes)...)

	if len(reasons) > 0 {
		return rejectedReasons(res, reasons)
	}

	baseline := mutationBaseline{
		Failed: branchBaseline{
			Setup:  prep.FailedSetup,
			Action: prep.FailedAction,
			Inputs: prep.FailedInputs,
			Result: failedRes,
			Driver: driver,
		},
		Working: branchBaseline{
			Setup:  prep.WorkingSetup,
			Action: prep.WorkingAction,
			Inputs: prep.WorkingInputs,
			Result: workingRes,
			Driver: driver,
		},
		Diff:    e.Verification.Differential,
		Timeout: timeout,
	}
	mutReasons, supported := runMutations(e, baseline)
	if !supported {
		return tierUnsupportedReasons(res, mutReasons)
	}
	if len(mutReasons) > 0 {
		return rejectedReasons(res, mutReasons)
	}

	res.Status = "verified"
	return res
}

// rejected fills res with a single rejected reason and returns it.
func rejected(res Result, code, message string) Result {
	return rejectedReasons(res, []Reason{{Code: code, Message: message}})
}

// rejectedReasons fills res with a pre-built list of rejection reasons and
// returns it. Multi-reason callers (every tier orchestrator after a
// prepareBranches / matchOutcome / mutation aggregation) shared the same
// `res.Status = "rejected"; res.Reasons = X; return res` triple inline; this
// helper centralises that triple so the rejected() singleton case and the
// multi-reason case use the same status string in exactly one place.
func rejectedReasons(res Result, reasons []Reason) Result {
	res.Status = "rejected"
	res.Reasons = reasons
	return res
}

// tierUnsupported fills res with a single tier_unsupported reason and returns
// it. Mirrors the rejected() helper for the tier_unsupported status, which is
// the verifier's "well-formed but not implemented in this build" outcome —
// distinct from rejected (entry violated a check) and verified (passed). Used
// by every tier orchestrator (unit/integration replay/integration reexecute)
// when a schema-recognised value (isolation, runtime tool, tier) has no driver
// in this build.
func tierUnsupported(res Result, code, message string) Result {
	return tierUnsupportedReasons(res, []Reason{{Code: code, Message: message}})
}

// tierUnsupportedReasons mirrors rejectedReasons for the tier_unsupported
// status: the unsupported-strategy branch in each tier's mutation orchestrator
// surfaces a list of mutation reasons (because iterateMutations may have
// already aggregated reasons from earlier mutations before short-circuiting on
// supported=false). Keeps the status string in one place.
func tierUnsupportedReasons(res Result, reasons []Reason) Result {
	res.Status = "tier_unsupported"
	res.Reasons = reasons
	return res
}

// isEnvErr reports whether err is an environmental failure (timeout, missing
// interpreter) that should surface as `mutation_runner_error` rather than be
// reclassified as outcomeFail. Shared by every per-tier mutation runner
// (unit, integration replay, integration reexecute) so the env-vs-real-fail
// boundary stays in one place.
func isEnvErr(err error) bool {
	return errors.Is(err, runner.ErrTimeout) || errors.Is(err, runner.ErrInterpreterMissing)
}

// runnerError maps a runner error to a verifier outcome. Interpreter or
// language unsupported degrade to tier_unsupported (the submitter cannot
// fix the entry to verify on this host); other errors are rejection.
func runnerError(res Result, branch string, err error) Result {
	switch {
	case errors.Is(err, runner.ErrInterpreterMissing):
		return tierUnsupported(res, "runtime_unavailable",
			fmt.Sprintf("python3 is not installed on the verifier host (running %s): %v", branch, err))
	case errors.Is(err, runner.ErrLanguageUnsupported):
		return tierUnsupported(res, "language_not_yet_implemented",
			fmt.Sprintf("%s: %v", branch, err))
	case errors.Is(err, runner.ErrTimeout):
		return rejected(res, "branch_timeout", fmt.Sprintf("%s: %v", branch, err))
	default:
		return rejected(res, "branch_runner_error", fmt.Sprintf("%s: %v", branch, err))
	}
}

// runUnitSubprocess handles tier == "unit" + isolation == "subprocess". The
// shape mirrors runReexecute (per-branch tmpdir + SubprocessDriver + per-
// mutation fresh sandbox) but without the cassette: unit-tier subprocess has
// no setup_script / teardown_script / step matching, just the branch's typed
// setup+action steps run in a workdir-rooted sandbox.
//
// This is the path Godot/Node/Ruby/etc. take — no language-specific Driver
// implementation is needed because the host CLI ("godot --headless --script
// foo.gd", "node -e", "ruby -e", …) is just a shell invocation, and
// SubprocessDriver with Tool=shell already executes those.
func runUnitSubprocess(e *Entry) Result {
	res := Result{UnitID: e.UnitID, Tier: "unit"}

	if e.Verification.Runtime == nil || e.Verification.Runtime.Tool == "" {
		return rejected(res, "verification_runtime_missing",
			"verification.runtime.tool is required when isolation: subprocess "+
				"(names which CLI to drive: shell, sqlite, ...)")
	}
	tool := e.Verification.Runtime.Tool
	if !unitSubprocessSupportedTools[tool] {
		return tierUnsupported(res, "runtime_unsupported", fmt.Sprintf(
			"verification.runtime.tool %q is not implemented in this verifier "+
				"build at unit-tier — shell + sqlite ship first; postgres / "+
				"redis / git / docker land in follow-up commits",
			tool,
		))
	}

	// path-extract is a no-op at subprocess unit-tier: SubprocessDriver
	// returns last-step stdout as a string-typed $RESULT, so dotted-key
	// dict path extraction does not apply.
	prep, prepReason := prepareBranches(e, false)
	if prepReason != nil {
		return rejectedReasons(res, []Reason{*prepReason})
	}

	if r := validateTimeoutSeconds(e); r != nil {
		return rejectedReasons(res, []Reason{*r})
	}
	timeout := e.Verification.TimeoutSeconds

	failedRes, failedDriver, failedReason, _ := runUnitSubprocessBranch(
		"failed_approach", tool, prep.FailedSetup, prep.FailedAction, prep.FailedInputs, timeout)
	if failedReason != nil {
		return rejectedReasons(res, []Reason{*failedReason})
	}
	defer cleanupUnitSubprocessSandbox(failedDriver)

	workingRes, workingDriver, workingReason, _ := runUnitSubprocessBranch(
		"working_approach", tool, prep.WorkingSetup, prep.WorkingAction, prep.WorkingInputs, timeout)
	if workingReason != nil {
		return rejectedReasons(res, []Reason{*workingReason})
	}
	defer cleanupUnitSubprocessSandbox(workingDriver)

	var reasons []Reason
	reasons = append(reasons, matchOutcome(branchFailed, failedRes, e.Verification.Differential)...)
	reasons = append(reasons, matchOutcome(branchWorking, workingRes, e.Verification.Differential)...)
	reasons = append(reasons, matchActionPlanNodeTiming("failed_approach", e.FailedApproach.Assertion, failedRes)...)
	reasons = append(reasons, matchActionPlanNodeTiming("working_approach", e.WorkingApproach.Assertion, workingRes)...)
	reasons = append(reasons, matchActionOutputPattern("failed_approach", e.FailedApproach.Assertion, failedRes)...)
	reasons = append(reasons, matchActionOutputPattern("working_approach", e.WorkingApproach.Assertion, workingRes)...)
	if len(reasons) > 0 {
		return rejectedReasons(res, reasons)
	}

	baseline := mutationBaseline{
		Failed: branchBaseline{
			Setup:  prep.FailedSetup,
			Action: prep.FailedAction,
			Inputs: prep.FailedInputs,
			Result: failedRes,
			Driver: failedDriver,
		},
		Working: branchBaseline{
			Setup:  prep.WorkingSetup,
			Action: prep.WorkingAction,
			Inputs: prep.WorkingInputs,
			Result: workingRes,
			Driver: workingDriver,
		},
		Diff:    e.Verification.Differential,
		Timeout: timeout,
	}

	mutReasons, supported := runUnitSubprocessMutations(e, baseline, tool)
	if !supported {
		return tierUnsupportedReasons(res, mutReasons)
	}
	if len(mutReasons) > 0 {
		return rejectedReasons(res, mutReasons)
	}

	res.Status = "verified"
	return res
}

// runUnitSubprocessBranch allocates a fresh tmpdir, constructs a
// SubprocessDriver pointed at it, and runs the branch's setup+action.
// Mirrors runReexecuteBranch but without the cassette setup_script step.
// Returns the action's ExecResult, the driver instance (so the caller can
// defer the sandbox cleanup), a non-nil Reason when the run failed, and
// the underlying runner sentinel error when relevant.
func runUnitSubprocessBranch(branchName, tool string, setup, action []runner.Step, inputs map[string]any, timeout float64) (runner.ExecResult, runner.SubprocessDriver, *Reason, error) {
	workdir, err := os.MkdirTemp("", "runlog-unit-")
	if err != nil {
		r := Reason{
			Code:    "sandbox_alloc_failed",
			Message: fmt.Sprintf("%s: %v", branchName, err),
		}
		return runner.ExecResult{}, runner.SubprocessDriver{}, &r, nil
	}
	driver := runner.SubprocessDriver{Tool: tool, Workdir: workdir}

	res, err := driver.Run(setup, action, inputs, timeout)
	if err != nil {
		_ = os.RemoveAll(workdir)
		code := unitSubprocessRunErrorCode(err, "branch_runner_error")
		r := Reason{Code: code, Message: fmt.Sprintf("%s: %v", branchName, err)}
		return runner.ExecResult{}, driver, &r, err
	}
	return res, driver, nil, nil
}

// cleanupUnitSubprocessSandbox os.RemoveAll's the workdir. Safe to call with a
// zero-value SubprocessDriver — the os.RemoveAll on an empty path is a no-op.
// No teardown_script: unit-tier subprocess has no cassette.
func cleanupUnitSubprocessSandbox(driver runner.SubprocessDriver) {
	if driver.Workdir == "" {
		return
	}
	_ = os.RemoveAll(driver.Workdir)
}

// unitSubprocessRunErrorCode maps a SubprocessDriver error to the Reason.Code
// unit-tier subprocess emits. Mirrors reexecuteRunErrorCode but uses
// `input_invalid_name` (no cassette to scope the error to) and reuses the rest
// of the error→code mapping wholesale so behaviour stays uniform across tiers.
func unitSubprocessRunErrorCode(err error, defaultCode string) string {
	switch {
	case errors.Is(err, runner.ErrInputInvalidName):
		return "input_invalid_name"
	case errors.Is(err, runner.ErrInterpreterMissing):
		return "runtime_unavailable"
	case errors.Is(err, runner.ErrTimeout):
		return "branch_timeout"
	case errors.Is(err, runner.ErrLanguageUnsupported), errors.Is(err, runner.ErrSubprocessTool):
		return "language_not_yet_implemented"
	}
	return defaultCode
}

// runUnitSubprocessMutations applies each declared mutation in a fresh
// per-mutation tmpdir and verifies the outcome matches the declared
// expectation. Cassette-response mutations are unsupported (no HTTP responses
// at unit-tier subprocess). Mirrors runReexecuteMutations.
func runUnitSubprocessMutations(e *Entry, b mutationBaseline, tool string) ([]Reason, bool) {
	return iterateMutations(e, func(m Mutation, i int) ([]Reason, bool) {
		return runOneUnitSubprocessMutation(b, m, i, tool)
	})
}

// runOneUnitSubprocessMutation mirrors runOneReexecuteMutation but with no
// cassette setup_script to re-apply per-mutation: each mutation just gets a
// fresh tmpdir, the branch's typed setup steps run inside the SubprocessDriver
// before the mutated action.
func runOneUnitSubprocessMutation(b mutationBaseline, m Mutation, idx int, tool string) ([]Reason, bool) {
	if isCassetteResponseStrategy(m.Strategy) {
		return []Reason{{
			Code: "mutation_strategy_unsupported",
			Message: fmt.Sprintf(
				"mutation #%d strategy %q targets a cassette response, but unit-tier "+
					"subprocess has no HTTP responses to perturb. Use mutate_fixture / "+
					"set_literal_value / swap_* / remove_kwarg / drop_flag at this tier",
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
			return []Reason{mutationTargetInvalidReason(idx, m, branch, err)}
		}

		// Per-mutation fresh sandbox. Wrapped in a closure so cleanup runs
		// deterministically at the end of *this* mutation rather than
		// accumulating across iterations. Mirrors runOneReexecuteMutation.
		return func() []Reason {
			got, mutDriver, runReason, runErr := runUnitSubprocessBranch(
				fmt.Sprintf("mutation #%d (%s) on %s", idx+1, m.Strategy, branch),
				tool, baseline.Setup, mutAction, mutInputs, b.Timeout)
			defer cleanupUnitSubprocessSandbox(mutDriver)

			if runReason != nil {
				if runErr != nil && isEnvErr(runErr) {
					return []Reason{mutationRunnerErrorReasonMsg(runReason.Message)}
				}
				got = synthesizeMutationCrashMessage(runReason.Message)
			}

			actual := classifyOutcome(branch, got, baseline.Result, b.Diff)
			if actual == expected {
				return nil
			}
			if expected == outcomeFail &&
				actual == outcomeUnchanged &&
				discriminatingStrategies[m.Strategy] &&
				!stepBodiesEqual(baseline.Action, mutAction) {
				return []Reason{mutationDidNotDiscriminateReason(idx, m, branch, "source")}
			}
			return []Reason{mutationOutcomeMismatchReason(idx, m, branch, expected, actual)}
		}()
	})
	return reasons, true
}
