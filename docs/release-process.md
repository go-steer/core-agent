# Release process

Cutting a release is fully automated from a tag push. The two release workflows fire in parallel:

- **`.github/workflows/release.yml`** — cross-compiles `core-agent` + `core-agent-tui` binaries via GoReleaser (see [`.goreleaser.yml`](../.goreleaser.yml)), signs the checksum file via Sigstore keyless, and publishes the GitHub Release with notes drawn from `CHANGELOG.md` plus a static install/verify footer.
- **`.github/workflows/release-images.yml`** — builds and pushes three multi-arch container images (`core-agent`, `core-agent-slim`, `core-agent-tui`) to ghcr.io, signed via Sigstore keyless.

Both trigger on `push: tags: ['v*.*.*']`.

## Cut a release

1. **Update `CHANGELOG.md`.** Promote the `## [Unreleased]` block to `## [X.Y.Z] — YYYY-MM-DD`. Add a short headline paragraph describing the release before the `### Added` / `### Changed` / `### Fixed` / `### Removed` sections — that paragraph is what shows up at the top of the GitHub Release page.
2. **Bump `internal/version.Version`** in [`internal/version/version.go`](../internal/version/version.go) to `vX.Y.Z` (the tag you're about to cut), commit.
3. **Tag and push:**
   ```bash
   git tag vX.Y.Z
   git push origin vX.Y.Z
   ```
4. **Bump `internal/version.Version`** to `v<next-minor>.0-dev` (e.g. `v2.4.0` release → main becomes `v2.5.0-dev`) so post-release builds report their next-target version. Commit + push. Enforced by [`dev/ci/presubmits/verify-version-fallback`](../dev/ci/presubmits/verify-version-fallback) — the next PR after a release will fail CI until this bump lands, so drift can't rot silently (this was retroactive after the bump was skipped for v2.5.0 + v2.6.0).
5. **Verify both workflows went green** on the [Actions tab](https://github.com/go-steer/core-agent/actions):
   - `Release` → produces 8 archives (`core-agent` + `core-agent-tui`, each in linux/darwin × amd64/arm64), `checksums.txt`, `checksums.txt.sig`, `checksums.txt.pem`. All attached to the GitHub Release.
   - `Release images` → publishes `:X.Y.Z`, `:X.Y`, `:X`, `:latest` tags for each of the three images plus their cosign signatures.
6. **Sanity-check the GitHub Release page** — confirm the body shows the right CHANGELOG content, the assets list looks complete, and the "Latest" badge appears (non-prerelease tags only).

## Republish (rerun the workflows against an existing tag)

If the workflow itself was missing or buggy at the time the tag was first pushed:

```bash
gh workflow run release.yml        --ref vX.Y.Z
gh workflow run release-images.yml --ref vX.Y.Z
```

The image workflow also takes an optional `-f tag=vX.Y.Z` input for situations where the workflow at the target ref pre-dates `workflow_dispatch` support; see the comment in [`release-images.yml`](../.github/workflows/release-images.yml).

## Verify a release locally

```bash
# Binaries — checksum file signs every archive transitively.
gh release download vX.Y.Z --repo go-steer/core-agent --pattern 'checksums.txt*'
cosign verify-blob \
  --signature checksums.txt.sig --certificate checksums.txt.pem \
  --certificate-identity-regexp '^https://github.com/go-steer/core-agent' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  checksums.txt
sha256sum -c checksums.txt --ignore-missing

# Container images.
cosign verify ghcr.io/go-steer/core-agent:X.Y.Z \
  --certificate-identity-regexp '^https://github.com/go-steer/core-agent' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

## Dry-run GoReleaser locally

```bash
goreleaser release --snapshot --clean
```

Writes to `./dist/` without publishing. Useful for checking the archive layout and ldflags injection before pushing a real tag. The `--snapshot` flag fabricates a version like `2.3.2-next` so it works on any branch.

## Dry-run the workflow

To smoke-test `release.yml` from a feature branch (without publishing anything):

```bash
gh workflow run release.yml \
  --ref <your-branch> \
  -f tag=v2.3.1 \
  -f dry_run=true
```

This invokes the workflow file on `<your-branch>` (so changes to the workflow itself are exercised), uses `v2.3.1` to resolve the CHANGELOG section, and runs `goreleaser release --snapshot --clean --skip=sign`. The built `dist/` is uploaded as a workflow artifact named `dist-dry-run-v2.3.1` for inspection — no Release is created, no signatures emitted.
