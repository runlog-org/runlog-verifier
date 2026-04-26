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
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"time"
)

//go:embed driver.py.tmpl
var driverTemplate string

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

// asyncStep detects step bodies that need an asyncio.run wrapper. The pattern
// matches Python keywords with word boundaries: a bare `await EXPR` statement,
// an `async with` block, or an `async for` loop. False positives (e.g. the
// substring "await" inside an identifier or string literal) are tolerated —
// the wrapper is correct for sync code too, just slightly more complex.
var asyncStep = regexp.MustCompile(`\b(await|async\s+with|async\s+for)\b`)

// actionIsAsync reports whether any action step body contains async syntax
// requiring an asyncio.run wrapper. Operates on the raw step body before
// $-mangling (mangleVars only rewrites $VARS — async keywords pass through).
func actionIsAsync(action []Step) bool {
	for _, s := range action {
		if asyncStep.MatchString(s.Body) {
			return true
		}
	}
	return false
}

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

// buildPythonScript composes the driver script from driver.py.tmpl by filling
// six explicit placeholders via strings.Replace. Determinism: input variables
// are sorted by name so two equal entries produce byte-identical scripts. The
// capture block (static in the template) always emits exactly one JSON object
// to stdout.
//
// Placeholder contract:
//
//	# {{IMPORTS}}   — extra top-level imports; "import asyncio\n" for async, "\n" for sync
//	# {{INPUTS}}    — per-input binding lines (sorted); empty string when no inputs
//	# {{SETUP}}     — setup step bodies; empty string when no setup
//	# {{ACTION_OPEN}}  — async: "async def _v_main():\n    global _v_RESULT\n"; sync: "try:\n"
//	# {{ACTION_BODY}}  — indented action step lines (always 4-space indented)
//	# {{ACTION_CLOSE}} — async: "\ntry:\n    asyncio.run(_v_main())\nexcept...\n    sys.exit(0)\n\n"
//	                     sync:  "except BaseException as _exc:\n    ...\n    sys.exit(0)\n\n"
func buildPythonScript(setup, action []Step, inputs map[string]any) (string, error) {
	const excHandler = `except BaseException as _exc:
    sys.stdout.write(json.dumps({"raised": True, "exception": type(_exc).__name__, "message": str(_exc)}))
    sys.exit(0)

`

	// --- # {{IMPORTS}} ---
	imports := "\n" // sync: blank line preserving the empty line after "import json, sys"
	if actionIsAsync(action) {
		imports = "import asyncio\n"
	}

	// --- # {{INPUTS}} ---
	var inputsSec string
	if len(inputs) > 0 {
		var b strings.Builder
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
			// Re-encoding valBytes (already a JSON value) as a JSON string
			// yields a Python-valid double-quoted string literal that
			// json.loads can decode back to the original value at runtime.
			pyLit, err := json.Marshal(string(valBytes))
			if err != nil {
				return "", fmt.Errorf("buildPythonScript: re-encode input %q: %w", k, err)
			}
			fmt.Fprintf(&b, "_v_%s = json.loads(%s)\n", name, string(pyLit))
		}
		b.WriteString("\n")
		inputsSec = b.String()
	}

	// --- # {{SETUP}} ---
	var setupSec string
	if len(setup) > 0 {
		var b strings.Builder
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
		setupSec = b.String()
	}

	// --- # {{ACTION_BODY}} ---
	var bodyLines strings.Builder
	wroteAction := false
	for _, s := range action {
		body := strings.TrimRight(mangleVars(s.Body), "\n")
		if body == "" {
			continue
		}
		for _, line := range strings.Split(body, "\n") {
			bodyLines.WriteString("    " + line + "\n")
			wroteAction = true
		}
	}
	if !wroteAction {
		bodyLines.WriteString("    pass\n")
	}
	actionBody := bodyLines.String()

	// --- # {{ACTION_OPEN}} and # {{ACTION_CLOSE}} ---
	//
	// Async path: wrap the action in an async def so that top-level
	// await/async-with/async-for are syntactically valid. asyncio.run()
	// drives the coroutine from inside the outer try/except so exceptions
	// still flow into _exc exactly as the sync path does.
	//
	// Sync path: the action body sits directly inside the try block.
	var actionOpen, actionClose string
	if actionIsAsync(action) {
		actionOpen = "async def _v_main():\n    global _v_RESULT\n"
		actionClose = "\ntry:\n    asyncio.run(_v_main())\n" + excHandler
	} else {
		actionOpen = "try:\n"
		actionClose = excHandler
	}

	// Fill placeholders (each is unique — order is irrelevant).
	script := driverTemplate
	script = strings.Replace(script, "# {{IMPORTS}}\n", imports, 1)
	script = strings.Replace(script, "# {{INPUTS}}", inputsSec, 1)
	script = strings.Replace(script, "# {{SETUP}}", setupSec, 1)
	script = strings.Replace(script, "# {{ACTION_OPEN}}", actionOpen, 1)
	script = strings.Replace(script, "# {{ACTION_BODY}}", actionBody, 1)
	script = strings.Replace(script, "# {{ACTION_CLOSE}}", actionClose, 1)

	return script, nil
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
