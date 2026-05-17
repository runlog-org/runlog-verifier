package verify

import (
	"strings"
	"testing"
)

const k8sAssertionOnly = `
unit_id: k8s-quantity-canonical
domain: [kubernetes]
version_constraints:
  spec: { name: K8s, anchor: quantity }
failed_approach:
  description: assumes 1000m and 1 differ
  setup: []
  action: []
  assertion: { type: value_equals, expect: fail, path: assumed_different, value: true }
working_approach:
  description: parses both via canonical milliCPU
  setup: []
  action: []
  assertion: { type: value_equals, expect: success, path: equal, value: true }
verification:
  type: assertion_only
  primitives_required: [equal, k8s.parse_quantity_to_milli]
  differential:
    failed_approach_assertion: { assumed_different: true }
    working_approach_assertion: { equal: true }
  mutations:
    - strategy: mutate_fixture
      target: $LITERAL_2
      new_value: '0.5'
      expected_branch_outcome: { working_approach: assertion_does_not_match, failed_approach: unchanged }
    - strategy: swap_identifier
      target: working_approach.action
      token: parse_quantity_to_milli
      new_value: identity
      expected_result: fail
  timeout_seconds: 2
`

func TestRunVerified(t *testing.T) {
	res, err := Run([]byte(k8sAssertionOnly))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "verified" {
		t.Fatalf("expected status=verified, got %q with reasons=%v", res.Status, res.Reasons)
	}
	if res.Tier != "assertion_only" {
		t.Fatalf("expected tier=assertion_only, got %q", res.Tier)
	}
	if res.UnitID != "k8s-quantity-canonical" {
		t.Fatalf("unit_id mismatch: %q", res.UnitID)
	}
}

func TestRunTautologicalBranches(t *testing.T) {
	// Same assertion on both branches → must reject as non-discriminating.
	yaml := strings.Replace(
		k8sAssertionOnly,
		"assertion: { type: value_equals, expect: success, path: equal, value: true }",
		"assertion: { type: value_equals, expect: fail, path: assumed_different, value: true }",
		1,
	)
	res, err := Run([]byte(yaml))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "rejected" {
		t.Fatalf("expected rejected, got %q", res.Status)
	}
	if !hasReason(res.Reasons, "tautological_branches") {
		t.Fatalf("expected tautological_branches reason, got %v", res.Reasons)
	}
}

func TestRunMissingMutateFixture(t *testing.T) {
	// Strip the mutate_fixture mutation, keep the discriminating one.
	yaml := strings.Replace(
		k8sAssertionOnly,
		`    - strategy: mutate_fixture
      target: $LITERAL_2
      new_value: '0.5'
      expected_branch_outcome: { working_approach: assertion_does_not_match, failed_approach: unchanged }
`,
		"",
		1,
	)
	res, err := Run([]byte(yaml))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "rejected" {
		t.Fatalf("expected rejected, got %q", res.Status)
	}
	if !hasReason(res.Reasons, "missing_mutate_fixture") {
		t.Fatalf("expected missing_mutate_fixture, got %v", res.Reasons)
	}
}

func TestRunUnregisteredPrimitive(t *testing.T) {
	yaml := strings.Replace(
		k8sAssertionOnly,
		"primitives_required: [equal, k8s.parse_quantity_to_milli]",
		"primitives_required: [equal, fictional.bogus_primitive]",
		1,
	)
	res, err := Run([]byte(yaml))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "rejected" {
		t.Fatalf("expected rejected, got %q", res.Status)
	}
	if !hasReason(res.Reasons, "unregistered_primitives") {
		t.Fatalf("expected unregistered_primitives, got %v", res.Reasons)
	}
}

func TestRunIntegrationIsolationUnsupported(t *testing.T) {
	// Integration entry whose cassette parses cleanly but whose isolation
	// (compiler) doesn't dispatch under either replay or reexecute in v0.1.
	// The entry must declare a cassette to get past cassette.Parse — an
	// integration entry without a cassette is rejected as cassette_malformed,
	// a different (and load-bearing) failure mode tested elsewhere.
	yaml := `
unit_id: integration-compiler-unsupported
domain: [test]
failed_approach:
  description: stub
  setup: []
  action: [{type: code, lang: shell, body: "true"}]
  assertion: { type: returns, expect: fail }
working_approach:
  description: stub
  setup: []
  action: [{type: code, lang: shell, body: "true"}]
  assertion: { type: returns, expect: success }
verification:
  type: integration
  isolation: compiler
  cassette:
    mode: reexecute
    artifact: compiler-stub.cassette.yaml
    runtime: { tool: shell }
    captures: [stub]
    strips: [stub]
    replay_targets: [stub]
  differential:
    failed_branch_must_return: { type: string, value_equals: "" }
    working_branch_must_return: { type: string, value_equals: "" }
  mutations:
    - { strategy: mutate_fixture, target: $X, new_value: 1, branch: working_approach, expected_result: fail }
    - { strategy: mutate_fixture, target: $X, new_value: 2, branch: working_approach, expected_result: unchanged }
  timeout_seconds: 5
`
	res, err := Run([]byte(yaml))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "tier_unsupported" {
		t.Fatalf("expected tier_unsupported, got %q (reasons=%v)", res.Status, res.Reasons)
	}
	if !hasReason(res.Reasons, "isolation_unsupported") {
		t.Fatalf("expected isolation_unsupported, got %v", res.Reasons)
	}
}

func TestRunUnknownTierUnsupported(t *testing.T) {
	// A tier that isn't in the switch dispatches to the default arm and gets
	// tier_not_yet_implemented. Guards against regression of the dispatch
	// shape when new tiers are added.
	yaml := strings.Replace(
		k8sAssertionOnly,
		"type: assertion_only",
		"type: speculation",
		1,
	)
	res, err := Run([]byte(yaml))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "tier_unsupported" {
		t.Fatalf("expected tier_unsupported, got %q (reasons=%v)", res.Status, res.Reasons)
	}
	if !hasReason(res.Reasons, "tier_not_yet_implemented") {
		t.Fatalf("expected tier_not_yet_implemented, got %v", res.Reasons)
	}
}

func TestRunMissingVerificationType(t *testing.T) {
	// Strip the leading two-space indent along with the line so the
	// surrounding mapping stays at a uniform indentation level — yaml.v3
	// rejects a child line that's deeper-indented than its siblings.
	yaml := strings.Replace(k8sAssertionOnly, "  type: assertion_only\n", "", 1)
	res, err := Run([]byte(yaml))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "rejected" {
		t.Fatalf("expected rejected, got %q", res.Status)
	}
	if !hasReason(res.Reasons, "missing_verification_type") {
		t.Fatalf("expected missing_verification_type, got %v", res.Reasons)
	}
}

func TestRunInvalidYAML(t *testing.T) {
	if _, err := Run([]byte("::: not yaml :::")); err == nil {
		t.Fatalf("expected YAML parse error, got nil")
	}
}

func hasReason(rs []Reason, code string) bool {
	for _, r := range rs {
		if r.Code == code {
			return true
		}
	}
	return false
}
