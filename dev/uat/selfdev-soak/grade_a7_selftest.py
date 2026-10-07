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

"""Self-test for grade_a7.py, run by dev/tools/verify-gke-drill.

One passing run, testdata/a7/pass, and a doctored copy of it per way a
run can fail or be void. Each case asserts the exit code, every row's
verdict, and the message that names why: a row can go red for the wrong
reason and still look like a catch.

What is recorded and what is built, in testdata/a7/pass:

  * daemon.log: the boot lines of a real daemon, started with the soak
    overlay (config.soak.json) on the echo provider, multi-session,
    with sa:selfdev-dispatcher in the bearer table. Paths are rewritten
    to the soak's mounts and the `kubectl logs --timestamps` prefix is
    added; the lines are otherwise verbatim.
  * replay.sse: that daemon's GET …/events?since=0 for the session the
    dispatcher identity created and injected into (the capabilities,
    status and usage frames and the inject are recorded). The echo model
    does not use tools, so the turn after the inject is built in the
    recorded event shape: a plan, a gate/approver allow row (pkg/agent/
    approver_context.go's metadata), a bash call and a final answer.
  * perms.json: the same daemon's GET …/perms (mode, allow list,
    settable modes), plus one approver-allowed approval row in
    ApprovalInfo's shape (pkg/attach/state.go). perms-baseline.json is
    the same capture without the approval row: a fresh session on that
    daemon reads identical allow and deny lists.
  * eventlog.json: the agent_eventlog rows. The two recorded rows carry
    the caller sidecar the daemon wrote; the built ones copy it. The
    self-test loads them into a SQLite file with the real table's columns.
  * github/*.json: GitHub's pulls and workflow-runs shapes, with {TIP}
    and {BASE} filled in from a git repository the self-test builds.
  * go/*.txt: the real output of the oracle at origin/main (the pre-fix
    tree, exit 1) and at the same tree with #1234's recovery half fixed
    (exit 0). testdata/a7/fakebin/go replays them; fakebin/gh answers
    the two GET paths. Nothing reaches GitHub or compiles.

Prints one line per case and exits non-zero if any case fails.
"""

from __future__ import annotations

import copy
import json
import os
import pathlib
import shutil
import sqlite3
import subprocess
import sys
import tempfile
from dataclasses import dataclass, field
from typing import Any, Callable

HERE = pathlib.Path(__file__).resolve().parent
GRADER = HERE / "grade_a7.py"
FIXTURE = HERE / "testdata" / "a7" / "pass"
FAKEBIN = HERE / "testdata" / "a7" / "fakebin"
ROWS = ("run identity", "posture", "no human", "ended by work", "degraded visibly", "task oracle", "CI")
ALL_PASS = {r: "PASS" for r in ROWS} | {"degraded visibly": "RECORDED"}
SCHEMA = ("CREATE TABLE agent_eventlog (seq integer PRIMARY KEY AUTOINCREMENT, app_name text NOT NULL, "
          "user_id text NOT NULL, session_id text NOT NULL, event_id text NOT NULL, branch text, author text, "
          "timestamp datetime, invocation_id text, metadata text)")

WRITER = "core-agent-selfdev-writer[bot]"
COMMIT_EMAIL = "selfdev-soak@example.com"

failures = 0


@dataclass
class Run:
    """A run's artifacts, loaded so a case can doctor them."""
    log: list[str]
    frames: list[tuple[str, Any]]
    perms: dict[str, Any]
    baseline: dict[str, Any]
    db: dict[str, Any]
    pr: dict[str, Any]
    runs: dict[str, Any]
    go: dict[str, tuple[str, str]]
    base_files: dict[str, str] = field(default_factory=dict)
    tip_files: dict[str, str] = field(default_factory=dict)
    tip_email: str = ""
    orphan_tip: bool = False
    gh_fail: bool = False
    session: str = ""
    args: dict[str, str] = field(default_factory=dict)

    # -- event helpers

    def events(self) -> list[dict[str, Any]]:
        return [d["event"] for e, d in self.frames if e == "agent"]

    def final_index(self) -> int:
        return max(i for i, (e, _) in enumerate(self.frames) if e == "agent")

    def add_event(self, author: str, *, meta: dict[str, Any] | None = None, content: Any = None,
                  caller: str = "sa:selfdev-dispatcher", at: int | None = None, ts: str = "2026-10-07T17:32:00Z") -> dict[str, Any]:
        """Insert an event (and its eventlog row) before the final answer,
        or at frame index `at`."""
        tmpl = copy.deepcopy(self.events()[-1])
        n = len(self.db["rows"]) + 100
        tmpl.update({"Author": author, "CustomMetadata": meta, "Content": content, "ID": f"doctored-{n}",
                     "Timestamp": ts, "TurnComplete": False})
        i = self.final_index() if at is None else at
        self.frames.insert(i, ("agent", {"seq": n, "event": tmpl}))
        self.db["rows"].append({"seq": n, "author": author, "event_id": tmpl["ID"], "timestamp": ts,
                                "metadata": json.dumps({"caller": caller}) if caller else ""})
        return tmpl

    def ci_run(self, path: str) -> dict[str, Any]:
        return next(w for w in self.runs["workflow_runs"] if w["path"] == path)


