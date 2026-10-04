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

import json
import pathlib
import sys
import tempfile

HERE = pathlib.Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
import a2_count as a2  # noqa: E402

REAL = HERE / "testdata" / "a2" / "0913-failed-delegation.sse"
failures = 0

# Log lines, each in the exact shape its source writes.
RETRY = "2026-09-13T10:14:32.106Z core-agent: gemini: transient provider error (Error 429, RESOURCE_EXHAUSTED) — retrying once after 2s"
RETRY_SUPPRESSED = ("2026-09-13T10:14:38.653Z core-agent: gemini: transient provider error (Error 429, RESOURCE_EXHAUSTED) "
                    "NOT retried: the shared retry budget is spent (burst 2, one refill per 30s)")
RETRY_RECOVERED = "2026-09-13T10:14:34.200Z core-agent: gemini: transient provider error recovered on retry (attempt 2/2)"
CUT = ("2026-09-14T21:50:42Z agent: [session s-1] watchdog guardrail cut the turn in flight — the cancellation "
       "error that follows is this cut, not a provider failure: looping")
CUT_OTHER = CUT.replace("s-1", "s-2")
BOUNDARY = "2026-09-14T21:51:00Z agent: [session s-1] cost_ceiling guardrail tripped: session ceiling exceeded"
STORM = ("2026-09-14T21:52:00Z agent: [session s-1] refusal_storm guardrail cut the turn in flight — the cancellation "
         "error that follows is this cut, not a provider failure: refused 5")
TURN_ERR = "2026-09-14T19:18:52Z core-agent: session s-1 turn: Error 400, Message: Request contains an invalid argument."
REPL_TURN_ERR = "2026-09-14T19:18:52Z core-agent: turn: context canceled"


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
        # its transcript witness is the turn-error kind.
        storm_err = ("turn-error", {"kind": "refusal_storm", "message": "refused", "retryable": False})
        c, _ = run(tmp, "storm", [STORM, TURN_ERR], [sse(storm_err)])
        expect(c, "guardrail trip", 1, 1, a2.PASS, "a refusal storm pairs with its turn-error kind, not a guardrail-trip frame")
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

        print("scope")
        c, _ = run(tmp, "session", [CUT, CUT_OTHER], [sse(trip)], session="s-1")
        expect(c, "guardrail trip", 1, 1, a2.PASS, "--session keeps only that session's log lines")
        c, _ = run(tmp, "session-retry", [RETRY], [sse()], session="s-1")
        check("whole log" in c["provider retry"].note and c["provider retry"].log == 1,
              "--session cannot scope retry lines, and says so", c["provider retry"].note)

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
