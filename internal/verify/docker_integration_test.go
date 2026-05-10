package verify

import (
	"os/exec"
	"strings"
	"testing"
)

// TestReexecuteDockerEndToEnd is the integration sanity gate for the
// docker reexecute driver. It builds a synthetic Entry whose two branches
// echo different values from a one-shot alpine container whose name is
// composed from the per-branch $DOCKER_PREFIX, runs verify.Run, and
// asserts status: "verified".
//
// This test is the smallest end-to-end exercise that covers:
//   - reexecuteSupportedTools accepting "docker"
//   - reexecuteSupportedIsolations accepting "docker_daemon"
//   - runReexecuteBranch provisioning a fresh `runlog-verify-<8-hex>`
//     prefix per branch (and CleanDockerSandbox sweeping it on teardown)
//   - $DOCKER_PREFIX injection through to lang=shell steps under tool=docker
//   - the supported differential shape (value_equals) flowing through
//
// Skips when:
//   - docker not on PATH (the driver shells out to it)
//   - docker daemon unreachable (probed via `docker version`)
//
// Uses a reachability probe (docker version) to verify the daemon is up
// before running, because a stale docker binary (no daemon) would
// otherwise show up as a real failure rather than a clean skip.
func TestReexecuteDockerEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not on PATH")
	}
	probe := exec.Command("docker", "version", "--format", "{{.Server.Version}}")
	probe.Stderr = nil
	probe.Stdout = nil
	if err := probe.Run(); err != nil {
		t.Skipf("docker on PATH but daemon unreachable: %v", err)
	}

	yaml := `unit_id: synthetic-docker-reexecute-smoke
domain: ['docker']
failed_approach:
  description: returns "failed-value"
  setup: []
  action:
    - type: code
      lang: shell
      body: |
        docker run --rm --name $DOCKER_PREFIX-test alpine:3 echo -n failed-value
  assertion:
    type: string
    expect: success
working_approach:
  description: returns "working-value"
  setup: []
  action:
    - type: code
      lang: shell
      body: |
        docker run --rm --name $DOCKER_PREFIX-test alpine:3 echo -n working-value
  assertion:
    type: string
    expect: success
verification:
  type: integration
  isolation: docker_daemon
  cassette:
    mode: reexecute
    artifact: synthetic.cassette.json
    runtime:
      tool: docker
      version: '>=20'
  differential:
    failed_branch_must_return:
      type: string
      value_equals: "failed-value"
    working_branch_must_return:
      type: string
      value_equals: "working-value"
  timeout_seconds: 60
`

	res, err := Run([]byte(yaml))
	if err != nil {
		t.Fatalf("verify.Run returned error: %v", err)
	}
	if res.Status != "verified" {
		// Surface the full result for diagnostic value when the test
		// fails — the reasons array carries the exact rejection codes.
		t.Fatalf("status=%q, want %q\nresult=%+v", res.Status, "verified", res)
	}
	if !strings.Contains(res.UnitID, "synthetic-docker-reexecute-smoke") {
		t.Fatalf("UnitID=%q, want it to contain the synthetic id", res.UnitID)
	}
}
