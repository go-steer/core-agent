# Open-issue assessment & execution plan — 2026-11-28

**Status:** proposed (ready to apply on GitHub; no issues or milestones were mutated during this audit).

Scope: all 84 open issues and 4 open milestones on `go-steer/core-agent` at `origin/main` `1b7a41ce` (`v3.2.0` GA shipped at `0222deb4`; `Version` fallback bumped to `v3.3.0-dev` at `e3090556`). Every untriaged, recently filed, or potentially fixed issue was checked against `origin/main`, open PRs (`#1599`, `#1601`, `#1615`), and upstream trackers (`core-tui`, `switchboard`, `adk-go`).

## TL;DR

1. **Close 6 issues and 1 stale milestone immediately (84 → 78 open).**
   - Close the `v3.2` milestone (0 open, 35 closed; `v3.2.0` GA already shipped).
   - Land three already-open fix PRs to close `#1597` (PR `#1599`), `#1598` (PR `#1601`), and `#1600` (PR `#1615` — the underlying bug is already fixed on `main` by `#1614` bumping `genai` to `v1.79.1`).
   - Close 3 duplicate/overlapping issues: `#1591` (duplicate of `#1590`, after folding in its two unique checklist items), plus `#1281` and `#1564` (fold into `#1555`).
2. **Stop stuffing everything into `v3.3`.** `v3.3` bloated to **42 open issues** across eight unrelated domains, plus 9 untriaged issues piled up with no milestone. Adopt a **hybrid milestone model**: two tight, shippable release milestones (`v3.3` at 20 issues, `v3.4` at 7 issues) plus **three non-release theme tracks** (`Track: Transcript & Retention`, `Track: Subagent Roster & Coordination`, `Track: Repo, CI & Recipe Hygiene`) and `Backlog` (26 issues).
3. **Fix two live trust/correctness gaps first (Wave 0).** `#1593` (durable write gate `pkg/approval` and effect outbox `internal/effects` are not wired into any shipped binary) and `#1271` (`ci-docs.yml` no-op jobs still satisfy required checks on mixed Go+docs PRs) undermine both product claims and merge safety.
4. **Execute in four ordered waves** so `v3.3.0` ships in ~2–3 focused weeks without waiting on transcript design refreshes, multi-phase subagent steering, or `v3.4` external-state work.

## Headline numbers

| Bucket | Before | After | Delta / notes |
|---|---:|---:|---|
| Close via dedup / merge | — | 3 | `#1591` → `#1590`; `#1281`, `#1564` → `#1555` |
| Close via already-open PRs | — | 3 | `#1597` (PR `#1599`), `#1598` (PR `#1601`), `#1600` (PR `#1615`) |
| `v3.2` milestone | 0 open | **closed** | `v3.2.0` GA shipped `0222deb4` (35 closed) |
| `v3.3` (release milestone) | 42 | **20** | Autonomy correctness, shipmind unblockers, v3.2 UAT, config ergonomics |
| `Track: Transcript & Retention` (new, non-release) | — | **6** | `#887` + `#881`–`#885` (gated on design refresh) |
| `Track: Subagent Roster & Coordination` (new, non-release) | — | **11** | Epics `#1559`, `#1560` + P1–P4 children |
| `Track: Repo, CI & Recipe Hygiene` (new, non-release) | — | **8** | CI gates, surveyor baseline, dev scripts, recipes, rolling pins |
| `v3.4` (release milestone) | 7 | **7** | Unchanged: external state, K8s install, workload identity |
| `Backlog` | 26 | **26** | Valid, no active consumer or `blocked: upstream` |
| `(no milestone)` | 9 | **0** | Every open issue gets an explicit home |
| **Total open** | **84** | **78** | All 84 issues accounted for exactly once |

## What changed since 2026-11-16

The [2026-11-16 triage](2026-11-16-issue-triage.md) cut the backlog from 103 to 77 and got `v3.2.0` GA out the door. Over the next 12 days, focus shifted to getting `shipmind` ship-shape, and three forms of drift crept back in:

