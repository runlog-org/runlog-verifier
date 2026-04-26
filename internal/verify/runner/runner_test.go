package runner

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
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

func TestRunPythonReturnsListWithLength(t *testing.T) {
	skipIfNoPython(t)
	res, err := RunPython(nil, []Step{{Type: "code", Lang: "python", Body: "$RESULT = [1, 2, 3]"}}, nil, 5)
	if err != nil {
		t.Fatalf("RunPython: %v", err)
	}
	if res.Raised {
		t.Fatalf("unexpected exception: %s: %s", res.Exception, res.Message)
	}
	if res.Length == nil || *res.Length != 3 {
		t.Fatalf("length=%v, want *3", res.Length)
	}
	want := []string{"int", "int", "int"}
	if len(res.ElementTypes) != len(want) {
		t.Fatalf("element_types=%v, want %v", res.ElementTypes, want)
	}
	for i, v := range want {
		if res.ElementTypes[i] != v {
			t.Fatalf("element_types[%d]=%q, want %q", i, res.ElementTypes[i], v)
		}
	}
}

func TestRunPythonReturnsStringWithLength(t *testing.T) {
	skipIfNoPython(t)
	res, err := RunPython(nil, []Step{{Type: "code", Lang: "python", Body: `$RESULT = "hello"`}}, nil, 5)
	if err != nil {
		t.Fatalf("RunPython: %v", err)
	}
	if res.Raised {
		t.Fatalf("unexpected exception: %s: %s", res.Exception, res.Message)
	}
	if res.Length == nil || *res.Length != 5 {
		t.Fatalf("length=%v, want *5", res.Length)
	}
	if res.ElementTypes != nil {
		t.Fatalf("element_types=%v, want nil (string excluded)", res.ElementTypes)
	}
}

func TestRunPythonReturnsDictWithLength(t *testing.T) {
	skipIfNoPython(t)
	res, err := RunPython(nil, []Step{{Type: "code", Lang: "python", Body: `$RESULT = {"a": 1, "b": 2}`}}, nil, 5)
	if err != nil {
		t.Fatalf("RunPython: %v", err)
	}
	if res.Raised {
		t.Fatalf("unexpected exception: %s: %s", res.Exception, res.Message)
	}
	if res.Length == nil || *res.Length != 2 {
		t.Fatalf("length=%v, want *2", res.Length)
	}
	if res.ElementTypes != nil {
		t.Fatalf("element_types=%v, want nil (dict excluded)", res.ElementTypes)
	}
}

func TestRunPythonReturnsIntNoLength(t *testing.T) {
	skipIfNoPython(t)
	res, err := RunPython(nil, []Step{{Type: "code", Lang: "python", Body: "$RESULT = 42"}}, nil, 5)
	if err != nil {
		t.Fatalf("RunPython: %v", err)
	}
	if res.Raised {
		t.Fatalf("unexpected exception: %s: %s", res.Exception, res.Message)
	}
	if res.Length != nil {
		t.Fatalf("length=%v, want nil (int has no len())", *res.Length)
	}
	if res.ElementTypes != nil {
		t.Fatalf("element_types=%v, want nil", res.ElementTypes)
	}
}

func TestRunPythonReturnsListOfMixedTypes(t *testing.T) {
	skipIfNoPython(t)
	res, err := RunPython(
		nil,
		[]Step{{Type: "code", Lang: "python", Body: `$RESULT = [1, "x", 3.14, ValueError("oops")]`}},
		nil,
		5,
	)
	if err != nil {
		t.Fatalf("RunPython: %v", err)
	}
	if res.Raised {
		t.Fatalf("unexpected exception: %s: %s", res.Exception, res.Message)
	}
	if res.Length == nil || *res.Length != 4 {
		t.Fatalf("length=%v, want *4", res.Length)
	}
	want := []string{"int", "str", "float", "ValueError"}
	if len(res.ElementTypes) != len(want) {
		t.Fatalf("element_types=%v, want %v", res.ElementTypes, want)
	}
	for i, v := range want {
		if res.ElementTypes[i] != v {
			t.Fatalf("element_types[%d]=%q, want %q", i, res.ElementTypes[i], v)
		}
	}
}

func TestRunPythonInputPythonExprList(t *testing.T) {
	skipIfNoPython(t)
	res, err := RunPython(
		nil,
		[]Step{{Type: "code", Lang: "python", Body: "$RESULT = $ITEMS"}},
		map[string]any{"$ITEMS": map[string]any{"python_expr": "list(range(5))"}},
		5,
	)
	if err != nil {
		t.Fatalf("RunPython: %v", err)
	}
	if res.Raised {
		t.Fatalf("unexpected exception: %s: %s", res.Exception, res.Message)
	}
	if res.TypeName != "list" {
		t.Fatalf("type=%q, want list", res.TypeName)
	}
	var got []int
	if err := json.Unmarshal(res.JSONValue, &got); err != nil {
		t.Fatalf("unmarshal json_value=%q: %v", string(res.JSONValue), err)
	}
	want := []int{0, 1, 2, 3, 4}
	if len(got) != len(want) {
		t.Fatalf("got=%v, want=%v", got, want)
	}
	for i, v := range want {
		if got[i] != v {
			t.Fatalf("got[%d]=%d, want %d", i, got[i], v)
		}
	}
	if res.Length == nil || *res.Length != 5 {
		t.Fatalf("length=%v, want *5", res.Length)
	}
}

