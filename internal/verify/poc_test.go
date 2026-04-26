package verify

import (
	"os"
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
