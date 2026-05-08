// Shared one-shot CLI helper for the verifier's per-branch ephemeral-resource
// provisioners (postgres CREATE/DROP DATABASE, redis FLUSHDB). Both helpers
// share the same shape — exec a single CLI invocation with a generous timeout,
// capture stderr, wrap non-zero exits as a typed sentinel error with a
// caller-supplied label — so the scaffolding lives here once.
//
// Design note: each provisioner keeps its own public API
// (`ProvisionPostgresDB`, `ProvisionRedisDB`, …) and its own typed sentinel
// (`ErrPostgresProvision`, `ErrRedisProvision`, …) so callers in
// reexecute.go can route on `errors.Is` per-tool. The helper only unifies the
// exec + stderr-capture + error-wrap mechanics — NOT the per-tool API surface.
//
// When a third one-shot CLI provisioner lands (docker create / docker rm under
// F71, or similar), it should declare its own sentinel + thin wrapper around
// `execProvisionCLI`, mirroring how postgres.go / redis.go do today.

package runner

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// provisionCLITimeout caps every one-shot provisioning CLI invocation
// (CREATE DATABASE, DROP DATABASE, FLUSHDB, …). Generous because the
// connecting server may be on a remote host. Per-tool overrides would
// belong on the function signature, not here, if a future tool needs a
// different cap.
const provisionCLITimeout = 30 * time.Second

// execProvisionCLI runs `bin args...` with a provisioning-grade timeout,
// capturing stderr. On non-zero exit (or any other run error), returns
// `fmt.Errorf("%w: <label>: <err>: <stderr>", sentinel)` so callers can
// surface a typed reason via `errors.Is(err, sentinel)` while keeping the
// label + stderr context for diagnostics.
//
// label is a free-form caller-supplied string spliced into the error
// message — typically the SQL statement (postgres) or the redis-cli
// argument list. Keep it short; long values get included verbatim.
//
// The helper does NOT:
//   - capture stdout (provisioning CLIs are side-effect-only; their stdout
//     is noise on success and redundant with stderr on failure);
//   - shell out via `sh -c` (exec.CommandContext invokes the binary
//     directly — no shell-injection surface).
func execProvisionCLI(bin string, args []string, sentinel error, label string) error {
	ctx, cancel := context.WithTimeout(context.Background(), provisionCLITimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w: %s: %v: %s",
			sentinel, label, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}
