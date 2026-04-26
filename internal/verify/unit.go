package verify

import (
	"errors"
	"fmt"

	"github.com/runlog/verifier/internal/verify/runner"
)

// runUnit handles tier == "unit". v0.1 supports isolation == "function"
// with python code steps; everything else returns tier_unsupported with a
// specific reason so the submitter knows what to fix or wait for.
func runUnit(e *Entry) Result {
	res := Result{UnitID: e.UnitID, Tier: "unit"}

	iso := e.Verification.Isolation
	if iso != "function" {
		res.Status = "tier_unsupported"
		res.Reasons = []Reason{{
			Code: "isolation_not_yet_implemented",
			Message: fmt.Sprintf(
				"isolation %q is not implemented in this verifier build — "+
					"unit tier ships with isolation: function first; subprocess / "+
					"compiler / database / http_client land in follow-up commits",
				iso,
			),
		}}
		return res
	}

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

	failedPath, err := returnPathFromDifferential(e.Verification.Differential, "failed_branch_must_return")
	if err != nil {
		return rejected(res, "malformed_return_path", err.Error())
	}
	workingPath, err := returnPathFromDifferential(e.Verification.Differential, "working_branch_must_return")
	if err != nil {
		return rejected(res, "malformed_return_path", err.Error())
	}
	if failedPath != "" {
		failedAction = append(failedAction, pathExtractStep(failedPath))
	}
	if workingPath != "" {
		workingAction = append(workingAction, pathExtractStep(workingPath))
	}

	failedInputs, workingInputs, err := splitInputs(e.Verification.Differential)
	if err != nil {
		return rejected(res, "malformed_inputs", err.Error())
	}

	failedInputs = mergeLiterals(e.Literals, failedInputs)
	workingInputs = mergeLiterals(e.Literals, workingInputs)

	timeout := e.Verification.TimeoutSeconds

	failedRes, err := runner.RunPython(failedSetup, failedAction, failedInputs, timeout)
	if err != nil {
		return runnerError(res, "failed_approach", err)
	}
	workingRes, err := runner.RunPython(workingSetup, workingAction, workingInputs, timeout)
	if err != nil {
		return runnerError(res, "working_approach", err)
	}

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

	baseline := mutationBaseline{
		Failed: branchBaseline{
			Setup:  failedSetup,
			Action: failedAction,
			Inputs: failedInputs,
			Result: failedRes,
		},
		Working: branchBaseline{
			Setup:  workingSetup,
			Action: workingAction,
			Inputs: workingInputs,
			Result: workingRes,
		},
		Diff:    e.Verification.Differential,
		Timeout: timeout,
	}
	mutReasons, supported := runMutations(e, baseline)
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

// rejected fills res with a single rejected reason and returns it.
func rejected(res Result, code, message string) Result {
	res.Status = "rejected"
	res.Reasons = []Reason{{Code: code, Message: message}}
	return res
}

// runnerError maps a runner error to a verifier outcome. Interpreter or
// language unsupported degrade to tier_unsupported (the submitter cannot
// fix the entry to verify on this host); other errors are rejection.
func runnerError(res Result, branch string, err error) Result {
	switch {
	case errors.Is(err, runner.ErrInterpreterMissing):
		res.Status = "tier_unsupported"
		res.Reasons = []Reason{{
			Code:    "runtime_unavailable",
			Message: fmt.Sprintf("python3 is not installed on the verifier host (running %s): %v", branch, err),
		}}
	case errors.Is(err, runner.ErrLanguageUnsupported):
		res.Status = "tier_unsupported"
		res.Reasons = []Reason{{
			Code:    "language_not_yet_implemented",
			Message: fmt.Sprintf("%s: %v", branch, err),
		}}
	case errors.Is(err, runner.ErrTimeout):
		res.Status = "rejected"
		res.Reasons = []Reason{{
			Code:    "branch_timeout",
			Message: fmt.Sprintf("%s: %v", branch, err),
		}}
	default:
		res.Status = "rejected"
		res.Reasons = []Reason{{
			Code:    "branch_runner_error",
			Message: fmt.Sprintf("%s: %v", branch, err),
		}}
	}
	return res
}
