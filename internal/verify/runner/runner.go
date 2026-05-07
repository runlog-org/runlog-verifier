// Package runner executes the user code declared in a runlog entry's
// failed_approach / working_approach branches. The MVP supports the
// schema's `isolation: function` value via a Python-in-subprocess
// driver (see python.go) and `isolation: subprocess` / `isolation:
// database` (with cassette.runtime.tool ∈ {shell, sqlite, postgres})
// via SubprocessDriver (see subprocess.go); other isolation values
// declared by the schema (compiler, http_client, docker_daemon) are
// recognised by the registry but not yet implemented — callers
// surface a typed `isolation_unsupported` reason naming the requested
// value so the unit-tier check can degrade to tier_unsupported.
//
// Verification happens on the submitter's host (CLAUDE.md load-bearing
// invariant #6), so this package does not sandbox — it relies on the
// host's process boundary, the schema-bounded timeout_seconds, and a
// driver wrapper that captures both successful returns and raised
// exceptions in a structured JSON outcome.
package runner

import (
	"encoding/json"
	"errors"
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

// Errors returned by drivers. Callers map these to verify.Reason codes
// or to tier_unsupported, depending on the cause. Each driver may
// surface any subset of these — the Python driver uses all of them.
var (
	ErrLanguageUnsupported = errors.New("runner: language not supported")
	ErrInterpreterMissing  = errors.New("runner: interpreter not found on PATH")
	ErrTimeout             = errors.New("runner: subprocess timed out")
	ErrDriverOutput        = errors.New("runner: driver output not parseable")
	ErrEmptyAction         = errors.New("runner: action contains no steps")
)

// Driver executes one branch's setup+action and returns a structured
// ExecResult. Implementations must be stateless across calls — the
// dispatcher in verify/unit.go assumes Run is safe to call once per
// branch and once per mutation re-run with no shared state between
// calls.
//
// Inputs keys may be supplied with or without the `$` prefix; drivers
// strip the prefix before binding the value into their language's
// runtime. timeoutSec follows verification.timeout_seconds (schema
// bounds: > 0, <= 300) and a value <= 0 must be treated as a
// driver-default fallback rather than a hard error.
type Driver interface {
	Run(setup, action []Step, inputs map[string]any, timeoutSec float64) (ExecResult, error)
}

// drivers is the registry mapping schema-side isolation names to
// installed drivers. Today only "function" resolves; the other values
// declared by schema/entry.schema.yaml verification.isolation enum
// (subprocess, compiler, database, http_client, docker_daemon) return
// (nil, false) so the dispatcher can emit a precise reason.
//
// Adding a driver is a one-line registry edit plus a new file in this
// package implementing the Driver interface.
var drivers = map[string]Driver{
	"function": PythonDriver{},
}

// DriverFor returns the registered driver for a schema isolation name,
// or (nil, false) when the name is recognised by the schema but not
// yet implemented in this build. The dispatcher in verify/unit.go is
// responsible for distinguishing schema-recognised-but-unimplemented
// from schema-unknown values; this function does not validate against
// the schema enum on its own.
func DriverFor(isolation string) (Driver, bool) {
	d, ok := drivers[isolation]
	return d, ok
}

// RunPython is a package-level shortcut for the Python driver — kept
// as a stable entrypoint so existing callers (verify/mutate.go, tests)
// don't have to look up a driver instance every call. New code should
// prefer DriverFor on the entry's verification.isolation value when
// dispatch is required; direct RunPython use is correct only when the
// caller already knows it's running in the function/Python tier (e.g.
// mutate.go re-runs within an already-classified function-tier entry).
func RunPython(setup, action []Step, inputs map[string]any, timeoutSec float64) (ExecResult, error) {
	return PythonDriver{}.Run(setup, action, inputs, timeoutSec)
}