def load() -> Run:
    frames = []
    for blk in (FIXTURE / "replay.sse").read_text().split("\n\n"):
        ls = blk.strip().splitlines()
        if ls:
            frames.append((ls[0][6:].strip(), json.loads("".join(x[5:] for x in ls[1:]))))
    db = json.loads((FIXTURE / "eventlog.json").read_text())
    return Run(
        log=(FIXTURE / "daemon.log").read_text().splitlines(),
        frames=frames,
        perms=json.loads((FIXTURE / "perms.json").read_text()),
        baseline=json.loads((FIXTURE / "perms-baseline.json").read_text()),
        db=db,
        pr=json.loads((FIXTURE / "github" / "pulls.json").read_text()),
        runs=json.loads((FIXTURE / "github" / "runs.json").read_text()),
        go={s: ((FIXTURE / "go" / f"{s}.txt").read_text(), (FIXTURE / "go" / f"{s}.rc").read_text()) for s in ("base", "tip")},
        session=db["session_id"],
    )


def git(repo: pathlib.Path, *argv: str, email: str = "maintainer@example.com") -> str:
    env = dict(os.environ, GIT_AUTHOR_NAME="Fixture", GIT_AUTHOR_EMAIL=email,
               GIT_COMMITTER_NAME="Fixture", GIT_COMMITTER_EMAIL=email,
               GIT_CONFIG_GLOBAL=os.devnull, GIT_CONFIG_SYSTEM=os.devnull)
    p = subprocess.run(["git", "-C", str(repo), "-c", "commit.gpgsign=false", *argv],
                       capture_output=True, text=True, env=env, check=True)
    return p.stdout.strip()


def commit(repo: pathlib.Path, files: dict[str, str], msg: str, email: str = "maintainer@example.com") -> str:
    for rel, body in files.items():
        p = repo / rel
        p.parent.mkdir(parents=True, exist_ok=True)
        p.write_text(body)
    git(repo, "add", "-A")
    git(repo, "commit", "-q", "-m", msg, email=email)
    return git(repo, "rev-parse", "HEAD")


def build_repo(repo: pathlib.Path, run: Run) -> tuple[str, str]:
    repo.mkdir()
    git(repo, "init", "-q", "-b", "main")
    base = commit(repo, {"go.mod": "module github.com/go-steer/core-agent/v2\n",
                         "pkg/agent/doc.go": "package agent\n", "a7-fixture-state": "base\n", **run.base_files}, "base")
    if run.orphan_tip:
        git(repo, "checkout", "-q", "--orphan", "elsewhere")
    tip = commit(repo, {"pkg/agent/fix.go": "package agent\n", "a7-fixture-state": "tip\n", **run.tip_files}, "tip",
                 email=run.tip_email or COMMIT_EMAIL)
    return base, tip


def write(run: Run, tmp: pathlib.Path) -> list[str]:
    repo = tmp / "repo"
    base, tip = build_repo(repo, run)

    def fill(s: str) -> str:
        return s.replace("{TIP}", tip).replace("{BASE}", base)

    (tmp / "daemon.log").write_text("\n".join(run.log) + "\n")
    with (tmp / "replay.sse").open("w") as f:
        for e, d in run.frames:
            f.write(f"event: {e}\ndata: {json.dumps(d, ensure_ascii=False)}\n\n")
    (tmp / "perms.json").write_text(json.dumps(run.perms))
    (tmp / "perms-baseline.json").write_text(json.dumps(run.baseline))
    con = sqlite3.connect(tmp / "sessions.db")
    con.execute(SCHEMA)
    for r in run.db["rows"]:
        con.execute("INSERT INTO agent_eventlog (seq, app_name, user_id, session_id, event_id, branch, author, timestamp, "
                    "invocation_id, metadata) VALUES (?,?,?,?,?,?,?,?,?,?)",
                    (r["seq"], run.db["app_name"], run.db["user_id"], run.db.get("row_session", run.db["session_id"]),
                     r["event_id"], "", r["author"], r["timestamp"], "", r["metadata"]))
    con.commit()
    con.close()
    gh = tmp / "gh"
    gh.mkdir()
    (gh / "pulls.json").write_text(fill(json.dumps(run.pr)))
    (gh / "runs.json").write_text(fill(json.dumps(run.runs)))
    godir = tmp / "go"
    godir.mkdir()
    for s, (out, rc) in run.go.items():
        (godir / f"{s}.txt").write_text(out)
        (godir / f"{s}.rc").write_text(rc)
    argv = {"--log": str(tmp / "daemon.log"), "--replay": str(tmp / "replay.sse"), "--perms": str(tmp / "perms.json"),
            "--perms-baseline": str(tmp / "perms-baseline.json"),
            "--eventlog-db": str(tmp / "sessions.db"), "--session": run.session,
            "--repo": "mastersingh24/core-agent-selfdev", "--pr": "7", "--writer": WRITER,
            "--commit-email": COMMIT_EMAIL, "--checkout": str(repo),
            "--base": base, "--tip": tip, "--gh": str(FAKEBIN / "gh"), "--go": str(FAKEBIN / "go")}
    argv.update({k: fill(v) for k, v in run.args.items()})
    return [x for kv in argv.items() for x in kv]


def grade(run: Run, json_out: bool = True) -> tuple[int, Any, str, list[list[str]]]:
    with tempfile.TemporaryDirectory(prefix="a7-selftest-") as t:
        tmp = pathlib.Path(t)
        argv = write(run, tmp)
        env = dict(os.environ, FAKE_GH_DIR=str(tmp / "gh"), FAKE_GO_DIR=str(tmp / "go"))
        if run.gh_fail:
            env["FAKE_GH_FAIL"] = "1"
        cmd = [sys.executable, "-B", str(GRADER), *argv] + (["--json"] if json_out else [])
        p = subprocess.run(cmd, capture_output=True, text=True, env=env, check=False)
        calls_log = tmp / "gh" / "calls.log"
        calls = [json.loads(l) for l in calls_log.read_text().splitlines()] if calls_log.exists() else []
        body: Any = None
        if json_out:
            try:
                body = json.loads(p.stdout)
            except json.JSONDecodeError:
                body = None
        return p.returncode, body, p.stdout + p.stderr, calls


