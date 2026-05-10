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
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// ErrSubprocessTool is returned when SubprocessDriver is asked to run a step
// whose Lang is not valid for the configured Tool. The reexecute orchestrator
// surfaces this as a typed reason naming both the requested lang and the
// configured tool.
var ErrSubprocessTool = errors.New("runner: step language not valid for the configured runtime tool")

// ErrInputInvalidName is returned when a cassette/differential input key
// does not pass validateInputName — either it matches a reserved env-var
// the host shell relies on (PATH, HOME, LD_PRELOAD, …) or it isn't a
// well-formed C identifier. The reexecute orchestrator surfaces this as
// `cassette_input_invalid_name`.
var ErrInputInvalidName = errors.New("runner: cassette input name is reserved or malformed")

// reservedInputNames is the set of env-var names that a submitter MUST
// NOT redefine via cassette inputs. Setting any of these would let a
// hostile entry redirect interpreter lookup, library loading, locale
// handling, or shell startup before our subprocesses even begin parsing
// step bodies. Names are compared after $-stripping and case-sensitively
// (POSIX env-var names are case-sensitive).
var reservedInputNames = map[string]bool{
	"PATH":            true,
	"IFS":             true,
	"HOME":            true,
	"USER":            true,
	"SHELL":           true,
	"BASH_ENV":        true,
	"ENV":             true,
	"LD_PRELOAD":      true,
	"LD_LIBRARY_PATH": true,
	"LANG":            true,
}

// inputNameRE matches a valid POSIX-shell-safe identifier: leading letter
// or underscore, then letters/digits/underscores. Anything else (including
// empty, leading digit, dashes, dots) is rejected so we don't end up
// shell-injecting a key like `FOO=bar; rm -rf /` into the env list.
var inputNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// validateInputName enforces the env-var denylist + name shape against a
// caller-supplied input key. Both `$NAME` and `NAME` spellings are accepted;
// the `$` is stripped before validation. Returns a wrapped ErrInputInvalidName
// so the reexecute orchestrator can surface a typed reason.
//
// Inputs are treated as DATA, not code: the denylist exists to keep a
// hostile cassette from redefining the host's interpreter / loader / locale
// vars, and the shape check exists to keep names from smuggling shell
// metacharacters into the env-list assembly in buildEnv.
func validateInputName(key string) error {
	bare := strings.TrimPrefix(key, "$")
	if !inputNameRE.MatchString(bare) {
		return fmt.Errorf("%w: %q is not a valid identifier ([A-Za-z_][A-Za-z0-9_]*)",
			ErrInputInvalidName, key)
	}
	if reservedInputNames[bare] {
		return fmt.Errorf("%w: %q is reserved (host env var)",
			ErrInputInvalidName, key)
	}
	if strings.HasPrefix(bare, "DYLD_") || strings.HasPrefix(bare, "LC_") {
		return fmt.Errorf("%w: %q is reserved (host env var)",
			ErrInputInvalidName, key)
	}
	return nil
}

