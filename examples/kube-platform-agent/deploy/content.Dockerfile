# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Content-only OCI artifact for the kube-platform-agent recipe.
#
# This image carries the recipe DIRECTORY — nothing else. It never
# contains the core-agent binary, so content and brain have independent
# lifecycles (bump one without rebuilding the other). See
# docs/agent-content-distribution-design.md for the full rationale.
#
# The image root reproduces the recipe directory verbatim, so a pod that
# materializes it at <mount> can run:
#   core-agent -c <mount>/.agents/config.hub.json
# and every content_root / @include / on-demand governance read resolves
# exactly as it does on a laptop.
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
#                                      image-volume floor. Chainguard
#                                      busybox is Wolfi-based, ~busybox
#                                      sized, and rebuilt to typically
#                                      zero known CVEs — strictly better
#                                      than upstream busybox at the same
#                                      size. (distroless is NOT an option:
#                                      static/base carry no cp, and the
#                                      :debug tags only add one by
#                                      embedding busybox.)
#
# Build (run from the recipe root, examples/kube-platform-agent/):
#   # image-volume flavor
#   docker build -f deploy/content.Dockerfile \
#     -t ghcr.io/go-steer/kube-platform-agent-content:<tag> .
#   # initContainer-copy flavor
#   docker build -f deploy/content.Dockerfile \
#     --build-arg BASE=cgr.dev/chainguard/busybox \
#     -t ghcr.io/go-steer/kube-platform-agent-content:<tag>-copy .
#
# Pin the pushed image by digest in your overlay (see
# deploy/overlays/example/kustomization.yaml).
ARG BASE=scratch
FROM ${BASE}

# The loader-consumed recipe tree. deploy/ and recipe_test.go are
# excluded by construction (not COPY'd). The vendored upstream/ snapshot
# carries the 18 skills + governance + docs + SOUL.md; a live-checkout
# deployment overrides content_roots in config.hub.json and can drop
# upstream/ from the image (see the recipe README).
#
# .agents/ includes an (empty) plans/ directory. That is deliberate: the
# image-volume overlay nests a writable `plans` emptyDir at
# .agents/plans INSIDE this read-only mount, and a read-only image layer
# can't have that mount point created at mount time — so it must be
# pre-baked here. See .agents/plans/.gitkeep and deploy/base/50-deployment-daemon.yaml.
COPY .agents/   /.agents/
COPY AGENTS.md  /AGENTS.md
COPY AGENTS.d/  /AGENTS.d/
COPY upstream/  /upstream/
