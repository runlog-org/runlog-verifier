package verify

import (
	"errors"
	"fmt"
	"strings"

	"github.com/runlog-org/runlog-verifier/internal/verify/runner"
)

// errActionStepsBlockUnsupported is returned by stepsFromAny when the input is
// the schema's action_steps_block shape (`{steps: [...]}`). The schema
// recognises the shape but v0.1 has no driver for it — callers route this
// through the typed `action_shape_unsupported` Reason code rather than
// letting it be wrapped as `malformed_action`, which would mislead seed
// authors into looking for a parser bug.
var errActionStepsBlockUnsupported = errors.New("action_steps_block shape (steps:) not yet implemented in v0.1")

// stepsFromAny normalizes the schema's `setup` (always array) and
// `action` (single | array | block) shapes into []runner.Step. The
// action_steps_block shape (`{steps: [...]}`) returns
// errActionStepsBlockUnsupported — recognised-but-unimplemented, distinct
// from a true shape error.
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
			return nil, errActionStepsBlockUnsupported
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
// After per-branch overrides land, a single-pass `$KEY → out[$KEY]` resolution
// lets seeds wire chained literal swaps (e.g. `$BASE_IMAGE: $LITERAL_1`);
// deeper chains are out of scope.
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
	// Chained $-token resolution (single pass): if a value is a string of
	// form $KEY and $KEY is itself a key in the merged map, replace the
	// value with what $KEY resolves to. Enables seeds to wire per-branch
	// literal swaps like:
	//   differential.inputs.failed_approach.$BASE_IMAGE: $LITERAL_1
	//   differential.inputs.working_approach.$BASE_IMAGE: $LITERAL_2
	// so $BASE_IMAGE inside a body resolves to the literal block's value
	// (e.g. "debian:12") rather than the raw "$LITERAL_1" string.
	//
	// Single pass only — deeper chains ($A → $B → $C) are out of scope.
	// Self-references ($X → $X) and missing references ($Y where $Y is
	// not in the map) pass through unchanged (substituteVars handles the
	// missing case at substitution time; self-references are no-ops).
	for k, v := range out {
		s, ok := v.(string)
		if !ok {
			continue
		}
		if !strings.HasPrefix(s, "$") {
			continue
		}
		if s == k {
			continue // self-reference; leave alone
		}
		if resolved, present := out[s]; present {
			out[k] = resolved
		}
	}
	return out
}

