# Releasing runlog-verifier

Releases are cut by pushing a version tag. The
[`release`](.github/workflows/release.yml) workflow does the rest:
cross-compiles four binaries with the same reproducibility flags as
`make build`, checks each one builds byte-identically twice, generates
a `SHA256SUMS` manifest, and creates a GitHub Release with the
artefacts attached.

## Cut a release

1. Make sure `main` is green and you're on it.

       git checkout main && git pull --ff-only

2. Tag and push. The current convention is the path-scoped shape
   `verifier/vX.Y.Z` per the M02 release-train discipline
   (see [`runlog-docs/13-release-trains.md`](https://github.com/runlog-org/runlog-docs/blob/main/13-release-trains.md)):

       git tag -a verifier/v0.2.0 -m "v0.2.0"
       git push origin verifier/v0.2.0

   The legacy unprefixed shape `vX.Y.Z` continues to fire the same
   workflow (soft cut), so existing pinned consumers of `v0.1.0` stay
   valid forever — they don't need to migrate. New releases should use
   the prefixed shape; the unprefixed shape is kept only so we never
   need a flag day.

   Tags matching `*-rc*`, `*-beta*`, or `*-alpha*` produce a **draft**
   Release; everything else is published immediately. The prerelease
   regex matches the suffix and works on both tag shapes.

3. Watch the workflow run on GitHub Actions. On success, the Release
   page lists four binaries plus `SHA256SUMS`:

   - `runlog-verifier-linux-amd64`
   - `runlog-verifier-linux-arm64`
   - `runlog-verifier-darwin-amd64`
   - `runlog-verifier-darwin-arm64`
   - `SHA256SUMS`

## Verify a release locally

End users (and you, before announcing) can confirm the published binary
matches its source by re-running the same build and comparing hashes:

    # Download the binary + SHA256SUMS from the Release page, then:
    sha256sum -c --ignore-missing SHA256SUMS

To rebuild from source and compare:

    git checkout verifier/v0.2.0
    make release
    diff <(sort -k2 dist/SHA256SUMS) <(sort -k2 /path/to/downloaded/SHA256SUMS)

Identical hashes prove the published binary was built from this source
with the published toolchain — the trust assumption documented in
`docs/03-verification-and-provenance.md` §5.4.

## Smoke the workflow without a tag

The release workflow accepts `workflow_dispatch` for manual smoke runs.
Trigger it from the Actions tab; it builds and verifies the artefacts
but skips the Release-creation step.

`make release` reproduces the workflow's Go invocations locally.
