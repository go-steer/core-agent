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

"""Grade v3.0 box A7, "Develops itself", over one self-development run.

    ./grade_a7.py --log daemon.log --replay replay-<sid>.sse \\
        --perms perms.json --perms-baseline perms-fresh.json \\
        --eventlog-db sessions.db --session <sid> \\
        --repo mastersingh24/core-agent-selfdev --pr <N> \\
        --writer <writer App login> --commit-email <soak commit email> \\
        --checkout <clone of the mirror> --base <sha> --tip <sha> [--json]

A7 (#1042) closes on the first issue whose PR opens with green CI and no
human between the task and the PR. The assertions are the "A7 grading"
table in docs/selfdev-soak-design.md, plus one this grader adds so the
others are about the same run. Every check names the source it reads,
because a wording change there silently disarms it.

  run identity   The inputs describe ONE run, made by the rig:
                   - the PR's head is --tip, on a branch agent/issue-<N>,
                     opened by --writer (the dispatcher's App);
                   - its body (dispatcher pr.go prBody) names --session and
                     `base <--base>`;
                   - every commit in base..tip was authored and committed
                     as --commit-email (decision 16), so a commit a person
                     pushed onto the branch is not graded as the agent's;
                   - the replay and the eventlog DB hold the same events
                     for the session, both ways. A replay cut short (curl
                     --max-time, the 5000-event cap that logs "replay
                     truncated", pkg/attach/broadcaster.go) would hide
                     every row after the cut.

  posture        The daemon's own boot lines (cmd/core-agent/main.go), on
                 every boot in the log:
                   "core-agent: watchdog: enforce mode [...]"
                   "core-agent: cost ceiling: per-turn=$X per-session=$Y [...]"
                 with X > 0 and Y > 0. The daemon prints NO permission-mode
                 boot line, so the mode is read from the session itself:
                 GET /sessions/<app>/<sid>/perms (`mode`, PermsInfo in
                 pkg/attach/state.go) must say "auto", and no
                 `attach/perm-mode` row may record a change. The
                 cmd/core-agent/approver.go warning that eligible and
                 eligible_bundles are both empty fails it too.

  no human       The daemon logs nothing for an attach client, a
                 /perms/respond or a /guardrails/reset, so this reads what
                 they leave behind:
                   - the eventlog DB's per-row sidecar (`agent_eventlog.
                     metadata`, pkg/agent/metadata.go): every row's
                     "caller" must be the dispatcher, core-agent/
                     auto-continue, or none (a row written outside any
                     request: an approver audit, a turn a background
                     report woke); no row may carry "proxy_by". That only
                     names every client because the daemon ran per-caller
                     auth, so every boot must print "multi-session auth:
                     <kind>, ..." and not "disabled";
                   - the approval log (`approvals` in the perms capture,
                     #830): a row without `approver_model` is an answer a
                     person gave, attributed (`by`) or not;
                   - a person's deny, which the approval log omits: a
                     function response whose error is
                     "<tool> denied by user: ..." (pkg/permissions/gate.go);
                   - every escalation (`gate/approver` row, verdict
                     escalate) must be answered by an expiry of the same
                     call ("permissions: approval request expired with no
                     answer ... (tool=T detail="D")"), or a person answered;
                   - the approval log must hold every approver allow the
                     eventlog does (`gate/approver`, verdict allow): fewer
                     means the capture came from a restarted process, whose
                     log is empty (VOID);
                   - POST /perms/allow and /perms/deny write no row, so the
                     session's allow and deny lists must equal those of a
                     fresh session on the same daemon (--perms-baseline);
                   - an `attach/guardrail-reset` row, whoever wrote it;
                   - "core-agent: session created|resumed (owner=X, id=<sid>)"
                     (pkg/compose/multi_session.go) must name the dispatcher.
                 A daemon restart or a session resume during the run makes
                 the row VOID: the approval log is held in memory and starts
                 empty, and the approver loses its task (P1).
                 Not visible on any surface: a client that only reads, and
                 POST /pause, /reload, /pricing/set and agents/<n>/stop,
                 which write nothing.

  ended by work  The PR is open (or a person merged it after), and the
                 session ended on a finished answer: its last model event
                 is text with no function call. Not ended by a halt
                 (`agent/guardrail-trip`), a per-turn trip
                 (`agent/guardrail-turn-trip`), a refusal storm
                 (`gate/refusal-storm`), a turn error with no model output
                 after it (`agent/turn-error`), an interrupt
                 (`attach/interrupt`: the dispatcher interrupts a session it
                 stops, including at its session timeout, the run's
                 wallclock), or a guardrail trip only the log witnessed
                 (pkg/agent/guardrail_halt.go's lines, matched as
                 a2_count.py does). The rows are #1258's. A compaction
                 boundary (CustomMetadata `compaction: summary`, mechanical
                 or not) makes the run VOID, not failed (decision 19), and
                 so does a checkpoint boundary followed by more tool calls:
                 both drop the approver's task.

  degraded visibly  dev/uat/gke-drill/a2_count.py over the log and the
                 replay, windowed to the session. Recorded, never gating.

  task oracle    oracle/seed0_1234_recovery_test.go, copied into pkg/agent
                 of an export of --base and of --tip (written blob by blob
                 from `git ls-tree`, so no .gitattributes in the tree shapes
                 it) and run with `go test -tags a7oracle`. It must FAIL at
                 the base for the oracle's reason ("A7 ORACLE UNFIXED:") and
                 PASS at the tip, both subtests by name. A failure marked
                 "A7 ORACLE RIG:", or a build failure, is evidence of
                 nothing. The tip must descend from the base. If the oracle
                 or this grader is in the worker's tree at either commit,
                 the answer key reached the worker (decision 20): VOID.

  CI             GitHub's workflow runs on the PR head, read with
                 `gh api` (GET only). The CI workflow
                 (.github/workflows/ci.yml, matched by PATH, never by name:
                 ci-docs.yml's no-op jobs carry the same check names, #1271)
                 must have run exactly once on the head, on its first
                 attempt, and concluded success. Every other workflow's
                 latest run must have completed without failing. A run
                 someone other than --writer triggered (a re-run, a reopen,
                 an edit) is a person in between. And the PR may not change
                 what CI runs: .github/, dev/ci/ or dev/tools/.

Verdict: VOID if any row is VOID (the run is not evidence either way);
otherwise FAIL if any gating row FAILs; otherwise PASS. Exit 0 on PASS,
1 on FAIL, 3 on VOID, 2 on a usage or input error.
"""

