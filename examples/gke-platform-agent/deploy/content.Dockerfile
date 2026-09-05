# Content-only OCI artifact for the native platform-agent recipe.
#
# This image carries the recipe DIRECTORY — nothing else. It never
# contains the core-agent binary, so content and brain have independent
# lifecycles (bump one without rebuilding the other).
#
# The image root reproduces the recipe directory verbatim, so a pod that
# materializes it at <mount> can run:
#   core-agent -c <mount>/.agents/config.hub.json
# and every subagent root resolves exactly as it does on a laptop.
#
# TWO FLAVORS, one file, selected by the BASE build arg. The COPY layers
# (the actual content) are byte-identical between flavors; only the base
# differs:
#
#   BASE=scratch                    -> image-volume flavor (default).
#                                      No base, no shell, no coupling.
#                                      Mounted via `volumes[].image`
#                                      (k8s 1.33 beta / GKE 1.35+).
#
#   BASE=cgr.dev/chainguard/busybox -> initContainer-copy flavor.
#                                      Provides `cp` so an initContainer
#                                      can copy the tree into a shared
#                                      emptyDir on clusters below the
#                                      image-volume floor.
#
# WHY THE CHAINGUARD BASE CARRIES NO TAG OR DIGEST, when every other
# image in this recipe is pinned. Chainguard's free tier publishes only
# `latest` and `latest-glibc` — there are no version tags to pin to —
# and it garbage-collects older digests, so a digest pin here would stop
# resolving within about a week and break the rebuild. The drift is
# bounded anyway: this base is consumed at BUILD time, and what the
# cluster pulls is the resulting content image, which IS pinned by an
# immutable tag (CONTENT_TAG). So the base moving does not change a
# running deployment — only the next `build-content-image.sh`.
#
# Build (run from this repo's root):
#   docker build -f deploy/content.Dockerfile -t <ref>:<tag> .
#   docker build -f deploy/content.Dockerfile \
#     --build-arg BASE=cgr.dev/chainguard/busybox -t <ref>:<tag>-copy .
ARG BASE=scratch
FROM ${BASE}

# The loader-consumed recipe tree. deploy/ and the harness scripts are
# excluded by construction (not COPY'd).
#
# NOTE the difference from examples/kube-platform-agent: there is no
# `COPY upstream/`. That recipe vendors Hermes's persona + 18 skills as an
# external content root; this one is self-contained by design — the whole
# point of the rewrite. See README.md.
#
# There is also no `COPY AGENTS.d/`. It held one file, an index stub for
# fleet-governance SOPs (cron audits, drift reconciliation, fleet cost
# analysis, security-patch orchestration) written against bash, a
# filesystem write path, git and a GitOps workspace — none of which this
# runtime has. Shipping a playbook the agent cannot execute is the exact
# failure this demo exists to disprove, so it was deleted rather than
# ported. If it ever comes back, this COPY list and the initContainer
# `cp` list in overlays/initcontainer-copy must change together.
#
# cluster/ is the `cluster` subagent's OWN content root: its AGENTS.md
# persona, its six GKE domain skills under cluster/skills/, and its
# read-only cluster/mcp.json. The subagent's config declares
# `"root": "../cluster"`, which resolves against the mounted agents dir
# (<mount>/.agents) to <mount>/cluster — so this tree MUST ship in the
# image or the subagent boots with no skills/persona.
#
# .agents/ includes an (empty) plans/ directory. That is deliberate: the
# image-volume overlay nests a writable `plans` emptyDir at .agents/plans
# INSIDE this read-only mount, and a read-only image layer can't have that
# mount point created at mount time — so it must be pre-baked here. See
# .agents/plans/.gitkeep and deploy/base/50-deployment-daemon.yaml.
COPY .agents/   /.agents/
COPY AGENTS.md  /AGENTS.md
COPY cluster/   /cluster/
