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

"""Self-test for soak_verdict.py, run by selftest.sh.

Two kinds of case:

  * The real run. testdata/soak-0914 is cut down from the 2026-09-14
    eight-hour soak (project id redacted). Graded as-is it must reproduce
    that run's hand verdict — survived PASS, wedge PASS, drops PASS,
    compaction NOT EXERCISED at a 37,321-token peak — and every doctored
    copy must flip exactly the clause it was doctored for, with the
    message that names why. A verdict without its message is not
    asserted: a clause can go red for the wrong reason and still look
    like a catch.
  * Synthetic compaction timelines. The real run never compacted, so the
    paths that matter most for A1 — a real cliff, a transient side-row
    dip, a compactions count, a crossing at the very end — are built
    here, because no archived run contains them.

Prints one line per case and exits non-zero if any case fails.
"""

from __future__ import annotations

import copy
import json
import pathlib
import shutil
import sys
import tempfile

HERE = pathlib.Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
import soak_verdict as sv  # noqa: E402

FIXTURE = HERE / "testdata" / "soak-0914"
THRESHOLD = 108_800  # 128K assumed window x 0.85, the run note's own figure

failures = 0


def report(ok: bool, desc: str, got: str = "") -> None:
    global failures
    if ok:
        print(f"  ok   {desc}")
    else:
        failures += 1
        print(f"  FAIL {desc}\n         got: {got}")


def by_name(clauses: list[sv.Clause]) -> dict[str, sv.Clause]:
    return {c.name: c for c in clauses}


def expect(clauses: list[sv.Clause], name: str, verdict: str, needle: str, desc: str) -> None:
    c = by_name(clauses)[name]
    ok = c.verdict == verdict and needle in c.reason
    report(ok, desc, f"{c.verdict}: {c.reason}")


def load_rows(run: pathlib.Path) -> list[dict]:
    return [json.loads(l) for l in (run / "timeline.jsonl").read_text().splitlines() if l.strip()]


def write_rows(run: pathlib.Path, rows: list[dict]) -> None:
    (run / "timeline.jsonl").write_text("".join(json.dumps(r) + "\n" for r in rows))


def doctored(tmp: pathlib.Path, name: str, rows_fn=None, log_fn=None, drop_log=False) -> pathlib.Path:
    run = tmp / name
    shutil.copytree(FIXTURE, run)
    if rows_fn is not None:
        rows = load_rows(run)
        rows_fn(rows)
        write_rows(run, rows)
    if drop_log:
        (run / "daemon.log").unlink()
    elif log_fn is not None:
        p = run / "daemon.log"
        p.write_text(log_fn(p.read_text()))
    return run


def samples(rows: list[dict]) -> list[dict]:
    return [r for r in rows if r.get("kind") == "sample"]


def real_run_cases(tmp: pathlib.Path) -> None:
    print("real run (testdata/soak-0914)")
    base = sv.grade(FIXTURE, THRESHOLD)
    expect(base, "survived", sv.PASS, "8.00h to the end row", "as-is: survived PASS")
    expect(base, "no wedge", sv.PASS, "16 of 16 observed incidents", "as-is: wedge PASS, all 16 incidents answered")
    expect(base, "compaction", sv.NOT_EXERCISED, "the peak was 37,321", "as-is: compaction NOT EXERCISED at the 37,321 peak")
    expect(base, "no drops", sv.PASS, "outside 1 blind window", "as-is: drops PASS, and the 16:47 blind window is disclosed")
    report(sv.exit_code(base) == 2, "as-is: exit 2 — A1 not closable on this run", str(sv.exit_code(base)))

    def no_end(rows):
        rows[:] = [r for r in rows if r.get("kind") != "end"]
    c = sv.grade(doctored(tmp, "no-end", no_end), THRESHOLD)
    expect(c, "survived", sv.FAIL, "never reached its end row", "no end row: survived FAIL")
    report(sv.exit_code(c) == 1, "no end row: exit 1", str(sv.exit_code(c)))

    def restart(rows):
        samples(rows)[-1]["restarts"] = "1"
    expect(sv.grade(doctored(tmp, "restart", restart), THRESHOLD),
           "survived", sv.FAIL, "restarted 1 time", "a pod restart: survived FAIL")

    def halted(rows):
        samples(rows)[5]["active"][0]["halted"] = True
    expect(sv.grade(doctored(tmp, "halted", halted), THRESHOLD),
           "no wedge", sv.FAIL, "reported halted", "a halted session: wedge FAIL")

    def frozen_cycle(rows):
        # Make the sample inside the third incident a copy of the one
        # before it: nothing moved, nobody answered.
        brk = [i for i, r in enumerate(rows) if r.get("kind") == "break"][2]
        before = max(i for i, r in enumerate(rows) if r.get("kind") == "sample" and i < brk)
        inside = next(i for i, r in enumerate(rows) if r.get("kind") == "sample" and i > brk)
        rows[inside]["active"] = copy.deepcopy(rows[before]["active"])
    expect(sv.grade(doctored(tmp, "frozen", frozen_cycle), THRESHOLD),
           "no wedge", sv.FAIL, "1 of 16 observed incidents got no response", "an unanswered incident: wedge FAIL")

    def inbox_drop(text):
        return text + "2026-09-14T18:00:00.000000000Z agent:s-1 inbox cap exceeded, dropped oldest message (id=m-1 head=\"x\")\n"
    expect(sv.grade(doctored(tmp, "drop", log_fn=inbox_drop), THRESHOLD),
           "no drops", sv.FAIL, "discarded queued inbox input 1 time", "an inbox drop in the log: drops FAIL")

    def both_down(text):
        return text + "2026-09-14T18:00:00.000000000Z agent:s-1 mechanical compaction fallback also failed: boom\n"
    expect(sv.grade(doctored(tmp, "cfail", log_fn=both_down), THRESHOLD),
           "compaction", sv.FAIL, "both strategies down", "both compaction strategies down: compaction FAIL")

    # A summarizer failure alone is a degradation the mechanical fallback
    # exists to cover (#974), and soak.sh's own summary calls a degraded
    # compaction a PASS for A1. It must be disclosed, not convicted.
    def summarizer_failed(text):
        return text + "2026-09-14T18:00:00.000000000Z agent:s-1 auto-compaction failed (consecutive failures=1, backing off 2 turns): boom\n"
    degraded = by_name(sv.grade(doctored(tmp, "degraded", log_fn=summarizer_failed), THRESHOLD))["compaction"]
    report(degraded.verdict == sv.NOT_EXERCISED and any("degraded" in d for d in degraded.detail),
           "a summarizer failure alone: disclosed as degraded, not a compaction FAIL",
           f"{degraded.verdict}: {degraded.reason} {degraded.detail}")

    # The real data never compacts, so a threshold it does reach must
    # convict it: a flat line past the threshold is the disabled case.
    expect(sv.grade(FIXTURE, 20_000), "compaction", sv.FAIL, "were not compacted",
           "real windows past a 20,000 threshold with no cliff: compaction FAIL")

    expect(sv.grade(FIXTURE, None), "compaction", sv.UNKNOWN, "--compaction-at",
           "no threshold given or recorded: compaction UNKNOWN")

    nolog = sv.grade(doctored(tmp, "nolog", drop_log=True), THRESHOLD)
    expect(nolog, "no drops", sv.UNKNOWN, "no daemon.log", "no daemon log: drops UNKNOWN, not PASS")