from __future__ import annotations

import argparse
import json
import os
import pathlib
import re
import shutil
import sqlite3
import subprocess
import sys
import tempfile
from dataclasses import dataclass, field
from datetime import datetime
from typing import Any

HERE = pathlib.Path(__file__).resolve().parent
sys.path.insert(0, str(HERE.parent / "gke-drill"))
import a2_count  # noqa: E402

PASS, FAIL, VOID, RECORDED = "PASS", "FAIL", "VOID", "RECORDED"
EXIT = {PASS: 0, FAIL: 1, VOID: 3}

DISPATCHER = "sa:selfdev-dispatcher"
AUTO_CONTINUE = "core-agent/auto-continue"  # pkg/agent/autocontinue.go AutoContinueOriginator

ORACLE = HERE / "oracle" / "seed0_1234_recovery_test.go"
ORACLE_TEST = "TestA7Oracle1234Recovery"
ORACLE_SUBTESTS = ("empty_stop_twice_writes_mechanical_boundary", "rate_limit_writes_no_boundary")
ORACLE_DEST = "pkg/agent/zz_a7_oracle_test.go"
ORACLE_UNFIXED, ORACLE_RIG = "A7 ORACLE UNFIXED:", "A7 ORACLE RIG:"
# In the worker's tree, any of these is the answer key (decision 20). The
# sentinel string is in the oracle and in this file, so a copy anywhere
# in the tree is found too.
ANSWER_KEY_PATHS = ("dev/uat/selfdev-soak/oracle", "dev/uat/selfdev-soak/grade_a7.py")
ANSWER_KEY_SENTINEL = "A7-ORACLE-SUMMARIZER-REQUEST"
ORACLE_TIMEOUT = 1200

CI_WORKFLOW = ".github/workflows/ci.yml"
CI_INPUTS = (".github/", "dev/ci/", "dev/tools/")
BAD_CONCLUSIONS = {"failure", "cancelled", "timed_out", "action_required", "startup_failure", "stale"}

TS_PREFIX_RE = re.compile(r"^\d{4}-\d\d-\d\dT\S+\s+")
WATCHDOG_RE = re.compile(r"core-agent: watchdog: (off|warn|feedback|enforce)\b(?: mode)? \[([^\]]*)\]")
CEILING_RE = re.compile(r"core-agent: cost ceiling: per-turn=\$([0-9.]+) per-session=(?:\$([0-9.]+)|(disabled)) "
                        r"\[session ceiling from ([^\]]*)\]")
PLAN_RE = re.compile(r"core-agent: plan mode: (\S+)")
EMPTY_ELIGIBLE = "permissions.auto.eligible and eligible_bundles are both empty"
AUTH_LINE_RE = re.compile(r"core-agent: multi-session auth: (\S+?),? ")  # pkg/compose/startup_summary.go
SESSION_LINE_RE = re.compile(r"core-agent: session (created|resumed) \(owner=([^,]*), id=([^)]+)\)")
REPLAY_TRUNCATED_RE = re.compile(r"attach: broadcaster [^/\s]+/(\S+) replay truncated")
DENIED_BY_USER_RE = re.compile(r"^\S+ denied by user: ")
EXPIRED_PREFIX = "permissions: approval request expired with no answer"  # pkg/permissions/prompter.go
GO_COMPILE_ERROR_RE = re.compile(r"\.go:\d+:\d+: ")
PR_SESSION_RE = re.compile(r"Session: `([^`]+)`")
PR_BASE_RE = re.compile(r" base `([0-9a-f]{7,40})`")
BRANCH_RE = re.compile(r"agent/issue-\d+")

HALT, TURN_TRIP, STORM = "agent/guardrail-trip", "agent/guardrail-turn-trip", "gate/refusal-storm"
TURN_ERROR, INTERRUPT = "agent/turn-error", "attach/interrupt"
RESET, PERM_MODE, APPROVER = "attach/guardrail-reset", "attach/perm-mode", "gate/approver"
COMPACTION_KEY, COMPACTION_TAG = "compaction", "summary"   # pkg/agent/compactor.go
CHECKPOINT_TAG, MECHANICAL_KEY = "checkpoint", "compaction_mechanical"


class InputError(Exception):
    """An input the grader cannot read as the shape it needs."""


@dataclass
class Row:
    name: str
    verdict: str = PASS
    evidence: list[str] = field(default_factory=list)
    gating: bool = True

    def fail(self, why: str) -> None:
        if self.verdict != VOID:
            self.verdict = FAIL
        self.evidence.append(why)

    def void(self, why: str) -> None:
        self.verdict = VOID
        self.evidence.append(why)

    def note(self, what: str) -> None:
        self.evidence.append(what)


@dataclass
class Inputs:
    lines: list[str]
    events: list[dict[str, Any]]
    perms: dict[str, Any]
    baseline: dict[str, Any]
    db_rows: list[dict[str, Any]] | None
    db_error: str
    pr: dict[str, Any] | None
    pr_error: str
    session: str

    def window(self) -> tuple[datetime | None, datetime | None]:
        stamps = [t for t in (a2_count.parse_ts(e.get("Timestamp")) for e in self.events) if t]
        return (min(stamps), max(stamps)) if stamps else (None, None)


def strip_ts(line: str) -> str:
    return TS_PREFIX_RE.sub("", line, count=1)


def line_ts(line: str) -> datetime | None:
    return a2_count.parse_ts(line.split(" ", 1)[0]) if TS_PREFIX_RE.match(line) else None


def load_events(path: pathlib.Path) -> list[dict[str, Any]]:
    """The session's events from a replay (GET …/events?since=0), in order,
    once each. Typed frames carry no durable state and are skipped."""
    out, seen = [], set()
    for ev, data in a2_count.sse_frames(path):
        event = data.get("event") if ev == "agent" and isinstance(data, dict) else None
        if not isinstance(event, dict):
            continue
        eid = event.get("ID")
        if isinstance(eid, str) and eid:
            if eid in seen:
                continue
            seen.add(eid)
        out.append(event)
    return out