1. **Untriaged intake (9 issues with no milestone).** Two subagent epics (`#1559`, `#1560`) were filed without milestones while their children were split between `v3.3` and `Backlog`. Seven more issues arrived (`#1591`, `#1593`, `#1595`, `#1597`, `#1598`, `#1600`, `#1613`), including one exact duplicate (`#1591` vs `#1590`) and three bugs that already have open fix PRs (`#1599`, `#1601`, `#1615`).
2. **An unshippable `v3.3` milestone (42 issues).** Putting every non-`v3.4` active item into `v3.3` mixed P0 autonomy bugs with upstream-blocked TUI chips (`#1178`, `#1321`), a rolling bot issue (`#1424`), a 6-issue transcript subsystem (`#881`–`#887`), recipes (`#1555`), and CI hygiene.
3. **Stale issue text and an unclosed milestone.** `v3.2` still shows as open at 0/35. `#885` still advertises Defect 2 (one-shot transcript missing answer), which shipped in `#1553` / PR `#1578`. `#1116` still links to `docs/self-development-design.md` (moved to `docs/archive/`). `#555` still cites `google/adk-go#479` as open, though upstream closed `#479` on 2026-08-12 (part 1, deferred `gen_ai` histogram semantics, remains upstream-blocked).

## Immediate cleanup (close 6 issues + 1 milestone)

### 1. Close via dedup / merge (3)

| # | Action | Evidence & migration notes |
|---|---|---|
| `#1591` | Merge into `#1590`, then close | Both filed 2026-11-20 for live Gemini Platform UAT of v3.2 Agent Engine Sessions + Memory Bank. Copy `#1591`'s two unique items into `#1590`: (a) unify `CORE_AGENT_TEST_REASONING_ENGINE` / `CORE_AGENT_TEST_AGENT_ENGINE`, and (b) test CLI `--continue`/`--resume <sid>` across SQLite wipe plus ~100 KB payload & parallel `:sub:` burst ceilings. |
| `#1281` | Fold into `#1555`, then close | BackOff routing bug in `examples/gke-platform-agent/cluster/skills/gke-workload-troubleshooting/SKILL.md`. `#1555` already owns refining/updating/pruning the GKE/K8s recipes; add as a checklist item there. |
| `#1564` | Fold into `#1555`, then close | `examples/gke-parallel-triage` doc/code drift (MCP inheritance, budget defaults, `--no-repl`). Already noted as related to `#1555`; add as a checklist item in `#1555` (note: `--no-repl` ad-hoc spawns are resolved cleanly once `#1565` lands). |

### 2. Close by landing already-open PRs (3)

| # | Open PR | Current state | Action |
|---|---|---|---|
| `#1600` | `#1615` | `BEHIND` `main` | **Bug already fixed on `main`** by `#1614` (`core-models v0.16.0` / `genai v1.79.1`). Rebase and merge `#1615` to pin the regression test and close `#1600`. |
| `#1597` | `#1599` | `DIRTY` | Rebase onto `origin/main`, verify CI, merge to close `#1597` (bash process-group kill on cancel). |
| `#1598` | `#1601` | `DIRTY` | Rebase onto `origin/main`, verify CI, merge to close `#1598` (`compactingService.Get` summary framing at index 0). |

### 3. Issue text rewrites needed (no closure)

- **`#885`:** Defect 2 (one-shot transcript missing answer) shipped in `#1553` / PR `#1578`. Retitle and trim to Defect 1 only (`.agents/sessions/` dual-writer schema reconciliation).
- **`#1116`:** Update body link from `docs/self-development-design.md` (archived) to `docs/selfdev-historical-replay-design.md` and retarget to the historical-replay corpus.
- **`#555`:** Drop the stale `google/adk-go#479` open-blocker framing (`#479` closed 2026-08-12); keep `blocked: upstream` for deferred `gen_ai` histogram semantics.
- **`#1321`:** Optionally land the cheap W2 README clarification now; keep the idle-wake behaviour item parked in `Backlog` (`blocked: upstream` on `core-tui#378`, still open).

## Proposed milestone structure

Keep **release milestones small and time-bounded** so minor releases actually ship. Move cohesive multi-PR initiatives and ongoing maintenance into **non-release track milestones** so they stay visible and organized without bloating or blocking a release cut.

| Milestone | Kind | Count | Purpose |
|---|---|---:|---|
| **`v3.3`** | Release | 20 | Autonomy correctness, active shipmind/consumer unblockers, v3.2 live UAT, config ergonomics |
| **`v3.4`** | Release | 7 | Platform: external state failover, first-party K8s install, workload identity |
| **`Track: Transcript & Retention`** | Non-release | 6 | End-to-end transcript export, CLI sessions, retention sweep (ships in `v3.3` or `v3.4` once design refreshes) |
| **`Track: Subagent Roster & Coordination`** | Non-release | 11 | Builtin roster (`#1559`) and fan-out/steering (`#1560`) across P1–P4 (P1 bug `#1563` ships with `v3.3`) |
| **`Track: Repo, CI & Recipe Hygiene`** | Non-release | 8 | CI gate integrity, surveyor baseline, dev scripts, rolling pins, K8s/GKE recipes |
| **`Backlog`** | Parking lot | 26 | Valid work with no active consumer yet, plus `blocked: upstream` items |

