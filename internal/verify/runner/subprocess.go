// Subprocess driver. Backs the schema's `isolation: subprocess` and
// `isolation: database` (with cassette.runtime.tool ∈ {sqlite, shell}) by
// executing per-step shell or SQL fragments in a per-branch tmpdir sandbox.
//
// Unlike PythonDriver, SubprocessDriver is constructed ad-hoc by the
// reexecute orchestrator with a Tool + Workdir, not registered in the
// package-level drivers map: each branch + each mutation re-run needs its
// own sandbox path, so a singleton-style registry doesn't fit.
//
// Step-language dispatch (per cassette.runtime.tool):
//   - tool: shell  → step.Lang must be "shell"; body runs as `sh -c "<body>"`.
//   - tool: sqlite → step.Lang ∈ {"shell", "sql"}; sql bodies run as
//                    `sqlite3 $DB_PATH` with body on stdin.
//
// $-token substitution is client-side and opt-in: each declared input key
// (including the synthesized $WORKDIR and, for sqlite, $DB_PATH) is
// substituted verbatim into step bodies before exec. Bare $-tokens that
// aren't in the inputs map pass through unchanged so shell's own runtime
// expansion (e.g. `$1` inside `awk '{print $1}'`) keeps working.

package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

// ErrSubprocessTool is returned when SubprocessDriver is asked to run a step
// whose Lang is not valid for the configured Tool. The reexecute orchestrator
// surfaces this as a typed reason naming both the requested lang and the
// configured tool.
var ErrSubprocessTool = errors.New("runner: step language not valid for the configured runtime tool")

// ErrSetupScriptFailed is returned by SubprocessDriver.RunSetupScript when a
// setup_script line exits non-zero. The sandbox is left intact for the caller
// to inspect (and to teardown) before os.RemoveAll.
var ErrSetupScriptFailed = errors.New("runner: setup_script step exited non-zero")

// SubprocessDriver executes setup+action steps in a workdir-rooted sandbox via
// `sh -c` (shell) or `sqlite3` (sql) subprocesses.
//
// Tool ∈ {"shell", "sqlite"} drives step-Lang validation and the auto-injected
// inputs map (always $WORKDIR; $DB_PATH when Tool == "sqlite"). Workdir must
// be an absolute path the caller has already created (typically via
// os.MkdirTemp); SubprocessDriver does not own the directory's lifetime.
//
// Implementations must be safe to call concurrently across goroutines for a
// given Tool — but a single Workdir must not be shared between concurrent
// Run calls (cassette state would race).
type SubprocessDriver struct {
	Tool    string
	Workdir string
}