def load_db(path: pathlib.Path, session: str) -> tuple[list[dict[str, Any]] | None, str]:
    """Rows of agent_eventlog (pkg/eventlog/sql.go) for one session.

    Reads a private copy of the DB and its -wal/-shm, so a WAL is applied
    even when the evidence directory is read-only, and the evidence itself
    is never written to."""
    with tempfile.TemporaryDirectory(prefix="a7-db-") as tmp:
        copy = pathlib.Path(tmp) / "sessions.db"
        try:
            shutil.copyfile(path, copy)
            for suffix in ("-wal", "-shm"):
                side = path.with_name(path.name + suffix)
                if side.is_file():
                    shutil.copyfile(side, copy.with_name(copy.name + suffix))
            con = sqlite3.connect(copy)
            try:
                cur = con.execute("SELECT seq, author, event_id, timestamp, metadata FROM agent_eventlog "
                                  "WHERE session_id = ? ORDER BY seq", (session,))
                rows = [dict(zip(("seq", "author", "event_id", "timestamp", "metadata"), r)) for r in cur]
            finally:
                con.close()
        except (OSError, sqlite3.Error) as e:
            return None, f"cannot read agent_eventlog from {path}: {e}"
    return rows, ""


def sidecar(row: dict[str, Any]) -> dict[str, str]:
    raw = row.get("metadata") or ""
    try:
        md = json.loads(raw) if raw else {}
    except json.JSONDecodeError:
        return {"caller": "<unparseable sidecar>"}
    if not isinstance(md, dict):
        return {"caller": "<unparseable sidecar>"}
    return {k: str(v) for k, v in md.items() if v}


def meta(ev: dict[str, Any]) -> dict[str, Any]:
    md = ev.get("CustomMetadata")
    return md if isinstance(md, dict) else {}


def parts(ev: dict[str, Any]) -> list[dict[str, Any]]:
    content = ev.get("Content")
    ps = content.get("parts") if isinstance(content, dict) else None
    return [p for p in (ps if isinstance(ps, list) else []) if isinstance(p, dict)]


def tool_errors(ev: dict[str, Any]) -> list[str]:
    """The `error` of every function response on an event: how ADK reports
    a tool that returned an error (llminternal/base_flow.go)."""
    out = []
    for p in parts(ev):
        fr = p.get("functionResponse")
        resp = fr.get("response") if isinstance(fr, dict) else None
        err = resp.get("error") if isinstance(resp, dict) else None
        if isinstance(err, str):
            out.append(err)
    return out


def has_call(ev: dict[str, Any]) -> bool:
    return any(isinstance(p.get("functionCall"), dict) for p in parts(ev))


def is_row(ev: dict[str, Any]) -> bool:
    """An eventlog row the runtime writes (agent/…, gate/…, attach/…), as
    opposed to a turn's own event."""
    return "/" in str(ev.get("Author") or "")


def is_model_event(ev: dict[str, Any]) -> bool:
    return not is_row(ev) and ev.get("Author") != "user" and bool(parts(ev))


def boundary_tag(ev: dict[str, Any]) -> str:
    tag = meta(ev).get(COMPACTION_KEY)
    return tag if isinstance(tag, str) else ""


def gh_api(gh: str, path: str) -> tuple[Any, str]:
    """GET one GitHub REST path through the gh CLI. Read-only: no method,
    no fields, so gh issues a GET."""
    try:
        p = subprocess.run([gh, "api", path], capture_output=True, text=True, timeout=120, check=False)
    except (OSError, subprocess.TimeoutExpired) as e:
        return None, f"gh api {path}: {e}"
    if p.returncode != 0:
        return None, f"gh api {path} exited {p.returncode}: {(p.stderr or p.stdout).strip()[:300]}"
    try:
        body = json.loads(p.stdout)
    except json.JSONDecodeError as e:
        return None, f"gh api {path}: not JSON: {e}"
    if not isinstance(body, dict):
        return None, f"gh api {path}: want a JSON object, got {type(body).__name__}"
    return body, ""


def git(checkout: pathlib.Path, *argv: str) -> subprocess.CompletedProcess:
    return subprocess.run(["git", "-C", str(checkout), *argv], capture_output=True, text=True, check=False)


# ---------------------------------------------------------------- rows