def report(ok: bool, desc: str, got: str = "") -> None:
    global failures
    if ok:
        print(f"  ok   {desc}")
    else:
        failures += 1
        print(f"  FAIL {desc}")
        if got:
            for line in got.splitlines()[:40]:
                print(f"         {line}")


def case(desc: str, doctor: Callable[[Run], None] | None, code: int, verdicts: dict[str, str] | None = None,
         row: str = "", message: str = "") -> None:
    run = load()
    if doctor:
        doctor(run)
    rc, body, out, calls = grade(run)
    want = dict(ALL_PASS, **(verdicts or {}))
    if body is None:
        report(False, desc, f"exit {rc}, no JSON:\n{out}")
        return
    got = {r["name"]: r["verdict"] for r in body["rows"]}
    problems = []
    if rc != code:
        problems.append(f"exit {rc}, want {code}")
    for name, v in want.items():
        if got.get(name) != v:
            problems.append(f"{name}: {got.get(name)}, want {v}")
    if row and message:
        evidence = next((r["evidence"] for r in body["rows"] if r["name"] == row), [])
        if not any(message in e for e in evidence):
            problems.append(f"{row} evidence lacks {message!r}: {evidence}")
    writes = [c for c in calls if c[:1] != ["api"] or len(c) != 2]
    if writes:
        problems.append(f"gh was called with more than `api <path>`: {writes}")
    report(not problems, desc, "; ".join(problems) + "\n" + json.dumps(body, indent=1)[:3000] if problems else "")


# ------------------------------------------------------------- doctors


def boot_line(prefix: str) -> Callable[[Run], int]:
    def find(run: Run) -> int:
        return next(i for i, l in enumerate(run.log) if prefix in l)
    return find


WATCHDOG, CEILING = boot_line("core-agent: watchdog: "), boot_line("core-agent: cost ceiling: ")


def set_line(find: Callable[[Run], int], old: str, new: str) -> Callable[[Run], None]:
    def doctor(run: Run) -> None:
        i = find(run)
        assert old in run.log[i], (old, run.log[i])
        run.log[i] = run.log[i].replace(old, new)
    return doctor


def drop_line(find: Callable[[Run], int]) -> Callable[[Run], None]:
    def doctor(run: Run) -> None:
        del run.log[find(run)]
    return doctor


def restart_without_ceiling(run: Run) -> None:
    # A second boot, as `kubectl logs --previous` + the new container
    # would show it, whose overlay lost max_*_cost_usd: watchdog line, no
    # ceiling line.
    run.log.append("2026-10-07T18:02:11.000000000Z " + run.log[WATCHDOG(run)].split(" ", 1)[1])


def no_boot(run: Run) -> None:
    run.log = [l for l in run.log if "core-agent: watchdog: " not in l and "core-agent: cost ceiling: " not in l]


def perms_mode(mode: str) -> Callable[[Run], None]:
    def doctor(run: Run) -> None:
        run.perms["mode"] = mode
    return doctor


def mode_change(run: Run) -> None:
    run.add_event("attach/perm-mode", meta={"source": "operator", "from": "auto", "to": "yolo",
                                            "caller": "sa:selfdev-dispatcher"})


def human_inject(run: Run) -> None:
    run.add_event("user", content={"role": "user", "parts": [{"text": "[Inbox]\n- from alice@example.com: try the other file"}]},
                  caller="alice@example.com")


def human_approval(run: Run) -> None:
    run.perms["approvals"].append({"tool": "bash", "key": "git commit -s -m fix", "decision": "allow-once",
                                   "at": "2026-10-07T17:32:40Z", "by": "alice@example.com"})


def unattributed_approval(run: Run) -> None:
    run.perms["approvals"].append({"tool": "edit_file", "key": "pkg/agent/compactor.go", "decision": "allow-session",
                                   "at": "2026-10-07T17:32:40Z"})


def human_deny(run: Run) -> None:
    run.add_event("core_agent", content={"role": "user", "parts": [{"functionResponse": {
        "id": "c9", "name": "bash", "response": {"error": "bash denied by user: git push origin agent/issue-1. Do not retry."}}}]})


def grep_mentions_deny(run: Run) -> None:
    # The worker greps pkg/permissions; the deny string in a tool's OUTPUT
    # is not a deny. Negative control for the anchored error match.
    run.add_event("core_agent", content={"role": "user", "parts": [{"functionResponse": {
        "id": "c8", "name": "grep", "response": {"matches": [
            'gate.go:1731: return fmt.Errorf("%s denied by user: %s.%s %s", req.ToolName, ...)']}}}]})
    run.add_event("core_agent", content={"role": "user", "parts": [{"functionResponse": {
        "id": "c10", "name": "read_file", "response": {"error": "read_file: notes.txt: no such file (it was to quote "
                                                                "'bash denied by user: x')"}}}]})


def guardrail_reset(run: Run) -> None:
    run.add_event("attach/guardrail-reset", meta={"source": "operator", "reset": ["cost_ceiling"],
                                                  "caller": "sa:selfdev-dispatcher"})


def foreign_owner(run: Run) -> None:
    i = next(i for i, l in enumerate(run.log) if "session created (owner=" in l)
    run.log[i] = run.log[i].replace("owner=sa:selfdev-dispatcher", "owner=alice@example.com")


