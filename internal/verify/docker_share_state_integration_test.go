package verify

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestReexecuteDockerShareState verifies the F87 share-state path against a
// real docker daemon. Same synthetic seed is run twice — share=false (default
// per-mutation isolation) and share=true (F87 shared sandbox). Asserts both
// paths produce status: verified, and that share=true wall-clock is at least
// 15% faster than share=false (lenient threshold catches silent fallback to
// the isolated path without flaking on slow CI).
//
// Skip-gate: docker not on PATH or daemon unreachable.
func TestReexecuteDockerShareState(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not on PATH")
	}
	probe := exec.Command("docker", "version", "--format", "{{.Server.Version}}")
	probe.Stderr = nil
	probe.Stdout = nil
	if err := probe.Run(); err != nil {
		t.Skipf("docker on PATH but daemon unreachable: %v", err)
	}

	// Pre-warm the alpine:3 image so that neither timed run pays the pull cost.
	// Errors are silently discarded — if the pull fails, the actual run will
	// surface a real error.
	_ = exec.Command("docker", "pull", "alpine:3").Run()

	// seedTemplate has SHARE_FLAG as a placeholder for true/false.
	const seedTemplate = `unit_id: synthetic-docker-share-state-smoke
domain: ['docker']
failed_approach:
  description: returns "alpha" via docker
  setup: []
  action:
    - type: code
      lang: shell
      body: |
        docker run --rm --name "$DOCKER_PREFIX-t" alpine:3 echo -n "$VALUE"
  assertion:
    type: string
    expect: success
working_approach:
  description: returns "beta" via docker
  setup: []
  action:
    - type: code
      lang: shell
      body: |
        docker run --rm --name "$DOCKER_PREFIX-t" alpine:3 echo -n "$VALUE"
  assertion:
    type: string
    expect: success
verification:
  type: integration
  isolation: docker_daemon
  cassette:
    mode: reexecute
    artifact: synthetic.cassette.json
    setup_script:
      - 'docker run --rm alpine:3 sleep 1'
    runtime:
      tool: docker
      version: '>=20'
      share_state_across_mutations: SHARE_FLAG
  differential:
    inputs:
      failed_approach: { $VALUE: "alpha" }
      working_approach: { $VALUE: "beta" }
    failed_branch_must_return:
      type: string
      value_equals: "alpha"
    working_branch_must_return:
      type: string
      value_equals: "beta"
  mutations:
    - { strategy: set_literal_value, target: $VALUE, new_value: "perturbed1", expected_result: fail }
    - { strategy: set_literal_value, target: $VALUE, new_value: "perturbed2", expected_result: fail }
  timeout_seconds: 90
`

	run := func(shareFlag string) (time.Duration, error) {
		yaml := strings.ReplaceAll(seedTemplate, "SHARE_FLAG", shareFlag)
		start := time.Now()
		res, err := Run([]byte(yaml))
		elapsed := time.Since(start)
		if err != nil {
			return elapsed, fmt.Errorf("verify.Run returned error: %w", err)
		}
		if res.Status != "verified" {
			return elapsed, errors.New(fmt.Sprintf("status=%q, want %q; result=%+v", res.Status, "verified", res))
		}
		return elapsed, nil
	}

	isolated, err := run("false")
	if err != nil {
		t.Fatalf("share=false run failed: %v (elapsed=%v)", err, isolated)
	}
	shared, err := run("true")
	if err != nil {
		t.Fatalf("share=true run failed: %v (elapsed=%v)", err, shared)
	}

	// Lenient perf threshold: share=true must be at least 15% faster than
	// share=false. The expected gain on this synthetic seed is much higher
	// (~50%+, two setup_script docker-sleep skips across two branches), but a
	// slow CI runner or noisy IO can compress the gap; 15% is the floor below
	// which we suspect silent fallback to the isolated path. Skip the perf
	// assertion entirely if the share=false run is <2s (too small to measure
	// reliably — e.g. no real docker cost).
	t.Logf("share=false: %v, share=true: %v (ratio: %.2f)", isolated, shared, float64(shared)/float64(isolated))
	if isolated < 2*time.Second {
		t.Logf("share=false run was <2s (%v) — skipping perf assertion as too noisy to measure", isolated)
		return
	}
	maxAllowedShared := time.Duration(float64(isolated) * 0.85)
	if shared > maxAllowedShared {
		t.Errorf("share=true wall-clock %v exceeds 85%% of share=false (%v); expected ≤ %v. Possible silent fallback to isolated path.",
			shared, isolated, maxAllowedShared)
	}
}
