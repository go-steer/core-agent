# Event log — implementation decisions log

A running record of design choices made while implementing `docs/eventlog-plan.md`. Each phase gets a section. Decisions are recorded as they're made; the design doc itself is updated only when something material changes.

## Phase 1 — Plug-in `session.Service`

Status: shipped on `main` (commit pending).

### What landed

- **`agent.WithSessionService(s session.Service) Option`** — the seam for swapping the default in-memory session backend with a durable one (or a test stub).
- **`(*Agent).SessionService() session.Service` accessor** — returns whatever Service the agent is using.
- **Default behavior preserved** — `agent.New(model)` with no option still constructs a fresh `session.InMemoryService()` per call, identical to prior semantics.
- **`agent/agent_test.go`** — five tests covering: nil-model rejection (preserved), default service is non-nil and per-instance, override is passed through by object identity, `WithSessionService(nil)` falls back to default rather than panicking, option order independence.
- **Package doc comment update** in `agent/agent.go` mentions the override path.

### Decisions made (with reasoning)

**Stored the service on the `Agent` struct (with public accessor) rather than only inside the constructor closure.**
The accessor is needed by the upcoming Phase 2 work (callers of `eventlog.Open(...)` will need to query the service for prior events) and useful for tests today. The cost is a single extra pointer field on `Agent`. No downside.

**`WithSessionService(nil)` falls back to the default rather than erroring or panicking.**
ADK's other With* options on this package treat nil/empty as "ignore" — keeping the convention. Also avoids surprise when a caller wires a Service from a function that returns nil on some configuration path.

**Default factory call (`session.InMemoryService()`) stays inside `New`, not pre-computed in `defaultOptions()`.**
`session.InMemoryService()` returns a fresh instance per call. If we cached it in `defaultOptions()`, every Agent built without an override would share state — a real bug. Constructing inside `New` (only when `o.sessionService == nil`) keeps the per-call freshness contract.

**No new package, no CLI flags, no docs site changes in Phase 1.**
The plan deliberately splits these into Phase 2 alongside the `eventlog/` package itself. Phase 1 is the smallest shippable seam — the option exists, but nothing in the repo wires durability through it yet. Library callers can already use `WithSessionService` to plug `database.NewSessionService(...)` from ADK directly if they want a quick durable backend before Phase 2 lands.

**Did not extend the `options` struct's TODO comment** about subagents.
That marker stays for the Phase 4 work; touching it here would be churn.

### What did NOT land in Phase 1 (deliberately)

- The `eventlog/` package — Phase 2.
- `WithEventLog(*eventlog.Handle)` convenience — Phase 2 (depends on the package existing).
- CLI flags `--session-db` / `--session-db-path` — Phase 2.
- ADK GORM driver dependency in `go.mod` — Phase 2 (no consumer of `database.NewSessionService` yet on the agent path).
- README / DESIGN / library-api doc updates — Phase 2 (small Phase 1 surface doesn't yet warrant user-facing docs; the changelog and the option's godoc cover it).

### Verification

```bash
go test ./agent/...        # 5 new tests + the existing autonomous suite, all pass
go vet ./...               # clean
go build ./...             # clean
for s in dev/ci/presubmits/*; do bash "$s"; done   # all 7 green
```

---

## Phase 2 — Event log substrate (in flight, not started)

(Decisions will land here as work proceeds.)

## Phase 3 — RunAutonomous resume (not started)

## Phase 4 — Subagent runner refresh (not started)