def no_dispatcher(run: Run) -> None:
    for r in run.db["rows"]:
        r["metadata"] = ""


def halt(run: Run) -> None:
    run.add_event("agent/guardrail-trip", meta={"source": "agent", "guardrail": "watchdog",
                                                "reason": "repeated identical tool call", "halted_turn": True})


def turn_trip(run: Run) -> None:
    run.add_event("agent/guardrail-turn-trip", meta={"source": "agent", "guardrail": "cost_ceiling",
                                                     "reason": "per-turn cost $10.41 exceeds the $10.00 ceiling",
                                                     "halted_turn": True})


def refusal_storm(run: Run) -> None:
    run.add_event("gate/refusal-storm", meta={"source": "agent", "refusals": 6})


def turn_error_last(run: Run) -> None:
    run.add_event("agent/turn-error", meta={"source": "agent", "kind": "provider", "message": "Error 529 overloaded",
                                            "retryable": True}, at=len(run.frames))


def turn_error_recovered(run: Run) -> None:
    run.add_event("agent/turn-error", meta={"source": "agent", "kind": "provider", "message": "Error 529 overloaded",
                                            "retryable": True})


def interrupted(run: Run) -> None:
    run.add_event("attach/interrupt", meta={"source": "operator"}, at=len(run.frames))


def ends_on_call(run: Run) -> None:
    run.add_event("core_agent", content={"role": "model", "parts": [{"functionCall": {
        "id": "c7", "name": "bash", "args": {"command": "go test ./..."}}}]}, at=len(run.frames))


def pr_state(state: str, merged: str | None) -> Callable[[Run], None]:
    def doctor(run: Run) -> None:
        run.pr["state"], run.pr["merged_at"] = state, merged
    return doctor


def compacted(run: Run, mechanical: bool = False) -> None:
    md: dict[str, Any] = {"compaction": "summary", "compaction_focus": ""}
    if mechanical:
        md["compaction_mechanical"] = True
    run.add_event("core_agent", meta=md, content={"role": "model", "parts": [{"text": "# Current state\n..."}]},
                  at=3)


def compacted_and_halted(run: Run) -> None:
    compacted(run)
    halt(run)


def checkpointed(run: Run) -> None:
    run.add_event("core_agent", meta={"compaction": "checkpoint", "checkpoint_note": "done"},
                  content={"role": "model", "parts": [{"text": "# Checkpoint\nshipped"}]})


def go_out(state: str, out: str, rc: str) -> Callable[[Run], None]:
    def doctor(run: Run) -> None:
        run.go[state] = (out, rc)
    return doctor


BUILD_FAILURE = ("# github.com/go-steer/core-agent/v2/pkg/agent_test [github.com/go-steer/core-agent/v2/pkg/agent.test]\n"
                 "pkg/agent/zz_a7_oracle_test.go:162:18: undefined: agent.WithoutSessionTitle\n"
                 "FAIL\tgithub.com/go-steer/core-agent/v2/pkg/agent [build failed]\nFAIL\n")


def rig_failure_at_tip(run: Run) -> None:
    out = run.go["tip"][0].replace("--- PASS: TestA7Oracle1234Recovery/rate_limit_writes_no_boundary",
                                   "--- FAIL: TestA7Oracle1234Recovery/rate_limit_writes_no_boundary")
    run.go["tip"] = (out.replace("PASS\nok", "    zz_a7_oracle_test.go:283: A7 ORACLE RIG: the summarizer was tried in 2 "
                                             "turns, want one\nFAIL\nFAIL"), "1")


def testmain_skips(run: Run) -> None:
    # A tip whose TestMain exits 0 without running anything: go test says
    # ok, but no subtest ever reports PASS.
    run.go["tip"] = ("ok  \tgithub.com/go-steer/core-agent/v2/pkg/agent\t0.004s\n", "0")


def contaminated(where: str) -> Callable[[Run], None]:
    def doctor(run: Run) -> None:
        files = run.base_files if where == "base" else run.tip_files
        if where == "base":
            files["dev/uat/selfdev-soak/oracle/seed0_1234_recovery_test.go"] = "//go:build a7oracle\n"
        else:
            files["notes/peek.txt"] = "the summarizer sentinel is A7-ORACLE-SUMMARIZER-REQUEST\n"
    return doctor


def orphan(run: Run) -> None:
    run.orphan_tip = True


def no_ci_workflow(run: Run) -> None:
    run.runs["workflow_runs"] = [w for w in run.runs["workflow_runs"] if w["path"] != ".github/workflows/ci.yml"]


def ci_failed(run: Run) -> None:
    run.ci_run(".github/workflows/ci.yml")["conclusion"] = "failure"


def ci_running(run: Run) -> None:
    w = run.ci_run(".github/workflows/ci.yml")
    w["status"], w["conclusion"] = "in_progress", None


def attribution_failed(run: Run) -> None:
    run.ci_run(".github/workflows/agent-attribution.yml")["conclusion"] = "failure"


def ci_rerun_green(run: Run) -> None:
    w = run.ci_run(".github/workflows/ci.yml")
    old = dict(w, id=w["id"] - 1000, conclusion="failure")
    run.runs["workflow_runs"].append(old)


def ci_rerun_red(run: Run) -> None:
    w = run.ci_run(".github/workflows/ci.yml")
    run.runs["workflow_runs"].append(dict(w, id=w["id"] + 1000, conclusion="failure"))


def ci_other_sha(run: Run) -> None:
    run.ci_run(".github/workflows/ci.yml")["head_sha"] = "0" * 40