def grade_identity(args: argparse.Namespace, inp: Inputs) -> Row:
    r = Row("run identity")
    base, tip = args.base, args.tip
    if inp.pr is None:
        r.fail(f"cannot read the PR: {inp.pr_error}")
    else:
        head = inp.pr.get("head") if isinstance(inp.pr.get("head"), dict) else {}
        if head.get("sha") != tip:
            r.fail(f"the PR's head is {str(head.get('sha') or '(none)')[:12]}, not --tip {tip[:12]}")
        if not BRANCH_RE.fullmatch(str(head.get("ref") or "")):
            r.fail(f"the PR's branch is {head.get('ref')!r}, not the dispatcher's agent/issue-<N>")
        user = inp.pr.get("user") if isinstance(inp.pr.get("user"), dict) else {}
        if user.get("login") != args.writer:
            r.fail(f"the PR was opened by {user.get('login')!r}, not the writer App {args.writer!r}")
        body = str(inp.pr.get("body") or "")
        m = PR_SESSION_RE.search(body)
        if not m:
            r.fail("the PR body names no session (`Session: `<id>``, dispatcher pr.go prBody)")
        elif m.group(1) != inp.session:
            r.fail(f"the PR body names session {m.group(1)}, not --session {inp.session}")
        m = PR_BASE_RE.search(body)
        if not m:
            r.fail("the PR body names no base commit (`base `<sha>``)")
        elif not base.startswith(m.group(1)):
            r.fail(f"the PR body names base {m.group(1)[:12]}, not --base {base[:12]}")
    log = git(args.checkout, "log", "--format=%H %ae %ce", f"{base}..{tip}")
    if log.returncode != 0:
        r.fail(f"cannot list base..tip in {args.checkout}: {log.stderr.strip()[:200]}")
    else:
        commits = [l.split(" ") for l in log.stdout.splitlines() if l.strip()]
        if not commits:
            r.fail("base..tip holds no commits")
        for sha, author, committer in commits:
            if author.casefold() != args.commit_email.casefold() or committer.casefold() != args.commit_email.casefold():
                r.fail(f"commit {sha[:12]} was authored by {author} and committed by {committer}, not the soak "
                       f"identity {args.commit_email}: a commit the session did not make")
    if not inp.events:
        r.fail("the replay holds no events")
    if inp.db_rows is None:
        r.fail(inp.db_error)
    elif inp.events:
        db_ids = {row["event_id"] for row in inp.db_rows}
        replay_ids = {e.get("ID") for e in inp.events}
        missing = [e.get("ID") for e in inp.events if e.get("ID") not in db_ids]
        if missing:
            r.fail(f"{len(missing)} of {len(inp.events)} replay events are not rows of session {inp.session} in the "
                   f"eventlog DB (first: {missing[0]}); copy the DB's -wal file with it, or pass the matching session")
        unseen = [row for row in inp.db_rows if row["event_id"] not in replay_ids]
        if unseen:
            r.fail(f"the replay lacks {len(unseen)} of the session's {len(inp.db_rows)} eventlog rows (first: "
                   f"{unseen[0]['author']} at {unseen[0]['timestamp']}); a replay cut short hides every row after the cut")
    for line in inp.lines:
        m = REPLAY_TRUNCATED_RE.search(line)
        if m and m.group(1) == inp.session:
            r.fail("the daemon truncated a replay of this session to its newest events")
            break
    if r.verdict == PASS:
        r.note(f"PR by {args.writer} on {head.get('ref')}, head {tip[:12]}, base {base[:12]}; {len(commits)} commit(s) "
               f"as {args.commit_email}; session {inp.session}: replay and eventlog DB agree on {len(inp.events)} events")
    return r


def boots(lines: list[str]) -> list[tuple[int, re.Match]]:
    return [(i, m) for i, m in ((i, WATCHDOG_RE.search(strip_ts(l))) for i, l in enumerate(lines)) if m]


def grade_posture(inp: Inputs) -> Row:
    r = Row("posture")
    lines = [strip_ts(l) for l in inp.lines]
    watchdogs = [m for _, m in boots(inp.lines)]
    ceilings = [m for m in (CEILING_RE.search(l) for l in lines) if m]
    if not watchdogs:
        r.fail("no `core-agent: watchdog:` boot line in the log; capture it from the pod's start "
               "(kubectl logs --timestamps, and --previous after a restart)")
    for m in watchdogs:
        if m.group(1) != "enforce":
            r.fail(f"a boot ran `watchdog: {m.group(1)}` [{m.group(2)}], not enforce")
    if watchdogs and len(ceilings) < len(watchdogs):
        r.fail(f"{len(watchdogs)} boot(s) in the log but {len(ceilings)} `cost ceiling:` line(s): a boot ran with no cost ceiling")
    for m in ceilings:
        turn = float(m.group(1))
        sess = None if m.group(3) else float(m.group(2))
        if turn <= 0:
            r.fail(f"the per-turn cost ceiling is ${m.group(1)}: not in force")
        if sess is None or sess <= 0:
            r.fail(f"the per-session cost ceiling is {m.group(3) or '$' + m.group(2)}: not in force")
    if any(EMPTY_ELIGIBLE in l for l in lines):
        r.fail("the daemon warned that permissions.auto.eligible and eligible_bundles are both empty: "
               "auto asked a person about every call")
    mode = inp.perms.get("mode")
    if mode != "auto":
        r.fail(f"the session's permission mode is {mode!r} (GET …/perms), not 'auto'")
    for ev in inp.events:
        if ev.get("Author") == PERM_MODE:
            md = meta(ev)
            r.fail(f"the permission mode changed mid-run: {md.get('from')} → {md.get('to')}"
                   f" by {md.get('caller') or 'an unattributed caller'} ({ev.get('Timestamp')})")
    if r.verdict == PASS:
        c = ceilings[-1]
        r.note(f"{len(watchdogs)} boot(s), each `watchdog: enforce`; ceilings per-turn=${float(c.group(1)):g} "
               f"per-session=${float(c.group(2)):g}; session mode auto, never changed")
    plans = {m.group(1) for m in (PLAN_RE.search(l) for l in lines) if m}
    if plans:
        r.note(f"plan mode: {', '.join(sorted(plans))}")
    return r


def mid_run(inp: Inputs, idx: int, created_at: int | None) -> bool:
    """Whether log line idx falls inside the run. Dated lines are placed by
    their timestamp against the session's events; an undated one by its
    position after the session's `created` line, the conservative way."""
    since, until = inp.window()
    t = line_ts(inp.lines[idx])
    if t is not None and since is not None and until is not None:
        return since <= t <= until
    return created_at is not None and idx > created_at


