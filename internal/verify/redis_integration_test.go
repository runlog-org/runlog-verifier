package verify

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestReexecuteRedisEndToEnd is the integration sanity gate for the redis
// reexecute driver. It builds a synthetic Entry whose two branches write
// different values to a per-branch ephemeral redis database, runs
// verify.Run, and asserts status: "verified".
//
// This test is the smallest end-to-end exercise that covers:
//   - reexecuteSupportedTools accepting "redis"
//   - runReexecuteBranch provisioning a fresh ephemeral Redis DB number
//     per branch (and flushing it on teardown)
//   - $REDIS_URL injection through to lang=shell steps under tool=redis
//   - the supported differential shape (value_equals) flowing through
//
// Skips when:
//   - redis-cli not on PATH (the driver shells out to it)
//   - RUNLOG_VERIFY_REDISURL is unset OR points at an unreachable server
//
// Uses a reachability probe (redis-cli -u $URL PING) to verify the server
// is up before running, because a stale RUNLOG_VERIFY_REDISURL would
// otherwise show up as a real failure rather than a clean skip.
func TestReexecuteRedisEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("redis-cli"); err != nil {
		t.Skip("redis-cli not on PATH")
	}
	redisURL := os.Getenv("RUNLOG_VERIFY_REDISURL")
	if redisURL == "" {
		t.Skip("RUNLOG_VERIFY_REDISURL not set")
	}
	// Reachability probe — a clear skip beats a misleading failure.
	probe := exec.Command("redis-cli", "-u", redisURL, "PING")
	probe.Stderr = nil
	probe.Stdout = nil
	if err := probe.Run(); err != nil {
		t.Skipf("RUNLOG_VERIFY_REDISURL %q is set but server is unreachable: %v", redisURL, err)
	}

	yaml := `unit_id: synthetic-redis-reexecute-smoke
domain: ['redis']
failed_approach:
  description: returns "failed-value"
  setup: []
  action:
    - type: code
      lang: shell
      body: |
        redis-cli -u $REDIS_URL SET k failed-value
        redis-cli -u $REDIS_URL GET k | tr -d '\n'
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
        redis-cli -u $REDIS_URL SET k working-value
        redis-cli -u $REDIS_URL GET k | tr -d '\n'
  assertion:
    type: string
    expect: success
verification:
  type: integration
  isolation: database
  cassette:
    mode: reexecute
    artifact: synthetic.cassette.json
    runtime:
      tool: redis
      version: '>=2.8'
  differential:
    failed_branch_must_return:
      type: string
      value_equals: "failed-value"
    working_branch_must_return:
      type: string
      value_equals: "working-value"
  timeout_seconds: 30
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
	if !strings.Contains(res.UnitID, "synthetic-redis-reexecute-smoke") {
		t.Fatalf("UnitID=%q, want it to contain the synthetic id", res.UnitID)
	}
}