func TestRunPythonInputPythonExprArith(t *testing.T) {
	skipIfNoPython(t)
	res, err := RunPython(
		nil,
		[]Step{{Type: "code", Lang: "python", Body: "$RESULT = $X"}},
		map[string]any{"$X": map[string]any{"python_expr": "10 * 2 + 5"}},
		5,
	)
	if err != nil {
		t.Fatalf("RunPython: %v", err)
	}
	if res.Raised {
		t.Fatalf("unexpected exception: %s: %s", res.Exception, res.Message)
	}
	if res.TypeName != "int" {
		t.Fatalf("type=%q, want int", res.TypeName)
	}
	if !bytes.Equal(res.JSONValue, []byte("25")) {
		t.Fatalf("json_value=%q, want 25", string(res.JSONValue))
	}
}

func TestRunPythonInputJSONFallback(t *testing.T) {
	skipIfNoPython(t)
	res, err := RunPython(
		nil,
		[]Step{{Type: "code", Lang: "python", Body: "$RESULT = $X"}},
		map[string]any{"$X": 42},
		5,
	)
	if err != nil {
		t.Fatalf("RunPython: %v", err)
	}
	if res.Raised {
		t.Fatalf("unexpected exception: %s: %s", res.Exception, res.Message)
	}
	if !bytes.Equal(res.JSONValue, []byte("42")) {
		t.Fatalf("json_value=%q, want 42", string(res.JSONValue))
	}
}

func TestRunPythonInputDictNotEvaluated(t *testing.T) {
	skipIfNoPython(t)
	// A regular dict (NOT python_expr shape) — the opt-in must be opt-in.
	res, err := RunPython(
		nil,
		[]Step{{Type: "code", Lang: "python", Body: "$RESULT = $X"}},
		map[string]any{"$X": map[string]any{"a": 1, "b": 2}},
		5,
	)
	if err != nil {
		t.Fatalf("RunPython: %v", err)
	}
	if res.Raised {
		t.Fatalf("unexpected exception: %s: %s", res.Exception, res.Message)
	}
	if res.TypeName != "dict" {
		t.Fatalf("type=%q, want dict", res.TypeName)
	}
	var got map[string]int
	if err := json.Unmarshal(res.JSONValue, &got); err != nil {
		t.Fatalf("unmarshal json_value=%q: %v", string(res.JSONValue), err)
	}
	want := map[string]int{"a": 1, "b": 2}
	if len(got) != len(want) {
		t.Fatalf("got=%v, want=%v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("got[%q]=%d, want %d", k, got[k], v)
		}
	}
}

func TestRunPythonInputPythonExprMultiKeyFallsThrough(t *testing.T) {
	skipIfNoPython(t)
	// A multi-key map containing python_expr alongside extras must NOT trigger
	// Python eval — len(m) != 1 short-circuits, so it goes through JSON binding
	// and the action sees a literal dict with both keys.
	res, err := RunPython(
		nil,
		[]Step{{Type: "code", Lang: "python", Body: "$RESULT = $INVOKED"}},
		map[string]any{"$INVOKED": map[string]any{"python_expr": "list(range(5))", "extra": "noise"}},
		5,
	)
	if err != nil {
		t.Fatalf("RunPython: %v", err)
	}
	if res.Raised {
		t.Fatalf("unexpected exception: %s: %s", res.Exception, res.Message)
	}
	if res.TypeName != "dict" {
		t.Fatalf("type=%q, want dict (multi-key map should not eval)", res.TypeName)
	}
}

// skipIfPythonBelow311 skips when the python3 interpreter is older than 3.11
// (asyncio.TaskGroup landed in 3.11). Tests requiring TaskGroup-specific
// behavior gate on this in addition to skipIfNoPython.
func skipIfPythonBelow311(t *testing.T) {
	t.Helper()
	out, err := exec.Command("python3", "-c", "import sys; print(f'{sys.version_info[0]}.{sys.version_info[1]}')").Output()
	if err != nil {
		t.Skipf("python3 version probe failed: %v", err)
	}
	s := strings.TrimSpace(string(out))
	var major, minor int
	if _, err := fmt.Sscanf(s, "%d.%d", &major, &minor); err != nil {
		t.Skipf("python3 version parse failed: %q", s)
	}
	if major < 3 || (major == 3 && minor < 11) {
		t.Skipf("python3 %d.%d < 3.11 (asyncio.TaskGroup not available)", major, minor)
	}
}