def grade_no_human(inp: Inputs) -> Row:
    r = Row("no human")
    if inp.db_rows is None:
        r.fail(inp.db_error)
    else:
        others: dict[str, list[str]] = {}
        for row in inp.db_rows:
            sc = sidecar(row)
            who = sc.get("caller", "")
            if sc.get("proxy_by"):
                r.fail(f"a row's caller was asserted by the proxy {sc['proxy_by']} ({row['author']}@{row['timestamp']}); "
                       f"the dispatcher authenticates directly")
            # No caller: written outside any request (an approver audit, a
            # turn a background report woke). With per-caller auth on, as
            # checked below, an attach client's write always has one.
            if who in (DISPATCHER, AUTO_CONTINUE, ""):
                continue
            others.setdefault(who, []).append(f"{row['author']}@{row['timestamp']}")
        for who, where in others.items():
            r.fail(f"{len(where)} eventlog row(s) written by {who}, not the dispatcher (first: {where[0]})")
        if not any(sidecar(row).get("caller") == DISPATCHER for row in inp.db_rows):
            r.fail(f"no row of the session carries {DISPATCHER}: the dispatcher did not drive it")
    approvals = inp.perms.get("approvals") or []
    for a in approvals:
        if not a.get("approver_model"):
            r.fail(f"a person answered a permission prompt: {a.get('decision')} {a.get('tool')} "
                   f"{str(a.get('key') or '')[:80]!r} by {a.get('by') or 'an unattributed answerer'} at {a.get('at')}")
    for key in ("allow", "deny"):
        added = sorted(set(inp.perms.get(key) or []) - set(inp.baseline.get(key) or []))
        if added:
            r.fail(f"the session's {key} list holds {len(added)} pattern(s) a fresh session does not "
                   f"(POST /perms/{key} writes no row): {', '.join(added[:5])}")
    escalations: list[tuple[str, str]] = []
    expired: list[str] = []
    approver_allows = 0
    for ev in inp.events:
        for err in tool_errors(ev):
            if DENIED_BY_USER_RE.match(err):
                r.fail(f"a person denied a permission prompt: {err[:160]!r} ({ev.get('Timestamp')})")
            elif EXPIRED_PREFIX in err:
                expired.append(err)
        if ev.get("Author") == APPROVER:
            md = meta(ev)
            if md.get("verdict") == "escalate":
                escalations.append((str(md.get("tool") or ""), str(md.get("detail") or "")))
            elif md.get("verdict") == "allow":
                approver_allows += 1
        if ev.get("Author") == RESET:
            md = meta(ev)
            r.fail(f"guardrails/reset in the session: {md.get('reset')} by "
                   f"{md.get('caller') or 'an unattributed caller'} ({ev.get('Timestamp')})")
    for tool, detail in escalations:
        # gate.go askWithTimeout: "... after <d> (tool=%s detail=%q); ..."
        want = f"(tool={tool} detail={json.dumps(detail, ensure_ascii=False)})"
        hit = next((e for e in expired if want in e), None)
        if hit is None:
            r.fail(f"the approver passed {tool} {detail[:80]!r} to a person, and no expired prompt answers it: "
                   f"a person may have answered")
        else:
            expired.remove(hit)
    logged_allows = sum(1 for a in approvals if a.get("approver_model"))
    if logged_allows < approver_allows:
        r.void(f"the approval log holds {logged_allows} approver allow(s) but the eventlog {approver_allows}: the perms "
               f"capture was read from a restarted process, whose log starts empty, so a person's answer would not show")
    created_at = None
    for i, line in enumerate(inp.lines):
        m = SESSION_LINE_RE.search(line)
        if not m or m.group(3) != inp.session:
            continue
        if m.group(1) == "created":
            created_at = i if created_at is None else created_at
        if m.group(2) != DISPATCHER:
            r.fail(f"the session was {m.group(1)} by {m.group(2)}, not {DISPATCHER}")
        if m.group(1) == "resumed" and mid_run(inp, i, created_at):
            r.void("the session was resumed mid-run (a restart or an eviction): its approval log restarted "
                   "empty, so a person's answer before it would not show (P1)")
    if created_at is None:
        r.fail(f"no `session created (owner=…, id={inp.session})` line in the log")
    auths = [m.group(1) for m in (AUTH_LINE_RE.search(l) for l in inp.lines) if m]
    if not auths or any(a in ("disabled", "<disabled>") for a in auths):
        r.fail("the daemon ran without per-caller auth (`multi-session auth: disabled`, or no such line), so its "
               "eventlog rows cannot name who wrote them")
    rebuilt = [i for i, _ in boots(inp.lines)] + [
        i for i, l in enumerate(inp.lines)
        if (m := SESSION_LINE_RE.search(l)) and m.group(1) == "resumed" and m.group(3) == inp.session]
    later = [i for i in rebuilt if created_at is not None and i > created_at and not mid_run(inp, i, created_at)]
    if later and approver_allows == 0:
        r.void("the daemon restarted or the session was resumed after the run, and the eventlog holds no approver "
               "allow to show whether the perms capture predates it")
    for i, _ in boots(inp.lines):
        if mid_run(inp, i, created_at):
            r.void(f"the daemon restarted mid-run ({inp.lines[i][:30].strip()}): the approval log restarted empty "
                   f"and the approver lost its task (P1)")
            break
    if r.verdict == PASS:
        r.note(f"{len(inp.db_rows or [])} eventlog rows, every caller {DISPATCHER} or the runtime; {logged_allows} "
               f"approver allow(s), all in the approval log, and no answer from a person; {len(escalations)} "
               f"escalation(s), each expired; allow and deny lists as a fresh session's")
    return r


