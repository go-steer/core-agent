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

"""Self-test for a2_count.py, run by selftest.sh.

No archived run pairs a daemon log with the transcripts of the same
sessions: the 2026-09-13 batch's counts were made from a live `kubectl
logs` that was never written to disk, which is why that run note's own
method section asks the next batch to capture it. So the one real
artifact here is a transcript — testdata/a2/0913-failed-delegation.sse,
run 20260913T101329Z-b, the delegation a 429 killed (project id
redacted) — and every log line is built from the format strings in the
source it names. Each case asserts the class's counts AND its verdict.
"""

from __future__ import annotations

import contextlib
import io
import json
import pathlib
import sys
import tempfile

HERE = pathlib.Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
import a2_count as a2  # noqa: E402
import adkv2_fixture as v2  # noqa: E402

REAL = HERE / "testdata" / "a2" / "0913-failed-delegation.sse"
failures = 0

# Log lines, each in the exact shape its source writes.
RETRY = "2026-09-13T10:14:32.106Z core-agent: gemini: transient provider error (Error 429, RESOURCE_EXHAUSTED) — retrying once after 2s"
RETRY_SUPPRESSED = ("2026-09-13T10:14:38.653Z core-agent: gemini: transient provider error (Error 429, RESOURCE_EXHAUSTED) "
                    "NOT retried: the shared retry budget is spent (burst 2, one refill per 30s)")
BARE_400 = "Error 400, Message: Request contains an invalid argument., Status: INVALID_ARGUMENT, Details: []"
RETRY_RECOVERED = "2026-09-13T10:14:34.200Z core-agent: gemini: transient provider error recovered on retry (attempt 2/2)"
CUT = ("2026-09-14T21:50:42Z agent: [session s-1] watchdog guardrail cut the turn in flight — the cancellation "
       "error that follows is this cut, not a provider failure: looping")
CUT_OTHER = CUT.replace("s-1", "s-2")
BOUNDARY = "2026-09-14T21:51:00Z agent: [session s-1] cost_ceiling guardrail tripped: session ceiling exceeded"
STORM = ("2026-09-14T21:52:00Z agent: [session s-1] refusal_storm guardrail cut the turn in flight — the cancellation "
         "error that follows is this cut, not a provider failure: refused 5")
TURN_ERR = "2026-09-14T19:18:52Z core-agent: session s-1 turn: Error 400, Message: Request contains an invalid argument."
REPL_TURN_ERR = "2026-09-14T19:18:52Z core-agent: turn: context canceled"


def row_frame(seq: int, author: str, eid: str, meta: dict) -> tuple[str, dict]:
    """A durable row (#1258) as an `agent` frame carries it."""
    return ("agent", {"seq": seq, "event": {"ID": eid, "Author": author, "InvocationID": author.split("/")[-1],
                                            "CustomMetadata": meta}})


def sse(*frames: tuple[str, dict]) -> str:
    return "".join(f"event: {e}\ndata: {json.dumps(d)}\n\n" for e, d in frames)


def run(tmp: pathlib.Path, name: str, log_lines: list[str], events: list[str], session: str | None = None):
    log = tmp / f"{name}.log"
    log.write_text("\n".join(log_lines) + ("\n" if log_lines else ""))
    paths = []
    for i, e in enumerate(events):
        p = tmp / f"{name}-{i}.sse"
        p.write_text(e)
        paths.append(p)
    return {c.name: c for c in a2.count(log, paths, session)}, a2


def expect(counts: dict, cls: str, log: int | None, transcript: int | None, verdict: str, desc: str) -> None:
    global failures
    c = counts[cls]
    ok = (c.log, c.transcript, c.verdict) == (log, transcript, verdict)
    if ok:
        print(f"  ok   {desc}")
    else:
        failures += 1
        print(f"  FAIL {desc}\n         got: log={c.log} transcript={c.transcript} verdict={c.verdict}")


