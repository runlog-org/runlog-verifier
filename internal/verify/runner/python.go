// Python-in-subprocess driver. Backs the schema's `isolation: function`
// value: the action runs in a fresh `python3 -` subprocess, with inputs
// bound as locals and exceptions captured into the structured ExecResult.
//
// Despite the schema name, the driver is already a real subprocess —
// "function" describes the granularity (one action per process) rather
// than the implementation. The to-be-shipped `subprocess` isolation
// will run an arbitrary external command rather than a pinned Python
// interpreter; the existing PythonDriver remains the function-tier
// driver.

package runner

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

//go:embed driver.py.tmpl
var driverTemplate string

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

// PythonDriver implements the Driver interface for the function-tier
// (schema isolation: function). It shells out to `python3 -` once per
// Run call, feeds in a script composed from driver.py.tmpl + the
// caller's setup/action steps, and parses a single JSON object from
// stdout into ExecResult.
//
// PythonDriver is a value receiver so it's cheap to embed in the registry
// and safe to share across goroutines.
//
// PythonPackages (F57) is the per-entry venv pin set: name → exact pin
// (value may carry a leading "=="). When non-empty, Run provisions an
// ephemeral venv, pip-installs exactly these pins, runs the action with
// that venv's interpreter, then tears the venv down. When nil/empty the
// driver behaves byte-for-byte identically to the original (no-field)
// PythonDriver: the bare `python3 -` path with zero venv overhead. Every
// existing caller — the registry's PythonDriver{}, RunPython, and the
// mutation re-run fallback — constructs the zero value, so back-compat is
// guaranteed by the len()==0 guard at the top of the provisioning branch.
type PythonDriver struct {
	PythonPackages map[string]string
}

// venvProvisionTimeout is the floor for the pip-install phase. pip does
// network I/O (index lookup + wheel download); letting the action's own
// timeout_seconds (schema floor: > 0) starve it would misclassify a slow
// download as a provisioning failure. The effective timeout is
// max(actionTimeout, this) so a generous action timeout still wins.
const venvProvisionTimeout = 120 * time.Second

// normalizePin strips an optional single leading "==" so a value supplied
// either as "1.16.0" or "==1.16.0" installs as name==1.16.0.
func normalizePin(v string) string {
	return strings.TrimPrefix(v, "==")
}

// provisionVenv creates an ephemeral venv under a fresh temp dir, pip-
// installs every declared pin (package names iterated in lexical order
// for deterministic install sequencing), and returns the venv's python
// interpreter path plus a teardown func the caller MUST defer. On any
// failure it removes the temp dir itself and returns a
// ErrVenvProvisionFailed-wrapped error (stderr redacted).
func provisionVenv(pkgs map[string]string, actionTimeout time.Duration) (pyPath string, teardown func(), err error) {
	dir, err := os.MkdirTemp("", "runlog-venv-")
	if err != nil {
		return "", nil, fmt.Errorf("%w: mkdtemp: %v", ErrVenvProvisionFailed, err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }

	provTimeout := venvProvisionTimeout
	if actionTimeout > provTimeout {
		provTimeout = actionTimeout
	}

	ctx, cancel := context.WithTimeout(context.Background(), provTimeout)
	defer cancel()

	var cstderr bytes.Buffer
	create := exec.CommandContext(ctx, "python3", "-m", "venv", dir)
	create.Stderr = &cstderr
	if cerr := create.Run(); cerr != nil {
		cleanup()
		if ctx.Err() == context.DeadlineExceeded {
			return "", nil, fmt.Errorf("%w: `python3 -m venv` timed out after %s", ErrVenvProvisionFailed, provTimeout)
		}
		return "", nil, fmt.Errorf("%w: `python3 -m venv` failed: %v (stderr=%s)",
			ErrVenvProvisionFailed, cerr, redactStderr(cstderr.Bytes()))
	}

	pip := filepath.Join(dir, "bin", "pip")
	py := filepath.Join(dir, "bin", "python")

	names := make([]string, 0, len(pkgs))
	for name := range pkgs {
		names = append(names, name)
	}
	sort.Strings(names)

	args := make([]string, 0, len(names)+1)
	args = append(args, "install")
	for _, name := range names {
		args = append(args, name+"=="+normalizePin(pkgs[name]))
	}

	var pstderr bytes.Buffer
	install := exec.CommandContext(ctx, pip, args...)
	install.Stderr = &pstderr
	if ierr := install.Run(); ierr != nil {
		cleanup()
		if ctx.Err() == context.DeadlineExceeded {
			return "", nil, fmt.Errorf("%w: `pip install` timed out after %s", ErrVenvProvisionFailed, provTimeout)
		}
		return "", nil, fmt.Errorf("%w: `pip install %s` failed: %v (stderr=%s)",
			ErrVenvProvisionFailed, strings.Join(args[1:], " "), ierr, redactStderr(pstderr.Bytes()))
	}

	return py, cleanup, nil
}

// Run runs setup+action as a Python 3 subprocess with inputs bound
// as locals. Each `$VARNAME` reference inside step bodies is rewritten
// to `_v_VARNAME` and the same name (without the `$`) is bound from
// inputs via json.loads of a JSON string literal embedded in the
// driver. Returns the structured outcome (success or raised exception).
//
// inputs keys may be supplied with or without the `$` prefix; the
// prefix is stripped before mangling.
func (d PythonDriver) Run(setup, action []Step, inputs map[string]any, timeoutSec float64) (ExecResult, error) {
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
	// F57: when python_packages pins are declared, run the script with an
	// ephemeral venv's interpreter instead of the host python3. The nil/
	// empty guard keeps every existing caller on the byte-for-byte
	// original path (interpreter stays "python3", no temp dir, no pip).
	interpreter := "python3"
	if len(d.PythonPackages) > 0 {
		pyPath, teardown, perr := provisionVenv(d.PythonPackages, timeout)
		if perr != nil {
			return ExecResult{}, perr
		}
		defer teardown()
		interpreter = pyPath
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, interpreter, "-")
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
		return ExecResult{}, fmt.Errorf("python subprocess: %w (stderr=%s)", runErr, redactStderr(stderr.Bytes()))
	}

	var res ExecResult
	if err := json.Unmarshal(stdout.Bytes(), &res); err != nil {
		return ExecResult{}, fmt.Errorf("%w: %v (stdout=%q, stderr=%q)",
			ErrDriverOutput, err, stdout.String(), redactStderr(stderr.Bytes()))
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
//	# {{IMPORTS}}      — extra top-level imports; "import asyncio\n" for async, "\n" for sync
//	# {{INPUTS}}\n     — per-input binding lines (sorted); empty string when no inputs (line consumed)
//	# {{SETUP}}\n      — setup step bodies; empty string when no setup (line consumed)
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
	script = strings.Replace(script, "# {{INPUTS}}\n", inputsSec, 1)
	script = strings.Replace(script, "# {{SETUP}}\n", setupSec, 1)
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