def grade_ended(inp: Inputs) -> Row:
    r = Row("ended by work")
    if inp.pr is None:
        r.fail(f"cannot read the PR: {inp.pr_error}")
    else:
        state, merged = inp.pr.get("state"), inp.pr.get("merged_at")
        if state == "closed" and not merged:
            r.fail("the PR was closed without merging")
        elif state == "closed":
            r.note(f"the PR was merged at {merged}, after the run (a person merges A7)")
        elif state != "open":
            r.fail(f"the PR's state is {state!r}")
    last_error: dict[str, Any] | None = None
    checkpoint_at = ""
    for ev in inp.events:
        author, md, ts = ev.get("Author"), meta(ev), ev.get("Timestamp")
        tag = boundary_tag(ev)
        if tag == COMPACTION_TAG:
            kind = "a mechanical" if md.get(MECHANICAL_KEY) is True else "a"
            r.void(f"the session compacted ({kind} boundary at {ts}); decision 19: the run is void, not failed, "
                   f"and waits for P1")
        elif tag == CHECKPOINT_TAG:
            checkpoint_at = str(ts)
            r.note(f"checkpoint boundary at {ts}")
        elif checkpoint_at and has_call(ev):
            r.void(f"the session called tools after a checkpoint at {checkpoint_at}, which dropped the approver's "
                   f"task: void, as a compaction is (decision 19, P1)")
            checkpoint_at = ""
        if author == HALT:
            r.fail(f"a guardrail halted the session: {md.get('guardrail')}: {md.get('reason')} ({ts})")
        elif author == TURN_TRIP:
            r.fail(f"a guardrail cut a turn: {md.get('guardrail')}: {md.get('reason')} ({ts})")
        elif author == STORM:
            r.fail(f"a refusal storm cut a turn ({ts})")
        elif author == INTERRUPT:
            r.fail(f"the session was interrupted at {ts}: the dispatcher interrupts a session it stops, "
                   f"including at its session timeout (the wallclock)")
        elif author == TURN_ERROR:
            last_error = ev
        elif is_model_event(ev):
            last_error = None
    if last_error is not None:
        md = meta(last_error)
        r.fail(f"the session's last turn ended in an error: {md.get('kind')}: {str(md.get('message'))[:160]} "
               f"({last_error.get('Timestamp')})")
    for line in inp.lines:
        m = a2_count.GUARDRAIL_RE.search(line)
        if m and a2_count.line_session(line) == inp.session:
            r.fail(f"the log records a {m.group(1)} guardrail trip on the session: {strip_ts(line)[:160]}")
    model = [ev for ev in inp.events if is_model_event(ev) and not boundary_tag(ev)]
    if not model:
        r.fail("the replay holds no model output")
    else:
        last = model[-1]
        if last.get("Partial"):
            r.fail(f"the session's last model event is a partial chunk ({last.get('Timestamp')})")
        elif has_call(last):
            names = [p["functionCall"].get("name") for p in parts(last) if isinstance(p.get("functionCall"), dict)]
            r.fail(f"the session's last model event is a call to {', '.join(map(str, names))}, not a finished answer "
                   f"({last.get('Timestamp')})")
        elif not any(isinstance(p.get("text"), str) and p["text"].strip() for p in parts(last)):
            r.fail(f"the session's last model event carries no text ({last.get('Timestamp')})")
        elif r.verdict == PASS:
            r.note(f"PR {inp.pr.get('state') if inp.pr else '?'}; the last model event ({last.get('Timestamp')}) is a "
                   f"finished answer; no halt, trip, refusal storm, interrupt or unrecovered turn error")
    return r


def grade_degraded(args: argparse.Namespace, inp: Inputs) -> Row:
    r = Row("degraded visibly", verdict=RECORDED, gating=False)
    since, until = inp.window()
    try:
        counts = a2_count.count(args.log, [args.replay], inp.session, [], since, until)
    except Exception as e:  # noqa: BLE001 - recorded, never gating
        r.note(f"a2_count.py could not run: {e}")
        return r
    code = a2_count.exit_code(counts)
    r.note(f"a2_count.py exit {code} ({ {0: 'met', 1: 'a failure only the log witnessed', 2: 'not closable'}[code] })")
    for c in counts:
        if c.log is None and c.transcript is None:
            continue
        r.note(f"{c.name}: {c.verdict} (log {'—' if c.log is None else c.log}, "
               f"transcript {'—' if c.transcript is None else c.transcript})")
    return r


def export_tree(checkout: pathlib.Path, sha: str, dest: pathlib.Path) -> str:
    """Write the tree at sha into dest, blob by blob. Returns an error or "".

    Not `git archive`: that honours the tree's own .gitattributes
    (export-ignore, export-subst), and the tip's tree is the agent's.
    Symlinks are refused and submodules skipped; the tree is agent-written
    and core-agent has neither."""
    ls = subprocess.run(["git", "-C", str(checkout), "ls-tree", "-r", "-z", "--full-tree", sha],
                        capture_output=True, check=False)
    if ls.returncode != 0:
        return f"git ls-tree {sha[:12]}: {ls.stderr.decode(errors='replace').strip()}"
    entries = []
    for rec in ls.stdout.split(b"\0"):
        if not rec:
            continue
        info, _, path = rec.partition(b"\t")
        mode, kind, obj = info.decode().split(" ")
        rel = path.decode("utf-8", errors="surrogateescape")
        if kind == "commit":
            continue
        if mode == "120000":
            return f"{sha[:12]} holds a symlink at {rel!r}; refusing to export it"
        parts_ = pathlib.PurePosixPath(rel).parts
        if not parts_ or any(p in ("", ".", "..") for p in parts_) or rel.startswith("/"):
            return f"{sha[:12]} holds a path that leaves the tree: {rel!r}"
        entries.append((mode, obj, rel))
    proc = subprocess.Popen(["git", "-C", str(checkout), "cat-file", "--batch"],
                            stdin=subprocess.PIPE, stdout=subprocess.PIPE)
    assert proc.stdin and proc.stdout
    try:
        for mode, obj, rel in entries:
            proc.stdin.write(obj.encode() + b"\n")
            proc.stdin.flush()
            header = proc.stdout.readline().split()
            if len(header) != 3 or header[1] != b"blob":
                return f"git cat-file {obj}: unexpected answer {header!r}"
            data = proc.stdout.read(int(header[2]))
            proc.stdout.read(1)  # the newline after each object
            target = dest / rel
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_bytes(data)
            if mode == "100755":
                target.chmod(0o755)
    except OSError as e:
        return f"export {sha[:12]}: {e}"
    finally:
        proc.kill()  # never wait on a reader that may still hold output
        proc.wait()
    return ""


def run_oracle(args: argparse.Namespace, sha: str) -> tuple[int | None, str]:
    with tempfile.TemporaryDirectory(prefix="a7-oracle-") as tmp:
        root = pathlib.Path(tmp)
        err = export_tree(args.checkout, sha, root)
        if err:
            return None, err
        dest = root / ORACLE_DEST
        if not dest.parent.is_dir():
            return None, f"{sha[:12]} has no {dest.parent.relative_to(root)}"
        if dest.exists():
            return None, f"{sha[:12]} already has a file at {ORACLE_DEST}"
        dest.write_bytes(args.oracle.read_bytes())
        env = dict(os.environ, GOWORK="off", GOFLAGS="")
        cmd = [args.go, "test", "-tags", "a7oracle", "-count=1", "-v", "-run", f"^{ORACLE_TEST}$", "./pkg/agent/"]
        try:
            p = subprocess.run(cmd, cwd=root, capture_output=True, text=True, env=env,
                               timeout=args.oracle_timeout, check=False)
        except subprocess.TimeoutExpired:
            return None, f"go test at {sha[:12]} ran past {args.oracle_timeout}s"
        except OSError as e:
            return None, f"cannot run {args.go}: {e}"
        return p.returncode, p.stdout + p.stderr