// validateInputs applies validateInputName to every key in inputs. The
// auto-injected sandbox vars ($WORKDIR, $DB_PATH) are exempt — they are
// driver-set, not submitter-set.
func validateInputs(inputs map[string]any) error {
	for k := range inputs {
		bare := strings.TrimPrefix(k, "$")
		if bare == "WORKDIR" || bare == "DB_PATH" {
			continue
		}
		if err := validateInputName(k); err != nil {
			return err
		}
	}
	return nil
}

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
	if err := validateInputs(inputs); err != nil {
		return ExecResult{}, err
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
	if err := validateInputs(inputs); err != nil {
		return err
	}
	merged := d.mergeInputs(inputs)
	for i, line := range lines {
		stdout, stderr, exit, err := d.execShell(substituteVars(line, merged, shellQuote), merged, timeoutSec)
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
	if err := validateInputs(inputs); err != nil {
		return err
	}
	merged := d.mergeInputs(inputs)
	for _, line := range lines {
		_, _, _, err := d.execShell(substituteVars(line, merged, shellQuote), merged, timeoutSec)
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
		case "postgres":
			if lang != "shell" && lang != "sql" {
				return fmt.Errorf("%w: tool=postgres supports lang ∈ {shell, sql}, got %q",
					ErrSubprocessTool, s.Lang)
			}
		case "redis":
			if lang != "shell" {
				return fmt.Errorf("%w: tool=redis requires lang=shell, got %q",
					ErrSubprocessTool, s.Lang)
			}
		case "docker":
			if lang != "shell" {
				return fmt.Errorf("%w: tool=docker requires lang=shell, got %q",
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
	switch lang {
	case "shell":
		// Input values are DATA, not code — single-quote them so a value
		// like `; rm -rf $HOME` is passed verbatim to the program rather
		// than re-parsed by sh.
		body := substituteVars(s.Body, inputs, shellQuote)
		return d.execShell(body, inputs, timeoutSec)
	case "sql":
		// Same treatment for sqlite3 SQL bodies — a value like `'); DROP
		// TABLE t; --` would otherwise close out the literal and tail
		// arbitrary SQL onto the statement.
		body := substituteVars(s.Body, inputs, sqlQuote)
		switch d.Tool {
		case "sqlite":
			return d.execSQL(body, inputs, timeoutSec)
		case "postgres":
			return d.execPostgres(body, inputs, timeoutSec)
		}
		return "", "", 0, fmt.Errorf("%w: lang=sql requires tool ∈ {sqlite, postgres}, got %q",
			ErrSubprocessTool, d.Tool)
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

// execPostgres runs `psql --dbname=$DATABASE_URL --no-psqlrc -X -q -v ON_ERROR_STOP=1`
// with body on stdin. The $DATABASE_URL value is supplied by the reexecute
// orchestrator (per-branch ephemeral DB created by ProvisionPostgresDB);
// SubprocessDriver itself stays stateless.
func (d SubprocessDriver) execPostgres(body string, inputs map[string]any, timeoutSec float64) (string, string, int, error) {
	dsn, _ := inputs["$DATABASE_URL"].(string)
	if dsn == "" {
		return "", "", 0, fmt.Errorf("%w: lang=sql under tool=postgres requires $DATABASE_URL (set by the reexecute orchestrator's per-branch provisioner)",
			ErrSubprocessTool)
	}
	return d.execCommand("psql",
		[]string{"--dbname=" + dsn, "--no-psqlrc", "-X", "-q", "-v", "ON_ERROR_STOP=1"},
		body, inputs, timeoutSec)
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
//
// Input keys are sorted before assembly so two equal inputs maps produce
// byte-identical env slices; non-deterministic map iteration would otherwise
// reshuffle the env order across runs and complicate reproducibility (a
// child process that snapshots env-var order would see drift between
// equivalent invocations).
func buildEnv(inputs map[string]any) []string {
	env := []string{"PATH=" + os.Getenv("PATH")}
	if home := os.Getenv("HOME"); home != "" {
		env = append(env, "HOME="+home)
	}
	keys := make([]string, 0, len(inputs))
	for k := range inputs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		bare := strings.TrimPrefix(k, "$")
		if bare == "" {
			continue
		}
		env = append(env, fmt.Sprintf("%s=%v", bare, inputs[k]))
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
//
// quote is applied to each substituted value before splicing — input values
// are treated as DATA, not code, so a value like `; rm -rf $HOME` must reach
// the underlying program as a literal argument rather than a fresh shell /
// SQL token. The driver-injected sandbox vars ($WORKDIR, $DB_PATH) are
// also quoted; they're set to absolute paths under os.MkdirTemp so quoting
// them is a no-op semantically but keeps the path uniform.
func substituteVars(s string, inputs map[string]any, quote func(string) string) string {
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
	sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) })
	for _, k := range keys {
		raw := fmt.Sprintf("%v", inputs[k])
		s = strings.ReplaceAll(s, k, quote(raw))
	}
	return s
}

// shellQuote returns a single-quoted POSIX shell literal that evaluates back
// to s exactly. Embedded single quotes are escaped via the canonical
// close-quote / backslash-quote / reopen-quote idiom (four characters).
// Use whenever a substituted input value lands inside `sh -c <body>` so a
// hostile value like `; rm -rf /` becomes the literal string
// `'; rm -rf /'` rather than a fresh statement.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// sqlQuote returns a SQLite-quoted string literal: single-quoted, with
// internal single quotes doubled. SQLite escapes an embedded single quote
// inside a string literal by doubling it (two consecutive apostrophes),
// not with a backslash. Use whenever a substituted input value lands
// inside a sqlite3 SQL body so a value like
// `'); DROP TABLE t; --` cannot close out the literal and append
// arbitrary SQL.
func sqlQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// combineStderr returns stderr if non-empty, else falls back to stdout. Used
// in the SubprocessError message when a step exits non-zero — many shell
// scripts write diagnostics to stdout (e.g. `set -x` traces, or tools that
// don't follow the convention), and the message field is more useful when
// it carries *some* output than when it's blank. Output is run through
// redactStderr so token-shaped substrings (API keys, bearer tokens, signing
// keys) never reach the bundle.
func combineStderr(stdout, stderr string) string {
	if stderr != "" {
		return redactStderr([]byte(stderr))
	}
	return redactStderr([]byte(stdout))
}

// stderrMaxBytes is the cap applied by redactStderr. Long enough to keep
// a stack trace useful, short enough to keep the bundle compact.
const stderrMaxBytes = 4096

// tokenLike matches a contiguous run of token-shaped characters of length
// >= 32 (alphanumerics plus `_` and `-`). Tuned to catch API keys, bearer
// tokens, base64-encoded signing material, and similar; deliberately loose
// enough to over-redact rather than under-redact.
var tokenLike = regexp.MustCompile(`[A-Za-z0-9_-]{32,}`)

// redactStderr truncates b to stderrMaxBytes (with a `…[truncated]` marker
// when bytes were dropped) and replaces every token-shaped substring with
// `<redacted-token>`. Used everywhere stderr from a child subprocess is
// folded into an error message that may end up in the signed bundle.
func redactStderr(b []byte) string {
	if len(b) > stderrMaxBytes {
		b = append([]byte(nil), b[:stderrMaxBytes]...)
		b = append(b, []byte("…[truncated]")...)
	}
	return tokenLike.ReplaceAllString(string(b), "<redacted-token>")
}
