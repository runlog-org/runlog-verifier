package runner

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// skipIfNoBin skips when the named binary is not on PATH. Mirrors
// skipIfNoPython but accepts any tool name so reexecute tests can gate on
// `sqlite3`, `git`, etc.
func skipIfNoBin(t *testing.T, name string) {
	t.Helper()
	if _, err := exec.LookPath(name); err != nil {
		t.Skipf("%s not on PATH", name)
	}
}

// newSandbox returns a fresh tmpdir and a t.Cleanup that os.RemoveAll's it.
// Tests use it instead of MkdirTemp + defer to keep the boilerplate down.
func newSandbox(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "runlog-subproc-test-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func TestSubprocessShellEcho(t *testing.T) {
	skipIfNoBin(t, "sh")
	d := SubprocessDriver{Tool: "shell", Workdir: newSandbox(t)}
	res, err := d.Run(
		nil,
		[]Step{{Type: "code", Lang: "shell", Body: "printf '%s' hi"}},
		nil,
		5,
	)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Raised {
		t.Fatalf("unexpected raised: %s: %s", res.Exception, res.Message)
	}
	if res.TypeName != "string" {
		t.Fatalf("type=%q, want string", res.TypeName)
	}
	if res.Repr != "hi" {
		t.Fatalf("repr=%q, want hi", res.Repr)
	}
	if string(res.JSONValue) != `"hi"` {
		t.Fatalf("json_value=%s, want \"hi\"", string(res.JSONValue))
	}
	if res.Length == nil || *res.Length != 2 {
		t.Fatalf("length=%v, want *2", res.Length)
	}
}

func TestSubprocessShellNonZeroExit(t *testing.T) {
	skipIfNoBin(t, "sh")
	d := SubprocessDriver{Tool: "shell", Workdir: newSandbox(t)}
	res, err := d.Run(
		nil,
		[]Step{{Type: "code", Lang: "shell", Body: "echo broken >&2; exit 7"}},
		nil,
		5,
	)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Raised {
		t.Fatalf("expected raised, got TypeName=%q", res.TypeName)
	}
	if res.Exception != "SubprocessError" {
		t.Fatalf("exception=%q, want SubprocessError", res.Exception)
	}
	if !strings.Contains(res.Message, "exited 7") {
		t.Fatalf("message=%q, want substring 'exited 7'", res.Message)
	}
}

func TestSubprocessSetupRunsBeforeAction(t *testing.T) {
	skipIfNoBin(t, "sh")
	dir := newSandbox(t)
	d := SubprocessDriver{Tool: "shell", Workdir: dir}
	res, err := d.Run(
		[]Step{{Type: "code", Lang: "shell", Body: "printf '%s' written-by-setup > $WORKDIR/marker"}},
		[]Step{{Type: "code", Lang: "shell", Body: "cat $WORKDIR/marker"}},
		nil,
		5,
	)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Raised {
		t.Fatalf("raised: %s: %s", res.Exception, res.Message)
	}
	if res.Repr != "written-by-setup" {
		t.Fatalf("repr=%q", res.Repr)
	}
}

func TestSubprocessVarSubstitution(t *testing.T) {
	skipIfNoBin(t, "sh")
	d := SubprocessDriver{Tool: "shell", Workdir: newSandbox(t)}
	res, err := d.Run(
		nil,
		[]Step{{Type: "code", Lang: "shell", Body: "printf '%s-%s' '$NAME' '$VALUE'"}},
		map[string]any{"$NAME": "alpha", "$VALUE": 42},
		5,
	)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Repr != "alpha-42" {
		t.Fatalf("repr=%q, want alpha-42", res.Repr)
	}
}

func TestSubprocessShellRejectsSqlLang(t *testing.T) {
	d := SubprocessDriver{Tool: "shell", Workdir: newSandbox(t)}
	_, err := d.Run(
		nil,
		[]Step{{Type: "code", Lang: "sql", Body: "SELECT 1"}},
		nil,
		5,
	)
	if !errors.Is(err, ErrSubprocessTool) {
		t.Fatalf("expected ErrSubprocessTool, got %v", err)
	}
}

func TestSubprocessTimeout(t *testing.T) {
	skipIfNoBin(t, "sh")
	d := SubprocessDriver{Tool: "shell", Workdir: newSandbox(t)}
	_, err := d.Run(
		nil,
		[]Step{{Type: "code", Lang: "shell", Body: "sleep 5"}},
		nil,
		0.2,
	)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("expected ErrTimeout, got %v", err)
	}
}

