package runner

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"testing"
)

// skipIfNoVenvProvision skips when the host cannot provision a venv: no
// python3, no `python3 -m venv`, or no pip. These tests do real network
// I/O (pip pulls a wheel from PyPI), so a sandbox with no network must
// SKIP cleanly rather than hard-fail. The RUNLOG_VERIFY_SKIP_VENV env
// gate forces a skip even where the toolchain exists (offline CI).
func skipIfNoVenvProvision(t *testing.T) {
	t.Helper()
	if v := os.Getenv("RUNLOG_VERIFY_SKIP_VENV"); v != "" {
		t.Skip("RUNLOG_VERIFY_SKIP_VENV set — skipping network-gated venv test")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	if err := exec.Command("python3", "-m", "venv", "--help").Run(); err != nil {
		t.Skipf("`python3 -m venv` unavailable: %v", err)
	}
}

// TestPythonDriverVenvInstallsPin provisions an ephemeral venv pinned to a
// tiny pure-Python package and asserts the action can import it. Network-
// gated: skips when offline / no venv toolchain.
func TestPythonDriverVenvInstallsPin(t *testing.T) {
	skipIfNoVenvProvision(t)

	d := PythonDriver{PythonPackages: map[string]string{"six": "1.16.0"}}
	res, err := d.Run(nil, []Step{{Type: "code", Lang: "python",
		Body: "import six\n$RESULT = six.__version__"}}, nil, 5)
	if err != nil {
		// pip needs PyPI; a sandbox with no network surfaces
		// ErrVenvProvisionFailed — skip rather than fail.
		if errors.Is(err, ErrVenvProvisionFailed) {
			t.Skipf("venv provisioning failed (likely offline): %v", err)
		}
		t.Fatalf("Run: %v", err)
	}
	if res.Raised {
		t.Fatalf("action raised %s: %s", res.Exception, res.Message)
	}
	if !bytes.Equal(res.JSONValue, []byte(`"1.16.0"`)) {
		t.Fatalf("json_value=%q, want \"1.16.0\" — pinned six not importable", string(res.JSONValue))
	}
}

// TestPythonDriverVenvPrefixedPin confirms a value supplied WITH a leading
// "==" normalizes to the same install as the bare form.
func TestPythonDriverVenvPrefixedPin(t *testing.T) {
	skipIfNoVenvProvision(t)

	d := PythonDriver{PythonPackages: map[string]string{"six": "==1.16.0"}}
	res, err := d.Run(nil, []Step{{Type: "code", Lang: "python",
		Body: "import six\n$RESULT = six.__version__"}}, nil, 5)
	if err != nil {
		if errors.Is(err, ErrVenvProvisionFailed) {
			t.Skipf("venv provisioning failed (likely offline): %v", err)
		}
		t.Fatalf("Run: %v", err)
	}
	if !bytes.Equal(res.JSONValue, []byte(`"1.16.0"`)) {
		t.Fatalf("json_value=%q, want \"1.16.0\"", string(res.JSONValue))
	}
}

// TestPythonDriverVenvBogusPinFails asserts an unresolvable pin yields
// ErrVenvProvisionFailed (pip can't resolve the package). Network-gated:
// the venv create itself needs no network but pip's index lookup does, so
// skip when the venv toolchain is unavailable.
func TestPythonDriverVenvBogusPinFails(t *testing.T) {
	skipIfNoVenvProvision(t)

	d := PythonDriver{PythonPackages: map[string]string{"thispkgdoesnotexist-xyz": "9.9.9"}}
	_, err := d.Run(nil, []Step{{Type: "code", Lang: "python",
		Body: "$RESULT = 1"}}, nil, 5)
	if err == nil {
		t.Fatal("expected ErrVenvProvisionFailed for an unresolvable pin, got nil")
	}
	if !errors.Is(err, ErrVenvProvisionFailed) {
		t.Fatalf("err=%v, want ErrVenvProvisionFailed", err)
	}
}

// TestPythonDriverNoPackagesUnchanged is the back-compat guard: a zero-
// value PythonDriver (no python_packages) must behave exactly like the
// original field-less driver — bare python3, no venv. It runs whenever
// python3 is present (no network needed).
func TestPythonDriverNoPackagesUnchanged(t *testing.T) {
	skipIfNoPython(t)

	d := PythonDriver{} // nil PythonPackages map
	res, err := d.Run(nil, []Step{{Type: "code", Lang: "python", Body: "$RESULT = 7"}}, nil, 5)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !bytes.Equal(res.JSONValue, []byte("7")) {
		t.Fatalf("json_value=%q, want 7 — nil python_packages must keep the bare python3 path", string(res.JSONValue))
	}
}

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
