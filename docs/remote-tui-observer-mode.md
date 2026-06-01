# Remote TUI observer mode (PR E, v2.2 target)

Design skeleton for extending the remote `core-agent-tui` from
operator-driver-only (Pattern A) to also support operator-observer
(Pattern B). Cross-repo work spanning `go-steer/core-tui` and
`go-steer/core-agent`.

## The two attachment patterns

**Pattern A — driver.** Operator types prompts, agent responds.
One turn per inject. The "remote REPL replacing a local one"
model. Today's `cmd/core-agent-tui` + `internal/coretuiremote`
adapter fit Pattern A cleanly: `coretui.Agent.Run(ctx, prompt)`
sends the inject, the iterator yields events for the resulting
turn, ends on `TurnComplete` (or the heuristic fallback).

**Pattern B — observer.** Agent is running on its own — via
`agent.RunAutonomous`, scheduled background subagents, other
attached clients' injects, MCP-server-triggered activity, etc.
Operator attaches to **watch** what's happening and occasionally
intervene. Many turns happen without operator input.

Pattern B is structurally different from Pattern A. The operator
isn't driving turns; they're observing a stream of events the
agent is producing autonomously. `coretui.Agent.Run` doesn't
model this — it assumes the operator's submission is what
triggers each turn.

## Why Option 3 (request_id correlation) alone doesn't fix Pattern B

Request_id correlation — stamping inject-triggered events with a
request_id so the adapter can attribute them to a specific Run()
call — correctly identifies "which events belong to my injected
turn." Useful for Pattern A.

But in Pattern B the operator might never inject anything. The
agent produces events under no request_id (autonomous turn,
subagent activity, MCP elicit, ...) and the adapter's per-turn
iterator filter would correctly attribute these to NO request_id
and dutifully drop them — leaving an empty scrollback while the
agent works.

The mismatch isn't correlation; it's that **the per-turn iterator
shape doesn't fit observation**.

## Proposed architecture

### coretui side: new `LiveAgent` capability

```go
// In go-steer/core-tui/tui/agent.go

// LiveAgent is an OPTIONAL capability for hosts whose agent isn't
// strictly driven by per-turn Run calls — e.g., remote-attached
// daemons running autonomously, observed via a continuous SSE
// stream. The TUI prefers LiveAgent over Run when both are
// implemented: it spawns a single long-lived goroutine that
// ranges over Events(ctx) and updates the chat scrollback from
// every event, regardless of whether the operator just typed a
// prompt.
//
// Run() remains supported (for per-turn-driving hosts), but is
// SKIPPED when LiveAgent is present. Operator submissions then
// flow through InjectableAgent.Inject if it's implemented; if not,
// operator typing is a no-op (read-only view).
type LiveAgent interface {
    Events(ctx context.Context) iter.Seq2[Event, error]
}
```

`coretui.Run`'s startup logic:
1. Check Agent for LiveAgent assertion. If yes → start LiveAgent
   drain goroutine; ignore Run.
2. Otherwise → existing per-turn Run flow.

The chat view's append-event logic doesn't need to change — it
already merges incoming Events into scrollback. What changes is
the EVENT SOURCE: continuous drain vs per-turn iterator.

### Remote adapter side: implement LiveAgent

```go
// In internal/coretuiremote/adapter.go

func (a *Adapter) Events(ctx context.Context) iter.Seq2[coretui.Event, error] {
    return func(yield func(coretui.Event, error) bool) {
        frames, err := a.client.Stream(ctx, a.sessionPath, a.lastSeq)
        if err != nil {
            yield(coretui.Event{}, err)
            return
        }
        for frame := range frames {
            if ctx.Err() != nil { return }
            if isReplay(frame.Event.Timestamp, a.connectedAt) {
                continue
            }
            ev := translateEvent(frame.Event)
            if isEmptyEvent(ev) { continue }
            if !yield(ev, nil) { return }
        }
    }
}
```

The adapter ALSO keeps Run() — but coretui won't call it when
LiveAgent is implemented. We could delete Run; cleanest is to
keep it as the Pattern-A fallback for environments that prefer
per-turn semantics.

