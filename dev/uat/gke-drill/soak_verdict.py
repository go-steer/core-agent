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

"""Grade one overnight soak against box A1 (#1042), after the fact.

    ./soak_verdict.py --run-dir ~/.gke-drill/soak/<stamp>-<cluster> \\
        [--compaction-at TOKENS] [--json]

soak.sh records and decides nothing: it must never invent a verdict and
never need a human while it runs, so it writes a timeline and a daemon
log and stops. Until this existed, box A1 was read off that timeline by a
person — which is exactly what box A5 ("measured, not asserted") says is
not good enough (#1199). This is the second half: a separate tool, run
afterwards, over the same artifacts.

## The four clauses, as amended on #1042

  survived     ran its full length to an `end` row, no pod restart
  no wedge     no session halted or tripped; every incident the soak
               injected while a sample was watching got a response
  compaction   a session whose context crossed the compaction threshold
               and then took another turn was compacted — compaction is
               marked when a turn ends and runs when the next one
               starts, so a session that crossed and went idle owed none
  no drops     the daemon never discarded queued inbox input

## Verdicts, and why there are four of them

  PASS           checked, and held
  FAIL           checked, and broken
  NOT EXERCISED  the run never created the conditions the clause is
                 about — the 2026-09-14 soak never reached the
                 compaction threshold, and reading that as a pass is the
                 exact reading its run note warned against
  UNKNOWN        an input the clause needs is missing

Only PASS on all four closes A1. Exit status: 0 all PASS, 1 any FAIL,
2 otherwise (nothing failed, but A1 is not closable on this run) — the
same split evalrun uses for an unmet precondition (#1061).

## Two things about the evidence that shaped the checks

A *successful* compaction logs nothing. The daemon logs only failures
(`auto-compaction failed`, `mechanical compaction fallback also
failed`), so the log can convict compaction but never acquit it. The
acquitting witness is the session's own context occupancy (`window`, the
last per-turn input count) falling off a cliff, or a `compactions` count
on the sample when soak.sh records one.

A single low `window` reading is not a cliff. `window` is the last row of
the session's per-turn ledger, and that ledger includes side calls — an
agentic subtask, a digest, the auto-mode approver — whose input is one
prompt, not the conversation. A sample taken mid-subtask therefore reads
low for one tick and recovers. Credited as a compaction, that transient
would turn a disabled compactor into a PASS, so a drop only counts when
the session's next sample still sits below the threshold.

The daemon log has blind windows. soak.sh reconnects a dropped log
stream with `--since=1s` after a 5-second pause and writes a marker, so
each marker is roughly six seconds nobody saw. A clause that rests on
"the log never says X" lists every such window next to its verdict
rather than passing across it silently.
"""

from __future__ import annotations

import argparse
import datetime
import json
import pathlib
import sys
from dataclasses import dataclass, field
from typing import Any

PASS, FAIL, NOT_EXERCISED, UNKNOWN = "PASS", "FAIL", "NOT EXERCISED", "UNKNOWN"

# Slack on the duration check: the soak's own start-up and tear-down sit
# inside the hours it was asked for.
DURATION_SLACK_S = 5 * 60

# A drop to this fraction of the occupancy at the crossing counts as a
# cliff. Compaction replaces history with a summary, so a real one lands
# far below this; a turn that merely shrank a little does not.
CLIFF_FRACTION = 0.6

# Log signatures. Each names the source line it matches, because a log
# wording change silently disarms a clause otherwise.
#   pkg/agent/compactor.go           auto-compaction failed (...)
#   pkg/agent/compaction_degraded.go mechanical compaction fallback also failed
#   pkg/agent/inbox.go               inbox cap exceeded, dropped oldest message
#   dev/uat/gke-drill/soak.sh        soak: --- log stream ended, reconnecting ---
#
# The two compaction lines mean different things. "auto-compaction
# failed" is the SUMMARIZER failing; after two in a row the mechanical
# fallback (#974) truncates instead, and a degraded compaction is a PASS
# for A1 — soak.sh's own summary says so. Only "fallback also failed" is
# both strategies down, which is the failure. Summarizer failures are
# listed beside the verdict, and whether compaction then actually happened
# is still decided by the cliff and the count.
COMPACTION_FAILURE_SIG = "mechanical compaction fallback also failed"
COMPACTION_DEGRADED_SIG = "auto-compaction failed"
INBOX_DROP_SIG = "inbox cap exceeded, dropped oldest message"
LOG_GAP_SIG = "soak: --- log stream ended, reconnecting ---"
LOG_GAP_SECONDS = 6