def first_reason(out: str, rc: int) -> str:
    """The line that best says why a go test run did not come out as
    wanted: a RIG marker, an UNFIXED marker, a FAIL line, a build error,
    in that order."""
    lines = [l.strip() for l in out.splitlines()]
    for pick in (lambda l: ORACLE_RIG in l, lambda l: ORACLE_UNFIXED in l, lambda l: l.startswith("--- FAIL"),
                 lambda l: GO_COMPILE_ERROR_RE.search(l) is not None, lambda l: l.endswith("[build failed]")):
        hit = next((l for l in lines if pick(l)), "")
        if hit:
            return hit
    return f"exit {rc}, and no subtest reported PASS" if rc == 0 else f"exit {rc}"


def subtest_line(state: str, name: str) -> str:
    return f"--- {state}: {ORACLE_TEST}/{name} "


def grade_oracle(args: argparse.Namespace) -> Row:
    r = Row("task oracle")
    base, tip = args.base, args.tip
    for sha, label in ((base, "base"), (tip, "tip")):
        if git(args.checkout, "cat-file", "-e", f"{sha}^{{commit}}").returncode != 0:
            r.fail(f"--{label} {sha[:12]} is not a commit in {args.checkout}")
    if r.verdict == FAIL:
        return r
    if base == tip:
        r.fail("--base and --tip are the same commit")
        return r
    if git(args.checkout, "merge-base", "--is-ancestor", base, tip).returncode != 0:
        r.fail(f"the tip {tip[:12]} does not descend from the base {base[:12]}")
        return r
    for sha, label in ((base, "base"), (tip, "tip")):
        found = [p for p in ANSWER_KEY_PATHS if git(args.checkout, "cat-file", "-e", f"{sha}:{p}").returncode == 0]
        g = git(args.checkout, "grep", "-l", "-F", ANSWER_KEY_SENTINEL, sha)
        found += [l.split(":", 1)[-1] for l in g.stdout.splitlines() if l.strip()]
        if found:
            r.void(f"the answer key is in the worker's tree at the {label} ({sorted(set(found))[0]}): "
                   f"decision 20, the run is not evidence")
    if r.verdict == VOID:
        return r
    rc, out = run_oracle(args, base)
    if rc is None:
        r.fail(f"the oracle did not run at the base: {out}")
    elif rc == 0:
        r.fail(f"the oracle does not fail at the base {base[:12]}: the issue is already fixed there, or the oracle is wrong")
    elif ORACLE_RIG in out or subtest_line("FAIL", ORACLE_SUBTESTS[0]) not in out or ORACLE_UNFIXED not in out:
        r.fail(f"the oracle failed at the base, but not for its reason (want {ORACLE_UNFIXED!r} in "
               f"{ORACLE_SUBTESTS[0]}): {first_reason(out, rc)[:200]}")
    else:
        line = next(l.strip() for l in out.splitlines() if ORACLE_UNFIXED in l)
        r.note(f"base {base[:12]} fails: {line[:220]}")
    rc, out = run_oracle(args, tip)
    if rc is None:
        r.fail(f"the oracle did not run at the tip: {out}")
    elif rc != 0 or ORACLE_RIG in out or not all(subtest_line("PASS", s) in out for s in ORACLE_SUBTESTS):
        r.fail(f"the oracle does not pass at the tip {tip[:12]}: {first_reason(out, rc)[:200]}")
    else:
        r.note(f"tip {tip[:12]} passes: {', '.join(ORACLE_SUBTESTS)}")
    return r


def grade_ci(args: argparse.Namespace) -> Row:
    r = Row("CI")
    tip = args.tip
    diff = git(args.checkout, "diff", "--name-only", "--no-renames", args.base, tip)
    if diff.returncode != 0:
        r.fail(f"cannot diff base..tip: {diff.stderr.strip()[:200]}")
    else:
        touched = [p for p in diff.stdout.splitlines() if p.startswith(CI_INPUTS)]
        if touched:
            r.fail(f"the PR changes what CI runs ({', '.join(touched[:3])}): CI cannot vouch for a change to itself")
    body, err = gh_api(args.gh, f"repos/{args.repo}/actions/runs?head_sha={tip}&per_page=100")
    if body is None:
        r.fail(err)
        return r
    listed = body.get("workflow_runs")
    if not isinstance(listed, list):
        r.fail("the workflow-runs answer has no workflow_runs list")
        return r
    if isinstance(body.get("total_count"), int) and body["total_count"] > len(listed):
        r.fail(f"{body['total_count']} workflow runs on the head, more than one page ({len(listed)}); grade by hand")
    runs = [w for w in listed if isinstance(w, dict) and w.get("head_sha") == tip]
    by_path: dict[str, list[dict[str, Any]]] = {}
    for w in runs:
        by_path.setdefault(str(w.get("path") or "").split("@", 1)[0], []).append(w)
    latest = {p: max(ws, key=lambda w: int(w.get("id") or 0)) for p, ws in by_path.items()}
    ci_runs = by_path.get(CI_WORKFLOW, [])
    ci = latest.get(CI_WORKFLOW)
    if ci is None:
        r.fail(f"the CI workflow ({CI_WORKFLOW}) never ran on {tip[:12]}; a job with the same check name from another "
               f"workflow is not CI (#1271)")
    else:
        if len(ci_runs) > 1:
            r.fail(f"the CI workflow ran {len(ci_runs)} times on {tip[:12]}: a reopen or a dispatch, not the PR opening")
        if int(ci.get("run_attempt") or 1) > 1:
            r.fail(f"the CI workflow's run is attempt {ci.get('run_attempt')}: someone re-ran it ({ci.get('html_url')})")
        if ci.get("status") != "completed":
            r.fail(f"the CI workflow is still {ci.get('status')} on {tip[:12]} ({ci.get('html_url')})")
        elif ci.get("conclusion") != "success":
            r.fail(f"the CI workflow concluded {ci.get('conclusion')} on {tip[:12]} ({ci.get('html_url')})")
    for path, w in sorted(latest.items()):
        actor = w.get("triggering_actor") if isinstance(w.get("triggering_actor"), dict) else {}
        if actor.get("login") and actor["login"] != args.writer:
            r.fail(f"{w.get('name')} ({path}) was triggered by {actor['login']}, not the writer App")
        if path == CI_WORKFLOW:
            continue
        if w.get("status") != "completed":
            r.fail(f"{w.get('name')} ({path}) is still {w.get('status')} on {tip[:12]}")
        elif w.get("conclusion") in BAD_CONCLUSIONS:
            r.fail(f"{w.get('name')} ({path}) concluded {w.get('conclusion')} on {tip[:12]} ({w.get('html_url')})")
    if r.verdict == PASS:
        r.note("; ".join(f"{w.get('name')} ({p.rsplit('/', 1)[-1]}) {w.get('conclusion')}" for p, w in sorted(latest.items())))
    return r


