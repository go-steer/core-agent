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

Each fixture is a single JSON document with the shape a producer
would emit on the SSE wire (i.e., the `data:` block, decoded).
File naming: `<event-type>-<variant>-v<protocol-version>.json`.

| File | Event | Since |
|---|---|---|
| `capabilities-v1.4.0.json` | `capabilities` | 1.4.0 |
| `status-update-with-capabilities-v1.4.0.json` | `status-update` merge frame carrying an embedded `capabilities` hot-update | 1.4.0 |

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
