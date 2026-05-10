package verify

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestReexecutePostgresEndToEnd is the integration sanity gate for the
// postgres reexecute driver. It builds a synthetic Entry whose two
// branches return different string values from a per-branch ephemeral
// postgres database, runs verify.Run, and asserts status: "verified".
//
// This test is the smallest end-to-end exercise that covers:
//   - reexecuteSupportedTools accepting "postgres"
//   - runReexecuteBranch provisioning a fresh runlog_verify_<rand> DB
//     per branch (and dropping it on teardown)
//   - $DATABASE_URL injection through to lang=sql AND lang=shell steps
//     under tool=postgres
//   - the supported differential shape (value_equals) flowing through
//
// Skips when:
//   - psql not on PATH (the driver shells out to it)
//   - RUNLOG_VERIFY_PGURL is unset OR points at an unreachable server
//
// Uses a simple connect probe (psql --dbname=$PGURL -c "SELECT 1") to
// verify reachability before running, because a stale RUNLOG_VERIFY_PGURL
// would otherwise show up as a real failure rather than a skip.
func TestReexecutePostgresEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("psql"); err != nil {
		t.Skip("psql not on PATH")
	}
	pgurl := os.Getenv("RUNLOG_VERIFY_PGURL")
	if pgurl == "" {
		t.Skip("RUNLOG_VERIFY_PGURL not set")
	}
	// Reachability probe — a clear skip beats a misleading failure.
	probe := exec.Command("psql",
		"--dbname="+pgurl,
		"--no-psqlrc", "-X", "-q",
		"-v", "ON_ERROR_STOP=1",
		"-c", "SELECT 1",
	)
	probe.Stderr = nil
	probe.Stdout = nil
	if err := probe.Run(); err != nil {
		t.Skipf("RUNLOG_VERIFY_PGURL %q is set but server is unreachable: %v", pgurl, err)
	}

	yaml := `unit_id: synthetic-postgres-reexecute-smoke
domain: ['postgresql']
failed_approach:
  description: returns "failed-value"
  setup:
    - type: code
      lang: sql
      body: |
        CREATE TABLE kv (k text, v text);
        INSERT INTO kv VALUES ('x', 'failed-value');
  action:
    - type: code
      lang: shell
      body: |
        psql -t -A --dbname=$DATABASE_URL -c "SELECT v FROM kv WHERE k='x'" | tr -d '\n'
  assertion:
    type: string
    expect: success
working_approach:
  description: returns "working-value"
  setup:
    - type: code
      lang: sql
      body: |
        CREATE TABLE kv (k text, v text);
        INSERT INTO kv VALUES ('x', 'working-value');
  action:
    - type: code
      lang: shell
      body: |
        psql -t -A --dbname=$DATABASE_URL -c "SELECT v FROM kv WHERE k='x'" | tr -d '\n'
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
      tool: postgres
      version: '>=12'
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
	if !strings.Contains(res.UnitID, "synthetic-postgres-reexecute-smoke") {
		t.Fatalf("UnitID=%q, want it to contain the synthetic id", res.UnitID)
	}
}
