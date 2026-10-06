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
                   transcript (#1206): a recovered retry stamps
                   `CustomMetadata.provider_retry` on the event it recovered
                   with; any other retry surfaces as an error led by
                   "provider retry persisted|abandoned|skipped|failed|
                   interrupted", counted in a `turn-error` frame's message,
                   in a function response's own fields (a failed delegation
                   carries its child's error verbatim; the nested `calls`
                   digest is skipped, since it repeats a grandchild's), or
                   in a `[Background reports]` block (a background
                   subagent's terminal alert). A retry inside a subagent is
                   on the CHILD's events, which the parent's stream does not
                   carry: pass the drill's subagents.json with
                   --subagent-events.
                   A side call (approver, summarizer, session title, /btw,
                   an MCP digest, an agentic tool's subtask) logs
                   "side call (<name>): " before the line. Its response
                   is never an event and its error never a turn error, so
                   those retries are reported in their own NOT COUNTABLE row
                   rather than failing this one.
                   Known gaps, each log-only: a retry whose consumer stopped
                   reading (a cancelled turn, a guardrail cut mid-stream);
                   an abandoned retry inside a subagent that its parent
                   reports only as a stop reason. An agentic tool's failed
                   retry reaches the parent's function response while its log
                   line is a side call's: TRANSCRIPT-ONLY, the safe side.
                   Overcounting is possible and is the safe direction (an
                   async child whose result is delivered both inline and as
                   a report), but it can mask an undercount of equal size.
  guardrail trip   log: pkg/agent/guardrail_halt.go (#1131) — "<name>
                   guardrail cut the turn in flight" / "<name> guardrail
                   tripped:".  transcript (#1258): one eventlog row per trip,
                   on an `agent` frame — `agent/guardrail-trip` (the trip
                   halted the session), `agent/guardrail-turn-trip` (it did
                   not), or `gate/refusal-storm` (the refusal storm, which
                   withholds the typed frame on purpose, #1081). A live
                   capture also holds the typed `guardrail-trip` frame; it
                   counts only when its `event_id` names no row in any
                   capture (a pre-#1258 daemon, or a row the captures
                   missed).
  turn error       log: pkg/runner/wakeloop.go "session <id> turn: <err>"
                   and the REPL's "core-agent: turn: <err>".  transcript
                   (#1258): one `agent/turn-error` row per error Run
                   reports, including a refused turn — this is where
                   #1042's "refused turn" is counted. The typed `turn-error`
                   frame counts only when its `event_id` names no row, as
                   for the trip. A refusal storm is one trip AND one turn
                   error on both sides: one cut line and one turn line in
                   the log; one refusal-storm row and one turn-error row
                   (`cut_by: refusal_storm`) in the transcript.
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
                   reading the session, so it satisfies A2 (settled
                   2026-10-04: "no class has more entries in the daemon log
                   than in the transcripts"); the counter overcounts where
                   it cannot be exact, and "equal" would fail on that
  NOT EXERCISED    zero on both sides — a run in which nothing failed
                   proves nothing about A2
  NOT COUNTABLE    the class has no signature on one side

Exit 0 when every countable class is PASS or TRANSCRIPT-ONLY, 1 on any
FAIL, 2 otherwise (a class NOT EXERCISED leaves A2 unproven on this run).

## Live captures and server-side replays

--events takes any mix of live captures (the drill's events.sse) and
server-side replays (replay_sessions.sh's replay-<sid>.sse, a
`GET …/events?since=0` read after the fact). A replay carries every
durable row and no typed frame; a live capture carries both. Every
`agent` frame is counted once across all inputs, keyed on its event ID,
so a session passed both live and replayed is not counted twice, and a
typed frame is dropped when its `event_id` names a row already counted.
Replays are the better input for A2: a live capture holds only the
sessions the drill attached to, and an incident can open more than one
(2026-10-06 run 2 opened two; the drill captured one).

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

RETRY_MARK_RE = re.compile(r"provider retry (persisted|abandoned|skipped|failed|interrupted)")
REPORTS_HEADER, REPORTS_SEP = "[Background reports]\n", "\n\n---\n\n"
SIDE_CALL = "side call ("

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


def subagent_frames(path: pathlib.Path) -> Iterator[tuple[str, Any]]:
    """Yield ("agent", frame) from a subagent capture.

    Two shapes: the drill's own <run>/subagents.json ({name: [frames]},
    written by lib.sh's drill_capture_subagents), and one GET
    …/agents/{name}/events body ({"events": [frames], ...}).
    """
    try:
        body = json.loads(path.read_text(errors="replace"))
    except json.JSONDecodeError:
        return
    if not isinstance(body, dict):
        return
    if isinstance(body.get("events"), list):
        lists = [body["events"]]
    else:
        lists = [v for v in body.values() if isinstance(v, list)]
    for frames in lists:
        for frame in frames:
            yield "agent", frame


def report_block(text: str) -> str:
    """The [Background reports] block of a user prompt, or "".

    It ends at the LAST separator, not the first: an alert body is often
    model-written markdown with a `---` rule in it, and stopping at that
    hid every later report. The reports are the innermost prepend
    (watchdog and inbox blocks go before them), so the only text after
    the last separator is the operator's own prompt.
    """
    h = text.find(REPORTS_HEADER)
    if h < 0:
        return ""
    body = text[h + len(REPORTS_HEADER):]
    end = body.rfind(REPORTS_SEP)
    return body if end < 0 else body[:end]


def strings_in(v: Any, skip: str = "calls") -> Iterator[str]:
    """Every string in a function response, skipping the nested `calls`
    digest, which repeats a grandchild's errors the child already counted."""
    if isinstance(v, str):
        yield v
    elif isinstance(v, dict):
        for k, x in v.items():
            if k != skip:
                yield from strings_in(x, skip)
    elif isinstance(v, list):
        for x in v:
            yield from strings_in(x, skip)


def retry_marks(ev: str, data: Any) -> int:
    """How many provider retries one `agent` frame records (#1206).

    A turn error's retry mark is counted by Transcript.turn_errors(),
    once per turn error, because since #1258 the same error can arrive
    as a typed frame AND as a row."""
    if ev != "agent":
        return 0
    event = data.get("event") if isinstance(data, dict) else None
    if not isinstance(event, dict):
        return 0
    n = 0
    meta = event.get("CustomMetadata")
    if isinstance(meta, dict) and isinstance(meta.get("provider_retry"), dict):
        n += 1
    for fr in function_responses(data):
        n += sum(len(RETRY_MARK_RE.findall(x)) for x in strings_in(fr.get("response")))
    if event.get("Author") == "user":
        # Only the report block: the operator's prompt and watchdog
        # feedback ("Last error: …") may quote a retry already counted.
        for part in ((event.get("Content") or {}).get("parts")) or []:
            if isinstance(part, dict) and isinstance(part.get("text"), str):
                n += len(RETRY_MARK_RE.findall(report_block(part["text"])))
    return n


def function_responses(payload: Any) -> Iterator[dict[str, Any]]:
    ev = payload.get("event") if isinstance(payload, dict) else None
    parts = (((ev or {}).get("Content") or {}).get("parts")) or []
    for p in parts:
        fr = p.get("functionResponse") if isinstance(p, dict) else None
        if isinstance(fr, dict):
            yield fr


GUARDRAIL_ROW_AUTHORS = ("agent/guardrail-trip", "agent/guardrail-turn-trip", "gate/refusal-storm")
TURN_ERROR_ROW_AUTHOR = "agent/turn-error"


@dataclass
class Transcript:
    """Everything the transcript side counts, deduplicated across inputs."""
    retries: int = 0
    failed: int = 0
    seen: set[str] = field(default_factory=set)
    trip_rows: set[str] = field(default_factory=set)
    halt_keys: list[tuple[str, str]] = field(default_factory=list)
    error_rows: dict[str, str] = field(default_factory=dict)
    typed_trips: list[tuple[str | None, tuple[str, str]]] = field(default_factory=list)
    typed_errors: list[tuple[str | None, str]] = field(default_factory=list)
    anon: int = 0

    def agent_frame(self, data: Any) -> None:
        event = data.get("event") if isinstance(data, dict) else None
        if not isinstance(event, dict):
            return
        eid = event.get("ID") if isinstance(event.get("ID"), str) and event.get("ID") else None
        if eid is not None:
            if eid in self.seen:
                return  # the same event in a second capture of the session
            self.seen.add(eid)
        else:
            self.anon += 1
            eid = f"<no id {self.anon}>"
        self.retries += retry_marks("agent", data)
        for fr in function_responses(data):
            resp = fr.get("response")
            if isinstance(resp, dict) and resp.get("status") == "failed" and "stop_reason" in resp:
                self.failed += 1
        author = event.get("Author")
        meta = event.get("CustomMetadata") if isinstance(event.get("CustomMetadata"), dict) else {}
        if author in GUARDRAIL_ROW_AUTHORS:
            self.trip_rows.add(eid)
            if author == "agent/guardrail-trip":
                self.halt_keys.append((str(meta.get("guardrail") or ""), str(meta.get("reason") or "")))
        elif author == TURN_ERROR_ROW_AUTHOR:
            self.error_rows[eid] = str(meta.get("message") or "")

    def typed_frame(self, ev: str, data: Any) -> None:
        if not isinstance(data, dict):
            return
        eid = data.get("event_id") if isinstance(data.get("event_id"), str) and data.get("event_id") else None
        if ev == "guardrail-trip":
            self.typed_trips.append((eid, (str(data.get("guardrail") or ""), str(data.get("reason") or ""))))
        elif ev == "turn-error":
            self.typed_errors.append((eid, str(data.get("message") or "")))

    def _unmatched(self, ids: list[str | None], rows: Any) -> list[int]:
        """Indexes of typed frames that name no row and no earlier frame."""
        out, counted = [], set()
        for i, eid in enumerate(ids):
            if eid is None:
                out.append(i)
            elif eid not in rows and eid not in counted:
                counted.add(eid)
                out.append(i)
        return out

    def trips(self) -> int:
        """Rows, plus the typed frames no row accounts for.

        A frame with no event_id comes from a pre-#1258 daemon, which
        wrote no per-turn rows but DID write the #643 halt row. Such a
        frame is the same trip as a halt row with the same guardrail and
        reason, so it pairs with one (each row once) instead of counting
        a halt twice when an old capture is re-graded."""
        unpaired = list(self.halt_keys)
        n = len(self.trip_rows)
        for i in self._unmatched([eid for eid, _ in self.typed_trips], self.trip_rows):
            eid, key = self.typed_trips[i]
            if eid is None and key in unpaired:
                unpaired.remove(key)
                continue
            n += 1
        return n

    def turn_errors(self) -> list[str]:
        """The message of every distinct turn error, row or frame."""
        msgs = list(self.error_rows.values())
        ids = [eid for eid, _ in self.typed_errors]
        msgs += [self.typed_errors[i][1] for i in self._unmatched(ids, self.error_rows)]
        return msgs


def scan(events: list[pathlib.Path], subagent_events: list[pathlib.Path]) -> tuple[Transcript, list[str]]:
    t, empty_inputs = Transcript(), []
    for path in subagent_events:
        frames = 0
        for ev, data in subagent_frames(path):
            frames += 1
            t.retries += retry_marks(ev, data)
        if frames == 0:
            empty_inputs.append(path.name)
    for path in events:
        for ev, data in sse_frames(path):
            if ev == "agent":
                t.agent_frame(data)
            else:
                t.typed_frame(ev, data)
    return t, empty_inputs


def line_session(line: str) -> str | None:
    m = SESSION_RE.search(line)
    return (m.group(1) or m.group(2)) if m else None


def count(log: pathlib.Path, events: list[pathlib.Path], session: str | None,
          subagent_events: list[pathlib.Path] | None = None) -> list[Count]:
    lines = log.read_text(errors="replace").splitlines()

    def scoped(line: str) -> bool:
        return session is None or line_session(line) == session

    all_retries = [l for l in lines if RETRY_RE.search(l)]
    side_retries = [l for l in all_retries if SIDE_CALL in l]
    retries = [l for l in all_retries if SIDE_CALL not in l]
    log_trips = [l for l in lines if GUARDRAIL_RE.search(l) and scoped(l)]
    log_turn_errors = [l for l in lines if TURN_ERROR_RE.search(l) and scoped(l)]

    t, empty_inputs = scan(events, subagent_events or [])
    turn_errors = t.turn_errors()
    t_retries = t.retries + sum(1 for m in turn_errors if RETRY_MARK_RE.match(m))
    t_trips, t_turn_errors, t_failed = t.trips(), len(turn_errors), t.failed

    out = [
        Count("provider retry", len(retries), t_retries,
              note="; ".join(x for x in [
                  "" if subagent_events else "no --subagent-events: a retry inside a subagent is not counted on the transcript side",
                  f"--subagent-events input with no frames: {', '.join(empty_inputs)}" if empty_inputs else "",
                  "retry lines name no session, so this counts the whole log" if session else "",
              ] if x),
              detail=[r[:160] for r in retries[:3]]),
        Count("provider retry (side call)", None, None,
              note=f"{len(side_retries)} in the log; a side call's response is never an event and its error never a turn error",
              detail=[r[:160] for r in side_retries[:3]]),
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
    if countable and all(c.verdict in (PASS, TRANSCRIPT_ONLY) for c in countable):
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
    out.append({0: "A2: **met on this run** — no countable class has more entries in the log than in the transcripts.",
                1: "A2: **failed** — at least one failure was witnessed only by the daemon log.",
                2: "A2: **not closable on this run** — nothing was log-only, but not every class was exercised."}[code])
    return "\n".join(out) + "\n"


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--log", required=True, type=pathlib.Path, help="captured daemon log (kubectl logs --timestamps)")
    ap.add_argument("--events", required=True, nargs="+", type=pathlib.Path,
                    help="SSE captures for the same sessions and window: live (events.sse) and/or "
                         "server-side replays (replay_sessions.sh); an event in several is counted once")
    ap.add_argument("--subagent-events", nargs="*", type=pathlib.Path, default=[],
                    help="GET /sessions/{id}/agents/{name}/events bodies for the same sessions' subagents")
    ap.add_argument("--session", help="only count log lines that name this session")
    ap.add_argument("--json", action="store_true")
    args = ap.parse_args(argv)
    counts = count(args.log.expanduser(), [p.expanduser() for p in args.events], args.session,
                   [p.expanduser() for p in args.subagent_events])
    if args.json:
        print(json.dumps([c.__dict__ for c in counts], indent=2))
    else:
        sys.stdout.write(render(counts))
    return exit_code(counts)


if __name__ == "__main__":
    sys.exit(main())