# ---------------------------------------------------------------- driver


def overall(rows: list[Row]) -> str:
    if any(r.verdict == VOID for r in rows):
        return VOID
    if any(r.gating and r.verdict == FAIL for r in rows):
        return FAIL
    return PASS


def render(rows: list[Row], verdict: str) -> str:
    out = ["| assertion | verdict | evidence |", "|---|---|---|"]
    for r in rows:
        ev = "<br>".join(e.replace("|", "\\|").replace("\n", " ") for e in r.evidence) or "—"
        out.append(f"| {r.name} | **{r.verdict}** | {ev} |")
    out.append("")
    out.append({PASS: "A7: **PASS**: the PR opened with green CI and no human between the task and the PR.",
                FAIL: "A7: **FAIL**: at least one gating assertion failed on this run.",
                VOID: "A7: **VOID**: this run is not evidence either way; see the VOID row."}[verdict])
    return "\n".join(out) + "\n"


def load_perms(path: pathlib.Path) -> dict[str, Any]:
    """A GET …/perms body (PermsInfo, pkg/attach/state.go)."""
    try:
        body = json.loads(path.read_text())
    except (OSError, json.JSONDecodeError) as e:
        raise InputError(f"{path}: not JSON: {e}") from e
    if not isinstance(body, dict):
        raise InputError(f"{path}: want the GET …/perms object")
    for key in ("allow", "deny", "approvals"):
        if body.get(key) is not None and not isinstance(body[key], list):
            raise InputError(f"{path}: `{key}` is not a list")
    if any(not isinstance(a, dict) for a in body.get("approvals") or []):
        raise InputError(f"{path}: an `approvals` entry is not an object")
    return body


def load_inputs(args: argparse.Namespace) -> Inputs:
    lines = args.log.read_text(errors="replace").splitlines()
    events = load_events(args.replay)
    perms, baseline = load_perms(args.perms), load_perms(args.perms_baseline)
    db_rows, db_error = load_db(args.eventlog_db, args.session)
    pr, pr_error = gh_api(args.gh, f"repos/{args.repo}/pulls/{args.pr}")
    return Inputs(lines, events, perms, baseline, db_rows, db_error, pr, pr_error, args.session)


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--log", required=True, type=pathlib.Path, help="the daemon log from its start (kubectl logs --timestamps)")
    ap.add_argument("--replay", required=True, type=pathlib.Path, help="GET /sessions/<app>/<sid>/events?since=0 for the run's session")
    ap.add_argument("--perms", required=True, type=pathlib.Path, help="GET /sessions/<app>/<sid>/perms, read when the run ended")
    ap.add_argument("--perms-baseline", required=True, type=pathlib.Path,
                    help="GET …/perms of a fresh session on the same daemon, for the allow and deny lists")
    ap.add_argument("--eventlog-db", required=True, type=pathlib.Path, help="a copy of the daemon's SQLite session DB (with its -wal)")
    ap.add_argument("--session", required=True, help="the run's session ID")
    ap.add_argument("--repo", required=True, help="the mirror, owner/name")
    ap.add_argument("--pr", required=True, type=int, help="the run's PR number in the mirror")
    ap.add_argument("--writer", required=True, help="the login of the App that opens the dispatcher's PRs")
    ap.add_argument("--commit-email", required=True, help="the soak's commit identity email (decision 16)")
    ap.add_argument("--checkout", required=True, type=pathlib.Path, help="a clone of the mirror holding --base and --tip")
    ap.add_argument("--base", required=True, help="the commit the dispatcher gave the session (full SHA)")
    ap.add_argument("--tip", required=True, help="the PR's head commit (full SHA)")
    ap.add_argument("--oracle", type=pathlib.Path, default=ORACLE, help=argparse.SUPPRESS)
    ap.add_argument("--oracle-timeout", type=int, default=ORACLE_TIMEOUT, help=argparse.SUPPRESS)
    ap.add_argument("--gh", default="gh", help="the gh CLI (default: gh on PATH)")
    ap.add_argument("--go", default="go", help="the go command (default: go on PATH)")
    ap.add_argument("--json", action="store_true")
    args = ap.parse_args(argv)
    for name in ("log", "replay", "perms", "perms_baseline", "eventlog_db", "oracle"):
        p = getattr(args, name)
        if not p.is_file():
            ap.error(f"--{name.replace('_', '-')} {p}: no such file")
    if not args.checkout.is_dir():
        ap.error(f"--checkout {args.checkout}: no such directory")
    for name in ("base", "tip"):
        if not re.fullmatch(r"[0-9a-f]{40}", getattr(args, name)):
            ap.error(f"--{name} must be a full 40-character commit SHA")
    try:
        inp = load_inputs(args)
    except InputError as e:
        print(f"grade_a7: {e}", file=sys.stderr)
        return 2
    rows = [grade_identity(args, inp), grade_posture(inp), grade_no_human(inp), grade_ended(inp),
            grade_degraded(args, inp), grade_oracle(args), grade_ci(args)]
    verdict = overall(rows)
    if args.json:
        print(json.dumps({"verdict": verdict, "rows": [r.__dict__ for r in rows]}, indent=2))
    else:
        sys.stdout.write(render(rows, verdict))
    return EXIT[verdict]


if __name__ == "__main__":
    sys.exit(main())
