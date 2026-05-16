# M04 post-acceptance smoke test

End-to-end validation of the public chain a real user walks: install →
register → verify → submit. Run this on a **clean machine with no Go
toolchain** after every stable `runlog-verifier` release. It exists to
catch the class of regression that broke `brew install` on 2026-05-16
(release-asset / formula sha256 desync — see `RELEASING.md` "Manual
recovery" and backlog items T98–T100).

This is the operator runbook for backlog item **T95**. The steps marked
**[machine-checked]** are verified automatically from the dev box by the
`/hv-work` cycle that maintains this file; the steps marked **[manual]**
require a clean environment and a human, and are the reason T95 cannot be
closed from CI or the dev box alone.

## Preconditions

- A machine (VM, fresh container, or laptop) with **no Go installed** —
  `command -v go` must return nothing. This proves the published binary
  runs standalone, not a locally-built one.
- macOS with Homebrew, or Linux with `curl` (and optionally `apt`/`dpkg`
  for the `.deb` path).
- Network access to `github.com` and `api.runlog.org`.
- A mailbox you can open to click the verification link.

## Machine-checked preflight (already automated)

These are asserted by the maintaining `/hv-work` cycle before this file
is updated; re-check by hand only if a release looks suspect:

1. The latest stable tag's GitHub release carries all four binaries
   (`darwin-amd64`, `darwin-arm64`, `linux-amd64`, `linux-arm64`) plus
   `SHA256SUMS`.
2. Every `sha256` in `homebrew-runlog/Formula/runlog-verifier.rb` equals
   the matching line in the release `SHA256SUMS`. Verify manually with:

   ```sh
   gh release download <tag> --repo runlog-org/runlog-verifier \
     --pattern SHA256SUMS --output -
   # diff each line against the formula's sha256 values
   ```

3. `https://api.runlog.org/health` returns `200`; `/register` returns
   `405` on `GET` (it is POST-only).
4. The verifier CLI exposes `register`, `verify`, `keygen`, `version`;
   the server exposes `POST /register`, `POST /register/verify`,
   `POST /v1/register-pubkey`, and the `runlog_submit` MCP tool.

If any preflight item fails, **stop** — the release is broken; do not
proceed to the manual steps.

## Manual run (clean machine)

### 1. Install — [manual]

Homebrew:

```sh
brew install runlog-org/runlog/runlog-verifier
runlog-verifier --version    # must print the expected stable version
```

Or the raw binary (no Homebrew):

```sh
curl -fsSL -o runlog-verifier \
  https://github.com/runlog-org/runlog-verifier/releases/download/<tag>/runlog-verifier-$(uname -s | tr A-Z a-z)-amd64
chmod +x runlog-verifier
./runlog-verifier --version
```

Confirm `command -v go` is still empty — the binary must run with no Go.

### 2. Register — [manual]

```sh
runlog-verifier register --email you@example.com
```

Expected: the command reports a verification email was sent. **Open the
mailbox, click the verification link.** The link hits
`POST /register/verify` on `api.runlog.org`; the page should confirm the
key is active.

### 3. Verify a seed — [manual]

Use any known-good cassette/seed (e.g. one from `tests/`):

```sh
runlog-verifier verify some-seed.yaml
```

Expected: `status: verified` and a signed bundle written to disk. Any
other status (`tier_unsupported`, `failed`, …) is a failure for this
smoke test unless the seed itself is intentionally unsupported.

### 4. Submit — [manual]

From an MCP client configured against `api.runlog.org`, call
`runlog_submit` with the signed bundle from step 3. Expected: the
submission is accepted and the entry becomes searchable via
`runlog_search` shortly after.

## Recording the result

T95 stays **open** until a full manual run on a clean machine passes.
When it does, close it with the run date, the machine description
("clean Ubuntu 24.04 container, no Go"), the release tag tested, and the
resulting entry slug, then mark M04 shippable.

If any **[manual]** step fails, capture a P0 bug with the exact command,
output, and release tag — a broken public chain blocks the milestone.