### `v3.3` — Autonomy Correctness, Shipmind Unblockers & v3.2 Verification (20)

- **P0 wiring bug (1):**
  - `#1593` — Wire durable write gate (`pkg/approval`) and recorded-effect outbox (`internal/effects`) into `pkg/compose` / `cmd/core-agent` and subagent runners, with a binary-level integration test so it cannot regress silently.
- **Autonomy & cost correctness — A1-soak family + pricing (9):**
  - `#1203` — Subagent turns still overwrite parent `Tracker.Last()`.
  - `#1298` — Subagents get no compaction; single-turn context bounded only by window.
  - `#1300` — Watchdog `record_plan` revise loop never cuts.
  - `#713` — No-progress / near-duplicate-args limit (gap 2 only; per-subagent budget shipped in `#850`).
  - `#1234` — Summarizer empty text (~1 in 4 on `gemini-3.7-flash`) + mechanical fallback for short sessions.
  - `#1275` — Per-day / process-wide cost cap (cheap over `#1359` spend ledger).
  - `#1304` — Soft budget wrap-up notice / `FinalReport` on async spawn.
  - `#1053` — Per-class guidance on terminal async subagent alerts.
  - `#1548` — Price each call on served `ModelVersion`, consistent between live and rebuild.
- **Active consumer / shipmind unblockers & v3.2 live verification (8):**
  - `#1613` — Attach MCP elicitation relay (`elicitation/create`) with fast-decline when unwatched (unblocks `shipmind#144`, `switchboard#207`, `mast-web#153`).
  - `#1590` — Live Gemini Platform `ReasoningEngine` UAT + Starlight memory docs + archive `docs/memory-service-design.md` (absorbs `#1591`).
  - `#865` — Auto-generate enforced constraints preamble from startup summary (`priority: high`).
  - `#866` — Caller-supplied correlation token on inbox inject (`priority: high`).
  - `#939` — `StatusInfo` `time.Time` fields: `omitempty` → `omitzero` (protocol minor bump).
  - `#943` — `turn-complete` `prompt_id` matches drained inject batch.
  - `#860` — Preserve transport identity alongside proxy-asserted sender label.
  - `#868` — Drop inbox dedup branches at N=1.
- **Config ergonomics (2):**
  - `#958` — `core-agent config validate` CLI subcommand (`priority: high`).
  - `#959` — Generated JSON Schema for `.agents/config.json` + CI drift gate.

### `Track: Transcript & Retention` (6)

All six issues depend on refreshing `docs/transcript-archival-design.md` for `eventlog.Transcript`, Agent Engine Sessions, and `--resume` first:
- `#887` (tracker), `#881` (attach transcript export), `#882` (CLI sessions ls/export), `#883` (eventlog retention sweep, `priority: high`), `#884` (replay truncation frame), `#885` (dual-schema reconciliation, Defect 1 only).

### `Track: Subagent Roster & Coordination` (11)

Keeps both merged designs (`docs/subagent-roster-design.md`, `docs/subagent-coordination-design.md`) and their phased children together instead of splitting them across `v3.3`, `Backlog`, and unmilestoned epics:
- **Roster (`#1559`):** `#1561` (P1: builtin `explore`/`plan`/`general` + `extends:`), `#1562` (P2: measure `builtin:explore` on T1 replay rig).
- **Coordination (`#1560`):** `#1563` (P1 bug: waiting `spawn_agent` holds mutation lock — **pull into Wave 1 alongside `v3.3`**), `#1220` (configurable concurrency cap), `#867` (invocation mode on spec; rename key since `mode` is taken by `#1401`), `#1565` (P2: `spawn_agents` batch fan-out with join), `#1566` (P3: steering `send_message`/`list_agents`), `#1567` (P4: A2A budget caps), `#1305` (`--record-to` into child models).

### `Track: Repo, CI & Recipe Hygiene` (8)

Continuous engineering hygiene that should not inflate or block a minor release:
- **Gate integrity & governance:** `#1271` (`ci-docs.yml` no-op check leak — **fix in Wave 0**), `#1595` (surveyor baseline: CODEOWNERS, CodeQL, secret scanning, rulesets), `#1554` (run `dev/smoke` in CI).
- **Dev scripts, pins & model UAT:** `#1160` + `#1276` (go-format worktree walk + `repo_root()` relative path — bundle in one PR), `#1424` (rolling `ax-pin-check` bot report; consider auto-close in workflow), `#936` (next Gemini frontier default UAT).
- **Recipes:** `#1555` (refine, update and prune K8s/GKE recipes; absorbs `#1281` and `#1564`).