// Run executes setup followed by action in the configured sandbox, returning
// the action's stdout as a string-typed ExecResult or a raised
// SubprocessError when any step exits non-zero. Setup-step failures and
// action-step failures both surface as raised in the same ExecResult shape;
// the orchestrator distinguishes them via the cassette.setup_script /
// per-branch boundary, not the driver.
//
// Inputs are merged with the auto-injected sandbox variables ($WORKDIR plus
// $DB_PATH when Tool == "sqlite") before substitution; caller-supplied values
// override the auto-injected ones, so a seed that wants to point at a
// pre-existing DB file can set $DB_PATH explicitly.
func (d SubprocessDriver) Run(setup, action []Step, inputs map[string]any, timeoutSec float64) (ExecResult, error) {
	if len(action) == 0 {
		return ExecResult{}, ErrEmptyAction
	}
	if d.Workdir == "" {
		return ExecResult{}, errors.New("runner: SubprocessDriver.Workdir is empty")
	}
	if err := d.validateSteps(setup); err != nil {
		return ExecResult{}, err
	}
	if err := d.validateSteps(action); err != nil {
		return ExecResult{}, err
	}

	merged := d.mergeInputs(inputs)

	// Setup steps: each runs in turn; first non-zero exit aborts and surfaces
	// as a raised ExecResult (Exception: SubprocessError) so callers can
	// classify as outcomeFail or rejected. This mirrors how the Python driver
	// surfaces a setup-step exception — captured into the same ExecResult shape
	// the action would produce, not a Go-level error.
	for i, s := range setup {
		stdout, stderr, exit, err := d.execStep(s, merged, timeoutSec)
		if err != nil {
			return ExecResult{}, err
		}
		if exit != 0 {
			return ExecResult{
				Raised:    true,
				Exception: "SubprocessError",
				Message: fmt.Sprintf("setup step %d (%s) exited %d: %s",
					i+1, s.Lang, exit, strings.TrimSpace(combineStderr(stdout, stderr))),
			}, nil
		}
	}

	// Action steps: same pattern, but the *last* successful step's stdout is
	// the captured $RESULT-equivalent. Earlier action steps' output is
	// discarded (matches the Python driver: only the final $RESULT
	// assignment matters; intermediate prints don't leak into the bundle).
	var lastStdout string
	for i, s := range action {
		stdout, stderr, exit, err := d.execStep(s, merged, timeoutSec)
		if err != nil {
			return ExecResult{}, err
		}
		if exit != 0 {
			return ExecResult{
				Raised:    true,
				Exception: "SubprocessError",
				Message: fmt.Sprintf("action step %d (%s) exited %d: %s",
					i+1, s.Lang, exit, strings.TrimSpace(combineStderr(stdout, stderr))),
			}, nil
		}
		lastStdout = stdout
	}

	jsonVal, err := json.Marshal(lastStdout)
	if err != nil {
		// json.Marshal of a string never fails in practice — guard for
		// completeness so a panic doesn't leak past the driver boundary.
		return ExecResult{}, fmt.Errorf("subprocess: marshal stdout: %w", err)
	}
	length := utf8.RuneCountInString(lastStdout)
	return ExecResult{
		Raised:       false,
		TypeName:     "string",
		JSONValue:    jsonVal,
		Serializable: true,
		Repr:         lastStdout,
		Length:       &length,
	}, nil
}

// RunSetupScript executes the cassette's setup_script lines in order. Each
// line runs as a shell command via `sh -c` with the merged inputs map exposed
// as environment variables, so e.g. "sqlite3 $DB_PATH 'CREATE TABLE foo (...)'"
// resolves $DB_PATH from the auto-injected sandbox vars.
//
// On non-zero exit, returns ErrSetupScriptFailed with a wrapped error message
// naming the offending line + exit code + stderr; the orchestrator surfaces
// this as `setup_script_failed` and skips the branch.
func (d SubprocessDriver) RunSetupScript(lines []string, inputs map[string]any, timeoutSec float64) error {
	if len(lines) == 0 {
		return nil
	}
	merged := d.mergeInputs(inputs)
	for i, line := range lines {
		stdout, stderr, exit, err := d.execShell(substituteVars(line, merged), merged, timeoutSec)
		if err != nil {
			return fmt.Errorf("setup_script[%d] %q: %w", i, line, err)
		}
		if exit != 0 {
			return fmt.Errorf("%w: setup_script[%d] %q exited %d: %s",
				ErrSetupScriptFailed, i, line, exit, strings.TrimSpace(combineStderr(stdout, stderr)))
		}
	}
	return nil
}

// RunTeardownScript executes the cassette's teardown_script lines in order,
// best-effort. Non-zero exits are tolerated (teardown is cleanup, not a
// trust signal — the sandbox is os.RemoveAll'd by the caller anyway). Any
// Go-level error (timeout, no shell on PATH) is returned so the orchestrator
// can surface it for diagnostics; non-zero exits are silently ignored.
func (d SubprocessDriver) RunTeardownScript(lines []string, inputs map[string]any, timeoutSec float64) error {
	if len(lines) == 0 {
		return nil
	}
	merged := d.mergeInputs(inputs)
	for _, line := range lines {
		_, _, _, err := d.execShell(substituteVars(line, merged), merged, timeoutSec)
		if err != nil {
			return err
		}
	}
	return nil
}