@dataclass
class Clause:
    name: str
    verdict: str
    reason: str
    detail: list[str] = field(default_factory=list)


def parse_ts(v: Any) -> datetime.datetime | None:
    """RFC3339 with or without fractional seconds, Z or an offset."""
    if not isinstance(v, str) or not v:
        return None
    s = v.strip()
    if s.endswith("Z"):
        s = s[:-1] + "+00:00"
    # Python <3.11 rejects nanosecond fractions; trim to microseconds.
    if "." in s:
        head, rest = s.split(".", 1)
        frac = "".join(ch for ch in rest if ch.isdigit())
        tz = rest[len(frac):]
        s = f"{head}.{frac[:6]}{tz}"
    try:
        return datetime.datetime.fromisoformat(s)
    except ValueError:
        return None


def load_timeline(path: pathlib.Path) -> list[dict[str, Any]]:
    rows = []
    for line in path.read_text().splitlines():
        line = line.strip()
        if not line:
            continue
        try:
            row = json.loads(line)
        except json.JSONDecodeError:
            continue
        if isinstance(row, dict):
            row["_at"] = parse_ts(row.get("at"))
            rows.append(row)
    return rows


def log_lines(path: pathlib.Path) -> list[tuple[datetime.datetime | None, str]]:
    out = []
    for line in path.read_text(errors="replace").splitlines():
        ts, _, rest = line.partition(" ")
        out.append((parse_ts(ts), line))
    return out


def blind_windows(lines: list[tuple[datetime.datetime | None, str]]) -> list[str]:
    gaps = []
    for ts, text in lines:
        if LOG_GAP_SIG in text:
            when = ts.strftime("%H:%M:%S") if ts else "?"
            gaps.append(f"log blind for ~{LOG_GAP_SECONDS}s at {when} (stream reconnect)")
    return gaps


def check_survived(rows: list[dict[str, Any]]) -> Clause:
    starts = [r for r in rows if r.get("kind") == "start"]
    ends = [r for r in rows if r.get("kind") == "end"]
    if not starts:
        return Clause("survived", UNKNOWN, "the timeline has no start row")
    start = starts[0]
    if not ends:
        return Clause("survived", FAIL, "the run never reached its end row — it was killed or crashed before its hours were up")
    end = ends[-1]
    try:
        hours = float(start.get("hours", ""))
    except ValueError:
        return Clause("survived", UNKNOWN, f"the start row's hours ({start.get('hours')!r}) is not a number")
    if start["_at"] is None or end["_at"] is None:
        return Clause("survived", UNKNOWN, "a start or end row has no parseable timestamp")
    ran = (end["_at"] - start["_at"]).total_seconds()
    want = hours * 3600
    detail = [f"ran {ran / 3600:.2f}h of {hours:g}h asked"]
    restarts = sorted({int(r["restarts"]) for r in rows
                       if r.get("kind") == "sample" and str(r.get("restarts", "")).isdigit()})
    if restarts and restarts[-1] > restarts[0]:
        return Clause("survived", FAIL, f"the daemon pod restarted {restarts[-1] - restarts[0]} time(s) during the run", detail)
    if ran + DURATION_SLACK_S < want:
        return Clause("survived", FAIL, f"the run ended after {ran / 3600:.2f}h, short of the {hours:g}h it was asked for", detail)
    return Clause("survived", PASS, f"{ran / 3600:.2f}h to the end row, no pod restart", detail)