### `v3.4` — Platform: External State, K8s Install & Workload Identity (7)

Unchanged scope from 2026-11-16:
- `#1192` — External-state failover tracker (P1 control/index, P2 residency lease, P4 K8s Lease, P5 Redis, P7 UAT).
- `#1313` — Purser phase 2 (mTLS/OIDC on attach; `switchboard#155` and `switchboard#159` are still open and waiting).
- `#590` — Per-caller MCP credential resolution (including `vcs_*` credentials).
- `#965`, `#957`, `#955`, `#951` — First-party K8s kustomize base, deployment posture in `DESIGN.md`, and outer-layer sandbox hardening page.

*(Pull `#1313` and `#590` forward into `v3.3` only if `shipmind`/`switchboard` blocks on them before `v3.3.0` cuts.)*

### `Backlog` (26)

Valid items with no active consumer or blocked upstream:
- **Upstream-blocked (`blocked: upstream`):** `#555` (already in `Backlog`; ADK `gen_ai` histogram semantics), `#1178` (moved from `v3.3`; `core-tui#347` still open), `#1321` (moved from `v3.3`; `core-tui#378` still open).
- **Moved out of `v3.3` (no active consumer blocking `v3.3`):** `#1116` (self-dev historical-replay corpus; pick up after `v3.3` Wave 1), `#1132` (headless operator-event emitter).
- **Agent & subagents:** `#521`, `#709`, `#934`, `#950`, `#952`, `#1034`, `#1253`, `#1306`.
- **Attach & streaming:** `#330`, `#995`, `#1308`, `#1309`, `#1310`, `#1311`.
- **Replay, observability, dev, install, security:** `#949`, `#554`, `#1097`, `#1265`, `#960`, `#961`, `#1219`.

## Phased execution plan

Work through the backlog in **four explicit waves**. Do not start Wave N+1 work until Wave N's exit criteria are met (except parallel hygiene PRs in `Track: Repo, CI & Recipe Hygiene`).

### Wave 0 — Unblock, merge in-flight fixes & close gate leaks (Days 1–2)

1. Apply this assessment on GitHub (close `v3.2`, close `#1591`/`#1281`/`#1564`, create the 3 track milestones, move issues).
2. Rebase and merge open PRs `#1615` (`#1600`), `#1599` (`#1597`), `#1601` (`#1598`), and `#1596` (lookout pin bump).
3. Fix `#1271` (`ci-docs.yml` no-op check leak on mixed PRs) and bundle `#1160` + `#1276` in one dev-script PR.
4. Land `#1593` (wire `pkg/approval` + `internal/effects` into `pkg/compose`/`cmd/core-agent` and subagent runners, with a binary-level refusal test).

**Exit gate:** 0 stale open fix PRs, CI gate integrity restored, durable write gate actually wired and tested in `cmd/core-agent`.

### Wave 1 — Autonomy & cost correctness (`v3.3` core, Days 3–8)

Parallelizable across isolated worktrees:
- **Subagent safety & accounting:** `#1563` (waiting `spawn_agent` mutation lock — P1 bug from Subagent track), `#1203` (`Tracker.Last()`), `#1298` (subagent compaction), `#1304` (soft wrap-up / `FinalReport`), `#1053` (terminal alert guidance).
- **Loop & spend guardrails:** `#1300` (watchdog `record_plan` revise loop), `#713` (no-progress detector), `#1234` (summarizer empty text + fallback), `#1275` (per-day cost cap), `#1548` (price on served `ModelVersion`).

**Exit gate:** Every A1-soak autonomy defect is closed with a pre-fix-verified regression test.

### Wave 2 — Active consumers, v3.2 verification & `v3.3.0` cut (Days 9–14)

- **Shipmind / switchboard unblockers:** `#1613` (attach MCP elicitation relay), `#865` (constraints preamble), `#866` (correlation token), `#939` (`omitzero`), `#943` (`prompt_id`), `#860`, `#868`.
- **v3.2 live verification & docs:** `#1590` (run live Gemini Platform ReasoningEngine UAT for Agent Engine Sessions + Memory Bank, publish Starlight memory docs, archive `docs/memory-service-design.md`).
- **Config ergonomics:** `#958` (`config validate`), `#959` (JSON Schema + drift gate).
- **Cut `v3.3.0-rc.1` → `v3.3.0` GA**, then immediately close the `v3.3` milestone and bump `internal/version.Version` to `v3.4.0-dev`.

