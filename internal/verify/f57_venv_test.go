package verify

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// F57: verification.runtime.python_packages declares an exact-pin set the
// verifier installs into an ephemeral per-entry venv before running a
// unit/function action. These tests cover the verify-package wiring:
// entry parsing, the tier-specific placement/pin-grammar pre-flight, and
// the tier_unsupported classification of a venv-provision failure.

// TestRuntimePythonPackagesRoundTrips confirms python_packages parses into
// RuntimeSpec.PythonPackages (name → pin, leading "==" preserved verbatim
// at the parse layer; normalization happens at install time).
func TestRuntimePythonPackagesRoundTrips(t *testing.T) {
	src := `
unit_id: f57-parse
verification:
  type: unit
  isolation: function
  runtime:
    tool: python
    python_packages:
      six: 1.16.0
      requests: "==2.31.0"
`
	var e Entry
	if err := yaml.Unmarshal([]byte(src), &e); err != nil {
		t.Fatalf("yaml unmarshal: %v", err)
	}
	if e.Verification.Runtime == nil {
		t.Fatal("verification.runtime is nil — python_packages did not parse")
	}
	got := e.Verification.Runtime.PythonPackages
	if got["six"] != "1.16.0" {
		t.Fatalf("python_packages[six]=%q, want 1.16.0", got["six"])
	}
	if got["requests"] != "==2.31.0" {
		t.Fatalf("python_packages[requests]=%q, want ==2.31.0", got["requests"])
	}
}

// f57FunctionPinned is unitGreenYAML with a runtime block declaring a
// single exact pin — a well-placed (unit/function) python_packages entry.
var f57FunctionPinned = strings.Replace(unitGreenYAML,
	"  isolation: function\n",
	"  isolation: function\n  runtime:\n    tool: python\n    python_packages:\n      six: 1.16.0\n",
	1)

// TestPythonPackagesMisplacedRejected: python_packages on a non-function
// isolation (subprocess) is rejected with python_packages_misplaced — the
// tier-specific pre-flight, not a universal-shape check.
func TestPythonPackagesMisplacedRejected(t *testing.T) {
	yamlSrc := strings.Replace(unitSubprocessShellYAML,
		"  runtime: { tool: shell }\n",
		"  runtime: { tool: shell, python_packages: { six: 1.16.0 } }\n",
		1)
	res, err := Run([]byte(yamlSrc))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "rejected" {
		t.Fatalf("status=%q, want rejected (reasons=%v)", res.Status, res.Reasons)
	}
	if !hasReason(res.Reasons, "python_packages_misplaced") {
		t.Fatalf("expected python_packages_misplaced, got %v", res.Reasons)
	}
}

// TestPythonPackagesInvalidPinRejected: a range/specifier value (not an
// exact pin) is rejected with python_packages_invalid_pin — server-parity
// defence on the CLI path even though the schema enforces exact pins.
func TestPythonPackagesInvalidPinRejected(t *testing.T) {
	for _, bad := range []string{">=1.16.0", "1.*", "1.16.0,<2", "~=1.16"} {
		t.Run(bad, func(t *testing.T) {
			yamlSrc := strings.Replace(unitGreenYAML,
				"  isolation: function\n",
				"  isolation: function\n  runtime:\n    tool: python\n    python_packages:\n      six: \""+bad+"\"\n",
				1)
			res, err := Run([]byte(yamlSrc))
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if res.Status != "rejected" {
				t.Fatalf("status=%q, want rejected (reasons=%v)", res.Status, res.Reasons)
			}
			if !hasReason(res.Reasons, "python_packages_invalid_pin") {
				t.Fatalf("expected python_packages_invalid_pin, got %v", res.Reasons)
			}
		})
	}
}

// TestPythonPackagesBogusPinTierUnsupported: a well-formed but
// unresolvable pin on a unit/function entry surfaces tier_unsupported /
// venv_provision_failed (pip cannot resolve the package). Network-gated:
// venv create needs no network but pip's index lookup does — skip when
// the venv toolchain is missing or RUNLOG_VERIFY_SKIP_VENV is set.
func TestPythonPackagesBogusPinTierUnsupported(t *testing.T) {
	skipIfNoVenv(t)

	yamlSrc := strings.Replace(unitGreenYAML,
		"  isolation: function\n",
		"  isolation: function\n  runtime:\n    tool: python\n    python_packages:\n      thispkgdoesnotexist-xyz: 9.9.9\n",
		1)
	res, err := Run([]byte(yamlSrc))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "tier_unsupported" {
		t.Fatalf("status=%q, want tier_unsupported (reasons=%v)", res.Status, res.Reasons)
	}
	if !hasReason(res.Reasons, "venv_provision_failed") {
		t.Fatalf("expected venv_provision_failed, got %v", res.Reasons)
	}
}

// TestPythonPackagesWellPlacedNotRejected confirms the placement pre-flight
// passes a correctly-placed exact-pin entry through to driver dispatch (it
// does NOT short-circuit to rejected). When the venv toolchain/network is
// available the entry runs end-to-end; otherwise it must degrade to
// tier_unsupported / venv_provision_failed — never rejected.
func TestPythonPackagesWellPlacedNotRejected(t *testing.T) {
	skipIfNoPython3(t)

	res, err := Run([]byte(f57FunctionPinned))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	switch res.Status {
	case "verified":
		// venv toolchain + network present, six installed, action ran.
	case "tier_unsupported":
		if !hasReason(res.Reasons, "venv_provision_failed") {
			t.Fatalf("tier_unsupported but reason != venv_provision_failed: %v", res.Reasons)
		}
	default:
		t.Fatalf("status=%q (reasons=%v) — a well-placed exact pin must never be rejected on placement grounds",
			res.Status, res.Reasons)
	}
}

// skipIfNoVenv mirrors the runner-package guard at the verify-package
// level: skip when the host cannot provision a venv so an offline sandbox
// SKIPs rather than hard-fails.
func skipIfNoVenv(t *testing.T) {
	t.Helper()
	skipIfNoPython3(t)
	if os.Getenv("RUNLOG_VERIFY_SKIP_VENV") != "" {
		t.Skip("RUNLOG_VERIFY_SKIP_VENV set — skipping network-gated venv test")
	}
	if err := exec.Command("python3", "-m", "venv", "--help").Run(); err != nil {
		t.Skipf("`python3 -m venv` unavailable: %v", err)
	}
}
