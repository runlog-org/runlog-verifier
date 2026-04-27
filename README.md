# Runlog Verifier — Signed Verification Agent

**Repo:** [`runlog-org/runlog-verifier`](https://github.com/runlog-org/runlog-verifier) — public, Apache-2.0
**Language:** Go
**Implements:** [`runlog-docs/03-verification-and-provenance.md`](https://github.com/runlog-org/runlog-docs/blob/main/03-verification-and-provenance.md)

Tamper-evident, reproducible-build binary distributed via package managers (`brew install runlog-verifier`, apt). Wraps test execution on the submitter's machine, runs both branches (§5.3), applies mutation testing, records integration cassettes (§7.5), and signs the bundle before submission. Target size: ~200 lines, fully auditable.

**Must be public.** The trust model depends on anyone being able to verify that the binary matches the source (§5.4).

## Layout

- `cmd/runlog-verifier/` — entry point
- `internal/verify/` — declarative verification of `assertion_only` entries
  (branch presence, non-tautology, mutation structure + discrimination,
  primitives allow-list)
- `internal/differential/` — both-branch executor for `unit` tier (§5.3) — to land
- `internal/mutation/` — mutation testing on the working branch — to land
- `internal/cassette/` — HTTP/RPC recorder for integration-tier entries (§7.5) — to land
- `internal/fingerprint/` — OS/runtime/package environment capture
- `internal/sanitize/` — pre-sign allow-list check (§8) — to land
- `internal/sign/` — embedded key + bundle signing
- `internal/token/` — time-limited verification tokens (anti-replay) — to land

## Depends on

- [`runlog-org/runlog-schema`](https://github.com/runlog-org/runlog-schema) — pinned Go module version
- [`runlog-org/runlog-vocabularies`](https://github.com/runlog-org/runlog-vocabularies) — pinned data version

## Build properties

- Reproducible: `-trimpath`, `-buildvcs=false`, pinned Go toolchain
- Checksummed releases verified across macOS/Linux/Windows before publishing

## Build

First-time setup on a fresh machine:

    go mod tidy        # writes go.sum
    make build         # writes bin/runlog-verifier
    make test          # roundtrip + fingerprint coverage

Reproducible-build flags (`-trimpath -buildvcs=false`) are wired into
the Makefile and validated on every push by
[`.github/workflows/verifier.yml`](./.github/workflows/verifier.yml) —
two consecutive builds must hash identically or CI fails. Signed-release
publishing of tagged binaries is deferred to the first Phase 2 release.

## CLI status

`assertion_only` entries are fully verified declaratively: branch
presence, non-tautology, mutation structure (schema rules §1–§3),
mutation discrimination (§5.3 step 4), and the primitives allow-list
all run on every `verify` call, producing a signed JSON bundle.

`unit` and `integration` tiers parse but exit with status
`tier_unsupported` (exit code 4) — subprocess execution and cassette
replay are still to land in Phase 2. The CLI's output shape is stable
and the server's `verification_signature` parameter already accepts
(and ignores) bundles in this format.

Exit codes: `0` verified, `1` user error, `2` internal error,
`3` rejected, `4` tier not yet implemented.
