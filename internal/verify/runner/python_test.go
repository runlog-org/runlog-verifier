package runner

import (
	"bytes"
	"testing"
)

// TestPythonDriverImplementsDriver is a compile-time check (the var
// assignment) plus a runtime smoke test confirming PythonDriver{} can be
// invoked through the Driver interface — the exact path the dispatcher
// in verify/unit.go takes after looking the driver up via DriverFor.
func TestPythonDriverImplementsDriver(t *testing.T) {
	skipIfNoPython(t)

	var d Driver = PythonDriver{}
	res, err := d.Run(nil, []Step{{Type: "code", Lang: "python", Body: "$RESULT = 7"}}, nil, 5)
	if err != nil {
		t.Fatalf("Driver.Run: %v", err)
	}
	if res.Raised {
		t.Fatalf("unexpected exception: %s: %s", res.Exception, res.Message)
	}
	if !bytes.Equal(res.JSONValue, []byte("7")) {
		t.Fatalf("json_value=%q, want 7", string(res.JSONValue))
	}
}

// TestDriverForFunctionResolves confirms the registry returns a real
// driver for the schema's "function" isolation value.
func TestDriverForFunctionResolves(t *testing.T) {
	d, ok := DriverFor("function")
	if !ok {
		t.Fatalf("DriverFor(\"function\") returned ok=false; expected the Python driver")
	}
	if d == nil {
		t.Fatalf("DriverFor(\"function\") returned a nil driver")
	}
}

// TestDriverForUnimplementedReturnsFalse confirms the registry reports
// schema-declared-but-unimplemented isolation values as missing — the
// dispatcher uses (nil, false) to emit isolation_unsupported.
func TestDriverForUnimplementedReturnsFalse(t *testing.T) {
	for _, iso := range []string{"subprocess", "compiler", "database", "http_client", "docker_daemon"} {
		t.Run(iso, func(t *testing.T) {
			d, ok := DriverFor(iso)
			if ok || d != nil {
				t.Fatalf("DriverFor(%q) = (%v, %v); want (nil, false)", iso, d, ok)
			}
		})
	}
}

// TestDriverForUnknownReturnsFalse covers the "not in the schema at all"
// case — DriverFor doesn't distinguish between unimplemented and unknown
// (that's the dispatcher's job), so both paths arrive here as (nil, false).
func TestDriverForUnknownReturnsFalse(t *testing.T) {
	d, ok := DriverFor("bogus_value")
	if ok || d != nil {
		t.Fatalf("DriverFor(\"bogus_value\") = (%v, %v); want (nil, false)", d, ok)
	}
}