func TestRunPythonAsyncWithTaskGroupRaises(t *testing.T) {
	skipIfNoPython(t)
	skipIfPythonBelow311(t)
	res, err := RunPython(
		[]Step{{Type: "code", Lang: "python", Body: "import asyncio\n\nasync def slow_ok():\n    await asyncio.sleep(0.01)\n    return 1\n\nasync def quick_fail():\n    raise ValueError(\"boom\")\n"}},
		[]Step{{Type: "code", Lang: "python", Body: "$RESULT = []\nasync with asyncio.TaskGroup() as tg:\n    tg.create_task(slow_ok())\n    tg.create_task(quick_fail())\n"}},
		nil,
		5,
	)
	if err != nil {
		t.Fatalf("RunPython: %v", err)
	}
	if !res.Raised {
		t.Fatalf("expected raised ExceptionGroup, got value type=%q", res.TypeName)
	}
	// CPython 3.11+ raises ExceptionGroup; older versions of the test could see BaseExceptionGroup.
	if res.Exception != "ExceptionGroup" && res.Exception != "BaseExceptionGroup" {
		t.Fatalf("exception=%q, want ExceptionGroup or BaseExceptionGroup", res.Exception)
	}
}

func TestBuildPythonScriptSyncStaysUnwrapped(t *testing.T) {
	script, err := buildPythonScript(nil, []Step{{Type: "code", Lang: "python", Body: "$RESULT = 18"}}, nil)
	if err != nil {
		t.Fatalf("buildPythonScript: %v", err)
	}
	if strings.Contains(script, "asyncio.run") {
		t.Fatalf("sync action should not be wrapped in asyncio.run, got:\n%s", script)
	}
	if strings.Contains(script, "async def _v_main") {
		t.Fatalf("sync action should not emit async def _v_main, got:\n%s", script)
	}
	if strings.Contains(script, "import asyncio") {
		t.Fatalf("sync action should not import asyncio, got:\n%s", script)
	}
}

func TestRunPythonAsyncBareAwaitGather(t *testing.T) {
	skipIfNoPython(t)
	res, err := RunPython(
		[]Step{{Type: "code", Lang: "python", Body: "import asyncio\n\nasync def double(x):\n    await asyncio.sleep(0)\n    return x * 2\n"}},
		[]Step{{Type: "code", Lang: "python", Body: "$RESULT = await asyncio.gather(double(1), double(2), double(3))"}},
		nil,
		5,
	)
	if err != nil {
		t.Fatalf("RunPython: %v", err)
	}
	if res.Raised {
		t.Fatalf("unexpected exception: %s: %s", res.Exception, res.Message)
	}
	if res.TypeName != "list" {
		t.Fatalf("type=%q, want list", res.TypeName)
	}
	if !bytes.Equal(res.JSONValue, []byte(`[2, 4, 6]`)) {
		t.Fatalf("json_value=%q, want [2, 4, 6]", string(res.JSONValue))
	}
}

func TestRunPythonAsyncWithPythonExprInput(t *testing.T) {
	skipIfNoPython(t)
	res, err := RunPython(
		[]Step{{Type: "code", Lang: "python", Body: "import asyncio\n\nasync def slow_ok(i):\n    await asyncio.sleep(0)\n    return i\n"}},
		[]Step{{Type: "code", Lang: "python", Body: "coros = [slow_ok(i) for i in $ITEMS]\n$RESULT = await asyncio.gather(*coros)"}},
		map[string]any{"$ITEMS": map[string]any{"python_expr": "list(range(3))"}},
		5,
	)
	if err != nil {
		t.Fatalf("RunPython: %v", err)
	}
	if res.Raised {
		t.Fatalf("unexpected exception: %s: %s", res.Exception, res.Message)
	}
	if !bytes.Equal(res.JSONValue, []byte(`[0, 1, 2]`)) {
		t.Fatalf("json_value=%q, want [0, 1, 2]", string(res.JSONValue))
	}
}

func TestRunPythonAsyncSyncSetupAsyncAction(t *testing.T) {
	skipIfNoPython(t)
	res, err := RunPython(
		[]Step{
			{Type: "code", Lang: "python", Body: "import asyncio"},
			{Type: "code", Lang: "python", Body: "BASE = 10"},
		},
		[]Step{{Type: "code", Lang: "python", Body: "await asyncio.sleep(0)\n$RESULT = BASE + 5"}},
		nil,
		5,
	)
	if err != nil {
		t.Fatalf("RunPython: %v", err)
	}
	if res.Raised {
		t.Fatalf("unexpected exception: %s: %s", res.Exception, res.Message)
	}
	if !bytes.Equal(res.JSONValue, []byte("15")) {
		t.Fatalf("json_value=%q, want 15", string(res.JSONValue))
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
