package runner

import (
	"bytes"
	"errors"
	"os/exec"
	"strings"
	"testing"
)

func skipIfNoPython(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
}

func TestRunPythonReturnsInt(t *testing.T) {
	skipIfNoPython(t)
	res, err := RunPython(nil, []Step{{Type: "code", Lang: "python", Body: "$RESULT = 18"}}, nil, 5)
	if err != nil {
		t.Fatalf("RunPython: %v", err)
	}
	if res.Raised {
		t.Fatalf("unexpected exception: %s: %s", res.Exception, res.Message)
	}
	if res.TypeName != "int" {
		t.Fatalf("type=%q, want int", res.TypeName)
	}
	if !bytes.Equal(res.JSONValue, []byte("18")) {
		t.Fatalf("json_value=%q, want 18", string(res.JSONValue))
	}
	if !res.Serializable {
		t.Fatalf("expected serializable=true")
	}
}

func TestRunPythonReturnsString(t *testing.T) {
	skipIfNoPython(t)
	res, err := RunPython(nil, []Step{{Type: "code", Lang: "python", Body: `$RESULT = "hello"`}}, nil, 5)
	if err != nil {
		t.Fatalf("RunPython: %v", err)
	}
	if res.Raised {
		t.Fatalf("unexpected exception: %s: %s", res.Exception, res.Message)
	}
	if res.TypeName != "str" {
		t.Fatalf("type=%q, want str", res.TypeName)
	}
	if !bytes.Equal(res.JSONValue, []byte(`"hello"`)) {
		t.Fatalf("json_value=%q, want \"hello\"", string(res.JSONValue))
	}
}

func TestRunPythonInputsBound(t *testing.T) {
	skipIfNoPython(t)
	res, err := RunPython(
		nil,
		[]Step{{Type: "code", Lang: "python", Body: "$RESULT = $A + $B"}},
		map[string]any{"$A": 10, "$B": 7},
		5,
	)
	if err != nil {
		t.Fatalf("RunPython: %v", err)
	}
	if res.Raised {
		t.Fatalf("unexpected exception: %s: %s", res.Exception, res.Message)
	}
	if !bytes.Equal(res.JSONValue, []byte("17")) {
		t.Fatalf("json_value=%q, want 17", string(res.JSONValue))
	}
}

func TestRunPythonExceptionRaised(t *testing.T) {
	skipIfNoPython(t)
	res, err := RunPython(nil, []Step{{Type: "code", Lang: "python", Body: "$RESULT = 1 / 0"}}, nil, 5)
	if err != nil {
		t.Fatalf("RunPython: %v", err)
	}
	if !res.Raised {
		t.Fatalf("expected raised, got value type=%q", res.TypeName)
	}
	if res.Exception != "ZeroDivisionError" {
		t.Fatalf("exception=%q, want ZeroDivisionError", res.Exception)
	}
}

func TestRunPythonResultUnbound(t *testing.T) {
	skipIfNoPython(t)
	// Action runs but never assigns $RESULT.
	res, err := RunPython(nil, []Step{{Type: "code", Lang: "python", Body: "x = 1"}}, nil, 5)
	if err != nil {
		t.Fatalf("RunPython: %v", err)
	}
	if !res.Raised {
		t.Fatalf("expected raised when $RESULT unbound, got value type=%q", res.TypeName)
	}
	if res.Exception != "NameError" {
		t.Fatalf("exception=%q, want NameError", res.Exception)
	}
}

func TestRunPythonSetupRuns(t *testing.T) {
	skipIfNoPython(t)
	res, err := RunPython(
		[]Step{{Type: "code", Lang: "python", Body: "import math"}},
		[]Step{{Type: "code", Lang: "python", Body: "$RESULT = int(math.pi * 100)"}},
		nil,
		5,
	)
	if err != nil {
		t.Fatalf("RunPython: %v", err)
	}
	if res.Raised {
		t.Fatalf("unexpected exception: %s: %s", res.Exception, res.Message)
	}
	if !bytes.Equal(res.JSONValue, []byte("314")) {
		t.Fatalf("json_value=%q, want 314", string(res.JSONValue))
	}
}

func TestRunPythonTimeout(t *testing.T) {
	skipIfNoPython(t)
	_, err := RunPython(
		nil,
		[]Step{{Type: "code", Lang: "python", Body: "import time\ntime.sleep(2)\n$RESULT = 1"}},
		nil,
		0.2,
	)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("expected ErrTimeout, got %v", err)
	}
}

func TestRunPythonEmptyAction(t *testing.T) {
	_, err := RunPython(nil, nil, nil, 5)
	if !errors.Is(err, ErrEmptyAction) {
		t.Fatalf("expected ErrEmptyAction, got %v", err)
	}
}

func TestRunPythonUnsupportedLang(t *testing.T) {
	_, err := RunPython(nil, []Step{{Type: "code", Lang: "ruby", Body: "@RESULT = 1"}}, nil, 5)
	if !errors.Is(err, ErrLanguageUnsupported) {
		t.Fatalf("expected ErrLanguageUnsupported, got %v", err)
	}
}

func TestBuildPythonScriptDeterministic(t *testing.T) {
	// Two equal inputs maps must produce byte-identical scripts so the
	// fingerprint of a bundle stays stable across re-runs.
	steps := []Step{{Type: "code", Lang: "python", Body: "$RESULT = $A"}}
	a, _ := buildPythonScript(nil, steps, map[string]any{"$A": 1, "$B": "hi"})
	b, _ := buildPythonScript(nil, steps, map[string]any{"$B": "hi", "$A": 1})
	if a != b {
		t.Fatalf("non-deterministic script:\n--- a ---\n%s\n--- b ---\n%s", a, b)
	}
	if !strings.Contains(a, "_v_A = json.loads") {
		t.Fatalf("expected mangled input binding, got:\n%s", a)
	}
}
