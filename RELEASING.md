# Releasing runlog-verifier

Releases are cut by pushing a version tag. The
[`release`](.github/workflows/release.yml) workflow does the rest:
cross-compiles four binaries with the same reproducibility flags as
`make build`, builds each one twice **within the run** and fails fast
if the pair diverges, generates a `SHA256SUMS` manifest, and creates a
GitHub Release with the artefacts attached.

> **Reproducibility — scope.** The twice-and-compare check proves
> *intra-run* determinism: the same toolchain, same source, same run
> yields the same bytes. *Cross-run* reproduction (rebuilding the same
> tag weeks later and getting identical hashes) holds only because the
> workflow pins an **exact** Go patch version
> (`actions/setup-go` `go-version: "1.26.0"`) rather than the floating
> `"1.26"` minor. With a floating minor, `setup-go` silently upgrades
> to the latest patch at run time and the binary changes — this is what
> broke `brew install` on 2026-05-16. Cross-run reproduction is
> therefore guaranteed *only against the exact pinned toolchain*;
> bumping that pin is a deliberate change and should be called out in
> the release notes.

> **A release run is one-shot for asset publication.** Re-running the
> `release` workflow on a tag whose Release already carries assets
> **skips** the Release-creation step rather than re-uploading over the
> originals — preventing the desync of published assets from the
> `homebrew-runlog` formula's pinned `sha256` and the original
> `SHA256SUMS`. The downstream `update-tap` job still runs, so a rerun
> remains a valid recovery path when only the tap bump failed (see
> [Homebrew tap auto-bump](#homebrew-tap-auto-bump)). To publish
> *changed* binaries you must cut a **new tag** — a rerun will never
> replace already-published assets.

## Cut a release

1. Make sure `main` is green and you're on it.

       git checkout main && git pull --ff-only

2. Tag and push. The canonical shape is plain `vX.Y.Z`:

       git tag -a v0.2.0 -m "v0.2.0"
       git push origin v0.2.0

   The path-scoped shape `verifier/vX.Y.Z` is also accepted by the
   workflow (a soft-cut concession for the brief window when the M02
   release-train convention defaulted to path-scoping for every repo),
   but it should not be used: `runlog-verifier` has `go.mod` at the
   repo root, and Go's module proxy (`proxy.golang.org` /
   `pkg.go.dev`) only resolves tags of the shape
   `<module-root>/vX.Y.Z`. A `verifier/vX.Y.Z` tag is invisible to the
   proxy and consumers cannot `go get` against it. See
   [`runlog-docs/13-release-trains.md`](https://github.com/runlog-org/runlog-docs/blob/main/13-release-trains.md#181-the-path-scoped-tag-convention)
   for the broader convention and the Go-module-at-root carve-out.

   Tags matching `*-rc*`, `*-beta*`, or `*-alpha*` produce a **draft**
   Release; everything else is published immediately. The prerelease
   regex matches the suffix and works on both tag shapes.

3. Watch the workflow run on GitHub Actions. On success, the Release
   page lists four binaries plus `SHA256SUMS`:

   - `runlog-verifier-linux-amd64`
   - `runlog-verifier-linux-arm64`
   - `runlog-verifier-darwin-amd64`
   - `runlog-verifier-darwin-arm64`
   - `runlog-verifier_X.Y.Z_amd64.deb`
   - `runlog-verifier_X.Y.Z_arm64.deb`
   - `SHA256SUMS`

## Verify a release locally

End users (and you, before announcing) can confirm the published binary
matches its source by re-running the same build and comparing hashes:

    # Download the binary + SHA256SUMS from the Release page, then:
    sha256sum -c --ignore-missing SHA256SUMS

To rebuild from source and compare:

    # Use the EXACT Go toolchain the release workflow pinned
    # (go-version in .github/workflows/release.yml — currently 1.26.0).
    # A different patch release will produce different bytes; that is a
    # toolchain mismatch, not a tampered binary.
    go version                       # must match the pinned version
    git checkout v0.2.0
    make release
    diff <(sort -k2 dist/SHA256SUMS) <(sort -k2 /path/to/downloaded/SHA256SUMS)

Identical hashes prove the published binary was built from this source
with the pinned toolchain — the trust assumption documented in
`docs/03-verification-and-provenance.md` §5.4. A hash *mismatch* means
either the source/toolchain differs or the asset was altered; check
your local `go version` against the pinned version before suspecting
tampering.

## Install via `.deb` (Linux)

The release attaches `.deb` packages for `linux-amd64` and `linux-arm64`. v0
ships them via GitHub Releases with no signing — users verify against
`SHA256SUMS` (trust-on-first-use), the same model as the Homebrew tap.

    VER=0.3.0  # replace with the desired release
    ARCH=amd64
    curl -fsSL -o /tmp/runlog-verifier.deb \
      "https://github.com/runlog-org/runlog-verifier/releases/download/v${VER}/runlog-verifier_${VER}_${ARCH}.deb"
    sudo dpkg -i /tmp/runlog-verifier.deb

To verify the download against the published trust anchor:

    curl -fsSL -o /tmp/SHA256SUMS \
      "https://github.com/runlog-org/runlog-verifier/releases/download/v${VER}/SHA256SUMS"
    (cd /tmp && sha256sum -c --ignore-missing SHA256SUMS)

A self-hosted, signed apt repo (Caddy + reprepro) is on the roadmap if
adoption warrants — see the [`runlog-verifier`](https://github.com/runlog-org/runlog-verifier)
roadmap.

## Homebrew tap auto-bump

The `update-tap` job in [`release.yml`](.github/workflows/release.yml) runs after
`build` on every stable tag (drafts are skipped) and pushes a refreshed
`Formula/runlog-verifier.rb` to [`runlog-org/homebrew-runlog`](https://github.com/runlog-org/homebrew-runlog).

End users install with:

    brew install runlog-org/runlog/runlog-verifier

### One-time secret setup

The job needs a fine-grained Personal Access Token (or GitHub App
installation token) with **`Contents: Read and write`** on
`runlog-org/homebrew-runlog`, stored as the repo secret
**`HOMEBREW_TAP_TOKEN`** in `runlog-org/runlog-verifier`.

If the secret is missing the job logs a warning and exits cleanly — the
release itself still succeeds. To recover after fixing the secret,
re-run the failed workflow run from the Actions tab.

### Manual recovery

If a tap bump fails for any reason, the formula can be edited by hand
in `runlog-org/homebrew-runlog`. The next stable tag will overwrite it
from the template, so manual fixes are stop-gaps.

## Smoke the workflow without a tag

The release workflow accepts `workflow_dispatch` for manual smoke runs.
Trigger it from the Actions tab; it builds and verifies the artefacts
but skips the Release-creation step.

`make release` reproduces the workflow's Go invocations locally.
