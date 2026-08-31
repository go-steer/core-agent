# Transcript export and archival

Design doc for turning "what happened in this session" into an artifact an operator can extract, keep, and eventually expire — and for resolving the three unrelated things this codebase currently calls a transcript.

**Status:** proposed. Nothing here is built. Filed out of the question "how do we fix `/transcripts` in attach mode?", whose honest answer turned out to be "mostly by not doing that" — see §"What is already solved".

**Tracking issue:** [#887](https://github.com/go-steer/core-agent/issues/887). Work items: [#881](https://github.com/go-steer/core-agent/issues/881) export endpoint, [#882](https://github.com/go-steer/core-agent/issues/882) `sessions` CLI, [#883](https://github.com/go-steer/core-agent/issues/883) retention, [#884](https://github.com/go-steer/core-agent/issues/884) replay ceiling, [#885](https://github.com/go-steer/core-agent/issues/885) the `.agents/sessions/` collision, [#886](https://github.com/go-steer/core-agent/issues/886) attach-client state dir, and [core-tui#301](https://github.com/go-steer/core-tui/issues/301) upstream.

## Motivation

`core-agent-tui` sets no `coretui.Options.AgentsDir`, so in attach mode `/transcripts` lists nothing and saves nothing. The obvious fix — give the attach client a state directory — is cheap and worth doing for unrelated reasons (§"The state-directory fix"), but it does not answer the question underneath: the daemon is the process that holds the complete record, and it currently has no way to hand any of it to anyone.

Investigating that surfaced four gaps, only one of which is about the TUI.

### Three artifacts, one word

| # | Artifact | Where | Written by | Contains |
|---|---|---|---|---|
| 1 | `pkg/transcript.Transcript`, schema **v1** | `.agents/sessions/<RFC3339>.json` | `cmd/core-agent` headless one-shot | The prompt and usage totals. Nothing else. |
| 2 | `coretui.Transcript`, schema **v2** | `.agents/sessions/<RFC3339>.json` | core-tui on TUI exit | Role + text + tool name/args/preview/call-ID |
| 3 | ADK `events` + `agent_eventlog` | the session DB | every turn, always | Everything. The source of truth. |

Artifacts 1 and 2 are different Go types with different schema versions writing the **byte-identical filename convention into the same directory** (`strings.ReplaceAll(started.UTC().Format(time.RFC3339), ":", "-") + ".json"`, independently defined in both), and only one of them can be read back: core-tui has `ListTranscripts` / `LoadTranscript`, `pkg/transcript` has `Save` and nothing else.

Artifact 1 is also close to empty. `persistTranscript` in `cmd/core-agent/main.go` builds its `Messages` slice from a single literal — `{Role: "user", Text: prompt}` — so a headless run records that a prompt was issued and what it cost, and discards the model's answer entirely. The save error is dropped on the floor too (`_, _ =`).

A `.agents/sessions/` populated by both runs is therefore a mixed-schema pile that `/transcripts` lists uniformly and loads misleadingly: the v2 files are real conversations, and the v1 files open as a one-message chat that looks like a session where the agent never replied.

Neither 1 nor 2 is *the record*. Both are projections of whatever one process observed. Artifact 3 is the record, and nothing renders it.

### What is already solved

Worth stating plainly, because it removes most of the apparent gap:

- **The attached session's own history.** The remote adapter's `Events()` connects with `since=0` on first attach precisely so "the operator sees the existing history" (`internal/coretuiremote/adapter.go`). Attaching already replays the conversation.
- **Reaching another session.** `cmd/core-agent-tui/picker.go` lists hub-local and peer sessions; `GET /sessions` unions the live registry with persisted-but-evicted rows carrying `title` and `last_touched_at`; session resume (v2.5, `docs/session-resume-design.md`) rehydrates whichever the operator picks.

So porting `/transcripts` to attach mode as-is would ship a partial, per-terminal duplicate of two surfaces that already exist and are better. That is the case for *not* doing it.

### The four real gaps

1. **No export artifact.** Nothing anywhere produces a durable, portable, greppable document from a session. This is the one thing artifacts 1 and 2 incidentally provide, and it is the only reason to want them.
2. **The full history is unreachable over attach.** `maxReplayEvents = 5000` clamps a subscriber's `since` cursor *upward* to the session's replay floor. `pkg/attach/broadcaster.go` says a client wanting more "can page it from the eventlog directly" — but no endpoint pages the eventlog; `GET /events` is the capped path. On a session past 5000 events, the early history cannot be retrieved over the wire at any cursor. Only direct database access gets it.
3. **No retention, at all.** `agent_eventlog` and ADK's `events` grow without bound. There is no TTL, no prune, no sweep, no vacuum. `DELETE /sessions` hard-deletes one session on demand and that is the entire story. `docs/session-resume-design.md` OQ #3 deferred soft-delete plus a sweep tool; it stayed deferred. Today "archive" and "unbounded growth" are the same feature.
4. **The v1/v2 collision** described above.

Gap 3 is the one with an operational deadline attached. A long-lived daemon — the shared-session Slack deployment, the K8s pod that has been up for a month — accumulates rows forever, and nobody can currently get the data out before deleting it.

## Goals

- **One canonical transcript document**, defined once, rendered from the eventlog, identical whether the requester is local or attached.
- **Complete.** The export reads the eventlog directly, so it is not subject to the broadcaster's replay cap. Closes gap 2 as a side effect.
- **Reachable without a TUI.** `curl` and a CLI subcommand, because archival is a scripted, unattended concern and the TUI is the wrong dependency for it.
- **Export before retention.** You cannot responsibly prune what nobody can extract. Ordering is a hard constraint, not a preference.
- **Bytes, not storage.** The daemon serves a document. Where it lands is the operator's tooling's problem.

## Non-goals

- **A storage backend.** No GCS/S3/blob writer in core-agent. A file written inside a Cloud Run container is not an archive. Serve the bytes and let the caller's pipeline place them.
- **Making core-tui's `/transcripts` work remotely.** It would need an upstream host interface — `ListTranscripts`/`LoadTranscript` are hardwired to `os.ReadDir`/`os.ReadFile` today. Worth an upstream issue, but it is the least valuable piece and it is downstream of deciding the document format. Filed separately, not scheduled here.
- **Changing what the TUI writes on exit.** Artifact 2 keeps working exactly as it does. This design gives it a canonical sibling; it does not migrate it.
- **Cross-daemon aggregation.** Sessions live in the daemon that created them (`docs/multi-session-design.md` §Non-goals). An archive spanning daemons is the caller's concatenation, not ours.
- **Compliance semantics.** Tamper-evidence, signing, WORM, legal hold. Real concerns for somebody, out of scope until somebody asks.

## Conceptual model

### The document

One schema, versioned, rendered server-side from `Stream.Since(ctx, 0, ForSession(...))` — which yields `Entry{Seq, Event, Metadata}` where `Event` is the full ADK `session.Event`. Everything needed is already there, uncapped.

Two formats from one renderer:

- **`json`** — the archival form. Structured, stable, machine-readable, carrying what the eventlog carries: seq, timestamps, author, invocation ID, the event parts, and the per-event metadata sidecar (`caller`, `proxy_by`) that is the audit trail. This is a superset of both existing schemas and should be numbered as its own thing, not as v3 of either.
- **`md`** — the human form. What an operator pastes into an incident review.

The JSON form is the contract; markdown is a rendering of it and may change freely.

### The endpoint

```
GET /sessions/{sid}/transcript?format=json|md
```

`session:read`, so it inherits the existing ACL — a read-only attachment can export, an unauthorized caller gets the same 404 it gets everywhere else. Attach protocol minor bump (currently 1.11.0), additive: new path, no existing shape moves.

Streaming the response matters. A 50k-event session must not be assembled in memory on either side.

**Wrinkle worth naming up front:** `GET /sessions` only unions the persisted-but-evicted rows when ACL enforcement is on (`h.enforceACL` plus a store). A single-user daemon with enforcement off sees live sessions only, so "list what I can export" is weaker exactly where the deployment is simplest. Either the export index has to read the ACL store unconditionally, or the CLI has to go to the database directly. Open question §1.

### The CLI

```
core-agent sessions ls
core-agent sessions export <sid> [--format json|md] [--out -]
```

Reads the session DB directly rather than going through HTTP, so it works against a stopped daemon and a copied `.db` file — which is the shape most post-mortems actually take. There is no `sessions` subcommand today; this would be the first.

### Retention, later

Deliberately a separate phase behind export, and deliberately under-specified here because it has more open questions than the export does. The shape it probably wants:

- **Soft-delete first**, per the session-resume doc's deferred plan: a tombstone the resumer refuses to rehydrate, leaving the rows readable by export.
- **A sweep** with an age and/or count policy, off by default, that hard-deletes tombstoned sessions past the horizon.
- **Never prune a session that has never been exported** — or at least make that the default, with an explicit override. The safe default is to grow.

## Phases

| Phase | Scope | Depends on | Issue |
|---|---|---|---|
| **A** | Canonical document type + renderer (JSON + markdown) over `Stream.Since` | — | #881 |
| **B** | `GET /sessions/{sid}/transcript`, protocol bump, docs | A | #881 |
| **C** | `core-agent sessions ls` / `export` | A | #882 |
| **D** | Soft-delete + retention sweep | B or C | #883 |
| **E** | Reconcile `pkg/transcript` v1 with the canonical type; stop two writers sharing one directory | A | #885 |
| **—** | `--state-dir` for `core-agent-tui` | nothing (independent) | #886 |

A/B/C are the useful unit. D is where the operational pressure is but it must not land first. E is cleanup that becomes obvious once A exists.

### The state-directory fix

Independent of everything above, and worth doing on its own: `core-agent-tui` has no local state directory, and that single absence disables transcript save/list/load **and** theme persistence **and** mouse persistence. An XDG state dir keyed by the daemon URL turns on all three. Notably it is also what would make the `PersistMouseChoice` that shipped in #863 apply to the attach client — which is the client #859 was actually filed about, and which currently still depends on the `--no-mouse` flag from #862.

This does not fix `/transcripts` in any interesting sense — the transcripts would still be per-terminal projections of what one client saw. It fixes the toggles.

## Alternatives considered

**Port `/transcripts` to attach mode as-is** (host interface upstream, remote adapter backs it with HTTP). Rejected as the primary move: it duplicates the picker and the `since=0` replay, and it puts the archive's shape in core-tui's hands, where core-agent cannot evolve it. It becomes reasonable *after* the document format exists, as a thin client of it.

**Client-side only** (`--state-dir` and stop there). Rejected as insufficient: it cannot see past what one terminal witnessed, cannot be scripted, does not reach the pre-5000-event history, and does nothing for retention. Worth doing, not worth calling done.

**Write the archive from the daemon on session end.** Rejected: there is no reliable "session end" (sessions are evicted, resumed, and outlive processes), and it requires exactly the storage backend this design refuses to own.

## Open questions

1. **Export index without ACL enforcement.** `GET /sessions` hides persisted-only rows when `enforceACL` is off. Does the export path read the ACL store unconditionally, or does the CLI-against-the-DB path cover this case well enough that the HTTP index can stay as it is?
2. **Does the canonical JSON supersede `pkg/transcript` v1, or sit beside it?** Superseding means changing what one-shot mode writes, which is a user-visible on-disk change for anyone parsing those files. Beside means three schemas instead of two.
3. **Subagent events.** `GET /sessions/{sid}/agents/{name}/events` is a separate stream. Does a parent's export inline its subagents' work, reference it, or omit it? Inlining is what a post-mortem wants and is also how the export gets large.
4. **Redaction.** Transcripts contain tool arguments and results verbatim — credentials pasted into a prompt, secrets returned by an MCP call. An export is a new, easier way to move that off the box. Is a redaction pass in scope for phase A, or is "the archive is as sensitive as the database" a sufficient posture?
5. **Retention units.** Age, event count, byte budget, or per-session-count? And per-app or global? Deferred to phase D, listed here so it is not rediscovered.
