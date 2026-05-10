# Runlog Verifier — Signed Verification Agent

> Part of the **[Runlog](https://github.com/runlog-org)** project. See the [project home](https://github.com/runlog-org) for the overview.

**Repo:** [`runlog-org/runlog-verifier`](https://github.com/runlog-org/runlog-verifier) (public, Apache-2.0)
**Language:** Go
**Role:** signed verification agent. Runs both branches of an entry locally and signs the resulting bundle. The verification model is summarised at [runlog.org/why-verification/](https://runlog.org/why-verification/).

> **About this project:** Runlog is a hobby side project by [Volker Otto](https://volkerotto.net) — not a commercial product today. A paid model is not ruled out for a later stage. See [About this project](https://runlog.org/#about) for the canonical framing.

Reproducible-build binary distributed via package managers (`brew install runlog-verifier`, apt). Wraps test execution on the submitter's machine, runs both branches (§5.3), records integration cassettes (§7.5), and signs the bundle before submission. Target size: ~200 lines.

**Must be public.** The trust model depends on anyone being able to verify that the binary matches the source (§5.4).

## Install / Update

Fetches the latest release, verifies SHA256, and drops the binary at
`~/.local/bin/runlog-verifier`. Re-run to update; idempotent if you're
already on the latest version:

    curl -sSL https://raw.githubusercontent.com/runlog-org/runlog-verifier/main/install.sh | sh

Pin a version or change the install dir via env vars:

    curl -sSL https://raw.githubusercontent.com/runlog-org/runlog-verifier/main/install.sh \
      | VERSION=v0.2.0 INSTALL_DIR=$HOME/bin sh

On Debian/Ubuntu, prebuilt `.deb` packages are attached to every release
(amd64, arm64). No `apt` repo to add; download and `dpkg -i`:

    VER=0.3.0  # latest release; check https://github.com/runlog-org/runlog-verifier/releases
    ARCH=amd64
    curl -fsSL -o /tmp/runlog-verifier.deb \
      "https://github.com/runlog-org/runlog-verifier/releases/download/v${VER}/runlog-verifier_${VER}_${ARCH}.deb"
    sudo dpkg -i /tmp/runlog-verifier.deb

The script is plain POSIX `sh` (`install.sh` at the repo root). It supports
`linux-amd64`, `linux-arm64`, `darwin-amd64`, `darwin-arm64`. For other
platforms, build from source per the [Build](#build) section below.

## Getting started

After installing, run a single command to register an account and provision
the local signing keypair:

    runlog-verifier register --email you@example.com

The command kicks off a registration with the server, opens the verification
URL in your browser (use `--no-browser` on a headless box and click the
printed URL from another machine), polls until you confirm, then persists
the issued API key to `~/.runlog/config.json` (mode `0600`) and uploads the
local Ed25519 public key from `~/.runlog/key`. After it returns, you can
immediately verify entries:

    runlog-verifier verify path/to/entry.yaml

`verify` reads the API key from `RUNLOG_API_KEY` if set, otherwise falls
back to `~/.runlog/config.json` — so once `register` succeeds, no further
environment setup is required.

## Runtime requirements

The verifier shells out to host-installed tools to drive each cassette
runtime. There's no embedded interpreter, no Docker, no implicit setup.

| Cassette runtime          | Required binaries on PATH | Required env                                                                                              |
|---------------------------|---------------------------|-----------------------------------------------------------------------------------------------------------|
| `tool: shell`             | `sh`                      | —                                                                                                         |
| `tool: sqlite`            | `sqlite3`                 | —                                                                                                         |
| `tool: postgres`          | `psql`                    | `RUNLOG_VERIFY_PGURL` (default `postgres://localhost:5432/postgres`); the role must have `CREATEDB` privilege. |
| `tool: redis`             | `redis-cli`               | `RUNLOG_VERIFY_REDISURL` (default `redis://localhost:6379`).                                              |
| `tool: docker`            | `docker`                  | — (uses `DOCKER_HOST` env / default socket); the user must be able to run `docker` without `sudo`.        |

The postgres driver provisions a fresh ephemeral `runlog_verify_<rand>`
database per branch (and per mutation re-run) via `CREATE DATABASE`,
and drops it on teardown. Stale databases from a crashed verifier can
be swept with `psql -c "SELECT datname FROM pg_database WHERE datname
LIKE 'runlog_verify_%'"`.

Quick local Postgres for testing:

    # Docker (one-shot, deletes itself on stop)
    docker run --rm -p 5432:5432 -e POSTGRES_HOST_AUTH_METHOD=trust postgres:16

    # Then point the verifier at it:
    export RUNLOG_VERIFY_PGURL=postgres://postgres@localhost:5432/postgres

The docker driver provisions a fresh `runlog-verify-<8-hex>` sandbox
prefix per branch (and per mutation re-run); the seed composes the
prefix into its own container / image / network / buildx-builder names
via `$DOCKER_PREFIX`, and teardown sweeps every prefix-matched resource
best-effort. Stale resources from a crashed verifier can be swept
manually with `docker ps -aq --filter 'name=^runlog-verify-' | xargs -r
docker rm -f` (and similar for `images` / `network ls` / `buildx ls`).

> **docker driver perf note.** Each branch run and each mutation re-run
> starts containers and (for buildx-using seeds) initialises buildx —
> typically ~30 s overhead per invocation. A 5-mutation seed can run
> ~2.5 min minimum on a cold cache. Per-mutation container reuse is a
> deferred optimisation; today every mutation is fully isolated.

Verifier function-tier (`isolation: function`) entries continue to use
the embedded Python driver and need only `python3` on PATH.

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
[`.github/workflows/verifier.yml`](./.github/workflows/verifier.yml).
Two consecutive builds must hash identically or CI fails. Signed-release
publishing of tagged binaries is deferred to the first Phase 2 release.

## CLI status

`assertion_only` entries are fully verified declaratively: branch
presence, non-tautology, mutation structure (schema rules §1–§3),
mutation discrimination (§5.3 step 4), and the primitives allow-list
all run on every `verify` call, producing a signed JSON bundle.

`unit` and `integration` tiers parse but exit with status
`tier_unsupported` (exit code 4). Subprocess execution and cassette
replay are still to land in Phase 2. The CLI's output shape is stable
and the server's `verification_signature` parameter already accepts
(and ignores) bundles in this format.

Exit codes: `0` verified, `1` user error, `2` internal error,
`3` rejected, `4` tier not yet implemented.
