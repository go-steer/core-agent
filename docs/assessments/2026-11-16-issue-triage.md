# Open-issue triage — 2026-11-16

**Status:** applied 2026-11-16. Filed during the apply: #1553 (the #885 defect-2 split, v3.2), #1554 (run dev/smoke in CI, v3.3) and #1555 (refine, update and prune the Kubernetes/GKE recipes, v3.3, which replaces the GKE demo line). Milestones are now v3.2, v3.3, v3.4 and Backlog; the seven themed milestones are closed.

Scope: all 103 open issues on `go-steer/core-agent` at `origin/main` `2c5dac6b`
(v3.2.0-rc.1 cut, v3.1.0 GA shipped, mast fold-in #1411 complete). Every issue
was checked against the code, git history and merged PRs, not its title.
Per-issue evidence (file:line, PR, SHA) is in the triage comment posted on
each closed or rescoped issue. The review ran in four lanes: the agent loop,
cost and subagents; attach, transcripts and the TUI; GKE, Kubernetes and the
companions; and dev tooling, CI, docs and trackers.

## Headline numbers

| Action | Count |
|---|---|
| Close outright (done / superseded / obsolete) | 15 |
| Merge into another issue, then close | 3 |
| Close a stale tracker; its children stand alone or move | 1 |
| Close the GKE demo line; replaced by #1555 | 10 |
| New issues filed (#1553, #1554, #1555) | 3 |
| **Open after cleanup** (103 − 29 + 3) | **77** |
| of which: v3.2 (before GA) | 10 |
| of which: v3.3 | 38 |
| of which: v3.4 (platform track) | 7 |
| of which: backlog | 22 |
| Issue text stale; needs a rewrite whatever its target | 17 |

## Overall assessment

1. **Most staleness is in trackers, not work items.** About 80% of the leaf issues
   are still valid as written or with a light rewrite. The dead weight is
   epics and trackers whose work shipped while the tracker stayed open:
   - #589 (Hermes)
   - #1411 (mast fold)
   - #1042 (v3.0 DoD)
   - #1235 and #1307 (umbrellas)
   - #869 and #877 (Aug-31 trackers)
   - the ladder half of #1116

   Process fix: closing the tracker is part of the release checklist for the
   milestone it tracks.
2. **Autonomy-correctness bugs from the A1 soak (Oct 3–10) are still live,
   and three releases of feature work went past them.**
   - #1203: subagent turns become the parent's `Last()`, on both delegation doors.
   - #1298: subagents get no compaction.
   - #1300: a `record_plan` revise loop never gets cut.
   - #1234: an empty summary, with no fallback for short sessions.
   - #1275: no per-day cost cap.
   - #713, the no-progress half: still not built.

   These sit squarely on north-star goal 1 ("durable, cost-efficient
   autonomous runtime") and should lead v3.3.
3. **Some of what we ship or document is false.**
   - `--ui` has never served anything in a release: `.mast-web-version` has been
     blank since July, while `reference/web-ui.md` says the assets ship embedded (#348).
   - `README.md:200` claims "v1 stability" on a `/v3` module (#956).
   - The daemon SA IAM comment overstates its secrets access, and the same text
     was copied into the Terraform module (#695).
   - The GKE recipe overlays pin image `2.9.0` (seen while checking #1097).
   - One-shot transcripts drop the model's answer (#885).

   These are cheap to fix and belong in v3.2.
4. **New data-loss exposure in rc.1.** The Agent Engine transcript backend
   (#1510/#1511) defaults to a 30-day idle TTL, and nothing can export a
   session first. That is the exact irreversible loss #883 warned about. At
   minimum, v3.2 should document it (#889). The transcript/retention cluster
   (#881–#887) should then lead v3.3's operability theme. Its design doc
   (`docs/transcript-archival-design.md`) predates the `eventlog.Transcript`
   seam, the Agent Engine backend, `--resume` and the CLI subcommands, so it
   needs a refresh first.
5. **One live security bug:** #1274. A path-prefix pattern (`bash:dir/*`) is a
   raw `strings.HasPrefix` (`pkg/permissions/policy.go:327-330`), so it also
   matches `dir/../../anything`. This affects plain `permissions.allow` too,
   where no approver stands in front. The repo's own soak config uses that
   shape. Fix it before v3.2 GA, starting with a probe test.
6. **Two direction questions the backlog can't answer for itself:**
   - **GKE demo line**: #1223, S1/S2/S4/S5 (#1215–#1218), #1219, #1221, #708,
     #620 and #1281, about 10 issues. There has been no core-agent work since
     2026-10-02, and shipmind (AX on Agent Substrate) became the
     production proving ground. **Decided:** close the demo line and instead
     refine, update and prune the recipes we ship (#1555, v3.3). The recipes
     have drifted: the frozen `kube-platform-agent` still ships (pinned to
     `2.9.0-dev.1`), the overlays pin `2.9.0`, there are four kustomize base
     forks, and no recipe uses v3.x features. #1219 (pre-model redaction) stays
     in the backlog because it stands on its own. The scenarios and
     tiered-tools designs moved to `docs/archive/` as superseded.
   - **First-party Kubernetes install path**: #957–#961, #965, #955. It was
     reaffirmed in #1411 (kustomize first-party, Helm retired), but no consumer
     is waiting: shipmind deploys as AX tasks. Meanwhile the kustomize forks
     grew from three to four. **Decided:** the cheap, broadly useful items
     (`config validate`, the JSON Schema) go to v3.3; the base, tracker and
     posture doc go to v3.4; `config init` and the operator design go to backlog.
7. **Upstream-blocked and spec items are fine parked.** These include #555
   (ADK metrics), #1178 (core-tui#347) and #1321 (core-tui#378). They should
   carry `blocked: upstream`, not a milestone.

## Milestone structure

Applied: **version milestones only**, plus a `Backlog` milestone. Themes
become labels (`area: transcript`, `area: k8s-install`).

| Milestone | Action |
|---|---|
| v3.0 — Proven autonomy | close (#1042 closes; #1116 moves to v3.3) |
| Hermes replacement (companions) | close (#589/#591/#592/#658 close; #590 goes to v3.4) |
| One harness: mast fold-in | close (#1411 closes; P6 is tracked in go-steer/shipmind#48) |
| Backlog: coding-assistant parity | close (empty) |
| Docs accuracy | close (#956/#889 go to v3.2, #955 to v3.4) |
| Transcript and retention | close; its issues go to v3.3 with an `area: transcript` label |
| Kubernetes install path | close; issues go to v3.3/v3.4 with an `area: k8s-install` label |
| v3.2 | gets the 10 pre-GA items below |
| **v3.3** (new) | autonomy correctness + operability |
| **v3.4** (new) | platform: external state, install, identity |
| **Backlog** (new, or no milestone) | valid, no consumer or priority yet |

## Close (15)

| # | Verdict | Evidence |
|---|---|---|
| 208 | obsolete | unsolicited third-party catalog promo (UTM outreach link), not a project ask |
| 346 | superseded | by #348 (mast-web is at v0.7.0; v0.2.1 is moot) |
| 553 | obsolete | FYI only; adk-go#1229 is recorded in `docs/library-friction-log.md` §1 and `docs/archive/eventlog-decisions.md` |
| 589 | done | Hermes epic: every workstream shipped, closed or superseded; W0 (#590) stands alone; design archived as shipped |
| 591 | done | switchboard lives in go-steer/switchboard (v0.9.2), runs against core-agent in staging (#1411, 11-06 comment) |
| 592 | superseded | by #1490 / PR #1512 (`vcs_*` tools + `builtin:propose-change`) |
| 658 | obsolete | `/v1/responses` was deferred "until a consumer needs it"; none appeared; A2A (#1407) became the interop surface |
| 953 | done | `pkg/skills` loads SKILL.md through ADK `skilltoolset`, which implements the agentskills.io spec |
| 963 | done | `docs/archive/` + status lines + docs-lint check 5 (#1526) |
| 1042 | done | v3.0 DoD: all seven boxes met; v3.0.0 GA 2026-10-21; follow-ups filed separately |
| 1194 | done | ADK/OTel bump in #1196; local `govulncheck` reports 0 affecting vulns |
| 1235 | superseded | split by its own comment into #1298, #1299 (closed), #1300 and #1203 |
| 1270 | done | #1293 added Content-Type to every POST (follow-up: dev/smoke still never runs in CI; worth its own issue) |
| 1307 | superseded | its three children (#1304/#1305/#1306) are unrelated in code; each stands alone |
| 1411 | done | every core-agent phase box checked; P6 (archive mast) is shipmind-gated → go-steer/shipmind#48 |

## Merge, then close (3)

| # | Into | Why |
|---|---|---|
| 142 | #1313 | Most of it has shipped (Caller, X-Asserted-Caller, ACLs, `/whoami`). What's left is a verified IAP/Cloudflare JWT authenticator, which is purser `authn/oidc` |
| 1331 | #887 | the remote `/transcripts` decision belongs in the transcript tracker (core-tui#301) |
| 1332 | #1116 | T2 staleness is subsumed by the historical-replay corpus (per-task pinned bases) |

## Close stale trackers (1)

| # | Disposition |
|---|---|
| 869 | "demo-3 prose standing in for runtime": #864 and core-tui#298 are closed; the open children (#860, #865, #866, #867, #868) go to v3.3 on their own and lose nothing |

## GKE demo line: closed, replaced by #1555 (10)

| # | Disposition |
|---|---|
| 1223 | demo epic: closed; scenario analysis archived as superseded; recipe work moved to #1555 |
| 1215, 1216, 1217, 1218 | demo scenarios S4/S1/S2/S5: closed with the epic |
| 877 | GKE recipe maintenance tracker: superseded by #1555 |
| 708, 620 | lookout bundle-first + sidecar: merged into #1555 as an optional item |
| 1221 | Cloud Monitoring MCP: merged into #1555 as an optional item |
| 200 | tiered tools: kode-gopher half shipped (#1391); `k8s-sensors` half moved to k8s-lookout; design archived as superseded |

## v3.2 — before GA (10)

| # | What | Why now |
|---|---|---|
| 1274 | path-prefix patterns match `dir/../..` (allow rules too) | **security**; widen the title to cover plain allow rules |
| 1547 | core-models v0.15.3: Gemini `HeaderTransport`, model tiers | profile headers silently dropped on every Gemini call |
| 1550 | cosign v3 bundles / installer v4 | planned for rc.2 |
| 1551 | three CI-only flakes (`freeAddr` race, …) | each hit costs a CI cycle |
| 885 | **split**: one-shot transcript saves only the prompt (`main.go:825-841`) and discards the save error | small bug; schema reconciliation (defect 1) goes to v3.3 |
| 889 | docs: session DB growth and pruning, **plus the Agent Engine 30-day TTL** | new data-loss exposure in rc.1 |
| 348 | `--ui`: pin mast-web v0.7.0 and smoke against protocol 1.20, *or* retire `--ui`; either way fix `reference/web-ui.md` now | the published docs are false |
| 956 | `README.md:200` "v1 stability" → v3 + compatibility-policy link | one line; the rest of the issue is done |
| 695 | daemon SA IAM comment (kustomize + Terraform) overstates its secrets access | priority: high; doc-only |
| 696 | set `model.builtin_tools.web_search: false` in gke-platform-agent configs | one config line; #876 shipped the knob and nobody adopted it (+ bump overlays off image `2.9.0`) |

## v3.3 (38): autonomy correctness and operability

**Autonomy and cost correctness** (goal 1; the A1-soak family)
- #1203: subagent turns still become the parent's `Last()`. Live on both doors.
- #1298: subagents get no compactor, and the in-turn bound is only the window.
- #1300: watchdog churn signal, or warning escalation, for revise loops.
- #713: **rewrite** to cover gap 2 only. A no-progress / near-duplicate-args limit; the per-subagent budget shipped in #850.
- #1304: **rewrite**. `FinalReport` on the async spawn door, plus an optional ~80% notice.
- #1275: a per-day or process-wide cost cap; the spend ledger (#1359) makes it cheap.
- #1234: summarizer empty text. Confirm the self-dev soak no longer needs it as its seed bug.
- #1053: per-class guidance on async terminal alerts.
- #1220: wire `WithMaxConcurrent` to a config key.
- #1305: `--record-to` for child models.
- #1548: price on the served `ModelVersion`, with one rule for live and rebuild.
- #936: **rewrite** as "next Gemini frontier default (3.8-flash vs gemini-4-*) after UAT".
- #865: generate the enforced-constraints preamble from the startup summary (priority: high).
- #867: invocation mode on the spec. Rename the key, since `mode` is taken by #1401.

**Transcript and retention** (`area: transcript`)
- #887: tracker. Refresh `docs/transcript-archival-design.md` first.
- #881: export reads through the `eventlog.Transcript` seam.
- #882: **rewrite**. Subcommands exist now, `ls` collides, and listing works on SQL only.
- #883: retention sweep.
- #884: replay truncation frame, plus a fix for the comment that points at nothing.
- #885: defect 1, the schema reconciliation.

**Attach wire and inbox correctness**
- #939: `omitempty` on `time.Time` → `omitzero`, with a protocol minor bump.
- #943: turn-complete `prompt_id` should be the drained batch's id.
- #860: show the proxy identity alongside the asserted sender.
- #866: correlation token on inject (priority: high).
- #868: inbox guidance at N=1.
- #1132: **rewrite**. Opt-in headless emitter, possibly via `stream-json`.
- #1178 and #1321: blocked on core-tui#347 and #378. The #1321 text fix is cheap now.

**Self-development replay** (north-star priority 3)
- #1116: **rewrite**. Close the ladder and retarget the issue to the historical-replay corpus (absorbs #1332).

**Config ergonomics** (cheap, useful beyond Kubernetes)
- #958: `core-agent config validate`.
- #959: generated JSON Schema with a drift gate.

**Dev and CI hygiene**
- #1160: go-format scripts walk nested worktrees.
- #1276: `repo_root()` with a relative path. Can go in one PR with #1160.
- #1271: ci-docs no-op jobs satisfy required checks on mixed PRs. This is a gate-integrity problem.
- #1424: bump the AX pins and close by hand; consider auto-close in `ax-pin-check.yml`.

**Recipes**
- #1555: refine, update and prune the Kubernetes/GKE recipes (absorbs #708/#620/#1221 as optional items).
- #1281: BackOff routing in the troubleshooting skill.

**Filed in this triage**
- #1554: run `dev/smoke` in CI.

## v3.4 (7): platform (external state, install, identity)

- #1192: external-state tracker. Tick P3 (Agent Engine) and P6-partial (memory); left are P1 control/index, P2 residency lease, P4 K8s Lease, P5 Redis, P7 UAT.
- #965: **rewrite** the Kubernetes install tracker. Tier 0 is done, the #685 coupling is gone; mention Terraform and AX.
- #957: first-party kustomize base. The fork count is now four.
- #955: the deployment posture in `docs/DESIGN.md`.
- #951: **rewrite** the hardening page around the shipped `safety.sandbox` drivers stacked with outer layers.
- #1313: **rewrite**. Drop the "after mast" ordering, since mast is being archived; absorb #142. switchboard#155/#159 are waiting on it.
- #590: **rewrite**. Drop the Hermes/v2.9 framing; make the `sub` check a hard requirement; add `vcs_*` credentials as a consumer.

Could move to v3.3 if the multi-tenant track pulls them: #1313, #590.

## Backlog (22): valid, no consumer or priority yet

- **Agent and subagents:** #521, #709 (**rewrite** to `attach: call:<id>` handles), #934, #950, #952 (**rewrite**: re-run the parallel-probe on current Gemini first), #1034 (**rewrite**: item 3 only), #1253, #1306
- **Attach and streaming:** #330 (**rewrite**: client replays the in-repo conformance fixtures), #995, #1311 tracker + #1308, #1309, #1310
- **Replay and observability:** #949, #554 (**rewrite**: "UAT OTLP metrics → Cloud Monitoring"), #555 (`blocked: upstream`)
- **Dev:** #1097 (correct the `read_only` claim), #1265 (**rewrite**: drop the two fixed lexer items)
- **Install:** #960, #961
- **Security:** #1219 (pre-model redaction; independent of the closed demo line)

## Follow-ups

- Filed: #1554 (run `dev/smoke/*` in CI; smoke scripts rotted unnoticed, #1270/#1293).
- Filed: #1555 includes bumping the recipe image pins and adding a pin-tracks-release check (from #1097).
- Done in a follow-up PR: the comment at `pkg/models/anthropic/anthropic.go:239` now points at `docs/archive/kube-agents-scenarios-design.md`.
