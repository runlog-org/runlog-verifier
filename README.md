# verifier/ — Signed Verification Agent

**Future repo:** `runlog-verifier` — public, Apache-2.0 (planned)
**Language:** Go
**Implements:** [`../docs/03-verification-and-provenance.md`](../docs/03-verification-and-provenance.md)

Tamper-evident, reproducible-build binary distributed via package managers (`brew install runlog-verifier`, apt). Wraps test execution on the submitter's machine, runs both branches (§5.3), applies mutation testing, records integration cassettes (§7.5), and signs the bundle before submission. Target size: ~200 lines, fully auditable.

**Must be public.** The trust model depends on anyone being able to verify that the binary matches the source (§5.4).

## Layout

- `cmd/runlog-verifier/` — entry point
- `internal/differential/` — both-branch executor (§5.3)
- `internal/mutation/` — mutation testing on the working branch
- `internal/cassette/` — HTTP/RPC recorder for integration-tier entries (§7.5)
- `internal/fingerprint/` — OS/runtime/package environment capture
- `internal/sanitize/` — pre-sign allow-list check (§8)
- `internal/sign/` — embedded key + bundle signing
- `internal/token/` — time-limited verification tokens (anti-replay)

## Depends on

- `../schema/` — pinned Go module version
- `../vocabularies/` — pinned data version

## Build properties

- Reproducible: `-trimpath`, `-buildvcs=false`, pinned Go toolchain
- Checksummed releases verified across macOS/Linux/Windows before publishing

## Build

First-time setup on a fresh machine:

    cd verifier/
    go mod tidy        # writes go.sum
    make build         # writes bin/runlog-verifier
    make test          # roundtrip + fingerprint coverage

Reproducible-build flags (`-trimpath -buildvcs=false`) are wired into
the Makefile and validated on every push by
[`.github/workflows/verifier.yml`](../.github/workflows/verifier.yml) —
two consecutive builds must hash identically or CI fails. Signed-release
publishing of tagged binaries is deferred to the first Phase 2 release.

## CLI status

v0.1 stub: structural validation + fingerprint capture + signed bundle.
Differential execution (§5.3) and mutation testing (§5.3) are the
next Phase 2 deliverables — not yet implemented. The CLI's output
shape is stable and the server's `verification_signature` parameter
already accepts (and ignores) bundles in this format.