def check(ok: bool, desc: str, got: str) -> None:
    global failures
    if ok:
        print(f"  ok   {desc}")
    else:
        failures += 1
        print(f"  FAIL {desc}\n         got: {got}")


def main() -> int:
    with tempfile.TemporaryDirectory() as d:
        tmp = pathlib.Path(d)

        print("real transcript (run 20260913T101329Z-b)")
        empty_log = tmp / "empty.log"
        empty_log.write_text("")
        counts = {c.name: c for c in a2.count(empty_log, [REAL], None)}
        expect(counts, "failed delegation", None, 1, a2.NOT_COUNTABLE,
               "the delegation a 429 killed is found in the real capture, and has no log side")
        expect(counts, "turn error", 0, 0, a2.NOT_EXERCISED, "the real capture has no turn-error frame")

        print("provider retry — the 2026-09-13 shape")
        c, _ = run(tmp, "retry", [RETRY, RETRY_RECOVERED, RETRY_SUPPRESSED], [REAL.read_text()])
        expect(c, "provider retry", 2, 0, a2.FAIL,
               "two retries reached the policy; a pre-#1206 capture records neither (recovered line not double-counted)")
        check(a2.exit_code(list(c.values())) == 1, "a log-only retry fails A2: exit 1", str(a2.exit_code(list(c.values()))))

        print("exit code — the settled wording (2026-10-04)")
        def row(name, verdict):
            return a2.Count(name, 1, 1, verdict=verdict)
        check(a2.exit_code([row("x", a2.PASS), row("y", a2.TRANSCRIPT_ONLY), row("z", a2.PASS)]) == 0,
              "an overcount (TRANSCRIPT-ONLY) does not keep A2 open: exit 0", "")
        check(a2.exit_code([row("x", a2.PASS), row("y", a2.NOT_EXERCISED)]) == 2,
              "an unexercised class leaves A2 unproven: exit 2", "")
        check(a2.exit_code([row("x", a2.TRANSCRIPT_ONLY), row("y", a2.FAIL)]) == 1,
              "a FAIL wins over everything: exit 1", "")

        print("provider retry — the #1206 surfaces")
        recovered = ("agent", {"seq": 7, "event": {"Author": "core-agent", "Partial": False,
                                                   "Content": {"role": "model", "parts": [{"text": "pods are fine"}]},
                                                   "CustomMetadata": {"provider_retry": {"outcome": "recovered", "attempts": 2, "error": "Error 429"}}}})
        skipped_err = ("turn-error", {"kind": "rate_limited", "retryable": True,
                                      "message": "provider retry skipped, budget spent: Error 429, Message: Resource exhausted."})
        c, _ = run(tmp, "retry-1206", [RETRY, RETRY_RECOVERED, RETRY_SUPPRESSED], [sse(recovered, skipped_err)])
        expect(c, "provider retry", 2, 2, a2.PASS,
               "a stamped recovery and a skipped retry's turn-error pair with the two log lines")
        persisted_fr = ("agent", {"seq": 9, "event": {"Author": "core-agent", "Content": {"role": "user", "parts": [
            {"functionResponse": {"name": "spawn_agent", "response": {
                "status": "failed", "stop_reason": "error",
                "output": "agent: RunSubtask: provider retry persisted: Error 429, Message: Resource exhausted."}}}]}}})
        c, _ = run(tmp, "retry-delegation", [RETRY], [sse(persisted_fr)])
        expect(c, "provider retry", 1, 1, a2.PASS, "a child's persisted retry, carried verbatim in the failed delegation's result")
        alert = ("agent", {"seq": 11, "event": {"Author": "user", "Content": {"role": "user", "parts": [
            {"text": "[Background reports]\ncluster-2 failed: provider retry abandoned: Error 429"}]}}})
        echo = ("agent", {"seq": 12, "event": {"Author": "core-agent", "Content": {"role": "model", "parts": [
            {"text": "The subagent hit 'provider retry abandoned: Error 429', so I'll retry."}]}}})
        c, _ = run(tmp, "retry-alert", [RETRY], [sse(alert, echo)])
        expect(c, "provider retry", 1, 1, a2.PASS,
               "a background alert (user-authored) counts; the model quoting it does not count again")

        print("provider retry — the bare 400 after a served call (#1247)")
        # Same format string as a 429's; the error in the parentheses is
        # the only difference, and it has no parentheses of its own.
        retry_400 = RETRY.replace("(Error 429, RESOURCE_EXHAUSTED)", "(" + BARE_400 + ")")
        check(a2.RETRY_RE.search(retry_400) is not None, "RETRY_RE matches the bare-400 retry line", retry_400)
        recovered_400 = ("agent", {"seq": 17, "event": {"Author": "core-agent", "Partial": False,
                                                        "Content": {"role": "model", "parts": [{"text": "pods are fine"}]},
                                                        "CustomMetadata": {"provider_retry": {"outcome": "recovered", "attempts": 2, "error": BARE_400}}}})
        persisted_400 = ("turn-error", {"kind": "config_error", "code": "400", "retryable": False,
                                        "message": "provider retry persisted: " + BARE_400})
        c, _ = run(tmp, "retry-400", [retry_400, RETRY_RECOVERED, retry_400], [sse(recovered_400, persisted_400)])
        expect(c, "provider retry", 2, 2, a2.PASS,
               "a bare-400 retry pairs like a 429's: the stamp, and the persisted turn-error's prefix")

        print("provider retry — inside a subagent")
        child = tmp / "child-cluster-2.json"
        child.write_text(json.dumps({"agent": "cluster-2", "events": [recovered[1]], "next_since": 7, "truncated": False}))
        drill_shape = tmp / "subagents.json"
        drill_shape.write_text(json.dumps({"cluster": [recovered[1]], "infra": []}))
        log = tmp / "child.log"
        log.write_text(RETRY + "\n" + RETRY_RECOVERED + "\n")
        parent = tmp / "child-parent.sse"
        parent.write_text(sse())
        c = {x.name: x for x in a2.count(log, [parent], None, [child])}
        expect(c, "provider retry", 1, 1, a2.PASS, "a child's recovered retry is found in its --subagent-events body")
        c = {x.name: x for x in a2.count(log, [parent], None, [drill_shape])}
        expect(c, "provider retry", 1, 1, a2.PASS, "…and in the drill's own subagents.json ({name: [frames]})")
        bogus = tmp / "bogus.json"
        bogus.write_text(json.dumps({"unexpected": {"shape": True}}))
        c = {x.name: x for x in a2.count(log, [parent], None, [bogus])}
        expect(c, "provider retry", 1, 0, a2.FAIL, "an input in neither shape counts nothing…")
        check("bogus.json" in c["provider retry"].note, "…and the report names it instead of passing silently",
              c["provider retry"].note)
        c = {x.name: x for x in a2.count(log, [parent], None)}
        expect(c, "provider retry", 1, 0, a2.FAIL, "…and without that body it is missing, which is a real gap in the capture")
        check("--subagent-events" in c["provider retry"].note, "the report names the missing input",
              c["provider retry"].note)

        print("provider retry — side calls and duplicates")
        side = RETRY.replace("gemini: transient", "gemini: side call (approver): transient")
        c, _ = run(tmp, "retry-side", [side, RETRY], [sse(recovered)])
        expect(c, "provider retry", 1, 1, a2.PASS, "a side call's retry is not held against the transcript")
        check(c["provider retry (side call)"].verdict == a2.NOT_COUNTABLE and "1 in the log" in c["provider retry (side call)"].note,
              "…it is reported in its own NOT COUNTABLE row", f'{c["provider retry (side call)"].verdict}: {c["provider retry (side call)"].note}')
        nested = ("agent", {"seq": 13, "event": {"Author": "core-agent", "Content": {"role": "user", "parts": [
            {"functionResponse": {"name": "spawn_agent", "response": {
                "status": "completed", "output": "fine",
                "calls": [{"name": "subagent", "error": "subagent \"g\": run: provider retry persisted: Error 429"}]}}}]}}})
        c, _ = run(tmp, "retry-nested", [], [sse(nested)])
        expect(c, "provider retry", 0, 0, a2.NOT_EXERCISED,
               "a grandchild's error repeated in the calls digest is not counted again")
        feedback = ("agent", {"seq": 14, "event": {"Author": "user", "Content": {"role": "user", "parts": [
            {"text": "[watchdog] tool failure streak. Last error: provider retry persisted: Error 429\n\nnext prompt"}]}}})
        c, _ = run(tmp, "retry-feedback", [], [sse(feedback)])
        expect(c, "provider retry", 0, 0, a2.NOT_EXERCISED,
               "watchdog feedback quoting a retry in the user prompt is not a report block")
        both = ("agent", {"seq": 15, "event": {"Author": "user", "Content": {"role": "user", "parts": [
            {"text": "[Background reports]\n- [c] (failed) provider retry skipped, budget spent: Error 429\n\n---\n\n"
                     "operator says: provider retry persisted is fine"}]}}})
        c, _ = run(tmp, "retry-block", [RETRY_SUPPRESSED], [sse(both)])
        expect(c, "provider retry", 1, 1, a2.PASS, "only the report block counts, not the prompt after its separator")
        ruled = ("agent", {"seq": 16, "event": {"Author": "user", "Content": {"role": "user", "parts": [
            {"text": "[Background reports]\n- [a] (completed) ## Findings\n\n---\n\nall fine\n"
                     "- [c] (failed) partial notes\n\nrun_error: provider retry interrupted after recovering: stream reset"
                     "\n\n---\n\nwhat did they find?"}]}}})
        c, _ = run(tmp, "retry-ruled", [RETRY], [sse(ruled)])
        expect(c, "provider retry", 1, 1, a2.PASS, "a markdown rule inside an earlier report does not hide a later one")

        print("guardrail trip")
        trip = ("guardrail-trip", {"guardrail": "watchdog", "reason": "looping", "halted_turn": True})
        c, _ = run(tmp, "trip-both", [CUT], [sse(trip)])
        expect(c, "guardrail trip", 1, 1, a2.PASS, "a cut in the log and a guardrail-trip frame: PASS")
        c, _ = run(tmp, "trip-logonly", [CUT], [sse()])
        expect(c, "guardrail trip", 1, 0, a2.FAIL, "a cut only the log saw: FAIL")
        c, _ = run(tmp, "trip-sseonly", [], [sse(trip)])
        expect(c, "guardrail trip", 0, 1, a2.TRANSCRIPT_ONLY, "a trip frame with no log line: TRANSCRIPT-ONLY, not FAIL")
        c, _ = run(tmp, "boundary", [BOUNDARY], [sse(("guardrail-trip", {"guardrail": "cost_ceiling", "reason": "x", "halted_turn": False}))])
        expect(c, "guardrail trip", 1, 1, a2.PASS, "a boundary trip ('guardrail tripped:') pairs with its frame")
        # The refusal storm withholds guardrail-trip on purpose (#1081);
        # its transcript witness is its own audit row, and its turn error
        # is an ordinary `canceled` whose row says which guardrail cut it.
        storm_row = row_frame(20, "gate/refusal-storm", "rs-1", {"source": "gate", "repeats": 3})
        storm_err_row = row_frame(21, "agent/turn-error", "te-rs", {"kind": "canceled", "message": "turn canceled",
                                                                     "retryable": False, "cut_by": "refusal_storm"})
        c, _ = run(tmp, "storm", [STORM, TURN_ERR], [sse(storm_row, storm_err_row)])
        expect(c, "guardrail trip", 1, 1, a2.PASS, "a refusal storm pairs with its refusal-storm row, not a guardrail-trip frame")
        expect(c, "turn error", 1, 1, a2.PASS, "…and its turn error is counted once on each side")

        print("turn error (includes refused turns)")
        err = ("turn-error", {"kind": "config_error", "message": "invalid argument", "retryable": False})
        c, _ = run(tmp, "te-both", [TURN_ERR], [sse(err)])
        expect(c, "turn error", 1, 1, a2.PASS, "a wake-loop turn error and its frame: PASS")
        c, _ = run(tmp, "te-repl", [REPL_TURN_ERR], [sse(err)])
        expect(c, "turn error", 1, 1, a2.PASS, "the REPL's 'core-agent: turn:' form counts too")
        # The 2026-09-13 note: two 400s landed after the capture stopped.
        c, _ = run(tmp, "te-logonly", [TURN_ERR, TURN_ERR.replace("19:18:52", "19:40:00")], [sse(err)])
        expect(c, "turn error", 2, 1, a2.FAIL, "a turn error the capture missed: FAIL, delta 1")

        print("durable rows — server-side replay (#1258)")
        # The 2026-10-06 fault batch: 7 cuts and 7 turn errors in the
        # log across three sessions; the drill's live capture held 3 of
        # each (a pre-#1258 daemon: typed frames, no event_id, no rows).
        cuts7 = [CUT.replace("s-1", f"s-{i % 3}") for i in range(7)]
        errs7 = [TURN_ERR.replace("s-1", f"s-{i % 3}") for i in range(7)]
        live_old = sse(*([trip, ("turn-error", {"kind": "canceled", "message": "turn canceled", "retryable": False})] * 3))
        c, _ = run(tmp, "fault-old", cuts7 + errs7, [live_old])
        expect(c, "guardrail trip", 7, 3, a2.FAIL, "the 2026-10-06 shape: live-only frames, 3 of 7 cuts")
        expect(c, "turn error", 7, 3, a2.FAIL, "…and 3 of 7 turn errors")
        # The same run on a #1258 daemon, graded on replays of every
        # session the incidents opened.
        replays = []
        for sid in range(3):
            n = len([i for i in range(7) if i % 3 == sid])
            frames = []
            for k in range(n):
                frames.append(row_frame(10 * k + 1, "agent/guardrail-turn-trip", f"tt-{sid}-{k}",
                                        {"guardrail": "cost_ceiling", "reason": "per-turn", "halted_turn": True}))
                frames.append(row_frame(10 * k + 2, "agent/turn-error", f"te-{sid}-{k}",
                                        {"kind": "canceled", "message": "turn canceled", "retryable": False,
                                         "cut_by": "cost_ceiling"}))
            replays.append(sse(*frames))
        c, _ = run(tmp, "fault-replay", cuts7 + errs7, replays)
        expect(c, "guardrail trip", 7, 7, a2.PASS, "replay-only captures: every cut is a row")
        expect(c, "turn error", 7, 7, a2.PASS, "…and every turn error is a row")
        # A halt is recorded by the halt row alone.
        halt_row = row_frame(30, "agent/guardrail-trip", "halt-1", {"guardrail": "cost_ceiling", "reason": "3 in a row"})
        c, _ = run(tmp, "halt-replay", [CUT], [sse(halt_row)])
        expect(c, "guardrail trip", 1, 1, a2.PASS, "the third per-turn trip halts the session: one halt row, one cut line")

        # A pre-#1258 daemon wrote the halt row but no event_id on the
        # frame: the frame and the row are one halt, not two.
        old_halt_frame = ("guardrail-trip", {"guardrail": "cost_ceiling", "reason": "3 in a row", "halted_turn": True})
        c, _ = run(tmp, "old-halt", [CUT], [sse(old_halt_frame, halt_row)])
        expect(c, "guardrail trip", 1, 1, a2.PASS, "an old capture's id-less halt frame pairs with its halt row")
        other_frame = ("guardrail-trip", {"guardrail": "watchdog", "reason": "looping", "halted_turn": True})
        c, _ = run(tmp, "old-halt-other", [CUT, CUT], [sse(other_frame, halt_row)])
        expect(c, "guardrail trip", 2, 2, a2.PASS, "…but an id-less frame for a different trip still counts")

        print("durable rows — live and replay together, never twice")
        trip_id = ("guardrail-trip", {"guardrail": "cost_ceiling", "reason": "per-turn", "halted_turn": True, "event_id": "tt-a"})
        trip_row = row_frame(40, "agent/guardrail-turn-trip", "tt-a", {"guardrail": "cost_ceiling", "reason": "per-turn", "halted_turn": True})
        err_row = row_frame(41, "agent/turn-error", "te-a", {"kind": "canceled", "message": "turn canceled", "retryable": False})
        err_id = ("turn-error", {"kind": "canceled", "message": "turn canceled", "retryable": False, "event_id": "te-a"})
        live_new = sse(trip_id, trip_row, err_row, err_id)
        c, _ = run(tmp, "live-new", [CUT, TURN_ERR], [live_new])
        expect(c, "guardrail trip", 1, 1, a2.PASS, "a live capture holds the frame and the row: counted once by event_id")
        expect(c, "turn error", 1, 1, a2.PASS, "…the turn error too, whichever arrived first")
        c, _ = run(tmp, "live-and-replay", [CUT, TURN_ERR], [live_new, sse(trip_row, err_row)])
        expect(c, "guardrail trip", 1, 1, a2.PASS, "the same session live AND replayed: each row counted once")
        expect(c, "turn error", 1, 1, a2.PASS, "…for both classes")
        recovered_id = ("agent", {"seq": 50, "event": {
            "ID": "ev-r", "Author": "core-agent", "Content": {"role": "model", "parts": [{"text": "ok"}]},
            "CustomMetadata": {"provider_retry": {"outcome": "recovered"}}}})
        c, _ = run(tmp, "replay-twice", [], [sse(recovered_id), sse(recovered_id)])
        expect(c, "provider retry", 0, 1, a2.TRANSCRIPT_ONLY, "a retry stamp in two captures of one session is one retry")
        c, _ = run(tmp, "frame-row-missing", [CUT], [sse(trip_id)])
        expect(c, "guardrail trip", 1, 1, a2.PASS, "a typed frame whose row no capture holds still counts")
        two_frames = sse(err_id, err_id)
        c, _ = run(tmp, "frame-twice", [TURN_ERR], [two_frames])
        expect(c, "turn error", 1, 1, a2.PASS, "two live captures of one frame (same event_id) count once")
        retry_row = row_frame(60, "agent/turn-error", "te-r", {"kind": "rate_limited", "retryable": True,
                                                              "message": "provider retry persisted: Error 429"})
        retry_typed = ("turn-error", {"kind": "rate_limited", "retryable": True, "event_id": "te-r",
                                      "message": "provider retry persisted: Error 429"})
        c, _ = run(tmp, "retry-row", [RETRY, TURN_ERR], [sse(retry_row, retry_typed)])
        expect(c, "provider retry", 1, 1, a2.PASS, "a persisted retry on a turn error seen as row and frame: one retry")
        expect(c, "turn error", 1, 1, a2.PASS, "…and one turn error")

        print("the window (--since / --until) — both sides, same span")
        since, until = a2.parse_ts("2026-09-14T21:00:00Z"), a2.parse_ts("2026-09-14T23:00:00Z")

        def run_w(name, log_lines, events):
            log = tmp / f"{name}.log"
            log.write_text("\n".join(log_lines) + "\n")
            paths = []
            for i, e in enumerate(events):
                pth = tmp / f"{name}-{i}.sse"
                pth.write_text(e)
                paths.append(pth)
            return ({x.name: x for x in a2.count(log, paths, None, None)},
                    {x.name: x for x in a2.count(log, paths, None, None, since, until)})

        def dated(frame, ts):
            event, data = frame
            data = json.loads(json.dumps(data))
            data["event"]["Timestamp"] = ts
            return (event, data)

        old_row = dated(row_frame(70, "agent/guardrail-turn-trip", "tt-old",
                                  {"guardrail": "cost_ceiling", "reason": "yesterday", "halted_turn": True}),
                        "2026-09-13T08:00:00.5Z")
        # Today's cut is log-only; a replay also holds yesterday's cut.
        # Without the window, yesterday's row covers today's failure.
        bare, win = run_w("window-mask", [CUT], [sse(old_row)])
        expect(bare, "guardrail trip", 1, 1, a2.PASS, "no window: an old row masks today's log-only cut (the bug)")
        expect(win, "guardrail trip", 1, 0, a2.FAIL, "with --since: the old row is dropped and the cut is log-only")
        check("dropped 1 transcript event(s)" in win["guardrail trip"].note,
              "…and the note says the window dropped it", win["guardrail trip"].note)
        today_row = dated(row_frame(71, "agent/guardrail-turn-trip", "tt-today",
                                    {"guardrail": "watchdog", "reason": "looping", "halted_turn": True}),
                          "2026-09-14T21:50:42.123456789Z")
        old_line = CUT.replace("2026-09-14T21:50:42Z", "2026-09-13T08:00:00Z")
        bare, win = run_w("window-line", [old_line, CUT], [sse(today_row)])
        expect(bare, "guardrail trip", 2, 1, a2.FAIL, "no window: a log line from before the batch is counted")
        expect(win, "guardrail trip", 1, 1, a2.PASS, "with --since: a log line outside the window is not counted")
        check("1 log line(s)" in win["guardrail trip"].note, "…and the note counts it", win["guardrail trip"].note)
        late_line = CUT.replace("2026-09-14T21:50:42Z", "2026-09-15T01:00:00Z")
        _, win = run_w("window-until", [CUT, late_line], [sse(today_row)])
        expect(win, "guardrail trip", 1, 1, a2.PASS, "--until drops a log line after the window")
        _, win = run_w("window-typed", [CUT], [sse(trip)])
        expect(win, "guardrail trip", 1, 1, a2.PASS, "a typed frame has no timestamp: kept under a window")
        check("1 typed frame(s) carry no timestamp" in win["guardrail trip"].note,
              "…and the note says so", win["guardrail trip"].note)
        undated = "agent: [session s-1] watchdog guardrail cut the turn in flight — x"
        _, win = run_w("window-undated", [undated], [sse(today_row)])
        expect(win, "guardrail trip", 1, 1, a2.PASS, "an undated log line is kept, never dropped")
        check("1 log line(s) carry no timestamp" in win["guardrail trip"].note,
              "…and counted in the note", win["guardrail trip"].note)
        try:
            with contextlib.redirect_stderr(io.StringIO()):
                a2.main(["--log", str(tmp / "window-mask.log"), "--events", str(tmp / "window-mask-0.sse"), "--since", "yesterday"])
            check(False, "a --since that is not RFC 3339 is refused", "accepted")
        except SystemExit as e:
            check(e.code == 2, "a --since that is not RFC 3339 is refused (exit 2)", str(e.code))
        check("--tui and -p" in win["turn error"].note,
              "the turn-error row says which hosts log no turn line", win["turn error"].note)

        print("scope")
        c, _ = run(tmp, "session", [CUT, CUT_OTHER], [sse(trip)], session="s-1")
        expect(c, "guardrail trip", 1, 1, a2.PASS, "--session keeps only that session's log lines")
        c, _ = run(tmp, "session-retry", [RETRY], [sse()], session="s-1")
        check("whole log" in c["provider retry"].note and c["provider retry"].log == 1,
              "--session cannot scope retry lines, and says so", c["provider retry"].note)

        print("ADK v2 wire form — camelCase keys, zero values omitted")
        # The same captures as above, rewritten the way an adk/v2 daemon
        # marshals session.Event. Each case asserts the counts its v1
        # twin asserts; a reader still keyed on "Author"/"ID"/... reads
        # nothing from these and lands on NOT EXERCISED or 0.
        real_v2 = tmp / "real-v2.sse"
        real_v2.write_text(v2.v2_sse(REAL.read_text()))
        check(not any(v2.has_v1_keys(d) for _, d in a2.sse_frames(real_v2)) and "\"author\"" in real_v2.read_text(),
              "v2: the rewrite left no v1 key on any event", "a v1 key survived the rewrite")
        counts = {c.name: c for c in a2.count(empty_log, [real_v2], None)}
        expect(counts, "failed delegation", None, 1, a2.NOT_COUNTABLE,
               "v2: the delegation a 429 killed is found in the camelCase capture")
        c, _ = run(tmp, "v2-retry-1206", [RETRY, RETRY_RECOVERED, RETRY_SUPPRESSED], [v2.v2_sse(sse(recovered, skipped_err))])
        expect(c, "provider retry", 2, 2, a2.PASS, "v2: a customMetadata provider_retry stamp pairs with its log line")
        c, _ = run(tmp, "v2-retry-alert", [RETRY], [v2.v2_sse(sse(alert, echo))])
        expect(c, "provider retry", 1, 1, a2.PASS, "v2: a user-authored background alert counts, the echo does not")
        c, _ = run(tmp, "v2-fault-replay", cuts7 + errs7, [v2.v2_sse(r) for r in replays])
        expect(c, "guardrail trip", 7, 7, a2.PASS, "v2: every cut is a row, read by author")
        expect(c, "turn error", 7, 7, a2.PASS, "v2: …and every turn error, read by author and customMetadata")
        c, _ = run(tmp, "v2-replay-twice", [], [v2.v2_sse(sse(recovered_id)), v2.v2_sse(sse(recovered_id))])
        expect(c, "provider retry", 0, 1, a2.TRANSCRIPT_ONLY, "v2: one event in two captures is one retry, deduplicated by id")
        bare, win = run_w("v2-window-mask", [CUT], [v2.v2_sse(sse(old_row))])
        expect(bare, "guardrail trip", 1, 1, a2.PASS, "v2: no window: the old row is read (by author) and counted")
        expect(win, "guardrail trip", 1, 0, a2.FAIL, "v2: --since drops that same row by its camelCase timestamp")
        child_v2 = tmp / "child-v2.json"
        child_v2.write_text(json.dumps(v2.v2_tree({"agent": "cluster-2", "events": [recovered[1]], "next_since": 7})))
        c = {x.name: x for x in a2.count(log, [parent], None, [child_v2])}
        expect(c, "provider retry", 1, 1, a2.PASS, "v2: a child's recovered retry is found in a camelCase --subagent-events body")

        print("empty run")
        c, _ = run(tmp, "empty", [], [sse()])
        check(all(x.verdict in (a2.NOT_EXERCISED, a2.NOT_COUNTABLE) for x in c.values()) and a2.exit_code(list(c.values())) == 2,
              "nothing failed and nothing was exercised: exit 2, not a pass", str({k: v.verdict for k, v in c.items()}))
        c, _ = run(tmp, "truncated", [CUT], [sse(trip) + "event: guardrail-trip\ndata: {\"guardr"])
        expect(c, "guardrail trip", 1, 1, a2.PASS, "a capture cut off mid-frame: the partial frame is not counted")

    print("all cases passed" if failures == 0 else f"{failures} case(s) failed")
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main())
