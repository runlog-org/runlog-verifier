// Package runner executes the user code declared in a runlog entry's
// failed_approach / working_approach branches, in a subprocess. The MVP
// supports Python only; other languages or step types return a typed
// error so the unit-tier check can degrade to tier_unsupported with a
// specific reason.
//
// Verification happens on the submitter's host (CLAUDE.md load-bearing
// invariant #6), so this package does not sandbox — it relies on the
// host's process boundary, the schema-bounded timeout_seconds, and a
// driver wrapper that captures both successful returns and raised
// exceptions in a structured JSON outcome.
package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Step mirrors the relevant fields of the schema's step_fragment. The
// runner reads only type, lang, and body; additional schema fields are
// ignored.
type Step struct {
	Type string `yaml:"type" json:"type"`
	Lang string `yaml:"lang" json:"lang"`
	Body string `yaml:"body" json:"body"`
}

// ExecResult is the structured outcome of running one branch's action.
// Either Raised is true (and Exception/Message describe the exception)
// or Raised is false (and TypeName/JSONValue/Repr describe the bound
// $RESULT). JSONValue is null when $RESULT is not JSON-serializable —
// Serializable disambiguates that from a bound $RESULT == None.
type ExecResult struct {
	Raised       bool            `json:"raised"`
	Exception    string          `json:"exception,omitempty"`
	Message      string          `json:"message,omitempty"`
	TypeName     string          `json:"type,omitempty"`
	JSONValue    json.RawMessage `json:"json_value,omitempty"`
	Serializable bool            `json:"json_serializable,omitempty"`
	Repr         string          `json:"repr,omitempty"`
	Length       *int            `json:"length,omitempty"`
	ElementTypes []string        `json:"element_types,omitempty"`
}

// Errors returned by RunPython. Callers map these to verify.Reason codes
// or to tier_unsupported, depending on the cause.
var (
	ErrLanguageUnsupported = errors.New("runner: language not supported")
	ErrInterpreterMissing  = errors.New("runner: interpreter not found on PATH")
	ErrTimeout             = errors.New("runner: subprocess timed out")
	ErrDriverOutput        = errors.New("runner: driver output not parseable")
	ErrEmptyAction         = errors.New("runner: action contains no steps")
)

// varRef matches `$VARNAME` references inside step bodies. The mangled
// form `_v_VARNAME` is a valid Python identifier.
var varRef = regexp.MustCompile(`\$([A-Z][A-Z0-9_]*)`)

// RunPython runs setup+action as a Python 3 subprocess with inputs bound
// as locals. Each `$VARNAME` reference inside step bodies is rewritten to
// `_v_VARNAME` and the same name (without the `$`) is bound from inputs
// via json.loads of a JSON string literal embedded in the driver. Returns
// the structured outcome (success or raised exception).
//
// inputs keys may be supplied with or without the `$` prefix; the prefix
// is stripped before mangling.
func RunPython(setup, action []Step, inputs map[string]any, timeoutSec float64) (ExecResult, error) {
	if len(action) == 0 {
		return ExecResult{}, ErrEmptyAction
	}
	combined := make([]Step, 0, len(setup)+len(action))
	combined = append(combined, setup...)
	combined = append(combined, action...)
	for _, s := range combined {
		if s.Type != "" && s.Type != "code" {
			return ExecResult{}, fmt.Errorf("%w: step type %q (only \"code\" is supported)",
				ErrLanguageUnsupported, s.Type)
		}
		if s.Lang != "" && s.Lang != "python" && s.Lang != "python3" {
			return ExecResult{}, fmt.Errorf("%w: lang %q", ErrLanguageUnsupported, s.Lang)
		}
	}

	script, err := buildPythonScript(setup, action, inputs)
	if err != nil {
		return ExecResult{}, err
	}

	timeout := time.Duration(timeoutSec * float64(time.Second))
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "python3", "-")
	cmd.Stdin = strings.NewReader(script)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return ExecResult{}, fmt.Errorf("%w after %s", ErrTimeout, timeout)
	}
	if runErr != nil {
		var execErr *exec.Error
		if errors.As(runErr, &execErr) && errors.Is(execErr.Err, exec.ErrNotFound) {
			return ExecResult{}, fmt.Errorf("%w: %v", ErrInterpreterMissing, runErr)
		}
		return ExecResult{}, fmt.Errorf("python subprocess: %w (stderr=%s)", runErr, stderr.String())
	}

	var res ExecResult
	if err := json.Unmarshal(stdout.Bytes(), &res); err != nil {
		return ExecResult{}, fmt.Errorf("%w: %v (stdout=%q, stderr=%q)",
			ErrDriverOutput, err, stdout.String(), stderr.String())
	}
	return res, nil
}

