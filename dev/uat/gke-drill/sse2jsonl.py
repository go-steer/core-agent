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

"""Turn a captured attach SSE stream into JSONL, one object per frame.

stdin is the raw stream; stdout is one line per frame:

    {"sse": "<event name>", "data": <the parsed data payload>}

Two shapes ride the same stream. Legacy `event: agent` frames carry
`{"seq": N, "event": <ADK session.Event>}` — the eventlog replay, and
what the drill scores. Typed frames (`turn-complete`, `status-update`,
...) carry their own payload and are live-only: the server never
replays them, so a capture that joined the stream late will contain
none. Both are preserved; the scorer decides what it needs.

Deliberately not jq: SSE is a line protocol with multi-line `data:`
continuation, and reconstructing that in a shell pipeline is the kind
of clever one-liner that silently drops the one frame that mattered.

A frame that will not parse as JSON is emitted as
`{"sse": ..., "parse_error": ..., "raw": ...}` rather than dropped. A
capture is evidence; losing a frame quietly is worse than carrying a
broken one forward.
"""

import json
import sys


def main() -> int:
    event_name = "message"  # the SSE default when no `event:` line is sent
    data_lines: list[str] = []
    out = sys.stdout

    def flush() -> None:
        nonlocal event_name, data_lines
        if data_lines:
            payload = "\n".join(data_lines)
            try:
                record = {"sse": event_name, "data": json.loads(payload)}
            except json.JSONDecodeError as err:
                record = {"sse": event_name, "parse_error": str(err), "raw": payload}
            out.write(json.dumps(record, separators=(",", ":")) + "\n")
        event_name = "message"
        data_lines = []

    for line in sys.stdin:
        line = line.rstrip("\n").rstrip("\r")
        if line == "":
            flush()
        elif line.startswith(":"):
            continue  # comment / keepalive
        elif line.startswith("event:"):
            event_name = line[len("event:"):].strip()
        elif line.startswith("data:"):
            # One leading space after the colon is protocol framing and
            # is stripped; anything beyond it is payload.
            data_lines.append(line[len("data:"):].removeprefix(" "))
        # Any other line (an `id:`, a `retry:`, or transport noise that
        # got into the file) is ignored on purpose.

    flush()  # a stream cut mid-frame still yields what it had
    return 0


if __name__ == "__main__":
    sys.exit(main())