// splitInputs extracts the per-branch inputs from differential.inputs.
// The schema permits either a per-branch object (with failed_approach /
// working_approach keys) or a shared object (any other keys, applied to
// both branches identically).
//
// When the per-branch shape is detected (either failed_approach or
// working_approach key is present), each present sub-key MUST be a mapping
// — otherwise the type assertion would silently produce nil and the
// branch's literals + per-branch overrides would be lost without a
// diagnostic. The CLI path has no upstream JSON Schema gate, so we surface
// the typed error here as `malformed_inputs` rather than letting a
// hand-crafted entry (e.g. `failed_approach: "foo"`) reach the runner with
// dropped inputs.
func splitInputs(diff map[string]any) (failed, working map[string]any, err error) {
	raw, ok := diff["inputs"]
	if !ok {
		return nil, nil, nil
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, nil, fmt.Errorf("differential.inputs is not a mapping (got %T)", raw)
	}
	rawF, hasF := m["failed_approach"]
	rawW, hasW := m["working_approach"]
	if hasF || hasW {
		if hasF {
			failed, ok = rawF.(map[string]any)
			if !ok {
				return nil, nil, fmt.Errorf("differential.inputs.failed_approach is not a mapping (got %T)", rawF)
			}
		}
		if hasW {
			working, ok = rawW.(map[string]any)
			if !ok {
				return nil, nil, fmt.Errorf("differential.inputs.working_approach is not a mapping (got %T)", rawW)
			}
		}
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

// pythonPathExtractStep builds a runner step that rebinds $RESULT to the value
// at the given dotted dict-key path. v0.1 supports only string-keyed dot paths
// (no numeric indices, no attribute access). Embedded dots in keys aren't
// supported — the path is plain `.`-split.
//
// Python-specific: emits dict-access syntax.
//
// Example: path = "a.b" -> body = "$RESULT = $RESULT['a']['b']"
//
// The step runs inside the action's try/except, so a missing key raises
// KeyError and is captured as a real exception by the existing handler.
func pythonPathExtractStep(path string) runner.Step {
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

// shapeReason wraps a stepsFromAny error into the appropriate Reason. The
// action_steps_block sentinel routes to the recognised-but-unimplemented
// `action_shape_unsupported` code so seed authors don't chase a parser bug;
// every other shape error stays under the per-field `malformed_<field>` code.
// fieldKey is the schema-side selector name without the `malformed_` prefix
// (e.g. "failed_action").
func shapeReason(fieldKey string, err error) *Reason {
	if errors.Is(err, errActionStepsBlockUnsupported) {
		return &Reason{
			Code: "action_shape_unsupported",
			Message: fmt.Sprintf(
				"%s: %v — the action_steps_block shape is recognised by the schema but no driver is "+
					"registered in this verifier build",
				fieldKey, err),
		}
	}
	return &Reason{Code: "malformed_" + fieldKey, Message: err.Error()}
}

// preparedBranches is the typed bundle of per-branch step shapes + inputs that
// every tier orchestrator (unit, integration replay, integration reexecute)
// builds before executing branches. Centralising the construction here
// eliminates the ~30-line copy-paste block that was duplicated across
// runUnit, runIntegration, and runReexecute.
type preparedBranches struct {
	FailedSetup   []runner.Step
	FailedAction  []runner.Step
	WorkingSetup  []runner.Step
	WorkingAction []runner.Step
	FailedInputs  map[string]any
	WorkingInputs map[string]any
}

// prepareBranches normalises both branches' setup/action step shapes,
// optionally appends a path-extract step to each branch's action (skipped when
// appendPathExtract is false — reexecute mode returns stdout strings, not
// Python objects, so dict path extraction does not apply), splits and merges
// inputs against the entry's literals block, and returns either the populated
// preparedBranches or a single rejection Reason naming the first malformed
// field. The Reason matches the legacy code shape so existing tier rejection
// codes (malformed_failed_setup, malformed_working_action, malformed_inputs,
// malformed_return_path) stay stable.
func prepareBranches(e *Entry, appendPathExtract bool) (preparedBranches, *Reason) {
	var p preparedBranches
	var err error

	if p.FailedSetup, err = stepsFromAny(e.FailedApproach.Setup); err != nil {
		return p, shapeReason("failed_setup", err)
	}
	if p.FailedAction, err = stepsFromAny(e.FailedApproach.Action); err != nil {
		return p, shapeReason("failed_action", err)
	}
	if p.WorkingSetup, err = stepsFromAny(e.WorkingApproach.Setup); err != nil {
		return p, shapeReason("working_setup", err)
	}
	if p.WorkingAction, err = stepsFromAny(e.WorkingApproach.Action); err != nil {
		return p, shapeReason("working_action", err)
	}

	if appendPathExtract {
		failedPath, err := returnPathFromDifferential(e.Verification.Differential, "failed_branch_must_return")
		if err != nil {
			return p, &Reason{Code: "malformed_return_path", Message: err.Error()}
		}
		workingPath, err := returnPathFromDifferential(e.Verification.Differential, "working_branch_must_return")
		if err != nil {
			return p, &Reason{Code: "malformed_return_path", Message: err.Error()}
		}
		if failedPath != "" {
			p.FailedAction = append(p.FailedAction, pythonPathExtractStep(failedPath))
		}
		if workingPath != "" {
			p.WorkingAction = append(p.WorkingAction, pythonPathExtractStep(workingPath))
		}
	}

	failedInputs, workingInputs, err := splitInputs(e.Verification.Differential)
	if err != nil {
		return p, &Reason{Code: "malformed_inputs", Message: err.Error()}
	}
	p.FailedInputs = mergeLiterals(e.Literals, failedInputs)
	p.WorkingInputs = mergeLiterals(e.Literals, workingInputs)

	return p, nil
}
