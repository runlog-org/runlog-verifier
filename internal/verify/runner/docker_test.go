package runner

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"
)

// skipIfNoDocker skips when `docker` is not on PATH or `docker version`
// fails (daemon unreachable, permission denied, etc.). The three-tier skip
// gate distinguishes "no daemon" from "wrong test expectation": without the
// reachability probe a stale daemon socket (binary on PATH but daemon down)
// would fail the test rather than skip cleanly.
func skipIfNoDocker(t *testing.T) {
	t.Helper()
	skipIfNoBin(t, "docker")
	probe := exec.Command("docker", "version", "--format", "{{.Server.Version}}")
	probe.Stderr = nil
	probe.Stdout = nil
	if err := probe.Run(); err != nil {
		t.Skipf("docker on PATH but daemon unreachable: %v", err)
	}
}

// TestRandomSandboxSuffixUnique sanity-checks that the random-suffix helper
// doesn't collide across many calls. With 32 bits of entropy a 100-call run
// would have a ~1-in-100M collision chance — effectively never.
func TestRandomSandboxSuffixUnique(t *testing.T) {
	seen := make(map[string]bool, 100)
	for i := 0; i < 100; i++ {
		s, err := randomSandboxSuffix()
		if err != nil {
			t.Fatalf("randomSandboxSuffix call %d: %v", i, err)
		}
		if len(s) != 8 {
			t.Fatalf("randomSandboxSuffix returned %q (len %d), want 8 hex chars", s, len(s))
		}
		if seen[s] {
			t.Fatalf("duplicate suffix %q at call %d", s, i)
		}
		seen[s] = true
	}
}

// TestErrDockerProvisionWraps verifies that errors.Is recognises the
// sentinel through fmt.Errorf %w wrapping — the same shape callers in
// reexecute.go use to route to typed reasons.
func TestErrDockerProvisionWraps(t *testing.T) {
	wrapped := fmt.Errorf("%w: contrived: %v", ErrDockerProvision, errors.New("boom"))
	if !errors.Is(wrapped, ErrDockerProvision) {
		t.Fatalf("errors.Is failed to recognise ErrDockerProvision through %%w wrapping; err=%v", wrapped)
	}
	if !strings.Contains(wrapped.Error(), "boom") {
		t.Fatalf("wrapped error %q lost the underlying cause", wrapped.Error())
	}
}

// TestDockerSandboxIDFormat verifies the prefix + suffix shape the
// orchestrator + manual-sweep tooling depends on. The `runlog-verify-`
// prefix is load-bearing for `docker ps -aq --filter
// 'name=^runlog-verify-' | xargs docker rm -f` style recovery.
func TestDockerSandboxIDFormat(t *testing.T) {
	skipIfNoDocker(t)
	id, err := ProvisionDockerSandbox()
	if err != nil {
		t.Fatalf("ProvisionDockerSandbox: %v", err)
	}
	t.Cleanup(func() { _ = CleanDockerSandbox(id) })

	if !strings.HasPrefix(id, "runlog-verify-") {
		t.Fatalf("sandbox id %q lacks the runlog-verify- prefix", id)
	}
	wantLen := len("runlog-verify-") + 8
	if len(id) != wantLen {
		t.Fatalf("sandbox id %q has length %d, want %d (prefix + 8 hex)",
			id, len(id), wantLen)
	}
}

// TestProvisionDockerSandbox_Reachable is the integration-tier success
// path: with the daemon up, provisioning returns a fresh prefix and no
// error. Skip-gated by the three-tier check.
func TestProvisionDockerSandbox_Reachable(t *testing.T) {
	skipIfNoDocker(t)
	id, err := ProvisionDockerSandbox()
	if err != nil {
		t.Fatalf("ProvisionDockerSandbox: %v", err)
	}
	t.Cleanup(func() { _ = CleanDockerSandbox(id) })

	if id == "" {
		t.Fatalf("ProvisionDockerSandbox returned empty id with no error")
	}
}

// TestCleanDockerSandbox_Idempotent verifies cleanup of a never-used
// prefix succeeds twice in a row — the orchestrator's deferred teardown
// runs even when no resources were created (e.g. a setup_script failure
// before the action ran).
func TestCleanDockerSandbox_Idempotent(t *testing.T) {
	skipIfNoDocker(t)
	id, err := ProvisionDockerSandbox()
	if err != nil {
		t.Fatalf("ProvisionDockerSandbox: %v", err)
	}
	if err := CleanDockerSandbox(id); err != nil {
		t.Fatalf("first CleanDockerSandbox: %v", err)
	}
	if err := CleanDockerSandbox(id); err != nil {
		t.Fatalf("second CleanDockerSandbox (idempotent): %v", err)
	}
	// Empty id must be a no-op too — the orchestrator may call cleanup
	// before provisioning has run on a sandbox-alloc-failed path.
	if err := CleanDockerSandbox(""); err != nil {
		t.Fatalf("CleanDockerSandbox(\"\"): %v", err)
	}
}

// TestCleanDockerSandbox_RemovesContainer is the end-to-end resource
// proof: start a container with the sandbox prefix, call cleanup, confirm
// `docker ps -aq --filter` returns empty.
func TestCleanDockerSandbox_RemovesContainer(t *testing.T) {
	skipIfNoDocker(t)
	id, err := ProvisionDockerSandbox()
	if err != nil {
		t.Fatalf("ProvisionDockerSandbox: %v", err)
	}
	t.Cleanup(func() { _ = CleanDockerSandbox(id) })

	containerName := id + "-test"
	runOut, err := exec.Command("docker", "run", "-d",
		"--name", containerName, "alpine:3", "sleep", "30").CombinedOutput()
	if err != nil {
		t.Skipf("docker run alpine:3 failed (image-pull or network gap?): %v: %s",
			err, strings.TrimSpace(string(runOut)))
	}

	// Confirm the container is visible before cleanup.
	preCleanup, err := exec.Command("docker", "ps", "-aq",
		"--filter", "name=^"+id).Output()
	if err != nil {
		t.Fatalf("pre-cleanup ps: %v", err)
	}
	if strings.TrimSpace(string(preCleanup)) == "" {
		t.Fatalf("expected container with prefix %q to be visible before cleanup", id)
	}

	if err := CleanDockerSandbox(id); err != nil {
		t.Fatalf("CleanDockerSandbox: %v", err)
	}

	postCleanup, err := exec.Command("docker", "ps", "-aq",
		"--filter", "name=^"+id).Output()
	if err != nil {
		t.Fatalf("post-cleanup ps: %v", err)
	}
	if got := strings.TrimSpace(string(postCleanup)); got != "" {
		t.Fatalf("expected no containers with prefix %q after cleanup, got ids: %q",
			id, got)
	}
}
