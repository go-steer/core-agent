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

"""Count each failure class in the daemon log and in the transcripts (box A2).

    ./a2_count.py --log daemon.log --events run1/events.sse [run2/events.sse ...]
                  [--session ID] [--json]

Box A2 (#1042, as amended): for one run, a named set of failure classes
produces equal counts in the captured daemon log and in the transcripts,
and a delta names the missing surface. The 2026-09-13 batch did this by
hand and found 13 provider retries with 0 transcript entries; this makes
the next batch produce the number instead of a paragraph (#1200).

## What counts as which

Every class names the source line it matches on each side, because a
wording change elsewhere silently disarms a count.

  provider retry   log: pkg/models/retry.go — "transient provider error
                   (…) — retrying once after" and "… NOT retried: …",
                   one per transient error that reached the retry policy.
                   transcript: NONE. No event carries a retry today, which
                   is the gap the 2026-09-13 batch found. Any retry in the
                   log is therefore missing from the transcript, and this
                   class fails until a surface exists (#1206).
  guardrail trip   log: pkg/agent/guardrail_halt.go (#1131) — "<name>
                   guardrail cut the turn in flight" / "<name> guardrail
                   tripped:".  transcript: a `guardrail-trip` frame, or for
                   the refusal storm — which withholds that frame on
                   purpose (#1081) — a `turn-error` whose kind is
                   `refusal_storm`.
  turn error       log: pkg/runner/wakeloop.go "session <id> turn: <err>"
                   and the REPL's "core-agent: turn: <err>".  transcript: a
                   `turn-error` frame (pkg/agent/agent.go emits one for every
                   turn error). A refused turn is a turn error, so this is
                   where #1042's "refused turn" is counted.
  failed delegation log: NONE — the daemon logs nothing when a delegation
                   fails.  transcript: a function response whose result has
                   `status: failed` and a `stop_reason` (the subagent return
                   contract, #727).
  wedged session   NOT COUNTABLE. Since #1040 a halt is inert and logged
                   once, so a wedge leaves no per-event line on either side;
                   it is graded by soak_verdict.py's wedge clause instead.

## Verdicts

  PASS             both sides counted, equal, and non-zero
  FAIL             more in the log than in the transcript: a failure only
                   the daemon log witnessed, which is what A2 forbids
  TRANSCRIPT-ONLY  more in the transcript than the log. Visible to anyone
                   reading the session, so not what A2 is about, but A2's
                   text says "equal" and this is reported, not hidden
  NOT EXERCISED    zero on both sides — a run in which nothing failed
                   proves nothing about A2
  NOT COUNTABLE    the class has no signature on one side

Exit 0 when every countable class passes, 1 on any FAIL, 2 otherwise.

## Scope

The log and the event streams must cover the same sessions over the same
window; the tool cannot check that for you. `--session ID` keeps only log
lines that name that session. Provider-retry lines carry no session id,
so under --session they are still counted across the whole log, and the
report says so.
"""

from __future__ import annotations

import argparse
import json
import pathlib
import re
import sys
from dataclasses import dataclass, field
from typing import Any, Iterator

PASS, FAIL, TRANSCRIPT_ONLY = "PASS", "FAIL", "TRANSCRIPT-ONLY"
NOT_EXERCISED, NOT_COUNTABLE = "NOT EXERCISED", "NOT COUNTABLE"

RETRY_RE = re.compile(r"transient provider error \(.*?\) (— retrying once after|NOT retried)")
GUARDRAIL_RE = re.compile(r"agent:(?: \[session [^\]]+\])? (\w+) guardrail (cut the turn in flight|tripped:)")
TURN_ERROR_RE = re.compile(r"core-agent: (?:session \S+ )?turn: ")
SESSION_RE = re.compile(r"\[session ([^\]]+)\]|session (\S+) turn:")


@dataclass
class Count:
    name: str
    log: int | None
    transcript: int | None
    verdict: str = ""
    note: str = ""
    detail: list[str] = field(default_factory=list)


def sse_frames(path: pathlib.Path) -> Iterator[tuple[str, Any]]:
    """Yield (event, data) from an SSE capture. Tolerates a truncated tail."""
    event, data = "", []
    for line in path.read_text(errors="replace").splitlines() + [""]:
        if line.startswith("event:"):
            event = line[6:].strip()
        elif line.startswith("data:"):
            data.append(line[5:].lstrip())
        elif line == "":
            if event and data:
                try:
                    yield event, json.loads("\n".join(data))
                except json.JSONDecodeError:
                    pass
            event, data = "", []