def synthetic(tmp: pathlib.Path, name: str, windows: list[int], compactions: list[int] | None = None) -> pathlib.Path:
    run = tmp / name
    run.mkdir()
    rows = [{"at": "2026-01-01T00:00:00+00:00", "kind": "start", "hours": "1"}]
    for i, w in enumerate(windows):
        sess = {"id": "s", "turns": i + 1, "window": w, "halted": False,
                "watchdog_tripped": False, "ceiling_tripped": False}
        if compactions is not None:
            sess["compactions"] = compactions[i]
        rows.append({"at": f"2026-01-01T00:{i + 1:02d}:00+00:00", "kind": "sample", "restarts": "0", "active": [sess]})
    rows.append({"at": "2026-01-01T01:00:00+00:00", "kind": "end"})
    write_rows(run, rows)
    (run / "daemon.log").write_text("")
    return run


def compaction_cases(tmp: pathlib.Path) -> None:
    print("synthetic compaction timelines (threshold 100,000)")
    t = 100_000

    def grade(run):
        return sv.grade(run, t)

    expect(grade(synthetic(tmp, "cliff", [90_000, 105_000, 30_000, 34_000])),
           "compaction", sv.PASS, "was compacted", "crossed, fell, and stayed down: PASS")
    # The side-row case: one mid-subtask sample reads a single prompt's
    # input, then the conversation is back. Credited as a cliff, this is
    # a disabled compactor graded PASS.
    expect(grade(synthetic(tmp, "transient", [90_000, 105_000, 4_000, 112_000, 118_000])),
           "compaction", sv.FAIL, "were not compacted", "a one-tick dip that recovers is not a compaction: FAIL")
    expect(grade(synthetic(tmp, "counted", [90_000, 105_000, 108_000], compactions=[0, 0, 1])),
           "compaction", sv.PASS, "was compacted", "a compactions count that rises: PASS without a cliff")
    expect(grade(synthetic(tmp, "late", [80_000, 90_000, 105_000])),
           "compaction", sv.NOT_EXERCISED, "too late in the run", "crossed on the last sample: NOT EXERCISED, not FAIL")
    expect(grade(synthetic(tmp, "lastdip", [90_000, 105_000, 30_000])),
           "compaction", sv.NOT_EXERCISED, "too late in the run", "a dip on the very last sample cannot be shown to persist: NOT EXERCISED")
    expect(grade(synthetic(tmp, "flat", [90_000, 105_000, 110_000, 115_000])),
           "compaction", sv.FAIL, "were not compacted", "crossed and kept climbing: FAIL")


def main() -> int:
    with tempfile.TemporaryDirectory() as d:
        tmp = pathlib.Path(d)
        real_run_cases(tmp)
        compaction_cases(tmp)
    print(f"{'all cases passed' if failures == 0 else f'{failures} case(s) failed'}")
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main())
