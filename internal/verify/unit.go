package verify

import (
	"errors"
	"fmt"

	"github.com/runlog-org/runlog-verifier/internal/verify/runner"
)

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

	driver, registered := runner.DriverFor(iso)
	if !registered {
		if schemaIsolations[iso] {
			res.Status = "tier_unsupported"
			res.Reasons = []Reason{{
				Code: "isolation_unsupported",
				Message: fmt.Sprintf(
					"isolation %q is recognised by the schema but not implemented "+
						"in this verifier build — function isolation ships first; "+
						"subprocess / compiler / database / http_client / "+
						"docker_daemon land in follow-up commits",
					iso,
				),
			}}
			return res
		}
		res.Status = "tier_unsupported"
		res.Reasons = []Reason{{
			Code: "isolation_unknown",
			Message: fmt.Sprintf(
				"isolation %q is not in the schema enum — accepted values are "+
					"function, subprocess, compiler, database, http_client, "+
					"docker_daemon",
				iso,
			),
		}}
		return res
	}

	prep, reason := prepareBranches(e, true)
	if reason != nil {
		res.Status = "rejected"
		res.Reasons = []Reason{*reason}
		return res
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