def sessions_in(sample: dict[str, Any]) -> list[dict[str, Any]]:
    act = sample.get("active")
    return [s for s in act if isinstance(s, dict) and s.get("id")] if isinstance(act, list) else []


def check_wedge(rows: list[dict[str, Any]]) -> Clause:
    samples = [r for r in rows if r.get("kind") == "sample" and r["_at"] is not None]
    if not samples:
        return Clause("no wedge", UNKNOWN, "the timeline has no samples")

    # (a) a halted or tripped session is a wedge by definition.
    for s in samples:
        for sess in sessions_in(s):
            for flag in ("halted", "watchdog_tripped", "ceiling_tripped"):
                if sess.get(flag) is True:
                    return Clause("no wedge", FAIL,
                                  f"session {sess['id']} reported {flag} at {s['at']}")

    # (b) every injected incident a sample was watching got a response:
    # some session's turn count moved between the last sample before the
    # break and a sample before its restore.
    breaks = [r for r in rows if r.get("kind") == "break" and r["_at"] is not None]
    restores = [r for r in rows if r.get("kind") == "restore" and r["_at"] is not None]
    observed = answered = 0
    detail = []
    for b in breaks:
        r = next((x for x in restores if x["_at"] >= b["_at"]), None)
        if r is None:
            continue
        before = [s for s in samples if s["_at"] <= b["_at"]]
        during = [s for s in samples if b["_at"] < s["_at"] <= r["_at"]]
        if not during:
            detail.append(f"cycle {b.get('cycle', '?')} ({b['at']}): no sample fell inside it")
            continue
        observed += 1
        base = {x["id"]: int(x.get("turns") or 0) for x in sessions_in(before[-1])} if before else {}
        moved = any(int(x.get("turns") or 0) > base.get(x["id"], 0)
                    for s in during for x in sessions_in(s))
        if moved:
            answered += 1
        else:
            detail.append(f"cycle {b.get('cycle', '?')} ({b['at']}): no session's turns moved before the restore")
    if observed == 0:
        return Clause("no wedge", NOT_EXERCISED, "no injected incident had a sample inside it", detail)
    if answered < observed:
        return Clause("no wedge", FAIL,
                      f"{observed - answered} of {observed} observed incidents got no response from any session", detail)
    return Clause("no wedge", PASS,
                  f"no session halted or tripped; {answered} of {observed} observed incidents got a response", detail)


