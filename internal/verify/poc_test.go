package verify

import (
	"os"
	"os/exec"
	"testing"
)

// TestPOCUnitShapeEndToEnd is an integration smoke test for the full Phase 2
// unit/function pipeline: literals merge (F12 caller-side), python_expr input
// evaluation (F12 runner-side), differential branch comparison, length /
// contains_exception_type spec matchers (F11), and mutation testing across
// mutate_fixture + set_literal_value strategies (F10). It loads a hand-crafted
// minimal entry whose shape mirrors the asyncio TaskGroup seed pattern but
// uses only features the verifier actually implements as of v0.1.
//
// Real production seeds aren't yet directly verifiable end-to-end — they were
// authored before the verifier was built and need shape adjustments (path
// extraction in returns, async runtime support, source-rewriting mutation
// strategies). This entry stands in as proof-of-life and a regression target.
func TestPOCUnitShapeEndToEnd(t *testing.T) {
	skipIfNoPython3(t)
	data, err := os.ReadFile("testdata/unit-shape-poc.yaml")
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	res, err := Run(data)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "verified" {
		t.Fatalf("status=%q, reasons=%v", res.Status, res.Reasons)
	}
	if res.Tier != "unit" {
		t.Fatalf("tier=%q, want unit", res.Tier)
	}
	if res.UnitID != "unit-shape-poc-asyncio-pattern" {
		t.Fatalf("unit_id=%q", res.UnitID)
	}
}

// TestPyYAMLShapeEndToEnd lifts the pyyaml seed shape to a verified bundle.
// Combines F11 (type/value_equals matchers), F12 (literals merge), F13 (path
// extractor), and F14 (swap_function_call mutation) — the first real-seed-
// shape entry to verify since the verifier was built. Skipped when pyyaml
// isn't installed locally; the action does import yaml.
func TestPyYAMLShapeEndToEnd(t *testing.T) {
	skipIfNoPython3(t)
	skipIfNoPyYAML(t)
	data, err := os.ReadFile("testdata/pyyaml-shape.yaml")
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	res, err := Run(data)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "verified" {
		t.Fatalf("status=%q, reasons=%v", res.Status, res.Reasons)
	}
	if res.Tier != "unit" {
		t.Fatalf("tier=%q, want unit", res.Tier)
	}
}

// skipIfNoPyYAML skips the test when the host's python3 doesn't have pyyaml
// installed — the action body imports yaml. Mirrors skipIfNoPython3 in shape.
func skipIfNoPyYAML(t *testing.T) {
	t.Helper()
	cmd := exec.Command("python3", "-c", "import yaml")
	if err := cmd.Run(); err != nil {
		t.Skip("pyyaml not installed on this host's python3")
	}
}
