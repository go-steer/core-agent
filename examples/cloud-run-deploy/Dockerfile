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

# Path B Dockerfile — builds a thin layer on top of the published
# core-agent image that bakes this directory's `.agents/` bundle
# in at /etc/core-agent/.agents/. The CMD is set so a bare
# `gcloud run deploy --source .` works without any --args plumbing.
#
# Pinned to a main-HEAD digest until v2.4.0 ships. The :2.3.1 tag
# predates the agent-card endpoint (cef0d1b), so the
# /.well-known/agent-card.json route this README verifies wouldn't
# register and would 404 against :2.3.1. This digest is from main
# HEAD with the endpoint in place. Sweep back to a semver tag
# (FROM ghcr.io/go-steer/core-agent:2.4.0) once the release cuts.
# Multi-arch (amd64 + arm64) and Sigstore-signed from the
# go-steer/core-agent repo's release workflow.
FROM ghcr.io/go-steer/core-agent@sha256:4a0da4d9a36dc6918b851fa48461520b474e4faefdaf16d2135a34bb60c5d7dd

# Bake the .agents/ bundle into the image. config.json sets the
# model provider and permissions; AGENTS.md primes the agent's
# identity. To customize, either edit the files in this directory
# before building, or COPY a different bundle in a downstream image.
COPY .agents/ /etc/core-agent/.agents/

# Cloud Run injects $PORT into the container's environment, but
# distroless/static doesn't ship a shell so we can't expand it in
# CMD. Instead, this recipe deploys with `gcloud run deploy --port=7777`
# which tells Cloud Run that the container listens on a fixed port —
# Cloud Run still sets $PORT, the container just ignores it.
CMD [ \
  "--no-repl", \
  "--attach-listen=:7777", \
  "--attach-token=ATTACH_TOKEN", \
  "--session-db", \
  "--session-db-path=/tmp/sessions.db", \
  "--agentic-tools", \
  "-c", "/etc/core-agent/.agents/config.json" \
]
