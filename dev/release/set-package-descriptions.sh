#!/usr/bin/env bash
#
# Set the GHCR "package description" tagline for each of the four
# container packages this repo publishes.
#
# The tagline is the one-line description GitHub renders under the
# package name on both the org packages listing and the individual
# package page. It is NOT derived from the OCI image labels — GHCR
# treats it as separate package metadata that must be set via the
# packages API. See:
#   https://docs.github.com/en/rest/packages/packages#update-a-package-for-an-organization
#
# One-off ops script: run once from a maintainer's workstation. The
# CI GITHUB_TOKEN lacks `admin:packages` scope so this is deliberately
# not wired into a workflow. Idempotent — re-run to update the strings.
#
# Requires: `gh` CLI authed as an org owner with `admin:packages`
#   (`gh auth refresh -h github.com -s admin:packages`).
#
# Descriptions must stay in sync with the matrix in
# .github/workflows/release-images.yml — that file owns the OCI
# `org.opencontainers.image.description` label, which is used by OCI
# clients / registries other than GHCR. Both should say the same
# thing.

set -euo pipefail

ORG="${ORG:-go-steer}"

declare -A DESCRIPTIONS=(
  [core-agent]="core-agent daemon: multi-turn LLM agent runtime with in-process TUI, remote attach, MCP, and per-session isolation."
  [core-agent-slim]="Headless core-agent daemon (no embedded TUI). ~5MB smaller image for distroless K8s deployments."
  [core-agent-tui]="Remote TUI client for core-agent — connects to a running daemon over the attach API. Not a runtime by itself."
  [k8s-event-watcher]="Kubernetes event-triage sidecar: watches Events, dedupes, and injects matched ones into a core-agent daemon."
)

for pkg in "${!DESCRIPTIONS[@]}"; do
  desc="${DESCRIPTIONS[$pkg]}"
  echo "-> ${pkg}: ${desc}"
  gh api --method PATCH "/orgs/${ORG}/packages/container/${pkg}" \
    -f "description=${desc}" \
    --silent
done

echo
echo "done. verify:"
for pkg in "${!DESCRIPTIONS[@]}"; do
  echo "  gh api /orgs/${ORG}/packages/container/${pkg} --jq .description"
done
