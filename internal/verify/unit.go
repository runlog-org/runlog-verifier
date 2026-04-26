package verify

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

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
		failedSetup:   failedSetup,
		failedAction:  failedAction,
		workingSetup:  workingSetup,
		workingAction: workingAction,
		failedInputs:  failedInputs,
		workingInputs: workingInputs,
		failedRes:     failedRes,
		workingRes:    workingRes,
		diff:          e.Verification.Differential,
		timeout:       timeout,
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

// mergeLiterals folds the entry's top-level literals block into a per-branch
// inputs map. Each literal is shaped {$LITERAL_N: {value: V, reason: ..., category: ...}};
// only the `value` field is bound at runtime. Per-branch inputs win on key
// collision — a differential.inputs.{branch}.$LITERAL_N override stays in effect.
//
// Literals without a `value` field are skipped silently (the schema validates
// the shape upstream; we don't redundantly enforce it here).
//
// Returns a new map; the input is not mutated. nil literals → returns inputs
// unchanged (possibly nil).
func mergeLiterals(literals map[string]any, inputs map[string]any) map[string]any {
	if len(literals) == 0 {
		return inputs
	}
	out := make(map[string]any, len(literals)+len(inputs))
	for name, lit := range literals {
		m, ok := lit.(map[string]any)
		if !ok {
			continue
		}
		v, ok := m["value"]
		if !ok {
			continue
		}
		out[name] = v
	}
	// Per-branch inputs override literals on key collision.
	for k, v := range inputs {
		out[k] = v
	}
	return out
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

// returnPathFromDifferential extracts the optional `path:` field from a
// per-branch return spec block. Empty string means no path extraction.
// Errors only when the field is present but malformed (non-string).
func returnPathFromDifferential(diff map[string]any, branchKey string) (string, error) {
	raw, ok := diff[branchKey]
	if !ok {
		return "", nil
	}
	spec, ok := raw.(map[string]any)
	if !ok {
		return "", nil
	}
	p, ok := spec["path"]
	if !ok {
		return "", nil
	}
	s, ok := p.(string)
	if !ok {
		return "", fmt.Errorf("%s.path must be a string, got %T", branchKey, p)
	}
	return s, nil
}

// pathExtractStep builds a runner step that rebinds $RESULT to the value at
// the given dotted dict-key path. v0.1 supports only string-keyed dot paths
// (no numeric indices, no attribute access). Embedded dots in keys aren't
// supported — the path is plain `.`-split.
//
// Example: path = "a.b" -> body = "$RESULT = $RESULT['a']['b']"
//
// The step runs inside the action's try/except, so a missing key raises
// KeyError and is captured as a real exception by the existing handler.
func pathExtractStep(path string) runner.Step {
	var sb strings.Builder
	sb.WriteString("$RESULT = $RESULT")
	for _, key := range strings.Split(path, ".") {
		// Single-quoted Python string — escape backslashes and single quotes.
		escaped := strings.ReplaceAll(key, `\`, `\\`)
		escaped = strings.ReplaceAll(escaped, "'", `\'`)
		sb.WriteString("['")
		sb.WriteString(escaped)
		sb.WriteString("']")
	}
	return runner.Step{Type: "code", Lang: "python", Body: sb.String()}
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
		wantTypes, wantPattern, err := exceptionFromSpec(raiseSpec)
		if err != nil {
			return []Reason{{Code: "malformed_raise_spec", Message: err.Error()}}
		}
		if len(wantTypes) > 0 {
			matched := false
			for _, t := range wantTypes {
				if t == got.Exception {
					matched = true
					break
				}
			}
			if !matched {
				return []Reason{{
					Code: "wrong_exception",
					Message: fmt.Sprintf("%s raised %s, spec required %s",
						branch, got.Exception, formatExpectedExceptions(wantTypes)),
				}}
			}
		}
		if wantPattern != "" {
			re, err := regexp.Compile(wantPattern)
			if err != nil {
				return []Reason{{
					Code:    "malformed_raise_spec",
					Message: fmt.Sprintf("message_pattern %q is not a valid regex: %v", wantPattern, err),
				}}
			}
			if !re.MatchString(got.Message) {
				return []Reason{{
					Code: "wrong_exception_message",
					Message: fmt.Sprintf("%s raised %s with message %q, spec required message matching /%s/",
						branch, got.Exception, got.Message, wantPattern),
				}}
			}
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

	if want, ok := lengthFromSpec(sm["length"]); ok {
		if got.Length == nil {
			out = append(out, Reason{
				Code: "non_sized_return",
				Message: fmt.Sprintf("%s returned %s value (no len()); cannot compare to length=%d",
					branch, got.TypeName, want),
			})
		} else if *got.Length != want {
			out = append(out, Reason{
				Code: "wrong_return_length",
				Message: fmt.Sprintf("%s returned length=%d, spec required length=%d",
					branch, *got.Length, want),
			})
		}
	}

	if elemType, ok := sm["contains_exception_type"].(string); ok {
		if len(got.ElementTypes) == 0 {
			out = append(out, Reason{
				Code: "non_iterable_return",
				Message: fmt.Sprintf("%s returned %s value (no element types captured); cannot check contains_exception_type=%s",
					branch, got.TypeName, elemType),
			})
		} else {
			found := false
			for _, t := range got.ElementTypes {
				if t == elemType {
					found = true
					break
				}
			}
			if !found {
				out = append(out, Reason{
					Code: "missing_exception_element",
					Message: fmt.Sprintf("%s returned elements %v, spec required at least one of type %s",
						branch, got.ElementTypes, elemType),
				})
			}
		}
	}

	return out
}

// lengthFromSpec extracts an integer length from a YAML-decoded value. yaml.v3
// decodes plain integers as int (Go), but as the exact YAML int kind. Accept
// both int and float64 (defensive — JSON-decoded values come through as float64).
func lengthFromSpec(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		// Reject non-integer floats.
		if n != float64(int(n)) {
			return 0, false
		}
		return int(n), true
	}
	return 0, false
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

// exceptionFromSpec extracts the expected exception class names and optional
// message-pattern regex from a *_must_raise spec. Accepts:
//   - nil → no constraints
//   - bare string → single class name, no message pattern
//   - {type|class|exception: <name>, message_pattern: <regex>} → single class + pattern
//   - {exception_any: [<name>, ...], message_pattern: <regex>} → any-of + pattern
//
// Returns (nil, "", nil) when the spec is nil. Returns (typeNames, pattern, nil)
// otherwise. Empty typeNames means "any exception class is acceptable" — the
// pattern alone constrains.
func exceptionFromSpec(spec any) (typeNames []string, messagePattern string, err error) {
	switch v := spec.(type) {
	case nil:
		return nil, "", nil
	case string:
		return []string{v}, "", nil
	case map[string]any:
		// exception_any takes precedence over single-name keys.
		if anyVal, ok := v["exception_any"]; ok {
			list, listOK := anyVal.([]any)
			if !listOK {
				return nil, "", fmt.Errorf("exception_any must be a list of class names")
			}
			for _, item := range list {
				s, sOK := item.(string)
				if !sOK {
					return nil, "", fmt.Errorf("exception_any entry %v is not a string", item)
				}
				typeNames = append(typeNames, s)
			}
		} else {
			// Single-name keys, in priority order: exception, type, class.
			for _, key := range []string{"exception", "type", "class"} {
				if s, ok := v[key].(string); ok {
					typeNames = []string{s}
					break
				}
			}
		}
		if mp, ok := v["message_pattern"].(string); ok {
			messagePattern = mp
		}
		if len(typeNames) == 0 && messagePattern == "" {
			return nil, "", errors.New("raise spec object missing exception/type/class/exception_any field")
		}
		return typeNames, messagePattern, nil
	default:
		return nil, "", fmt.Errorf("raise spec has unsupported shape %T", spec)
	}
}

// formatExpectedExceptions renders one or more expected exception class names
// for the Reason message — single name as `Foo`, multiple as `any of [A, B]`.
func formatExpectedExceptions(types []string) string {
	if len(types) == 1 {
		return types[0]
	}
	return fmt.Sprintf("any of %v", types)
}