func TestSubprocessEmptyAction(t *testing.T) {
	d := SubprocessDriver{Tool: "shell", Workdir: newSandbox(t)}
	_, err := d.Run(nil, nil, nil, 5)
	if !errors.Is(err, ErrEmptyAction) {
		t.Fatalf("expected ErrEmptyAction, got %v", err)
	}
}

func TestSubprocessEmptyWorkdir(t *testing.T) {
	d := SubprocessDriver{Tool: "shell"} // no Workdir
	_, err := d.Run(nil, []Step{{Lang: "shell", Body: "true"}}, nil, 5)
	if err == nil {
		t.Fatalf("expected error for empty Workdir")
	}
}

func TestSubprocessCwdIsWorkdir(t *testing.T) {
	skipIfNoBin(t, "sh")
	dir := newSandbox(t)
	d := SubprocessDriver{Tool: "shell", Workdir: dir}
	res, err := d.Run(
		nil,
		[]Step{{Type: "code", Lang: "shell", Body: "pwd"}},
		nil,
		5,
	)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// pwd may resolve symlinks (e.g. /var → /private/var on macOS); compare
	// canonical forms.
	wantPath, _ := filepath.EvalSymlinks(dir)
	gotPath, _ := filepath.EvalSymlinks(strings.TrimSpace(res.Repr))
	if gotPath != wantPath {
		t.Fatalf("pwd=%q, want %q (workdir)", gotPath, wantPath)
	}
}

func TestSubprocessSqliteRoundtrip(t *testing.T) {
	skipIfNoBin(t, "sqlite3")
	d := SubprocessDriver{Tool: "sqlite", Workdir: newSandbox(t)}
	res, err := d.Run(
		[]Step{
			{Type: "code", Lang: "sql", Body: "CREATE TABLE t (n INTEGER); INSERT INTO t VALUES (3), (4), (5);"},
		},
		[]Step{
			{Type: "code", Lang: "sql", Body: "SELECT SUM(n) FROM t;"},
		},
		nil,
		10,
	)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Raised {
		t.Fatalf("raised: %s: %s", res.Exception, res.Message)
	}
	if strings.TrimSpace(res.Repr) != "12" {
		t.Fatalf("repr=%q, want 12", res.Repr)
	}
}

func TestSubprocessSqliteShellMix(t *testing.T) {
	skipIfNoBin(t, "sqlite3")
	skipIfNoBin(t, "sh")
	d := SubprocessDriver{Tool: "sqlite", Workdir: newSandbox(t)}
	res, err := d.Run(
		[]Step{
			{Type: "code", Lang: "sql", Body: "CREATE TABLE t (s TEXT); INSERT INTO t VALUES ('hi');"},
		},
		[]Step{
			{Type: "code", Lang: "shell", Body: "sqlite3 $DB_PATH 'SELECT s FROM t;'"},
		},
		nil,
		10,
	)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Raised {
		t.Fatalf("raised: %s: %s", res.Exception, res.Message)
	}
	if strings.TrimSpace(res.Repr) != "hi" {
		t.Fatalf("repr=%q, want hi", res.Repr)
	}
}