def gh_down(run: Run) -> None:
    run.gh_fail = True


def head_moved(run: Run) -> None:
    run.pr["head"]["sha"] = "f" * 40


def body_session(run: Run) -> None:
    run.pr["body"] = run.pr["body"].replace(run.session, "01a1ffff-0000-7000-8000-000000000000")


def wrong_session_db(run: Run) -> None:
    run.db["row_session"] = "01a1ffff-0000-7000-8000-000000000000"


def replay_truncated(run: Run) -> None:
    run.log.append(f"2026-10-07T17:40:00.000000000Z core-agent: attach: broadcaster core-agent/{run.session} replay "
                   f"truncated: since=0 is below the session's replay floor 6011; replaying its newest 5000 events only")


def log_only_retry(run: Run) -> None:
    # A provider retry only the daemon log saw: a2_count FAILs it, and A7
    # still records rather than gates on it.
    run.log.append("2026-10-07T17:31:20.000000000Z core-agent: anthropic: transient provider error "
                   "(Error 429) — retrying once after 2s")


def ci_attempt_2(run: Run) -> None:
    run.ci_run(".github/workflows/ci.yml")["run_attempt"] = 2


def human_trigger(run: Run) -> None:
    run.ci_run(".github/workflows/review-gate.yml")["triggering_actor"] = {"login": "alice", "type": "User"}


def edits_workflow(run: Run) -> None:
    run.tip_files[".github/workflows/ci.yml"] = "name: CI\non: [pull_request]\njobs: {}\n"


def edits_presubmit(run: Run) -> None:
    run.tip_files["dev/ci/presubmits/test-unit"] = "#!/bin/sh\nexit 0\n"


def too_many_runs(run: Run) -> None:
    run.runs["total_count"] = 140


def runs_not_list(run: Run) -> None:
    run.runs["workflow_runs"] = {"oops": True}


def db_only_rows(run: Run) -> None:
    # A replay cut short (curl --max-time) after the inject: the halt and
    # the interrupt are in the DB only.
    for author, md in (("agent/guardrail-trip", {"guardrail": "watchdog", "reason": "loop"}),
                       ("attach/interrupt", {"source": "operator"})):
        n = len(run.db["rows"]) + 500
        run.db["rows"].append({"seq": n, "author": author, "event_id": f"db-only-{n}",
                               "timestamp": "2026-10-07T17:40:00Z", "metadata": ""})


def allow_drift(run: Run) -> None:
    run.perms["allow"] = run.perms["allow"] + ["bash:*"]


def deny_drift(run: Run) -> None:
    run.perms["deny"] = ["bash:git push*"]


def escalation(expired: bool) -> Callable[[Run], None]:
    def doctor(run: Run) -> None:
        run.add_event("gate/approver", caller="", meta={"source": "approver", "tool": "bash", "detail": "rm -rf /tmp/x",
                                                       "verdict": "escalate", "reason": "deletes files",
                                                       "model": "claude-haiku-4-5"})
        if expired:
            run.add_event("core_agent", content={"role": "user", "parts": [{"functionResponse": {
                "id": "c6", "name": "bash", "response": {"error": "permissions: approval request expired with no "
                                                                  "answer; the action was not taken after 30m0s "
                                                                  "(tool=bash detail=\"rm -rf /tmp/x\")"}}}]})
    return doctor


def boot_at(ts: str) -> Callable[[Run], None]:
    def doctor(run: Run) -> None:
        for find in (WATCHDOG, CEILING):
            run.log.append(f"{ts} " + run.log[find(run)].split(" ", 1)[1])
    return doctor


def resumed(owner: str, ts: str = "2026-10-07T17:30:00.000000000Z") -> Callable[[Run], None]:
    def doctor(run: Run) -> None:
        run.log.append(f"{ts} core-agent: session resumed (owner={owner}, id={run.session})")
    return doctor


def proxied(run: Run) -> None:
    run.db["rows"][0]["metadata"] = json.dumps({"caller": "sa:selfdev-dispatcher", "proxy_by": "sa:slack-bot"})


def anonymous_inject(run: Run) -> None:
    # runner.WakeLoop runs a woken turn on a context with no caller, so
    # the "[Background reports]" user event it writes has no sidecar.
    run.add_event("user", caller="", content={"role": "user", "parts": [{"text": "[Background reports]\n- reviewer: done"}]})


def auth_disabled(run: Run) -> None:
    i = next(i for i, l in enumerate(run.log) if "multi-session auth: " in l)
    run.log[i] = run.log[i].split(" core-agent: ", 1)[0] + (" core-agent: multi-session auth: disabled (single-user mode; "
                                                           "use --attach-token-file or --attach-token for bearer auth)")


def approvals_lost(run: Run) -> None:
    run.perms["approvals"] = []


def restart_after_no_allows(run: Run) -> None:
    boot_at("2026-10-07T18:30:00.000000000Z")(run)
    for i, (e, d) in enumerate(run.frames):
        if e == "agent" and d["event"]["Author"] == "gate/approver":
            del run.frames[i]
            run.db["rows"] = [r for r in run.db["rows"] if r["event_id"] != d["event"]["ID"]]
            break
    run.perms["approvals"] = []


def upper_email(run: Run) -> None:
    run.tip_email = COMMIT_EMAIL.upper()


def escalation_offset(run: Run) -> None:
    escalation(False)(run)
    run.add_event("core_agent", content={"role": "user", "parts": [{"functionResponse": {
        "id": "c5", "name": "write_file", "response": {"error": "permissions: approval request expired with no answer; "
                                                               "the action was not taken after 30m0s (tool=write_file "
                                                               "detail=\"/etc/hosts\")"}}}]})