// validateSteps rejects setup/action mixes that don't match the configured
// Tool. v0.1 supports:
//
//	tool: shell  → lang must be "shell" (or empty, treated as shell)
//	tool: sqlite → lang ∈ {"shell", "sql"} (empty defaults to shell)
//
// Step.Type must be empty or "code" (matching PythonDriver's accepted shape).
func (d SubprocessDriver) validateSteps(steps []Step) error {
	for _, s := range steps {
		if s.Type != "" && s.Type != "code" {
			return fmt.Errorf("%w: step type %q (only \"code\" is supported)",
				ErrLanguageUnsupported, s.Type)
		}
		lang := s.Lang
		if lang == "" {
			lang = "shell"
		}
		switch d.Tool {
		case "shell":
			if lang != "shell" {
				return fmt.Errorf("%w: tool=shell requires lang=shell, got %q",
					ErrSubprocessTool, s.Lang)
			}
		case "sqlite":
			if lang != "shell" && lang != "sql" {
				return fmt.Errorf("%w: tool=sqlite supports lang ∈ {shell, sql}, got %q",
					ErrSubprocessTool, s.Lang)
			}
		default:
			return fmt.Errorf("%w: SubprocessDriver tool %q is not implemented",
				ErrSubprocessTool, d.Tool)
		}
	}
	return nil
}

// mergeInputs folds the auto-injected sandbox vars into the caller's inputs
// map. Caller-supplied values win on key collision so a seed can override
// e.g. $DB_PATH to point at a pre-existing fixture.
func (d SubprocessDriver) mergeInputs(inputs map[string]any) map[string]any {
	out := make(map[string]any, len(inputs)+2)
	out["$WORKDIR"] = d.Workdir
	if d.Tool == "sqlite" {
		out["$DB_PATH"] = filepath.Join(d.Workdir, "db.sqlite")
	}
	for k, v := range inputs {
		// Normalize to bare $-prefixed form so substituteVars sees a stable
		// shape regardless of how the caller spelled the key.
		key := k
		if !strings.HasPrefix(key, "$") {
			key = "$" + key
		}
		out[key] = v
	}
	return out
}

// execStep dispatches one step on its Lang. Returns (stdout, stderr,
// exitCode, err); err is reserved for environmental failures (timeout,
// missing interpreter), exitCode for in-band step failures.
func (d SubprocessDriver) execStep(s Step, inputs map[string]any, timeoutSec float64) (string, string, int, error) {
	lang := s.Lang
	if lang == "" {
		lang = "shell"
	}
	body := substituteVars(s.Body, inputs)
	switch lang {
	case "shell":
		return d.execShell(body, inputs, timeoutSec)
	case "sql":
		return d.execSQL(body, inputs, timeoutSec)
	}
	// Unreachable — validateSteps already rejected unknown langs.
	return "", "", 0, fmt.Errorf("%w: lang %q", ErrLanguageUnsupported, lang)
}

// execShell runs `sh -c body` in the sandbox cwd with the merged inputs map
// exposed as environment variables (so seeds can mix client-side $-token
// substitution with shell's own runtime $-expansion).
func (d SubprocessDriver) execShell(body string, inputs map[string]any, timeoutSec float64) (string, string, int, error) {
	return d.execCommand("sh", []string{"-c", body}, "", inputs, timeoutSec)
}

// execSQL runs `sqlite3 $DB_PATH` with body on stdin.
func (d SubprocessDriver) execSQL(body string, inputs map[string]any, timeoutSec float64) (string, string, int, error) {
	dbPath, _ := inputs["$DB_PATH"].(string)
	if dbPath == "" {
		return "", "", 0, fmt.Errorf("%w: lang=sql requires $DB_PATH (set automatically when tool=sqlite)",
			ErrSubprocessTool)
	}
	return d.execCommand("sqlite3", []string{dbPath}, body, inputs, timeoutSec)
}

