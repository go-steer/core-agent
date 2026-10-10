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

"""Rewrite ADK v1 (PascalCase) session.Event JSON into ADK v2's wire form.

Test-only. The checked-in captures were recorded on an ADK v1 daemon,
which marshalled session.Event, the model.LLMResponse embedded in it and
its EventActions with Go's default field names and wrote every zero
value. ADK v2 (google.golang.org/adk/v2) tags those structs camelCase
with omitempty. The readers accept both; the selftests prove it by
grading a v1 fixture and its v2 rewrite and asserting the same result.

Only Event-level keys are renamed. Everything inside `content` (genai
keys, already lowercase) and inside `customMetadata` (core-agent's own
snake_case stamps) is left exactly as recorded.
"""

from __future__ import annotations

import json
from typing import Any

# v1 key -> (v2 key, omitempty). The always-present five are the ones
# v2 tags without omitempty.
EVENT_KEYS: dict[str, tuple[str, bool]] = {
    "ID": ("id", False),
    "Timestamp": ("timestamp", False),
    "InvocationID": ("invocationId", False),
    "Author": ("author", False),
    "Actions": ("actions", False),
    "Branch": ("branch", True),
    "LongRunningToolIDs": ("longRunningToolIds", True),
    # model.LLMResponse, embedded
    "Content": ("content", True),
    "CitationMetadata": ("citationMetadata", True),
    "GroundingMetadata": ("groundingMetadata", True),
    "UsageMetadata": ("usageMetadata", True),
    "CustomMetadata": ("customMetadata", True),
    "LogprobsResult": ("logprobsResult", True),
    "ModelVersion": ("modelVersion", True),
    "Partial": ("partial", True),
    "TurnComplete": ("turnComplete", True),
    "Interrupted": ("interrupted", True),
    "ErrorCode": ("errorCode", True),
    "ErrorMessage": ("errorMessage", True),
    "FinishReason": ("finishReason", True),
    "AvgLogprobs": ("avgLogprobs", True),
}

ACTION_KEYS: dict[str, str] = {
    "StateDelta": "stateDelta",
    "ArtifactDelta": "artifactDelta",
    "RequestedToolConfirmations": "requestedToolConfirmations",
    "SkipSummarization": "skipSummarization",
    "TransferToAgent": "transferToAgent",
    "Escalate": "escalate",
    "Compaction": "compaction",
}


def _zero(v: Any) -> bool:
    return v is None or v is False or v == "" or v == 0 or v == {} or v == []


def v2_event(ev: dict[str, Any]) -> dict[str, Any]:
    """One v1 Event as v2 would marshal it. Unknown keys pass through."""
    out: dict[str, Any] = {}
    for k, v in ev.items():
        if k not in EVENT_KEYS:
            out[k] = v
            continue
        nk, omitempty = EVENT_KEYS[k]
        if k == "Actions" and isinstance(v, dict):
            v = {ACTION_KEYS.get(ak, ak): av for ak, av in v.items() if not (ak in ACTION_KEYS and _zero(av))}
        if omitempty and _zero(v):
            continue
        out[nk] = v
    return out


def v2_tree(obj: Any) -> Any:
    """Every dict under an `event` key, anywhere in a JSON tree, rewritten
    with v2_event: an `agent` frame's data, a jsonl record's data, a
    subagents.json body or a --subagent-events body alike."""
    if isinstance(obj, dict):
        return {k: (v2_event(v2_tree(v)) if k == "event" and isinstance(v, dict) else v2_tree(v))
                for k, v in obj.items()}
    if isinstance(obj, list):
        return [v2_tree(x) for x in obj]
    return obj


def v2_sse(text: str) -> str:
    """An SSE capture with each single-line `data:` payload rewritten.
    Lines that are not JSON (a truncated tail) are kept verbatim."""
    out = []
    for line in text.splitlines(keepends=True):
        if line.startswith("data: "):
            body = line[len("data: "):]
            nl = "\n" if body.endswith("\n") else ""
            try:
                line = "data: " + json.dumps(v2_tree(json.loads(body))) + nl
            except json.JSONDecodeError:
                pass
        out.append(line)
    return "".join(out)


def v2_jsonl(text: str) -> str:
    out = []
    for line in text.splitlines():
        if not line.strip():
            continue
        try:
            out.append(json.dumps(v2_tree(json.loads(line))))
        except json.JSONDecodeError:
            out.append(line)
    return "".join(x + "\n" for x in out)


def has_v1_keys(obj: Any) -> bool:
    """True if any `event` dict in the tree still carries a v1 key — the
    selftests assert this is False after a rewrite, so a case cannot pass
    by accident on fixtures the rewrite never touched."""
    if isinstance(obj, dict):
        for k, v in obj.items():
            if k == "event" and isinstance(v, dict) and any(x in EVENT_KEYS for x in v):
                return True
            if has_v1_keys(v):
                return True
    elif isinstance(obj, list):
        return any(has_v1_keys(x) for x in obj)
    return False