def function_responses(payload: Any) -> Iterator[dict[str, Any]]:
    ev = payload.get("event") if isinstance(payload, dict) else None
    parts = (((ev or {}).get("Content") or {}).get("parts")) or []
    for p in parts:
        fr = p.get("functionResponse") if isinstance(p, dict) else None
        if isinstance(fr, dict):
            yield fr


def line_session(line: str) -> str | None:
    m = SESSION_RE.search(line)
    return (m.group(1) or m.group(2)) if m else None


def count(log: pathlib.Path, events: list[pathlib.Path], session: str | None) -> list[Count]:
    lines = log.read_text(errors="replace").splitlines()

    def scoped(line: str) -> bool:
        return session is None or line_session(line) == session

    retries = [l for l in lines if RETRY_RE.search(l)]
    log_trips = [l for l in lines if GUARDRAIL_RE.search(l) and scoped(l)]
    log_turn_errors = [l for l in lines if TURN_ERROR_RE.search(l) and scoped(l)]

    t_trips = t_turn_errors = t_failed = 0
    for path in events:
        for ev, data in sse_frames(path):
            if ev == "guardrail-trip":
                t_trips += 1
            elif ev == "turn-error":
                t_turn_errors += 1
                if isinstance(data, dict) and data.get("kind") == "refusal_storm":
                    t_trips += 1
            elif ev == "agent":
                for fr in function_responses(data):
                    resp = fr.get("response")
                    if isinstance(resp, dict) and resp.get("status") == "failed" and "stop_reason" in resp:
                        t_failed += 1

    out = [
        Count("provider retry", len(retries), None,
              note="no transcript surface exists for a retry"
                   + ("; retry lines name no session, so this counts the whole log" if session else ""),
              detail=[r[:160] for r in retries[:3]]),
        Count("guardrail trip", len(log_trips), t_trips, detail=[r[:160] for r in log_trips[:3]]),
        Count("turn error", len(log_turn_errors), t_turn_errors, detail=[r[:160] for r in log_turn_errors[:3]]),
        Count("failed delegation", None, t_failed, note="the daemon logs nothing when a delegation fails"),
        Count("wedged session", None, None, note="no per-event signature since #1040; graded by soak_verdict.py"),
    ]
    for c in out:
        c.verdict = verdict(c)
    return out


def verdict(c: Count) -> str:
    if c.log is None and c.transcript is None:
        return NOT_COUNTABLE
    if c.transcript is None:
        # A class with no transcript surface: anything in the log is,
        # by construction, missing from every transcript.
        return FAIL if c.log else NOT_EXERCISED
    if c.log is None:
        return NOT_COUNTABLE
    if c.log == 0 and c.transcript == 0:
        return NOT_EXERCISED
    if c.log > c.transcript:
        return FAIL
    if c.transcript > c.log:
        return TRANSCRIPT_ONLY
    return PASS


def exit_code(counts: list[Count]) -> int:
    if any(c.verdict == FAIL for c in counts):
        return 1
    countable = [c for c in counts if c.verdict != NOT_COUNTABLE]
    if countable and all(c.verdict == PASS for c in countable):
        return 0
    return 2


def render(counts: list[Count]) -> str:
    def cell(v: int | None) -> str:
        return "—" if v is None else str(v)

    out = ["| class | daemon log | transcript | delta | verdict | note |", "|---|---|---|---|---|---|"]
    for c in counts:
        # A class with no transcript surface has delta = its log count:
        # every one of those is missing from every transcript.
        delta = "—" if c.log is None else str(c.log - (c.transcript or 0))
        out.append(f"| {c.name} | {cell(c.log)} | {cell(c.transcript)} | {delta} | **{c.verdict}** | {c.note} |")
    code = exit_code(counts)
    out.append("")
    out.append({0: "A2: **every countable class agrees.**",
                1: "A2: **failed** — at least one failure was witnessed only by the daemon log.",
                2: "A2: **not closable on this run** — nothing was log-only, but not every class was exercised."}[code])
    return "\n".join(out) + "\n"


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--log", required=True, type=pathlib.Path, help="captured daemon log (kubectl logs --timestamps)")
    ap.add_argument("--events", required=True, nargs="+", type=pathlib.Path,
                    help="SSE captures (events.sse) for the same sessions and window")
    ap.add_argument("--session", help="only count log lines that name this session")
    ap.add_argument("--json", action="store_true")
    args = ap.parse_args(argv)
    counts = count(args.log.expanduser(), [p.expanduser() for p in args.events], args.session)
    if args.json:
        print(json.dumps([c.__dict__ for c in counts], indent=2))
    else:
        sys.stdout.write(render(counts))
    return exit_code(counts)


if __name__ == "__main__":
    sys.exit(main())