// execCommand runs an external program with the given args, stdin, env, and
// cwd, returning captured stdout/stderr and exit code. Environmental errors
// (timeout, ErrNotFound) come back via the err return; anything else is
// wrapped in a non-zero exitCode.
func (d SubprocessDriver) execCommand(name string, args []string, stdin string, inputs map[string]any, timeoutSec float64) (string, string, int, error) {
	timeout := time.Duration(timeoutSec * float64(time.Second))
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = d.Workdir
	cmd.Env = buildEnv(inputs)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return stdout.String(), stderr.String(), 0,
			fmt.Errorf("%w after %s", ErrTimeout, timeout)
	}
	if runErr != nil {
		var execErr *exec.Error
		if errors.As(runErr, &execErr) && errors.Is(execErr.Err, exec.ErrNotFound) {
			return "", "", 0, fmt.Errorf("%w: %v", ErrInterpreterMissing, runErr)
		}
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			return stdout.String(), stderr.String(), exitErr.ExitCode(), nil
		}
		return stdout.String(), stderr.String(), 0,
			fmt.Errorf("subprocess %s: %w", name, runErr)
	}
	return stdout.String(), stderr.String(), 0, nil
}

// buildEnv composes an environment slice from the merged inputs map. Each
// $-prefixed key is exposed as the bare name (so $WORKDIR becomes WORKDIR=…
// in the subprocess env) so shell's own $-expansion picks it up. The host's
// PATH is preserved so `sh`, `sqlite3`, etc. resolve.
func buildEnv(inputs map[string]any) []string {
	env := []string{"PATH=" + os.Getenv("PATH")}
	if home := os.Getenv("HOME"); home != "" {
		env = append(env, "HOME="+home)
	}
	for k, v := range inputs {
		bare := strings.TrimPrefix(k, "$")
		if bare == "" {
			continue
		}
		env = append(env, fmt.Sprintf("%s=%v", bare, v))
	}
	return env
}

// substituteVars replaces every `$KEY` reference in s with the corresponding
// inputs[$KEY] value. Substitution is verbatim string replacement, NOT
// regex-anchored — `$DB_PATH` and `$DB_PATH_SUFFIX` would both be matched if
// both keys exist. To avoid that ambiguity, longer keys are substituted first
// (so `$DB_PATH_SUFFIX` is replaced before `$DB_PATH` and the prefix doesn't
// eat the longer name).
//
// Bare `$-foo` references whose key isn't in inputs pass through unchanged so
// shell's runtime $-expansion (e.g. `awk '{print $1}'`) keeps working.
func substituteVars(s string, inputs map[string]any) string {
	if !strings.Contains(s, "$") {
		return s
	}
	keys := make([]string, 0, len(inputs))
	for k := range inputs {
		if strings.HasPrefix(k, "$") {
			keys = append(keys, k)
		}
	}
	// Sort by length descending so $DB_PATH_SUFFIX is replaced before $DB_PATH.
	for i := 0; i < len(keys); i++ {
		for j := i + 1; j < len(keys); j++ {
			if len(keys[j]) > len(keys[i]) {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	for _, k := range keys {
		s = strings.ReplaceAll(s, k, fmt.Sprintf("%v", inputs[k]))
	}
	return s
}

// combineStderr returns stderr if non-empty, else falls back to stdout. Used
// in the SubprocessError message when a step exits non-zero — many shell
// scripts write diagnostics to stdout (e.g. `set -x` traces, or tools that
// don't follow the convention), and the message field is more useful when
// it carries *some* output than when it's blank.
func combineStderr(stdout, stderr string) string {
	if stderr != "" {
		return stderr
	}
	return stdout
}