def no_created_line(run: Run) -> None:
    run.log = [l for l in run.log if "session created (owner=" not in l]


def foreign_author(run: Run) -> None:
    run.pr["user"]["login"] = "alice"


def foreign_branch(run: Run) -> None:
    run.pr["head"]["ref"] = "alice/fix"


def foreign_commit(run: Run) -> None:
    run.tip_email = "alice@example.com"


def checkpoint_then_calls(run: Run) -> None:
    run.add_event("core_agent", meta={"compaction": "checkpoint", "checkpoint_note": "half way"},
                  content={"role": "model", "parts": [{"text": "# Checkpoint"}]}, at=3)


def log_only_halt(run: Run) -> None:
    run.log.append(f"2026-10-07T17:32:30.000000000Z 2026/10/07 17:32:30 agent: [session {run.session}] watchdog "
                   f"guardrail tripped: repeated identical tool call")


def perms_approvals_object(run: Run) -> None:
    run.perms["approvals"] = {"tool": "bash"}


CASES: list[tuple[str, Callable[[Run], None] | None, int, dict[str, str], str, str]] = [
    ("the recorded passing run grades PASS", None, 0, {}, "task oracle", "base"),
    ("posture: watchdog warn", set_line(WATCHDOG, "watchdog: enforce mode", "watchdog: warn mode"), 1,
     {"posture": "FAIL"}, "posture", "`watchdog: warn`"),
    ("posture: watchdog off", set_line(WATCHDOG, "watchdog: enforce mode [safety.watchdog config]",
                                       "watchdog: off [safety.watchdog config]"), 1, {"posture": "FAIL"}, "posture", "`watchdog: off`"),
    ("posture: no cost ceiling line", drop_line(CEILING), 1, {"posture": "FAIL"}, "posture", "a boot ran with no cost ceiling"),
    ("posture: per-session ceiling disabled", set_line(CEILING, "per-session=$25.0000", "per-session=disabled"), 1,
     {"posture": "FAIL"}, "posture", "per-session cost ceiling is disabled"),
    ("posture: per-turn ceiling zero", set_line(CEILING, "per-turn=$10.0000", "per-turn=$0.0000"), 1,
     {"posture": "FAIL"}, "posture", "per-turn cost ceiling is $0.0000"),
    ("posture: a restart booted without a ceiling", restart_without_ceiling, 1, {"posture": "FAIL"},
     "posture", "2 boot(s) in the log but 1 `cost ceiling:` line(s)"),
    ("posture: no boot lines captured", no_boot, 1, {"posture": "FAIL"}, "posture", "--previous after a restart"),
    ("posture: session mode ask", perms_mode("ask"), 1, {"posture": "FAIL"}, "posture", "permission mode is 'ask'"),
    ("posture: mode changed mid-run", mode_change, 1, {"posture": "FAIL"}, "posture", "auto → yolo"),
    ("posture: auto with nothing eligible",
     lambda r: r.log.append("2026-10-07T17:25:41.500000000Z core-agent: permissions.auto.eligible and eligible_bundles are "
                            "both empty, so the approver decides nothing and mode \"auto\" asks a person about every call, "
                            "as \"ask\" does"), 1, {"posture": "FAIL"}, "posture", "both empty"),
    ("no human: a person injected into the session", human_inject, 1, {"no human": "FAIL"}, "no human",
     "1 eventlog row(s) written by alice@example.com"),
    ("no human: a person approved a prompt", human_approval, 1, {"no human": "FAIL"}, "no human",
     "by alice@example.com"),
    ("no human: an unattributed answer is a person's too", unattributed_approval, 1, {"no human": "FAIL"}, "no human",
     "by an unattributed answerer"),
    ("no human: a person denied a prompt", human_deny, 1, {"no human": "FAIL"}, "no human", "bash denied by user: git push"),
    ("no human: the deny string in a tool's output is not a deny", grep_mentions_deny, 0, {}, "", ""),
    ("no human: guardrails/reset in the history", guardrail_reset, 1, {"no human": "FAIL"}, "no human",
     "guardrails/reset in the session"),
    ("no human: the session belongs to someone else", foreign_owner, 1, {"no human": "FAIL"}, "no human",
     "created by alice@example.com"),
    ("no human: no row carries the dispatcher", no_dispatcher, 1, {"no human": "FAIL"}, "no human",
     "the dispatcher did not drive it"),
    ("ended by work: halted", halt, 1, {"ended by work": "FAIL"}, "ended by work",
     "a guardrail halted the session: watchdog"),
    ("ended by work: the per-turn ceiling cut a turn", turn_trip, 1, {"ended by work": "FAIL"}, "ended by work",
     "a guardrail cut a turn: cost_ceiling"),
    ("ended by work: a refusal storm", refusal_storm, 1, {"ended by work": "FAIL"}, "ended by work", "refusal storm"),
    ("ended by work: the last turn errored", turn_error_last, 1, {"ended by work": "FAIL"}, "ended by work",
     "last turn ended in an error: provider"),
    ("ended by work: a turn error a later turn recovered from", turn_error_recovered, 0, {}, "", ""),
    ("ended by work: interrupted at the wallclock", interrupted, 1, {"ended by work": "FAIL"}, "ended by work",
     "session timeout (the wallclock)"),
    ("ended by work: the last model event is a tool call", ends_on_call, 1, {"ended by work": "FAIL"}, "ended by work",
     "is a call to bash"),
    ("ended by work: PR closed unmerged", pr_state("closed", None), 1, {"ended by work": "FAIL"}, "ended by work",
     "closed without merging"),
    ("ended by work: PR merged by a person afterwards", pr_state("closed", "2026-10-07T19:00:00Z"), 0, {},
     "ended by work", "merged at 2026-10-07T19:00:00Z"),
    ("ended by work: a checkpoint is not a compaction", checkpointed, 0, {}, "ended by work", "checkpoint boundary"),
    ("VOID: the session compacted", compacted, 3, {"ended by work": "VOID"}, "ended by work", "decision 19"),
    ("VOID: a mechanical compaction is a compaction",
     lambda r: compacted(r, mechanical=True), 3, {"ended by work": "VOID"}, "ended by work", "a mechanical boundary"),
    ("VOID outranks FAIL, and the FAIL is still shown", compacted_and_halted, 3, {"ended by work": "VOID"},
     "ended by work", "a guardrail halted the session"),
    ("task oracle: passes at the base", go_out("base", load().go["tip"][0], "0"), 1, {"task oracle": "FAIL"},
     "task oracle", "does not fail at the base"),
    ("task oracle: a build failure at the base is not evidence", go_out("base", BUILD_FAILURE, "1"), 1,
     {"task oracle": "FAIL"}, "task oracle", "not for its reason (want 'A7 ORACLE UNFIXED:' in "
     "empty_stop_twice_writes_mechanical_boundary): pkg/agent/zz_a7_oracle_test.go:162:18: undefined: agent.WithoutSessionTitle"),
    ("task oracle: fails at the tip", go_out("tip", load().go["base"][0], "1"), 1, {"task oracle": "FAIL"},
     "task oracle", "A7 ORACLE UNFIXED: the mechanical fallback came only after 2 failed compactions"),
    ("task oracle: a RIG failure at the tip", rig_failure_at_tip, 1, {"task oracle": "FAIL"}, "task oracle",
     "A7 ORACLE RIG"),
    ("task oracle: `ok` with no subtest run", testmain_skips, 1, {"task oracle": "FAIL"}, "task oracle",
     "does not pass at the tip"),
    ("task oracle: the tip does not descend from the base", orphan, 1, {"task oracle": "FAIL"}, "task oracle",
     "does not descend from the base"),
    ("VOID: the oracle is in the worker's tree at the base", contaminated("base"), 3, {"task oracle": "VOID"},
     "task oracle", "dev/uat/selfdev-soak/oracle"),
    ("VOID: the oracle's sentinel is in the tip", contaminated("tip"), 3, {"task oracle": "VOID"}, "task oracle",
     "notes/peek.txt"),
    ("CI: no CI workflow run, only same-named no-op jobs (#1271)", no_ci_workflow, 1, {"CI": "FAIL"}, "CI", "#1271"),
    ("CI: CI failed while the no-op jobs passed", ci_failed, 1, {"CI": "FAIL"}, "CI", "concluded failure"),
    ("CI: still running", ci_running, 1, {"CI": "FAIL"}, "CI", "still in_progress"),
    ("CI: another required workflow failed", attribution_failed, 1, {"CI": "FAIL"}, "CI",
     "Agent attribution (.github/workflows/agent-attribution.yml) concluded failure"),
    ("CI: a second CI run on the head (a reopen) is a person in between", ci_rerun_green, 1, {"CI": "FAIL"}, "CI",
     "ran 2 times"),
    ("CI: a re-run attempt is a person in between", ci_attempt_2, 1, {"CI": "FAIL"}, "CI", "attempt 2: someone re-ran it"),
    ("CI: a run a person triggered", human_trigger, 1, {"CI": "FAIL"}, "CI", "triggered by alice"),
    ("CI: the PR edits a workflow", edits_workflow, 1, {"CI": "FAIL"}, "CI", "changes what CI runs (.github/workflows/ci.yml)"),
    ("CI: the PR edits a presubmit", edits_presubmit, 1, {"CI": "FAIL"}, "CI", "dev/ci/presubmits/test-unit"),
    ("CI: more runs than one page", too_many_runs, 1, {"CI": "FAIL"}, "CI", "more than one page"),
    ("CI: a malformed runs answer fails, without a traceback", runs_not_list, 1, {"CI": "FAIL"}, "CI",
     "no workflow_runs list"),
    ("CI: a green run superseded by a red re-run", ci_rerun_red, 1, {"CI": "FAIL"}, "CI", "concluded failure"),
    ("CI: a run on another commit does not count", ci_other_sha, 1, {"CI": "FAIL"}, "CI", "never ran on"),
    ("CI: GitHub unreachable", gh_down, 1, {"CI": "FAIL", "run identity": "FAIL", "ended by work": "FAIL"}, "CI",
     "exited 1: HTTP 502"),
    ("run identity: the PR head is not --tip", head_moved, 1, {"run identity": "FAIL"}, "run identity",
     "not --tip"),
    ("run identity: the PR names another session", body_session, 1, {"run identity": "FAIL"}, "run identity",
     "names session 01a1ffff"),
    ("run identity: the DB holds another session", wrong_session_db, 1,
     {"run identity": "FAIL", "no human": "FAIL"}, "run identity", "replay events are not rows of session"),
    ("run identity: the replay was truncated", replay_truncated, 1, {"run identity": "FAIL"}, "run identity",
     "truncated a replay"),
    ("degraded visibly is recorded, never gating", log_only_retry, 0, {}, "degraded visibly",
     "provider retry: FAIL (log 1, transcript 0)"),
    ("run identity: a DB row the replay lacks (a capture cut short)", db_only_rows, 1, {"run identity": "FAIL"},
     "run identity", "the replay lacks 2 of the session's"),
    ("run identity: the PR was opened by someone else", foreign_author, 1, {"run identity": "FAIL"}, "run identity",
     "opened by 'alice'"),
    ("run identity: the PR is not on the dispatcher's branch", foreign_branch, 1, {"run identity": "FAIL"},
     "run identity", "not the dispatcher's agent/issue-<N>"),
    ("run identity: a commit the session did not make", foreign_commit, 1, {"run identity": "FAIL"}, "run identity",
     "authored by alice@example.com"),
    ("no human: an allow pattern a fresh session lacks (POST /perms/allow)", allow_drift, 1, {"no human": "FAIL"},
     "no human", "allow list holds 1 pattern(s) a fresh session does not (POST /perms/allow writes no row): bash:*"),
    ("no human: a deny pattern a fresh session lacks", deny_drift, 1, {"no human": "FAIL"}, "no human",
     "deny list holds 1 pattern(s)"),
    ("no human: an escalation with no outcome", escalation(False), 1, {"no human": "FAIL"}, "no human",
     "passed bash 'rm -rf /tmp/x' to a person, and no expired prompt answers it"),
    ("no human: an expiry of another call does not answer an escalation", escalation_offset, 1, {"no human": "FAIL"},
     "no human", "passed bash 'rm -rf /tmp/x' to a person"),
    ("no human: an escalation that expired unanswered", escalation(True), 0, {}, "no human", "1 escalation(s), each expired"),
    ("no human: a proxy-asserted dispatcher", proxied, 1, {"no human": "FAIL"}, "no human", "asserted by the proxy sa:slack-bot"),
    ("no human: a turn a background report woke has no caller, and is not a person", anonymous_inject, 0, {}, "", ""),
    ("no human: the daemon ran without per-caller auth", auth_disabled, 1, {"no human": "FAIL"}, "no human",
     "without per-caller auth"),
    ("VOID: the perms capture lost the approver's allows (a restart before it)", approvals_lost, 3, {"no human": "VOID"},
     "no human", "the approval log holds 0 approver allow(s) but the eventlog 1"),
    ("VOID: a restart after the run, with no approver allow to check the capture against",
     restart_after_no_allows, 3, {"no human": "VOID"}, "no human", "restarted or the session was resumed after the run"),
    ("run identity: the commit email's case does not matter", upper_email, 0, {}, "", ""),
    ("no human: no `session created` line", no_created_line, 1, {"no human": "FAIL"}, "no human",
     "no `session created (owner=…"),
    ("VOID: the daemon restarted mid-run", boot_at("2026-10-07T17:28:00.000000000Z"), 3, {"no human": "VOID"},
     "no human", "the daemon restarted mid-run"),
    ("a restart after the run, with the approval log intact, is not mid-run", boot_at("2026-10-07T18:30:00.000000000Z"),
     0, {}, "", ""),
    ("VOID: the session was resumed mid-run", resumed("sa:selfdev-dispatcher"), 3, {"no human": "VOID"}, "no human",
     "resumed mid-run"),
    ("a person who resumed the session is named, and the run is void", resumed("alice@example.com"), 3,
     {"no human": "VOID"}, "no human", "resumed by alice@example.com"),
    ("VOID: tool calls after a checkpoint", checkpoint_then_calls, 3, {"ended by work": "VOID"}, "ended by work",
     "after a checkpoint"),
    ("ended by work: a trip only the log witnessed", log_only_halt, 1, {"ended by work": "FAIL"}, "ended by work",
     "the log records a watchdog guardrail trip"),
]