// buildPythonScript composes the driver script. Determinism: input
// variables are sorted by name so two equal entries produce byte-identical
// scripts. The capture block always emits exactly one JSON object to stdout.
func buildPythonScript(setup, action []Step, inputs map[string]any) (string, error) {
	var b strings.Builder
	b.WriteString("import json, sys\n\n")

	if len(inputs) > 0 {
		b.WriteString("# --- inputs ---\n")
		keys := make([]string, 0, len(inputs))
		for k := range inputs {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			name := strings.TrimPrefix(k, "$")
			val := inputs[k]
			if expr, ok := pythonExprFromValue(val); ok {
				fmt.Fprintf(&b, "_v_%s = (%s)\n", name, expr)
				continue
			}
			valBytes, err := json.Marshal(val)
			if err != nil {
				return "", fmt.Errorf("buildPythonScript: marshal input %q: %w", k, err)
			}
			// Re-encoding `valBytes` (already a JSON value) as a JSON string
			// yields a Python-valid double-quoted string literal that
			// json.loads can decode back to the original value at runtime.
			pyLit, err := json.Marshal(string(valBytes))
			if err != nil {
				return "", fmt.Errorf("buildPythonScript: re-encode input %q: %w", k, err)
			}
			fmt.Fprintf(&b, "_v_%s = json.loads(%s)\n", name, string(pyLit))
		}
		b.WriteString("\n")
	}

	if len(setup) > 0 {
		b.WriteString("# --- setup ---\n")
		for _, s := range setup {
			body := strings.TrimRight(mangleVars(s.Body), "\n")
			if body == "" {
				continue
			}
			b.WriteString(body)
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	b.WriteString("# --- action ---\n")
	b.WriteString("try:\n")
	wroteAction := false
	for _, s := range action {
		body := strings.TrimRight(mangleVars(s.Body), "\n")
		if body == "" {
			continue
		}
		for _, line := range strings.Split(body, "\n") {
			b.WriteString("    " + line + "\n")
			wroteAction = true
		}
	}
	if !wroteAction {
		b.WriteString("    pass\n")
	}
	b.WriteString("except BaseException as _exc:\n")
	b.WriteString(`    sys.stdout.write(json.dumps({"raised": True, "exception": type(_exc).__name__, "message": str(_exc)}))` + "\n")
	b.WriteString("    sys.exit(0)\n\n")

	b.WriteString(`# --- capture ---
try:
    _v_RESULT
except NameError:
    sys.stdout.write(json.dumps({"raised": True, "exception": "NameError", "message": "$RESULT was never bound by the action"}))
    sys.exit(0)
try:
    json.dumps(_v_RESULT)
    _ok = True
except Exception:
    _ok = False
_length = None
try:
    _length = len(_v_RESULT)
except Exception:
    pass

_element_types = None
# Iterate only true sequences/sets — exclude strings (would yield char types)
# and mappings (would yield key types). Bytes/bytearray excluded for the same
# reason as strings.
if not isinstance(_v_RESULT, (str, bytes, bytearray, dict)):
    try:
        _element_types = [type(x).__name__ for x in _v_RESULT]
    except Exception:
        _element_types = None

_outcome = {
    "raised": False,
    "type": type(_v_RESULT).__name__,
    "json_value": _v_RESULT if _ok else None,
    "json_serializable": _ok,
    "repr": repr(_v_RESULT),
}
if _length is not None:
    _outcome["length"] = _length
if _element_types is not None:
    _outcome["element_types"] = _element_types
sys.stdout.write(json.dumps(_outcome))
`)

	return b.String(), nil
}

// mangleVars rewrites `$VARNAME` → `_v_VARNAME` so step bodies are valid
// Python (`$` is not an identifier character in Python).
func mangleVars(src string) string {
	return varRef.ReplaceAllString(src, "_v_$1")
}

// pythonExprFromValue recognizes the opt-in `{python_expr: "<expr>"}` input
// shape. Returns (expr, true) when val is a map containing exactly that single
// key with a string value; ("", false) otherwise so the caller falls back to
// JSON binding.
//
// The shape is opt-in by design — bare strings stay literal so YAML payloads
// like 'permissions: 022' aren't accidentally evaluated.
func pythonExprFromValue(val any) (string, bool) {
	m, ok := val.(map[string]any)
	if !ok {
		return "", false
	}
	if len(m) != 1 {
		return "", false
	}
	raw, ok := m["python_expr"]
	if !ok {
		return "", false
	}
	expr, ok := raw.(string)
	if !ok {
		return "", false
	}
	return expr, true
}
