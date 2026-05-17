package verify

import (
	"os/exec"
	"strings"
	"testing"
)

// TestReexecuteSidecarEndToEnd is the integration sanity gate for the
// fixture_sidecar_process lifecycle. It builds a synthetic Entry that
// declares a sidecar (a shell loop appending to a workdir file every
// 50ms) and two trivially-discriminating branches whose actions do NOT
// depend on the sidecar. The test asserts that:
//
//   - verify.Run returns status: "verified" (proves Start + Stop both
//     succeeded — a startup failure would surface as a sidecar_* reason
//     code and a stop failure would leak the process but not affect the
//     status, so this lower-bounds the lifecycle works).
//   - No reason code is prefixed with "sidecar_" (defense in depth — if
//     the verified status ever stops being a tight gate, the explicit
//     scan keeps the lifecycle assertion honest).
//
// The sidecar itself is not exercised by the action — that's deliberate.
// A test where the action depends on the sidecar would conflate the
// driver lifecycle with substitution, env wiring, and the action's own
// shell semantics. The richer mutation-driven scenario lands with the
// canonical-seed migration in the F-NN-A follow-up.
//
// Skips when sh isn't on PATH (the synthetic sidecar shell-loops via sh).
func TestReexecuteSidecarEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not on PATH")
	}

	yaml := `unit_id: synthetic-sidecar-reexecute-smoke
domain: ['shell']
failed_approach:
  description: echoes "failed-side"
  setup: []
  action:
    - type: code
      lang: shell
      body: |
        printf failed-side
  assertion:
    type: string
    expect: fail
working_approach:
  description: echoes "working-side"
  setup: []
  action:
    - type: code
      lang: shell
      body: |
        printf %s $F36S
  assertion:
    type: string
    expect: success
verification:
  type: integration
  isolation: subprocess
  cassette:
    mode: reexecute
    artifact: synthetic.cassette.json
    runtime:
      tool: shell
    fixtures:
      $FIXTURE_BG_WRITER:
        kind: sidecar_process
        command:
          - sh
          - -c
          - 'while true; do echo tick >/dev/null; sleep 0.05; done'
        ready_when:
          delay_seconds: 0.05
  differential:
    inputs:
      $F36S: working-side
    failed_branch_must_return:
      type: string
      value_equals: "failed-side"
    working_branch_must_return:
      type: string
      value_equals: "working-side"
  mutations:
    - strategy: mutate_fixture
      target: $F36S
      new_value: "runlog_f36_break"
      branch: working_approach
      expected_result: fail
    - strategy: mutate_fixture
      target: $F36S
      new_value: working-side
      branch: working_approach
      expected_result: unchanged
  timeout_seconds: 10
`

	res, err := Run([]byte(yaml))
	if err != nil {
		t.Fatalf("verify.Run returned error: %v", err)
	}

	// Defense in depth: any sidecar_* rejection code surfaces a lifecycle
	// failure even if status somehow ended up "verified" (it shouldn't —
	// startup failure is a hard reject — but the explicit scan here makes
	// the intent obvious).
	for _, r := range res.Reasons {
		if strings.HasPrefix(r.Code, "sidecar_") {
			t.Fatalf("sidecar-related rejection: code=%q msg=%q", r.Code, r.Message)
		}
	}

	if res.Status != "verified" {
		t.Fatalf("status=%q, want %q\nresult=%+v", res.Status, "verified", res)
	}
	if !strings.Contains(res.UnitID, "synthetic-sidecar-reexecute-smoke") {
		t.Fatalf("UnitID=%q, want it to contain the synthetic id", res.UnitID)
	}
}