**Exit gate:** `v3.3.0` GA tagged, `v3.3` milestone closed, `shipmind#144` / `switchboard#207` / `mast-web#153` unblocked.

### Wave 3 — Parallel tracks & `v3.4` platform (Post-`v3.3.0`)

Pick two concurrent tracks based on immediate consumer pull:
1. **`Track: Transcript & Retention`:** Refresh `docs/transcript-archival-design.md` (`#887`), then land `#883` (retention sweep — highest operational urgency), `#881`, `#882`, `#884`, `#885`.
2. **`Track: Subagent Roster & Coordination`:** Land P1 (`#1561`, `#1220`, `#867`), run T1 measurement (`#1562`), then P2–P4 (`#1565`, `#1566`, `#1567`, `#1305`).
3. **`v3.4` Platform:** Start with `#1313` (purser phase 2, unblocking `switchboard#155`/`#159`) and `#590` (per-caller MCP creds), then `#1192` external-state phases (P1/P2/P4/P5) and `#957`/`#965` K8s kustomize base.
4. **Hygiene (background):** `#1595` (surveyor baseline), `#1554` (`dev/smoke` in CI), `#1555` (GKE/K8s recipe refine/prune), `#936` (Gemini frontier default UAT).

## Appendix: Ready-to-run apply commands

Once this assessment is approved, run the commands below to apply the milestone restructure and deduplication on GitHub:

<details>
<summary><b>GitHub CLI commands to apply the restructure</b></summary>

```bash
REPO=go-steer/core-agent

# 1. Close shipped v3.2 milestone (#16)
gh api -X PATCH "repos/$REPO/milestones/16" -f state=closed

# 2. Update v3.3 description (#17)
gh api -X PATCH "repos/$REPO/milestones/17" \
  -f description="Autonomy correctness, shipmind unblockers, v3.2 live UAT, config ergonomics (triage 2026-11-28)"

# 3. Create the three non-release theme track milestones
gh api -X POST "repos/$REPO/milestones" \
  -f title="Track: Transcript & Retention" \
  -f description="Non-release track: transcript export, CLI sessions, eventlog retention (gated on design refresh)"
gh api -X POST "repos/$REPO/milestones" \
  -f title="Track: Subagent Roster & Coordination" \
  -f description="Non-release track: builtin subagent roster (#1559) and fan-out/steering (#1560) across P1-P4"
gh api -X POST "repos/$REPO/milestones" \
  -f title="Track: Repo, CI & Recipe Hygiene" \
  -f description="Non-release track: CI gate integrity, surveyor baseline, dev scripts, rolling pins, K8s/GKE recipes"

# 4. Close duplicate / folded issues (after copying unique checklist items into #1590 and #1555)
# Clear v3.3 milestone on #1281/#1564 first so "not planned" folds do not pollute v3.3 progress.
gh issue close 1591 -R "$REPO" --reason "not planned" -c "Duplicate of #1590 (2026-11-28 assessment); unique env-var unification and --continue/--resume + payload burst items folded into #1590."
gh issue edit 1281 -R "$REPO" --remove-milestone
gh issue close 1281 -R "$REPO" --reason "not planned" -c "Folded into #1555 (GKE/K8s recipe refine/update/prune) per 2026-11-28 assessment."
gh issue edit 1564 -R "$REPO" --remove-milestone
gh issue close 1564 -R "$REPO" --reason "not planned" -c "Folded into #1555 (GKE/K8s recipe refine/update/prune) per 2026-11-28 assessment."

# 5. Assign untriaged issues to v3.3 (1597/1598/1600 close when PRs #1599/#1601/#1615 merge)
for n in 1593 1597 1598 1600 1613; do
  gh issue edit "$n" -R "$REPO" --milestone "v3.3"
done

# 6. Move Track: Transcript & Retention issues
for n in 881 882 883 884 885 887; do
  gh issue edit "$n" -R "$REPO" --milestone "Track: Transcript & Retention"
done

# 7. Move Track: Subagent Roster & Coordination issues
for n in 867 1220 1305 1559 1560 1561 1562 1563 1565 1566 1567; do
  gh issue edit "$n" -R "$REPO" --milestone "Track: Subagent Roster & Coordination"
done

# 8. Move Track: Repo, CI & Recipe Hygiene issues
for n in 936 1160 1271 1276 1424 1554 1555 1595; do
  gh issue edit "$n" -R "$REPO" --milestone "Track: Repo, CI & Recipe Hygiene"
done

# 9. Move upstream-blocked and deferred items out of v3.3 to Backlog
for n in 1116 1132 1178 1321; do
  gh issue edit "$n" -R "$REPO" --milestone "Backlog"
done
```

</details>