func TestSubprocessRunSetupScriptOK(t *testing.T) {
	skipIfNoBin(t, "sh")
	dir := newSandbox(t)
	d := SubprocessDriver{Tool: "shell", Workdir: dir}
	err := d.RunSetupScript(
		[]string{"printf '%s' setup-content > $WORKDIR/seeded.txt"},
		nil,
		5,
	)
	if err != nil {
		t.Fatalf("RunSetupScript: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "seeded.txt"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "setup-content" {
		t.Fatalf("file content=%q", string(got))
	}
}

func TestSubprocessRunSetupScriptNonZeroFails(t *testing.T) {
	skipIfNoBin(t, "sh")
	d := SubprocessDriver{Tool: "shell", Workdir: newSandbox(t)}
	err := d.RunSetupScript(
		[]string{"echo before; exit 3"},
		nil,
		5,
	)
	if !errors.Is(err, ErrSetupScriptFailed) {
		t.Fatalf("expected ErrSetupScriptFailed, got %v", err)
	}
}

func TestSubprocessTeardownTolerantOfNonZero(t *testing.T) {
	skipIfNoBin(t, "sh")
	d := SubprocessDriver{Tool: "shell", Workdir: newSandbox(t)}
	err := d.RunTeardownScript(
		[]string{"exit 5"},
		nil,
		5,
	)
	// Non-zero exit during teardown is tolerated; only env errors return.
	if err != nil {
		t.Fatalf("teardown should swallow non-zero exit, got %v", err)
	}
}

func TestSubstituteVarsLongestKeyFirst(t *testing.T) {
	// Sanity: $DB_PATH must be substituted before $DB to avoid the prefix
	// eating the longer name. Substituted values are shell-quoted (input
	// values are data, not code) so the assertion includes the surrounding
	// single quotes.
	out := substituteVars(
		"keep $DB_PATH and $DB",
		map[string]any{"$DB_PATH": "/x/y", "$DB": "shortcut"},
		shellQuote,
	)
	if out != "keep '/x/y' and 'shortcut'" {
		t.Fatalf("substituteVars: %q", out)
	}
}

func TestSubstituteVarsLeavesUnknownTokens(t *testing.T) {
	// Tokens not in inputs (e.g. shell's own $1) must pass through unchanged.
	out := substituteVars(
		"awk '{print $1}'",
		map[string]any{"$WORKDIR": "/tmp/x"},
		shellQuote,
	)
	if out != "awk '{print $1}'" {
		t.Fatalf("substituteVars: %q (should not touch $1)", out)
	}
}

func TestShellQuoteEscapesSingleQuote(t *testing.T) {
	// A value containing a literal single quote must be escaped via the
	// `'\''` close-reopen idiom so a hostile cassette can't break out of
	// the surrounding shell literal.
	got := shellQuote(`a'b`)
	want := `'a'\''b'`
	if got != want {
		t.Fatalf("shellQuote(%q) = %q, want %q", `a'b`, got, want)
	}
}

func TestShellQuoteNeutralizesInjection(t *testing.T) {
	// Round-trip a hostile value through substituteVars and confirm the
	// metacharacters end up inside the literal rather than being executed
	// as fresh shell tokens.
	out := substituteVars(
		"echo $X",
		map[string]any{"$X": "; rm -rf /"},
		shellQuote,
	)
	if out != `echo '; rm -rf /'` {
		t.Fatalf("substituteVars injection: %q", out)
	}
}

func TestSqlQuoteDoublesSingleQuote(t *testing.T) {
	if got := sqlQuote(`x'y`); got != `'x''y'` {
		t.Fatalf("sqlQuote(%q) = %q", `x'y`, got)
	}
}

func TestValidateInputNameRejectsReserved(t *testing.T) {
	for _, name := range []string{
		"PATH", "$PATH", "IFS", "HOME", "LD_PRELOAD", "LD_LIBRARY_PATH",
		"BASH_ENV", "ENV", "USER", "SHELL", "LANG",
		"DYLD_INSERT_LIBRARIES", "$DYLD_FRAMEWORK_PATH",
		"LC_ALL", "LC_MESSAGES",
		"", "1FOO", "FOO-BAR", "FOO BAR", "$",
	} {
		if err := validateInputName(name); err == nil {
			t.Errorf("validateInputName(%q) accepted; want rejection", name)
		}
	}
}

func TestValidateInputNameAcceptsOrdinary(t *testing.T) {
	for _, name := range []string{"PAYLOAD", "$PAYLOAD", "_x", "table_name", "K2"} {
		if err := validateInputName(name); err != nil {
			t.Errorf("validateInputName(%q) rejected: %v", name, err)
		}
	}
}

// TestValidateInputsRejectsReservedNames pins the wrapper's behaviour for the
// explicit reservedInputNames map. The wrapper is what `Run` /
// `RunSetupScript` / `RunTeardownScript` actually call; if a regression
// short-circuits the wrapper or drops the reserved-name check, hostile
// cassettes could redefine PATH / LD_PRELOAD / etc. and redirect interpreter
// lookup before our subprocesses run.
func TestValidateInputsRejectsReservedNames(t *testing.T) {
	for _, name := range []string{
		"PATH", "$PATH", "IFS", "HOME", "LD_PRELOAD", "LD_LIBRARY_PATH",
		"BASH_ENV", "ENV", "USER", "SHELL", "LANG",
	} {
		err := validateInputs(map[string]any{name: ""})
		if err == nil {
			t.Errorf("validateInputs(%q) accepted; want rejection", name)
			continue
		}
		if !errors.Is(err, ErrInputInvalidName) {
			t.Errorf("validateInputs(%q): err=%v, want wrapping ErrInputInvalidName", name, err)
		}
	}
}

// TestValidateInputsRejectsDYLDPrefix exercises the prefix branch
// (subprocess.go:96) through the wrapper. macOS-specific DYLD_* env vars
// aren't enumerated in reservedInputNames; the prefix check is what blocks
// them. A regression that drops the prefix check would let a cassette set
// DYLD_INSERT_LIBRARIES on a macOS submitter's host.
func TestValidateInputsRejectsDYLDPrefix(t *testing.T) {
	for _, name := range []string{
		"DYLD_INSERT_LIBRARIES",
		"DYLD_FALLBACK_LIBRARY_PATH",
		"$DYLD_FRAMEWORK_PATH",
		"DYLD_ANYTHING",
	} {
		err := validateInputs(map[string]any{name: ""})
		if err == nil {
			t.Errorf("validateInputs(%q) accepted; want rejection via DYLD_ prefix", name)
			continue
		}
		if !errors.Is(err, ErrInputInvalidName) {
			t.Errorf("validateInputs(%q): err=%v, want wrapping ErrInputInvalidName", name, err)
		}
	}
}

// TestValidateInputsRejectsLCPrefix exercises the LC_ prefix branch. Locale
// vars are reserved because shell behaviour (collation, message language,
// numeric formatting) shifts under them, and a hostile cassette setting LC_ALL
// could perturb branch outputs in subtle ways.
func TestValidateInputsRejectsLCPrefix(t *testing.T) {
	for _, name := range []string{
		"LC_ALL",
		"LC_MESSAGES",
		"LC_TIME",
		"$LC_RANDOM_THING",
	} {
		err := validateInputs(map[string]any{name: ""})
		if err == nil {
			t.Errorf("validateInputs(%q) accepted; want rejection via LC_ prefix", name)
			continue
		}
		if !errors.Is(err, ErrInputInvalidName) {
			t.Errorf("validateInputs(%q): err=%v, want wrapping ErrInputInvalidName", name, err)
		}
	}
}

// TestValidateInputsAcceptsOrdinaryNames pins the happy path. Ordinary
// identifiers must not be over-rejected by the wrapper.
func TestValidateInputsAcceptsOrdinaryNames(t *testing.T) {
	inputs := map[string]any{
		"PAYLOAD":    "x",
		"$PAYLOAD2":  "y",
		"_x":         "z",
		"table_name": "t",
		"K2":         "k",
	}
	if err := validateInputs(inputs); err != nil {
		t.Errorf("validateInputs(ordinary): err=%v, want nil", err)
	}
}

// TestValidateInputsAcceptsAutoInjectedSandboxVars pins the WORKDIR / DB_PATH
// exemption (subprocess.go:108-111). These are driver-set, not submitter-set:
// `Run` auto-injects them via mergeInputs, then re-validates the merged map.
// If a future refactor drops the carve-out, the driver breaks because
// validateInputs would reject the names the driver itself supplied.
func TestValidateInputsAcceptsAutoInjectedSandboxVars(t *testing.T) {
	inputs := map[string]any{
		"WORKDIR":  "/tmp/x",
		"$DB_PATH": "/tmp/x/db.sqlite",
		"PAYLOAD":  "ok",
	}
	if err := validateInputs(inputs); err != nil {
		t.Errorf("validateInputs(WORKDIR/DB_PATH/PAYLOAD): err=%v, want nil", err)
	}
}

// TestValidateInputsRejectsMalformedNames pins the shape check via the
// wrapper. Malformed names must be rejected before they reach buildEnv where
// they would become env-list assignments.
func TestValidateInputsRejectsMalformedNames(t *testing.T) {
	for _, name := range []string{
		"",
		"1FOO",
		"FOO-BAR",
		"FOO BAR",
		"$",
	} {
		err := validateInputs(map[string]any{name: ""})
		if err == nil {
			t.Errorf("validateInputs(%q) accepted; want rejection", name)
			continue
		}
		if !errors.Is(err, ErrInputInvalidName) {
			t.Errorf("validateInputs(%q): err=%v, want wrapping ErrInputInvalidName", name, err)
		}
	}
}

func TestRedactStderrRedactsTokens(t *testing.T) {
	const fakeToken = "EXAMPLE_TOKEN_NOT_A_REAL_SECRET_PADDING_PADDING"
	in := []byte("Bearer " + fakeToken + " and trailing")
	out := redactStderr(in)
	if !strings.Contains(out, "<redacted-token>") {
		t.Errorf("expected token redaction, got %q", out)
	}
	if strings.Contains(out, fakeToken) {
		t.Errorf("token leaked: %q", out)
	}
}

func TestRedactStderrTruncates(t *testing.T) {
	in := bytes.Repeat([]byte("a"), stderrMaxBytes+512)
	out := redactStderr(in)
	if !strings.Contains(out, "[truncated]") {
		t.Errorf("expected truncation marker, got len=%d", len(out))
	}
	if len(out) > stderrMaxBytes+64 {
		t.Errorf("redactStderr did not bound output: len=%d", len(out))
	}
}
