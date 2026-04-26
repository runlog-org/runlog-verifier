package verify

import (
	"errors"
	"fmt"
	"strings"

	"github.com/runlog/verifier/internal/verify/runner"
)

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
