package verify

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// skipIfDockerUnavailable surfaces a t.Skip when the docker reexecute
// driver's runtime prereqs are missing — docker on PATH, daemon
// reachable. Mirrors skipIfPostgresUnavailable; used by the canonical-
// seed test so each test stays focused on its seed-specific assertions.
func skipIfDockerUnavailable(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not on PATH")
	}
	probe := exec.Command("docker", "version", "--format", "{{.Server.Version}}")
	probe.Stderr = nil
	probe.Stdout = nil
	if err := probe.Run(); err != nil {
		t.Skipf("docker on PATH but daemon unreachable: %v", err)
	}
}

// TestVerifyCanonicalSeedDockerBuildkitCopyLinkCache runs the canonical
// `docker-buildkit-copy-link-cache-survives-base-image-changes.yaml`
// seed end-to-end through verify.Run and asserts status:"verified".
// This is the F95 canonical-seed coverage that proves F94's verifier
// support for type:dockerfile setup steps + fixture_directory
// materialization + chained $LITERAL resolution works end-to-end
// against a real docker daemon.
//
// The seed lives in the umbrella-sibling `runlog/` repo and is loaded
// via a relative path (`../../../runlog/server/seeds/<name>.yaml`) —
// works when runlog-verifier is checked out under the runlog umbrella
// alongside runlog/, which is the canonical dev layout.
//
// Skips when:
//   - the seed file is not at the expected relative path (standalone
//     verifier clone — no umbrella sibling);
//   - docker not on PATH (the docker reexecute driver shells out);
//   - docker daemon unreachable.
func TestVerifyCanonicalSeedDockerBuildkitCopyLinkCache(t *testing.T) {
	seedPath := filepath.Join(
		"..", "..", "..", "runlog", "server", "seeds",
		"docker-buildkit-copy-link-cache-survives-base-image-changes.yaml",
	)
	data, err := os.ReadFile(seedPath)
	if err != nil {
		// Standalone clones of runlog-verifier (no umbrella) won't have
		// the sibling runlog/ checkout — surface as a skip rather than
		// a failure so this test is meaningful only in the canonical
		// layout.
		t.Skipf("canonical seed not at %s: %v", seedPath, err)
	}

	skipIfDockerUnavailable(t)

	res, err := Run(data)
	if err != nil {
		t.Fatalf("verify.Run returned error: %v", err)
	}
	if res.Status != "verified" {
		// Surface the full result so the typed reason codes
		// (e.g. differential_outcome_mismatch, setup_script_failed)
		// are visible on failure.
		t.Fatalf("status=%q, want %q\nresult=%+v", res.Status, "verified", res)
	}
	if !strings.Contains(res.UnitID, "docker-buildkit-copy-link-cache-survives-base-image-changes") {
		t.Fatalf("UnitID=%q, does not contain canonical seed unit_id", res.UnitID)
	}
}
