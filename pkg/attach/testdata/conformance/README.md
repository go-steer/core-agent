# SSE spec conformance fixtures

Canonical JSON shapes for events the SSE event-stream protocol
(see `core-tui/docs/sse-event-stream-protocol.md`) requires
producers to emit. Fixtures are consumed by:

- **`pkg/attach/capabilities_conformance_test.go`** — pins the
  wire format against the runtime types so a struct-tag rename
  or field reorder fails visibly.
- Downstream consumers (mast-web, core-tui) MAY mirror these
  fixtures into their own harness to verify a producer implements
  the spec correctly. The version stamp on each fixture identifies
  the minimum protocol version it targets.

## Fixture layout

Each event fixture is a single JSON document with the shape a
producer would emit on the SSE wire (i.e., the `data:` block,
decoded). Event fixture naming:
`<event-type>-<variant>-v<protocol-version>.json`. REST fixtures
follow their own convention — see "REST response fixtures" below.

| File | Event | Since |
|---|---|---|
| `capabilities-v1.4.0.json` | `capabilities` | 1.4.0 |
| `status-update-with-capabilities-v1.4.0.json` | `status-update` merge frame carrying an embedded `capabilities` hot-update | 1.4.0 |

## REST response fixtures

`rest-*.json` fixtures pin the JSON *response* shapes of the plain
HTTP endpoints the same way the event fixtures pin SSE frames
(core-agent#536 — mast-web's bundled mock invented snake_case field
names for the sessions list, its client was written against the
mock, and the drift had no fixture to fail against). They are
versioned independently of the SSE protocol: the `-v<N>` suffix is a
REST-shape version starting at 1, bumped on any wire-shape change,
with the old fixture kept frozen.

| File | Endpoint | Since |
|---|---|---|
| `rest-sessions-list-v1.json` | `GET /sessions` (the `{"sessions": [...]}` envelope; one `active` + one `idle` row) | v1 |
| `rest-create-session-v1.json` | `POST /sessions` → 201 body | v1 |
| `rest-whoami-v1.json` | `GET /whoami` (asserted-proxy variant — populates the `omitempty` fields) | v1 |
| `rest-subagent-events-v1.json` | `GET /sessions/{app}/{sid}/agents/{name}/events` (a truncated page — populates `next_since` + `truncated`) | v1 |

Pinned by `rest_conformance_test.go`; add new REST fixtures there
following the same construct-marshal-diff pattern (plus, where a
handler assembles its envelope inline, a live-handler key-set test —
see the sessions-list pair).

Timestamp fields (`last_touched_at`) are RFC 3339 with arbitrary
sub-second precision and zone offset — active rows carry the
daemon's local offset, persisted rows typically UTC. Parse them;
don't pattern-match a fixed layout off the fixture examples. The
zero value `0001-01-01T00:00:00Z` means "never touched", not year 1.

## Adding a new fixture

1. Add the file under `pkg/attach/testdata/conformance/` following
   the naming convention above.
2. Add (or extend) a test case in
   `pkg/attach/capabilities_conformance_test.go` that constructs
   the runtime type, marshals it, and diffs against the fixture
   using `canonicalizeJSON`.
3. Bump the fixture version stamp when the wire shape changes.
   Fixtures are frozen to their spec version — a v1.4.0 fixture
   must round-trip against a v1.4.0-speaking producer indefinitely.

## Why this lives here (and not in `core-tui`)

The sibling issue (`core-tui#…`) is landing the cross-repo harness
that will host the shared spec-adjacent fixtures. Until then, these
files live in-tree so the core-agent side has a place to pin the
wire format. When the shared harness lands, this directory becomes
the canonical source and downstream consumers mirror it.