Operator injections still flow through `InjectableAgent.Inject`
(already implemented on the adapter). When operator types a
prompt:
1. Adapter.Inject(message) sends POST /inject to the server
2. Server processes the inject as it would normally
3. Resulting events flow through the LiveAgent's continuous Events
   stream like everything else
4. Operator sees their prompt appear in scrollback as a user-
   authored event (the SSE echo) followed by the model's response

### Request_id correlation, scoped down

With LiveAgent in place, request_id correlation becomes a SMALLER
question: it's no longer "which events does this turn own" but
"which events were initiated by ME vs. by the autonomous loop / a
subagent / another attached client." Useful for UI affordances:

- Mark operator-initiated turns with a glyph (the in-process TUI
  uses ↻ for auto-continue; we'd use, say, ✦ for operator-initiated)
- Filter the scrollback to "my interactions only" via a toggle
- Show inline confirmation when the operator's inject lands

Server-side: inject handler generates a `request_id`, returns it
in the POST response, stamps it on the user event + resulting
model events via `CustomMetadata["request_id"]`. Adapter holds the
ids of "my recent injects" in a small ring buffer; translateEvent
checks that field and surfaces it on `coretui.Event` (new field
or via a separate marker).

This part is small and additive — can land as a v2.2 follow-up
inside PR E or after.

### History on attach (the operator wants context)

With LiveAgent, history-on-attach is natural: drop the
`isReplay()` timestamp filter when LiveAgent is the source (in
Pattern B the operator wants to see what they walked into). The
broadcaster's existing since=0 replay does the right thing.

For very long sessions where the full replay is wasteful, add an
optional `?from=<unix-seconds>` or `?last=N` query parameter to
the events endpoint and let the adapter request the last hour /
last 1000 events instead of the whole log. Tunable, not required.

## Scope estimate

Cross-repo, ~400–600 LoC:

- core-tui upstream: add LiveAgent interface (~50 LoC) + program
  startup dispatch (~50 LoC) + tests (~100 LoC). Small upstream
  PR.
- core-agent adapter: implement Events() (~80 LoC), drop the
  Pattern-A-specific isReplay filter when LiveAgent is in use
  (~20 LoC), tests (~150 LoC).
- Optional: request_id correlation (~200 LoC across server +
  adapter).
- Smoke doc update (~30 LoC).

## What this doesn't decide

- **Whether to delete Run() from the adapter** once LiveAgent
  works. Keeping Run as a fallback adds little code; choose at
  PR-time.
- **How operator injections should visually mark "mine" in the
  scrollback**. Style decision; defer to PR-time.
- **Multi-client coordination.** If two operators attach
  simultaneously, both see the same live stream. Inject from
  either flows through naturally. No conflict resolution needed
  unless we add presence indicators ("operator-B is watching") —
  out of scope.
- **History-replay UX for very long sessions.** Punt to "scrolls
  fine for now; add `?last=N` if someone complains."

## Sequencing

1. **Land v2.1** as Pattern A only (this train). Document the
   Pattern-B gap in the smoke doc. ✓ shipped 2026-05-31.
2. **Upstream core-tui PR**: add LiveAgent interface. Get review
   + a tagged release. ✓ shipped in core-tui v0.6.6 (issue #22).
3. **core-agent adapter PR**: implement Events(); bump core-tui
   dependency; update smoke doc to add an observer-mode section.
   ✓ this PR — `Adapter.Events(ctx)` at
   `internal/coretuiremote/adapter.go`, core-tui bumped to v0.6.6.
4. **Optional PR**: request_id correlation + visual marking of
   operator-initiated turns. Small follow-up; not yet started.
5. **Cogo flip (task #75)**: cogo migrates from internal/tui to
   coretui directly. Independent of E; both benefit from LiveAgent
   once it lands.

## When to update this doc

- Before opening the upstream core-tui PR: turn the LiveAgent
  proposal sketch into a fully spec'd interface (handling of
  ctx cancel mid-iter, error semantics on transient stream
  failures, what happens when Events ends — does the TUI exit?).
- After landing PR E: collapse the "Proposed architecture"
  section into "Architecture (shipped)" with pointers to the
  actual interface + commit history.