def text_case() -> None:
    rc, _, out, _ = grade(load(), json_out=False)
    ok = rc == 0 and "| task oracle | **PASS** |" in out and "A7: **PASS**" in out and \
        "A7 ORACLE UNFIXED: the mechanical fallback came only after 2 failed compactions" in out
    report(ok, "the table names the base's oracle failure line and the overall verdict", out)


def input_error_case() -> None:
    run = load()
    perms_approvals_object(run)
    with tempfile.TemporaryDirectory() as t:
        argv = write(run, pathlib.Path(t))
        env = dict(os.environ, FAKE_GH_DIR=str(pathlib.Path(t) / "gh"), FAKE_GO_DIR=str(pathlib.Path(t) / "go"))
        p = subprocess.run([sys.executable, "-B", str(GRADER), *argv], capture_output=True, text=True, env=env, check=False)
    report(p.returncode == 2 and "`approvals` is not a list" in p.stderr,
           "a perms capture of the wrong shape is an input error (exit 2), not a silent pass", p.stdout + p.stderr)


def usage_case() -> None:
    run = load()
    run.args = {"--base": "abc123"}
    with tempfile.TemporaryDirectory() as t:
        argv = write(run, pathlib.Path(t))
        p = subprocess.run([sys.executable, "-B", str(GRADER), *argv], capture_output=True, text=True, check=False)
    report(p.returncode == 2 and "full 40-character commit SHA" in p.stderr, "a short SHA is a usage error (exit 2)",
           p.stdout + p.stderr)


def main() -> int:
    for t in ("git",):
        if shutil.which(t) is None:
            print(f"  FAIL {t} is not on PATH")
            return 1
    print("grade_a7.py self-test")
    for desc, doctor, code, verdicts, row, message in CASES:
        case(desc, doctor, code, verdicts, row, message)
    text_case()
    input_error_case()
    usage_case()
    print(f"\n{len(CASES) + 3 - failures} passed, {failures} failed")
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main())
