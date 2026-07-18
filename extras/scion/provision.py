#!/usr/bin/env python3
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
"""scion-agent container-side provisioner.

Runs inside the agent container during the pre-start lifecycle hook, invoked
by `sciontool harness provision --manifest ...`. Responsibilities:

  1. Resolve auth from staged candidates (GOOGLE_API_KEY or GOOGLE_CLOUD_PROJECT).
  2. Copy staged instructions (and system prompt) to AGENTS.md in workspace.
  3. Pre-create .agents/ directory.
  4. Write outputs/env.json and outputs/resolved-auth.json.
  5. Apply universal MCP servers to .agents/mcp.json.

Uses the shared scion_harness library.
"""

from __future__ import annotations

import os
import sys
from typing import Any

# Add the bundle dir to sys.path so we can import the staged scion_harness helper
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import scion_harness

assert scion_harness.INTERFACE_VERSION >= 2, (
    f"scion_harness INTERFACE_VERSION {scion_harness.INTERFACE_VERSION} < 2"
)

SCION_AGENT_AUTH = scion_harness.AuthSpec(
    "scion-agent",
    [
        scion_harness.env_method(
            "vertex-ai",
            all_of=["GOOGLE_CLOUD_PROJECT"],
            any_of=["GOOGLE_CLOUD_LOCATION"],
            env_fallback=True,
            hint="set GOOGLE_CLOUD_PROJECT",
        ),
        scion_harness.env_method(
            "api-key",
            any_of=["GOOGLE_API_KEY", "GEMINI_API_KEY"],
            env_fallback=True,
            hint="set GOOGLE_API_KEY or GEMINI_API_KEY",
        ),
    ],
    fallback_to_none_on_error=True,
)


def provision(ctx: scion_harness.ProvisionContext) -> None:
    resolved = ctx.select_auth(SCION_AGENT_AUTH)
    method = resolved.method

    env_payload: dict[str, Any] = {}

    if method == "api-key":
        api_key = ctx.read_secret("GOOGLE_API_KEY", env_fallback=True) or ctx.read_secret("GEMINI_API_KEY", env_fallback=True)
        if not api_key:
            raise scion_harness.ProvisionError("GOOGLE_API_KEY/GEMINI_API_KEY secret is empty")
        env_payload["GOOGLE_API_KEY"] = api_key
        env_payload["GEMINI_API_KEY"] = api_key

    elif method == "vertex-ai":
        project = ctx.read_secret("GOOGLE_CLOUD_PROJECT", env_fallback=True)
        if not project:
            raise scion_harness.ProvisionError("GOOGLE_CLOUD_PROJECT secret is empty")
        location = ctx.read_secret("GOOGLE_CLOUD_LOCATION", env_fallback=True) or os.environ.get("GOOGLE_CLOUD_LOCATION") or "us-central1"
        env_payload["GOOGLE_CLOUD_PROJECT"] = project
        env_payload["GOOGLE_CLOUD_LOCATION"] = location
        env_payload["GOOGLE_GENAI_USE_VERTEXAI"] = "true"

    ctx.write_outputs(resolved, env=env_payload)

    # 1. Ensure project directories exist inside the container workspace
    agents_dir = os.path.join(ctx.workspace, ".agents")
    skills_dir = os.path.join(agents_dir, "skills")
    os.makedirs(skills_dir, exist_ok=True)

    # 2. Reconcile memory files: AGENTS.md
    try:
        scion_harness.project_instructions(ctx, os.path.join(ctx.workspace, "AGENTS.md"))
    except Exception as exc:
        ctx.warn(f"failed to write AGENTS.md to workspace: {exc}")

    # 3. Apply universal MCP servers to .agents/mcp.json
    mcp_mapping = ctx.harness_config.get("mcp") or {}
    if mcp_mapping:
        mcp_json_path = os.path.join(agents_dir, "mcp.json")
        if not os.path.isfile(mcp_json_path):
            try:
                scion_harness.atomic_write_json(mcp_json_path, {"version": 1, "servers": {}})
            except OSError as exc:
                ctx.warn(f"failed to pre-seed mcp.json: {exc}")

        try:
            scion_harness.apply_mcp_servers_simple(ctx.bundle_dir, mcp_mapping, ctx.workspace)
        except Exception as exc:
            ctx.warn(f"failed to apply MCP servers: {exc}")

    ctx.info(f"method={method}")


if __name__ == "__main__":
    scion_harness.run("scion-agent", provision)