def check_compaction(rows: list[dict[str, Any]], lines: list[tuple[datetime.datetime | None, str]] | None,
                     threshold: int | None) -> Clause:
    gaps = blind_windows(lines) if lines is not None else []
    if lines is not None:
        failures = [text for _, text in lines if COMPACTION_FAILURE_SIG in text]
        if failures:
            return Clause("compaction", FAIL,
                          f"the daemon logged {len(failures)} compaction failure(s) with both strategies down",
                          [f[:200] for f in failures[:5]] + gaps)
        degraded = [text for _, text in lines if COMPACTION_DEGRADED_SIG in text]
        gaps = [f"degraded (summarizer failed, fallback may have covered it): {d[:160]}"
                for d in degraded[:5]] + gaps
    if threshold is None:
        return Clause("compaction", UNKNOWN,
                      "no compaction threshold: pass --compaction-at TOKENS (the recipe model's window x its threshold)", gaps)

    # Per session: (sample time, window, compactions count, turns).
    series: dict[str, list[tuple[datetime.datetime, int, int | None, int | None]]] = {}
    for s in rows:
        if s.get("kind") != "sample" or s["_at"] is None:
            continue
        for sess in sessions_in(s):
            w = sess.get("window")
            if isinstance(w, int):
                c = sess.get("compactions")
                t = sess.get("turns")
                series.setdefault(sess["id"], []).append(
                    (s["_at"], w, c if isinstance(c, int) else None, t if isinstance(t, int) else None))

    peak_id, peak = max(((sid, max(p[1] for p in pts)) for sid, pts in series.items()),
                        key=lambda p: p[1], default=(None, 0))
    crossed = {sid: pts for sid, pts in series.items() if any(p[1] >= threshold for p in pts)}
    if not crossed:
        return Clause("compaction", NOT_EXERCISED,
                      f"no session reached the threshold of {threshold:,} tokens; the peak was {peak:,} ({peak_id})", gaps)

    detail = []
    compacted = failed = unowed = 0
    for sid, pts in crossed.items():
        i = next(k for k, p in enumerate(pts) if p[1] >= threshold)
        at_cross, w_cross, c_cross, _ = pts[i]
        later = pts[i + 1:]
        if c_cross is not None and any(p[2] is not None and p[2] > c_cross for p in later):
            compacted += 1
            detail.append(f"{sid}: crossed at {at_cross:%H:%M} ({w_cross:,}); compactions count rose afterwards")
            continue
        # With a count on the samples, the count is the whole answer and a
        # drop in `window` proves nothing. `window` is the last per-turn
        # row, and a session whose last call was a side call (a digest, an
        # agentic subtask) reads that one prompt for as long as it stays
        # idle — a PERSISTENT low reading the cliff rule below cannot tell
        # from a compaction. Seen on the 2026-10-02 soak: sessions ending
        # the night at ~15K, well under their crossing, with zero
        # compactions. The cliff is kept only for runs that predate the
        # count.
        has_count = c_cross is not None
        cliff = None if has_count else next(
            (k for k, p in enumerate(later) if p[1] <= CLIFF_FRACTION * w_cross), None)
        # Persistent: the next sample after the drop must still be below
        # the threshold, so a mid-subtask side-row reading cannot pass.
        if cliff is not None and cliff + 1 < len(later) and later[cliff + 1][1] < threshold:
            compacted += 1
            detail.append(f"{sid}: crossed at {at_cross:%H:%M} ({w_cross:,}); fell to {later[cliff][1]:,} "
                          f"at {later[cliff][0]:%H:%M} and stayed below the threshold")
            continue
        resumed_at = resumed_after(pts, i)
        if resumed_at is None:
            # Compaction is marked when a turn ENDS and run when the NEXT
            # one starts (pkg/agent: agent.go's post-turn hook, preturn's
            # runPendingCompaction). A session that crossed and then never
            # started another turn owed nothing: there was no next request
            # for the context to overflow. Counting it as a failure graded
            # correct behaviour as a defect — every per-incident session
            # that finishes its one incident above the threshold.
            unowed += 1
            detail.append(f"{sid}: crossed at {at_cross:%H:%M} ({w_cross:,}) and never started another turn, "
                          f"so no compaction was owed")
            continue
        failed += 1
        detail.append(f"{sid}: crossed at {at_cross:%H:%M} ({w_cross:,}), started a new turn at "
                      f"{resumed_at:%H:%M}, and was never compacted — it read {pts[-1][1]:,} at {pts[-1][0]:%H:%M}")
    if failed:
        return Clause("compaction", FAIL,
                      f"{failed} session(s) crossed the {threshold:,}-token threshold, took another turn, "
                      f"and were not compacted", detail + gaps)
    if compacted:
        return Clause("compaction", PASS,
                      f"{compacted} session(s) crossed {threshold:,} tokens and were compacted"
                      + (f"; {unowed} more crossed and never took another turn" if unowed else ""), detail + gaps)
    return Clause("compaction", NOT_EXERCISED,
                  f"{unowed} session(s) crossed {threshold:,} tokens but none took another turn afterwards, "
                  f"so compaction was never owed", detail + gaps)


# QUIET_SAMPLES consecutive samples with an unchanged turn count mark a
# turn as over. Two intervals (~4 minutes at soak.sh's 2-minute cadence),
# not one: a single tool call — a delegated subagent, a large MCP read —
# can hold a turn still for longer than one interval, and mistaking that
# pause for a new turn would grade a session that never resumed as a
# compaction failure. The cost is the opposite error on a session resumed
# within four minutes, which reads as never resumed and is NOT EXERCISED,
# not PASS — the safe direction.
QUIET_SAMPLES = 2


