package verify

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestVerifyCanonicalSeedJsonbContainment runs the canonical
// `postgres-jsonb-containment-requires-gin-not-btree.yaml` seed end-to-end
// through verify.Run and asserts status:"verified". This is the
// canonical-seed half of F78 — replaces the synthetic-seed coverage of
// F69 T4 (postgres_integration_test.go) with a test that exercises the
// F77 plan_node-containment differential shape against a real EXPLAIN.
//
// The seed lives in the umbrella-sibling `runlog/` repo and is loaded via
// a relative path (`../../../runlog/server/seeds/<name>.yaml`) — works
// when runlog-verifier is checked out under the runlog umbrella alongside
// runlog/, which is the canonical dev layout.
//
// Skips when:
//   - the seed file is not at the expected relative path (standalone
//     verifier clone — no umbrella sibling);
//   - psql is not on PATH (the postgres reexecute driver shells out);
//   - RUNLOG_VERIFY_PGURL is unset OR points at an unreachable server.
func TestVerifyCanonicalSeedJsonbContainment(t *testing.T) {
	seedPath := filepath.Join(
		"..", "..", "..", "runlog", "server", "seeds",
		"postgres-jsonb-containment-requires-gin-not-btree.yaml",
	)
	data, err := os.ReadFile(seedPath)
	if err != nil {
		// Standalone clones of runlog-verifier (no umbrella) won't have
		// the sibling runlog/ checkout — surface as a skip rather than a
		// failure so this test is meaningful only in the canonical layout.
		t.Skipf("canonical seed not at %s: %v", seedPath, err)
	}

	if _, err := exec.LookPath("psql"); err != nil {
		t.Skip("psql not on PATH")
	}
	pgurl := os.Getenv("RUNLOG_VERIFY_PGURL")
	if pgurl == "" {
		t.Skip("RUNLOG_VERIFY_PGURL not set")
	}
	// Reachability probe — a clear skip beats a misleading failure when
	// RUNLOG_VERIFY_PGURL is set but the server is gone.
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

	res, err := Run(data)
	if err != nil {
		t.Fatalf("verify.Run returned error: %v", err)
	}
	if res.Status != "verified" {
		// Surface the full result so the typed reason codes
		// (e.g. differential_plan_node_mismatch) are visible on failure.
		t.Fatalf("status=%q, want %q\nresult=%+v", res.Status, "verified", res)
	}
	if !strings.Contains(res.UnitID, "postgres-jsonb-containment-requires-gin-not-btree") {
		t.Fatalf("UnitID=%q, does not contain canonical seed unit_id", res.UnitID)
	}
}
