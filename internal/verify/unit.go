package verify

import (
	"bytes"
	"encoding/json"
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

	failedInputs, workingInputs, err := splitInputs(e.Verification.Differential)
	if err != nil {
		return rejected(res, "malformed_inputs", err.Error())
	}

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

// stepsFromAny normalizes the schema's `setup` (always array) and
// `action` (single | array | block) shapes into []runner.Step. The
// action_steps_block shape (`{steps: [...]}`) is rejected as not yet
// implemented — flagged distinctly so submitters know it isn't their bug.
func stepsFromAny(v any) ([]runner.Step, error) {
	if v == nil {
		return nil, nil
	}
	switch t := v.(type) {
	case []any:
		out := make([]runner.Step, 0, len(t))
		for i, item := range t {
			m, ok := item.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("step %d is not a mapping (got %T)", i, item)
			}
			out = append(out, stepFromMap(m))
		}
		return out, nil
	case map[string]any:
		if _, hasSteps := t["steps"]; hasSteps {
			return nil, errors.New("action_steps_block shape (steps:) not yet implemented in v0.1")
		}
		return []runner.Step{stepFromMap(t)}, nil
	default:
		return nil, fmt.Errorf("setup/action has unexpected shape %T", v)
	}
}

func stepFromMap(m map[string]any) runner.Step {
	s := runner.Step{}
	if v, ok := m["type"].(string); ok {
		s.Type = v
	}
	if v, ok := m["lang"].(string); ok {
		s.Lang = v
	}
	if v, ok := m["body"].(string); ok {
		s.Body = v
	}
	return s
}

// splitInputs extracts the per-branch inputs from differential.inputs.
// The schema permits either a per-branch object (with failed_approach /
// working_approach keys) or a shared object (any other keys, applied to
// both branches identically).
func splitInputs(diff map[string]any) (failed, working map[string]any, err error) {
	raw, ok := diff["inputs"]
	if !ok {
		return nil, nil, nil
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, nil, fmt.Errorf("differential.inputs is not a mapping (got %T)", raw)
	}
	_, hasF := m["failed_approach"]
	_, hasW := m["working_approach"]
	if hasF || hasW {
		failed, _ = m["failed_approach"].(map[string]any)
		working, _ = m["working_approach"].(map[string]any)
		return failed, working, nil
	}
	return m, m, nil
}

// matchOutcome compares one branch's ExecResult against the relevant
// differential keys. Returns the (possibly empty) list of mismatch reasons.
func matchOutcome(branch string, got runner.ExecResult, diff map[string]any, retKey, raiseKey string) []Reason {
	retSpec, hasRet := diff[retKey]
	raiseSpec, hasRaise := diff[raiseKey]

	if !hasRet && !hasRaise {
		// No expectation declared for this branch — accept silently.
		// failed_branch_must_fail_with is a separate spec key, deferred.
		return nil
	}

	if got.Raised {
		if !hasRaise {
			return []Reason{{
				Code: "unexpected_exception",
				Message: fmt.Sprintf("%s raised %s (%s) but spec required a return value",
					branch, got.Exception, got.Message),
			}}
		}
		want, err := exceptionFromSpec(raiseSpec)
		if err != nil {
			return []Reason{{Code: "malformed_raise_spec", Message: err.Error()}}
		}
		if want != "" && want != got.Exception {
			return []Reason{{
				Code: "wrong_exception",
				Message: fmt.Sprintf("%s raised %s, spec required %s",
					branch, got.Exception, want),
			}}
		}
		return nil
	}

	if !hasRet {
		return []Reason{{
			Code: "unexpected_return",
			Message: fmt.Sprintf("%s returned %s value, spec required a raised exception",
				branch, got.TypeName),
		}}
	}
	return matchReturnSpec(branch, got, retSpec)
}

// matchReturnSpec compares ExecResult against a {type, value_equals} spec.
// The schema's open-ended `failed_branch_must_return: {}` allows other
// shapes; v0.1 supports {type, value_equals} and rejects others with a
// distinct reason so submitters know to wait for the next slice.
func matchReturnSpec(branch string, got runner.ExecResult, spec any) []Reason {
	sm, ok := spec.(map[string]any)
	if !ok {
		return []Reason{{
			Code: "unsupported_return_spec",
			Message: fmt.Sprintf("%s_branch_must_return: only {type, value_equals} shape is supported in v0.1, got %T",
				branch, spec),
		}}
	}

	var out []Reason

	if specType, ok := sm["type"].(string); ok && specType != got.TypeName {
		out = append(out, Reason{
			Code: "wrong_return_type",
			Message: fmt.Sprintf("%s returned type %s, spec required %s",
				branch, got.TypeName, specType),
		})
	}

	if val, hasVal := sm["value_equals"]; hasVal {
		if !got.Serializable {
			out = append(out, Reason{
				Code: "non_serializable_return",
				Message: fmt.Sprintf("%s returned non-serializable value (repr=%s); cannot compare to value_equals",
					branch, got.Repr),
			})
			return out
		}
		wantBytes, err := json.Marshal(val)
		if err != nil {
			out = append(out, Reason{
				Code:    "malformed_value_equals",
				Message: fmt.Sprintf("%s_branch_must_return.value_equals: %v", branch, err),
			})
			return out
		}
		gotCanonical, err := canonicalizeJSON(got.JSONValue)
		if err != nil {
			out = append(out, Reason{
				Code:    "branch_runner_error",
				Message: fmt.Sprintf("%s: canonicalize return value: %v", branch, err),
			})
			return out
		}
		if !bytes.Equal(wantBytes, gotCanonical) {
			out = append(out, Reason{
				Code: "wrong_return_value",
				Message: fmt.Sprintf("%s returned %s, spec required %s",
					branch, string(gotCanonical), string(wantBytes)),
			})
		}
	}

	return out
}

// canonicalizeJSON re-marshals raw through Go's encoder so that comparisons
// don't fail on whitespace differences between Python's json.dumps and
// Go's encoding/json (e.g., Python emits "[1, 2]", Go emits "[1,2]").
func canonicalizeJSON(raw json.RawMessage) ([]byte, error) {
	if len(raw) == 0 {
		return []byte("null"), nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

// exceptionFromSpec extracts the expected exception class name from a
// failed_branch_must_raise / working_branch_must_raise spec. Accepts a
// bare string (class name) or {type|class: <name>} object.
func exceptionFromSpec(spec any) (string, error) {
	switch v := spec.(type) {
	case nil:
		return "", nil
	case string:
		return v, nil
	case map[string]any:
		if t, ok := v["type"].(string); ok {
			return t, nil
		}
		if c, ok := v["class"].(string); ok {
			return c, nil
		}
		return "", errors.New("raise spec object missing type/class field")
	default:
		return "", fmt.Errorf("raise spec has unsupported shape %T", spec)
	}
}