def resumed_after(pts: list[tuple[datetime.datetime, int, int | None, int | None]], i: int) -> datetime.datetime | None:
    """When the session started a new turn after sample i, or None.

    A new turn is turns rising again after a quiet stretch: QUIET_SAMPLES
    consecutive samples at or after i with no change in the count.
    """
    turns = [p[3] for p in pts]
    quiet = 0
    for k in range(max(i, 1), len(pts)):
        if turns[k] is None or turns[k - 1] is None:
            quiet = 0
            continue
        if turns[k] == turns[k - 1]:
            quiet += 1
        elif turns[k] > turns[k - 1]:
            if quiet >= QUIET_SAMPLES:
                return pts[k][0]
            quiet = 0
    return None


def check_drops(lines: list[tuple[datetime.datetime | None, str]] | None) -> Clause:
    if lines is None:
        return Clause("no drops", UNKNOWN, "no daemon.log in the run directory")
    drops = [text for _, text in lines if INBOX_DROP_SIG in text]
    gaps = blind_windows(lines)
    if drops:
        return Clause("no drops", FAIL, f"the daemon discarded queued inbox input {len(drops)} time(s)",
                      [d[:200] for d in drops[:5]] + gaps)
    reason = "the daemon log records no dropped inbox input"
    if gaps:
        reason += f" (outside {len(gaps)} blind window(s))"
    return Clause("no drops", PASS, reason, gaps)


def grade(run: pathlib.Path, compaction_at: int | None) -> list[Clause]:
    timeline = run / "timeline.jsonl"
    if not timeline.is_file():
        raise SystemExit(f"soak_verdict: {timeline} not found")
    rows = load_timeline(timeline)
    log = run / "daemon.log"
    lines = log_lines(log) if log.is_file() else None
    if compaction_at is None:
        start = next((r for r in rows if r.get("kind") == "start"), {})
        v = start.get("compaction_at")
        compaction_at = int(v) if isinstance(v, (int, str)) and str(v).isdigit() else None
    return [
        check_survived(rows),
        check_wedge(rows),
        check_compaction(rows, lines, compaction_at),
        check_drops(lines),
    ]


def exit_code(clauses: list[Clause]) -> int:
    if any(c.verdict == FAIL for c in clauses):
        return 1
    if all(c.verdict == PASS for c in clauses):
        return 0
    return 2


def render(run: pathlib.Path, clauses: list[Clause]) -> str:
    out = [f"# A1 verdict — {run.name}", "", "| clause | verdict | why |", "|---|---|---|"]
    for c in clauses:
        out.append(f"| {c.name} | **{c.verdict}** | {c.reason} |")
    out.append("")
    for c in clauses:
        if c.detail:
            out.append(f"**{c.name}**")
            out.extend(f"- {d}" for d in c.detail)
            out.append("")
    code = exit_code(clauses)
    out.append({0: "A1: **closable on this run.**",
                1: "A1: **failed on this run.**",
                2: "A1: **not closable on this run** — nothing failed, but not every clause was exercised or checkable."}[code])
    return "\n".join(out) + "\n"


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--run-dir", required=True, type=pathlib.Path)
    ap.add_argument("--compaction-at", type=int, default=None,
                    help="context tokens at which compaction should fire (window x threshold); "
                         "overrides a compaction_at on the timeline's start row")
    ap.add_argument("--json", action="store_true", help="emit the clauses as JSON instead of markdown")
    args = ap.parse_args(argv)
    clauses = grade(args.run_dir.expanduser(), args.compaction_at)
    if args.json:
        print(json.dumps([c.__dict__ for c in clauses], indent=2))
    else:
        sys.stdout.write(render(args.run_dir, clauses))
    return exit_code(clauses)


if __name__ == "__main__":
    sys.exit(main())
