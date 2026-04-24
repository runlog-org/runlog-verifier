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
