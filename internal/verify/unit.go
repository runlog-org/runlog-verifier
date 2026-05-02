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
