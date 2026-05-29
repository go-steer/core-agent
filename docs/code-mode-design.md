# Code Mode for core-agent: in-process Go execution (Phase 1)

Design doc for the first slice of "Code Mode" — a single
`execute_go(code string)` tool, backed by an embedded Go interpreter,
that lets the model write Go directly against the host process's
package graph instead of teaching it N per-service tool wrappers.
Untracked sibling to `docs/attach-mode-design.md`,
`docs/bidirectional-mcp-design.md`,
`docs/background-subagents-design.md`,
`docs/scion-harness-improvements-design.md`.

Targets a future minor release. Tag TBD; the scope is small enough
to land as the headline feature of one minor on its own.

## Context

Today, when a consumer wants their core-agent to touch a cloud
service, the available paths all scale poorly:

1. **Shell out via `bash`** to `gcloud`, `gsutil`, `bq`, `kubectl`,
   `aws`, etc. Works for one-offs; brittle for anything beyond
   trivia (text parsing, retry, JSON wrangling, version drift on
   the CLI tool, no structured error model).
2. **Hand-write a per-service tool wrapper** (`gcs_list_buckets`,
   `gcs_read_object`, `bigquery_run_query`, ...). Each wrapper is
   schema + handler + tests. The combinatorial explosion is real:
   GCP alone has ~120 services; AWS more. Doing this exhaustively
   would dwarf the rest of the tool suite.
3. **Stand up an MCP server per service.** Offloads the wrapper
   burden to whoever wrote the server, but multiplies the
   infra surface (server processes to manage, lifecycles to
   supervise, schemas to project, namespacing collisions). Still
   one wrapper per service in aggregate, just hidden behind
   `tools/list`.

None of these is an answer to "agent that can talk to *any* cloud
service." The shape we want is one tool, one substrate, one auth
story, with full coverage of whatever the underlying SDK exposes.

### The insight (Cloudflare's, borrowed)

Cloudflare published a piece called *Code Mode*
(https://blog.cloudflare.com/code-mode/) arguing — and showing —
that LLMs are far better at calling APIs by writing real code than
by emitting synthetic JSON tool calls. They're trained on enormous
volumes of real code in the wild. They have not been trained on
made-up JSON shapes invented for any one agent harness.

Cloudflare's implementation uses TypeScript executed inside V8
isolates with each MCP tool projected as a TypeScript function
callable from the script. The substrate (TypeScript + V8) is forced
by their Workers runtime.

Our substrate falls out differently:

- **core-agent is itself a Go library** that consumers embed in
  their own Go services.
- **Go has a mature pure-Go interpreter** (Yaegi —
  https://github.com/traefik/yaegi, Apache-2.0). No `go build` /
  `exec` round-trip, no toolchain at runtime, no container.
- **Yaegi's `Use(map[string]map[string]reflect.Value)` mechanism
  injects pre-compiled symbols** directly into the interpreter's
  namespace. Generated code can call them with zero JSON marshaling
  and zero schema projection.

### The differentiator (vs Cloudflare)

In Cloudflare's TypeScript Code Mode, every callable function the
model wants to reach has to be projected through V8 — and the
exposed surface is whatever MCP servers happen to be wired up.

In core-agent's Code Mode, the substrate language is the same
language the host is written in. A consumer's domain packages —
`accounts`, `billing`, `pricing.Compute(...)` — are first-class
symbols the model can call **by writing the exact same Go a
human engineer would write to use them**. No JSON wrapper. No
tool declaration. No schema. The model writes:

```go
acct, _ := accounts.Lookup(ctx, "cust-9182")
total, _ := pricing.Compute(ctx, acct, billing.Q1)
fmt.Printf("Q1 total for %s: $%.2f\n", acct.Name, total)
```

…and the host's compiled code runs, with the consumer's auth, the
consumer's types, the consumer's metrics middleware, on the same
heap as the rest of the process.

No other agent framework can do this because no other one is
*itself a Go library you embed in a Go service*. Python agent
frameworks have `eval()` but their host language isn't the
language the model is best at; TypeScript frameworks (Cloudflare
included) can't inject your Go packages. **Project-symbol-injection
is the moat.** The design treats it as a first-class concern, not
an afterthought.

### Settled decisions (do not relitigate — design around them)

- **Substrate: Yaegi.** Not `go build` + `exec` (hostile to
  distroless / scratch images, requires Go toolchain at runtime,
  adds compile-time latency to every call). Not WASM (the Google
  Cloud Go SDK doesn't trivially target wasm — it uses `net`,
  `crypto/tls`, and cgo-adjacent transports; building a wasm-safe
  subset is its own multi-quarter project). Not a containerized
  runner (kills the per-call latency budget for short scripts and
  pulls in container infrastructure we don't ship).
- **Tool surface: a single `execute_go(code string)` tool.** Returns
  stdout + a captured return value + structured errors. The code
  string is the model's prompt.
- **Default symbol pack: curated, deny-by-default for I/O.** Bundles
  a hand-picked safe subset of the Go standard library plus a
  starter set of GCP SDK packages. Explicit deny on `os/exec`,
  `syscall`, network primitives the SDK doesn't need, and the
  rougher edges of `os` (process-level mutation, `os.Remove`, etc.).
- **Project-symbol-injection seam:**
  `tools.NewGoExecutor(opts...)` accepts a
  `WithProjectSymbols(map[string]map[string]reflect.Value)` option
  so consumers `Use()` their own packages alongside the default
  pack. This is the moat.
- **Permission integration:** `execute_go` flows through the
  existing `permissions.Gate`. In `ask` mode the operator sees the
  full code body before approval. Per-tool allow/deny patterns
  work. The curated symbol map is the *primary* security boundary;
  the gate is the *secondary* (operator can refuse a specific
  call). We are honest about this throughout.

## Goals and non-goals

### Goals

- **Replace N per-service wrappers with one substrate.** Adding GCP
  Compute, BigQuery, or Pub/Sub coverage becomes "the model imports
  the relevant `cloud.google.com/go/<service>` package and writes
  the code" — no new tool, no new schema, no new release.
- **Make project-symbol-injection a first-class API.** A consumer
  who runs an embedded core-agent inside their Go service should be
  able to expose their domain packages with one option call. The
  Go the model writes against those symbols is the same Go the
  consumer's engineers write.
- **Inherit ADC auth for free.** Calls to `cloud.google.com/go/...`
  from inside the interpreter find Application Default Credentials
  the same way the host process does — because they're running in
  the host process. No service-account JSON shuffling, no token
  brokering layer.
- **Stay opt-in.** A consumer who doesn't want the tool simply
  doesn't add it. The Yaegi dep and the symbol packs are isolated
  behind a single new sub-package (`tools/gocode/`) consumers can
  ignore.
- **Match the existing tool ergonomics.** `execute_go` is just
  another `tool.Tool` returned from a constructor; it slots into
  `agent.WithTools(...)` and `tools.Build(...)` the way every other
  built-in does. The permission gate, the audit log, the eventlog
  recording infrastructure all work without special-casing.
- **Be honest about the security posture.** The default pack is the
  fence. Yaegi is not a sandbox. We document that clearly and
  design the defaults conservatively.

### Non-goals

- **Not a generalized sandbox.** Yaegi runs inside the host
  process. It does not isolate memory, capabilities, syscalls, or
  CPU. We curate symbols deny-by-default; that *is* the boundary.
  A consumer who wants gVisor-grade isolation runs `execute_go`'s
  caller in a separate process / VM. We don't pretend to offer
  what we don't.
- **Not full stdlib coverage.** `os/exec`, `syscall`, `net` (beyond
  what the GCP SDK transitively pulls), `unsafe`, `plugin`,
  `runtime/debug`'s mutating bits, `os.Remove`/`Rename`/`Chmod` —
  all explicitly deny-listed in the default pack. Consumers can
  add them back via `WithExtraSymbols(...)` but the load-bearing
  word is *can*: this is opt-in, not default.
- **Not persistent compiled artifacts.** Phase 1 is ephemeral. Each
  `execute_go` call constructs a fresh interpreter (or pulls one
  from a per-tool pool, see "Lifecycle"), runs once, drops the
  interpreter state. No "save this function and call it later."
  That's Phase 3 territory (tool synthesis with eval-gated
  promotion) and gated on the not-yet-built eval framework.
- **Not an MCP server extraction.** Phase 2 wraps the executor
  behind a standalone MCP server so non-core-agent hosts (Claude
  Desktop, gemini-cli, generic MCP clients) can use the same
  sandbox + symbol packs. Phase 1's code structure must make that
  extraction clean (see "Future work"); it does not ship the
  extraction itself.
- **Not a substitute for `bash` or the structured file tools.**
  `execute_go` shines for SDK calls and structured data
  manipulation. `bash` still wins for git, builds, OS-level glue,
  and any binary that doesn't have a Go SDK. The structured tools
  (`read_file`, `grep`, `edit_file`) still win for workspace edits
  — they're cheaper per call and have tighter output caps.
- **Not multi-step / stateful sessions across calls.** Each
  `execute_go` call is independent. Vars don't persist across
  invocations in Phase 1; the consumer (or the model) re-declares.
  See "Open questions" for the cross-call-state discussion.

## What gets exposed: the central decision

Three plausible tool surfaces, each with a different mental model.
**We pick A.**

### A. One tool: `execute_go(code string)` (the choice)

```jsonc
{
  "name": "execute_go",
  "description": "...",
  "inputSchema": {
    "type": "object",
    "properties": {
      "code":    {"type": "string", "description": "Go source body. Imports are auto-resolved from the available symbol pack. The body runs as the body of a generated `func main()` in package main; declare your own `func main()` to override."},
      "timeout_seconds": {"type": "integer", "description": "Wall-clock cap; default 30, max 300."}
    },
    "required": ["code"]
  }
}
```

- Single named action in `tools/list`. The host's model sees
  exactly one Code Mode tool regardless of how many SDK packages
  are bundled.
- Default symbol pack is implicit: the model doesn't have to
  declare which packages it wants; the executor pre-imports
  everything in the pack so the script can use any of them with
  no setup. (Yaegi's REPL behaves the same way.)
- The code body is exactly the Go a human would write at a REPL.
  No template, no boilerplate, no JSON wrapper. The model is
  generating in the language it's strongest at.

### B. One tool per package (`gcs_execute`, `bigquery_execute`, ...)

Rejected. This reintroduces the per-service wrapper problem we're
trying to escape, just at a coarser grain. Adding a new GCP
service would still mean a new tool, a new release, a new schema.

### C. Two tools: `execute_go(code)` + `list_available_symbols(prefix)`

Considered. The `list_available_symbols` companion would let the
model query "what cloud packages are available?" before writing
code. Rejected for v1 because:

- The default pack list is documented in
  `tools/gocode/symbols/README.md` and surfaced in the tool's
  description so the model knows what's available without an
  extra round-trip.
- Adding `list_available_symbols` later is purely additive — the
  same executor backs both — so deferring costs nothing.

The model is expected to discover the pack from the tool
description plus its training-time familiarity with the GCP Go
SDK. If that proves wrong in practice, the companion tool is a
short follow-up.

## Tool API surface

### Function signature seen by the model

```jsonc
// Tool declaration (returned in tools/list):
{
  "name": "execute_go",
  "description": "Execute a Go source snippet against the bundled \
package pack (Go stdlib subset + GCP SDK + the host's project \
packages). Stdout from the snippet is captured and returned. If the \
snippet evaluates to a value at top level (a `result := ...` \
declaration), that value is captured and returned alongside stdout. \
Available packages: see PACK_LIST in this description. Prefer this \
tool over `bash gcloud` or shelling out to per-service CLIs — it has \
typed return values, structured errors, and no subprocess overhead.",
  "inputSchema": { /* as above */ }
}
```

`PACK_LIST` is auto-generated at tool-construction time by
introspecting the assembled symbol map; the description ends with a
canonical alphabetical list of importable package paths. Cost: ~2KB
of description text at steady state (the GCP SDK has ~30 importable
packages in the default pack). The benefit: the model picks the
right import without guessing, and the description is always in
sync with what's actually exposed (auto-regenerated, never
hand-maintained).

### Argument and return-value JSON shape

The tool call:

```jsonc
{
  "method": "tools/call",
  "params": {
    "name": "execute_go",
    "arguments": {
      "code": "buckets, err := storage.NewClient(ctx).Buckets(ctx, \"my-project\")\nfor { b, err := buckets.Next(); if err == iterator.Done { break }; if err != nil { return err }; fmt.Println(b.Name) }",
      "timeout_seconds": 60
    }
  }
}
```

The response:

```jsonc
{
  "stdout":        "bucket-a\nbucket-b\nbucket-c\n",
  "result":        null,                              // no explicit `result :=` decl
  "duration_ms":   847,
  "imports_used":  ["cloud.google.com/go/storage", "fmt", "google.golang.org/api/iterator"],
  "truncated":     false
}
```

When the snippet declares `result := <expr>` at top level, the
executor captures it and serializes via `json.Marshal` with a
sensible fallback for unmarshalable types:

```jsonc
{
  "stdout":      "",
  "result":      {"buckets": ["bucket-a", "bucket-b", "bucket-c"], "count": 3},
  "duration_ms": 612,
  "imports_used": ["cloud.google.com/go/storage"],
  "truncated":   false
}
```

Both `stdout` and `result` may be truncated per the tool's
`MaxBytesPerField` config (defaults to 16 KiB each). When
truncation fires, `truncated: true` is set on the response and the
last 128 bytes of the truncated field are replaced with a marker
the model can recognize:

```
...
…[truncated; 4923 bytes elided, see `truncated: true`]
```

This is the same pattern the file tools' truncate helper uses
(`tools/truncate.go`); we reuse it verbatim.

### Error shape — designed for retry

When generated code fails to parse or evaluate, the response
returns a structured error that gives the model enough signal to
fix and retry. **Workflow design, not just plumbing.**

```jsonc
{
  "error": {
    "kind":     "parse",            // "parse" | "import" | "eval" | "timeout" | "denied" | "panic"
    "message":  "expected ';', found ')'",
    "line":     12,
    "column":   34,
    "snippet":  "  fmt.Println(b.Name)\n  for buckets.Next( {\n                    ^",
    "hint":     ""                   // optional; populated for common-mistake patterns
  }
}
```

Error shapes:

- **`parse`** — Yaegi's `interp.Eval` returns a `scanner.Error` or
  parse error. We unwrap it to extract `Line`/`Column` from
  `token.Position`, render a three-line snippet (the offending
  line plus one above/below) with a `^` caret under the column,
  and surface it. Model gets a compiler-style diagnostic identical
  in shape to what `go vet` or `gopls` would produce — material
  it's seen during training.
- **`import`** — the snippet referenced a package not in the
  assembled symbol map. Message format:
  `package "<path>" is not in the executor's symbol pack. Available
  packages: see tool description. To request a new package, the
  consumer must add it via WithExtraSymbols(...).` The last
  sentence is a deliberate breadcrumb for the operator reading
  the audit log — the *consumer*, not the model, controls the pack.
- **`eval`** — runtime panic or returned error from the snippet
  (the code parsed and started running but threw). The Yaegi
  panic's `String()` is captured; we render line numbers where
  Yaegi provides them (it does, in most cases). `stdout` is still
  returned alongside the error so the model can see the partial
  progress its code made before failing.
- **`timeout`** — wall-clock cap hit. Yaegi is cooperatively
  cancellable through context plumbing (`interp.ExecuteWithContext`);
  the goroutine running the script gets cancelled and we wait up
  to 5 seconds for it to unwind before returning. Message includes
  the cap and the elapsed time so the model can choose to retry
  with a larger `timeout_seconds`.
- **`denied`** — the permission gate refused the call. Message is
  the gate's standard refusal string. The audit log already
  captures the full code body separately, so the model just gets
  "permission denied for execute_go" without re-echoing.
- **`panic`** — the snippet caused Yaegi itself to panic (not the
  user code — a Yaegi-internal panic). Rare; we wrap every Eval
  in `defer recover()` to make sure these don't crash the host
  process. Message includes the recover'd value verbatim plus a
  note that this likely indicates a Yaegi bug worth reporting.

The error shape gives the model what it would see in a Go REPL
plus what it would see in a `go vet` run. Empirically these are the
diagnostics models retry against most successfully — they're
training-distribution shaped.

## Symbol-pack architecture

### Layered composition

```
                            ┌──────────────────────────────────────┐
                            │ WithExtraSymbols(...)                │ ← consumer-added stdlib
                            │   user-driven additions               │
                            └────────────────┬─────────────────────┘
                                             │
┌──────────────────────────────┐    ┌────────▼─────────┐
│ WithProjectSymbols(map)      │ ──▶│  assembledPack   │ ← what Yaegi.Use() actually sees
│   the MOAT — host packages   │    │  (the final map) │
└──────────────────────────────┘    └────────▲─────────┘
                                             │
                            ┌────────────────┴─────────────────────┐
                            │ tools/gocode/symbols/                │
                            │   stdlib_safe.go (curated stdlib)    │
                            │   gcp_pack.go    (GCP SDK starter)   │
                            │   deny.go        (explicit deny set) │
                            └──────────────────────────────────────┘
```

The executor assembles the final symbol map at construction time:

1. Start with the curated default stdlib pack
   (`symbols.StdlibSafe`).
2. Overlay the GCP SDK starter pack (`symbols.GCPDefault`) if
   `WithGCPDefaultPack()` is set (default: on; can be turned off
   with `WithoutGCPDefaultPack()` for consumers who don't want
   GCP code in their address space).
3. Overlay anything from `WithExtraSymbols(...)` (additional
   stdlib or third-party packages the consumer wants exposed).
4. Overlay `WithProjectSymbols(...)` (the consumer's own
   `internal/`-style packages, exposed under whatever import
   path the consumer chose). **Project symbols win over any
   collision with stdlib or GCP** — the consumer's namespace
   is authoritative.
5. Apply the deny list — any package path matching
   `WithDenyPackages(...)` or the built-in deny defaults
   (`os/exec`, `syscall`, `unsafe`, `plugin`, `runtime/debug`)
   is removed regardless of how it got into the assembled map.

The final map is `map[string]map[string]reflect.Value` (Yaegi's
shape) keyed by package import path, then by exported symbol name.

### Allow-list vs deny-list philosophy

**Default is allow-list, with deny as a final hard floor.** The
stdlib pack enumerates exactly which packages and which symbols
get exposed — we don't take all of `os` and remove the dangerous
bits, we take only the parts of `os` that are safe (`os.Getenv`,
`os.Stat`, `os.ReadFile`, time-related helpers) and omit the
rest. This is much safer than the inverse:

- New stdlib symbols added in future Go versions don't silently
  become available — they require a deliberate pack regen.
- Future stdlib *additions* to currently-allowed packages
  (`os.NewRiskyThing` shipping in Go 1.30) don't silently expand
  the surface area.
- The deny list catches anything that slipped through the
  allow-list (defense in depth) and lets consumers veto specific
  packages from the GCP pack or their own project pack.

The opposite policy (start with everything and remove dangerous
parts) is a foot-gun. We don't ship it.

### Default stdlib pack (proposal)

The curated `symbols.StdlibSafe` map exposes (full list, alphabetical):

```
bufio              context              crypto/sha256
encoding/base64    encoding/csv         encoding/hex
encoding/json      encoding/xml         errors
fmt                hash                 hash/fnv
io                 maps                 math
math/big           math/rand            net/url
path               path/filepath        regexp
slices             sort                 strconv
strings            sync                 sync/atomic
time               unicode              unicode/utf8
```

Notable omissions and why:

- **`os`** — `os.Getenv`/`Stat`/`ReadFile` are useful but the
  package as a whole includes `os.Remove`, `os.RemoveAll`,
  `os.Chmod`, `os.Setenv`, `os.Exit`, `os.StartProcess`. We
  cherry-pick *individual symbols* under the synthetic
  package path `"safeos"` (rebranded to make the curation
  obvious to the model), exposing only the read-only and
  diagnostic helpers. The `os` package proper is denied.
- **`os/exec`** — denied. Subprocess execution should go through
  the `bash` tool (with its denylist) or through a consumer-supplied
  helper, not through Code Mode.
- **`syscall`** — denied. Same reasoning, lower-level.
- **`net`** — denied at the top level. The GCP SDK transitively
  imports `net/http` for its transports; Yaegi resolves
  pre-compiled symbols at the package level, so the SDK's
  internal use of net works without `net` being exposed to the
  *script's* import namespace. The script can't `import "net"`
  and open arbitrary sockets, but the SDK can still make its
  authorized HTTPS calls. We confirm this in the test harness
  (a unit test attempts `net.Dial` from the script and asserts
  the import error).
- **`net/http`** — denied for the same reason as `net`. Script
  cannot construct its own HTTP clients; SDK packages that need
  HTTP get it through their internal symbol-pack entries.
- **`reflect`** — exposed (the GCP SDK uses it; scripts may want
  it for diagnostics). Note that Yaegi already documents minor
  reflect-representation differences from compiled mode.
- **`runtime`** — denied. Scripts don't need `runtime.GC()`,
  `SetFinalizer`, `Goexit`, etc.
- **`unsafe`** — denied. (Yaegi's stdlib already excludes it.)
- **`plugin`** — denied. Loading native plugins would bypass the
  whole sandbox.
- **`go/...`** (`go/ast`, `go/parser`, etc.) — omitted from the
  default pack but easy to add via `WithExtraSymbols` for
  consumers building meta-tooling. Not safety-relevant; just not
  load-bearing for the GCP use case.
- **`text/template`** / **`html/template`** — omitted from the
  default pack; arbitrary template execution adds complexity for
  no clear Code Mode win. Add via `WithExtraSymbols` if needed.

The pack lives in `tools/gocode/symbols/stdlib_safe.go` as a
generated file. The header explicitly notes "DO NOT EDIT BY HAND
— regenerated by tools/gocode/symbols/gen/main.go".

### Default GCP SDK pack (proposal)

The starter `symbols.GCPDefault` map exposes:

```
cloud.google.com/go/bigquery
cloud.google.com/go/compute/apiv1     # compute/v1 in the doc shorthand
cloud.google.com/go/firestore
cloud.google.com/go/iam
cloud.google.com/go/pubsub
cloud.google.com/go/run/apiv2
cloud.google.com/go/secretmanager
cloud.google.com/go/storage
google.golang.org/api/iterator        # iterator.Done, used by every paginated call
```

Selection criteria:

- The services consumers most often want first (compute,
  storage, pubsub, secrets, IAM, BigQuery).
- The shapes the model is most familiar with from training
  (these packages have public examples, blog posts, godoc, and
  course material the GCP team produced over years).
- Tractable to keep in sync — these are all in the
  `cloud.google.com/go` mono-module, which means one
  go.mod entry covers the lot.

Out of the starter pack but a single `WithExtraSymbols(...)`
call away: Datastore, Spanner, Cloud SQL Admin, Cloud
Functions, Cloud Build, Cloud Tasks, Cloud Scheduler, KMS,
Logging, Monitoring, Trace, Translate, Vision, Speech, AutoML,
AI Platform, Dataflow, Dataproc, Dialogflow, Healthcare,
Recommender, ResourceManager, ServiceUsage, Workflows, plus the
non-cloud `google.golang.org/api/...` v1 surfaces. The
maintenance burden of adding any of these is one symbol-pack
regen plus a Go module update; the *consumer* burden is one
option call.

### How a consumer adds a new SDK package

The README for `tools/gocode/symbols/` walks the consumer
through:

1. `go get cloud.google.com/go/datastore@latest` in your project.
2. `go run github.com/traefik/yaegi/cmd/yaegi extract cloud.google.com/go/datastore` — generates a symbol-map `.go`
   file in the current directory.
3. `tools.NewGoExecutor(tools.WithExtraSymbols(yourDatastoreSymbols))`
   — the consumer pulls in the generated file and feeds it as a
   `map[string]map[string]reflect.Value`.

No core-agent release required. The consumer's package becomes
available to the model the next time the executor is constructed.

### How the bundle is versioned

The default packs are pinned in `tools/gocode/symbols/go.mod`
(separate go.mod from core-agent's main one, so the SDK version
churn doesn't infect the rest of the library's dep graph). Each
core-agent release records the GCP SDK versions baked into the
default pack in `CHANGELOG.md` under the Code Mode section. Bumping
the GCP SDK happens on a regular cadence (see "Symbol-pack
maintenance plan" below) and is its own minor release entry.

The pack version is also surfaced through a `symbols.Version`
constant in the generated stdlib file and an
`gcp.SDKVersions` map in the generated GCP pack file, so the audit
log can record "ran against Go 1.27.3 stdlib pack, GCP SDK
storage@v1.46.0" if a debugging session needs that level of
precision later.

## Symbol-pack maintenance plan

The honest answer is "Yaegi's `extract` tool plus disciplined
regen." Phased automation:

### Phase 1a — manual regen with a documented procedure

`tools/gocode/symbols/gen/main.go` is a thin Go program that runs
the Yaegi `extract` command against a hand-edited list of
packages and writes the resulting files. The procedure to update
the stdlib pack:

```bash
cd tools/gocode/symbols
go run ./gen --stdlib    # regenerates stdlib_safe.go
go run ./gen --gcp       # regenerates gcp_pack.go
go test ./...            # ensures the new pack still passes smoke tests
git diff                 # human reviews the symbol surface delta
```

The committed pack files are the source of truth at build time —
we don't run `extract` during normal build. (Yaegi's extract
requires `go/types` introspection and the target packages
installed; running it at build would couple the library build to
the GCP SDK release cadence in a way that breaks reproducibility.)

### Phase 1b — Go version bump procedure

When the host project bumps Go (currently 1.26 minimum), the
stdlib pack needs regenerating because new packages may exist,
old ones may have gained or lost exported symbols. Procedure:

1. Update `go.mod` to the new Go version.
2. Re-run `go run ./gen --stdlib`.
3. Diff the regenerated `stdlib_safe.go` and review for:
   - New packages — should they be added to the allow-list?
     (Probably not by default; conservative addition only.)
   - New symbols on existing packages — almost always fine to
     accept; they're additive surface.
   - Removed symbols (rare) — note in the CHANGELOG.
4. Run the smoke tests; commit.

### Phase 1c — CI cadence for GCP SDK

A new GitHub Actions job, `.github/workflows/symbol-pack-bump.yml`,
runs weekly and:

1. Checks the latest released versions of each GCP SDK package
   pinned in `tools/gocode/symbols/go.mod`.
2. If any are out of date, opens a PR that bumps the
   `tools/gocode/symbols/go.mod`, regenerates `gcp_pack.go`, and
   tags a designated reviewer for surface-area review.
3. The PR's body lists the symbol delta (added / removed exported
   symbols per package) so the reviewer doesn't have to diff the
   generated file by hand.

This is *not* an auto-merge job. New SDK symbols can introduce
deny-worthy capabilities (a hypothetical
`storage.Client.AllowAllUsersAccess(...)` could turn an existing
script into a security incident), so a human eye is the safety net.

### Phase 1d — what's bundled by default vs opt-in

| Pack          | Default? | Toggle                       |
|---------------|----------|------------------------------|
| Stdlib safe   | Yes      | `WithoutStdlibSafe()` to remove (rare; you'd need something else with the same name) |
| GCP starter   | Yes      | `WithoutGCPDefaultPack()` to remove (consumer's app has no GCP) |
| Project syms  | Off      | `WithProjectSymbols(...)` to add (the moat) |
| Extras        | Off      | `WithExtraSymbols(...)` to add (other SDKs, third-party libs) |

Consumers running an embedded core-agent inside a non-GCP host —
say, an AWS service or an on-prem app — will commonly disable the
GCP default pack and add their own (`WithExtraSymbols(awsSymbols)`)
or just use project symbols. The default pulls GCP in because
that's the most common case for our existing consumers; nothing
about the design requires it.

## Sandbox / permission model

**Yaegi inherits the host process's permissions.** The OS-level
view is: `core-agent` (or the embedding consumer's binary) calls
`storage.NewClient(ctx)`, with whatever ADC the process holds, on
behalf of the model. The interpreter is not a sandbox in the kernel
sense. It is a *symbol-namespace fence*.

This must be stated plainly. We do not hedge it. Code Mode's
security posture is:

1. **Primary boundary: the curated symbol map.** The script can
   only call functions whose `reflect.Value` was added to the
   assembled pack. It cannot:
   - `import "os/exec"` and `exec.Command` (the import errors at
     parse time with `package "os/exec" is not in the symbol
     pack`).
   - Reach into the host process's globals (Yaegi's namespace is
     separate; only `Use()`-injected symbols are visible).
   - Use `unsafe` to forge pointers (`unsafe` is denied; and even
     if it weren't, Yaegi's interpreter doesn't honor `unsafe` the
     way the compiler does — pointer arithmetic doesn't work).
2. **Secondary boundary: the permission gate.** Every
   `execute_go` call goes through `gate.CheckGeneric(ctx,
   "execute_go", summary)`. In `ask` mode, the operator sees the
   full code body and approves or refuses each call. In `allow`
   mode, the operator allowlists specific summaries (rare;
   allowlist-by-script-content is a footgun, so the more common
   pattern is denylist-by-substring — `--deny-tool="execute_go:*storage.Client.DeleteBucket*"`).
3. **Tertiary boundary: per-call constraints.** Wall-clock
   timeout (default 30s, max 5min), max code size
   (`WithMaxCodeBytes`, default 32 KiB), max stdout/result size,
   panic recovery so a bad script can't crash the host.

What this is *not*:

- Not memory isolation. A runaway allocation in a script can
  OOM the host process. The `WithTimeout` cancellation triggers
  before most runaway-allocation cases mature, but a script that
  allocates 100GB inside the first second is going to take the
  host down. Operators concerned about this run the host in a
  cgroup-memory-capped container, the same way they should
  already be doing for any non-trivial agent process.
- Not CPU isolation. A tight loop that ignores context
  cancellation (Yaegi cooperatively checks ctx between
  instructions; non-blocking infinite loops do get interrupted)
  can pin a goroutine. The wall-clock timeout is the safety net.
- Not goroutine isolation. A script that spawns goroutines
  (`go func() { ... }()`) creates real OS-scheduled goroutines
  on the host. They die when the script's context is cancelled
  (if they honor ctx); if they leak, they leak the host process.
  We document this and recommend scripts avoid `go` for Phase 1.
  A future hardening could intercept `go` statements at the AST
  level and refuse them; not in scope here.

### What happens when generated code tries to import a denied package

Yaegi returns an error from `Eval`:

```
1:8: import "os/exec" error: undefined: os/exec
```

We catch this in the error classifier, recognize it as an import
failure against the assembled pack, and surface it as the `import`
error shape described earlier. The model sees:

```
package "os/exec" is not in the executor's symbol pack.
Available packages: bufio, context, ... (see tool description).
To request a new package, the consumer must add it via
WithExtraSymbols(...).
```

The last sentence is the model's signal that *the consumer*, not
the model, controls the surface. The model should adapt by trying
a different approach (e.g. "I'll use the `bash` tool for this
since os/exec isn't available in Code Mode") rather than pestering
the operator.

### What happens when the gate refuses

The gate's `CheckGeneric` returns an error; the tool returns the
`denied` error shape. The audit log still records the full
attempted code body (see "Audit-log integration" below) so the
operator has the forensic trail of what was *asked*, distinct from
what was *allowed*.

## Error feedback loop

The shape of the error response was sketched under "Tool API
surface"; the *workflow* matters as much as the shape.

### Compiler-style diagnostics

Models trained on Go code have seen millions of `gopls` and
`go vet` outputs. Our error format mimics them:

```
12:34: expected ';', found ')'
  10:   buckets := storage.NewClient(ctx).Buckets(ctx, "my-project")
  11:   for {
> 12:     b, err := buckets.Next(; if err != nil { return err }
                                ^
  13:     fmt.Println(b.Name)
  14:   }
```

The caret-under-column is what compilers do; it's training-
distribution shaped. The model retries against it the same way a
human iterates against `go run`.

### Round-trip example

Model emits:

```go
client, err := storage.NewClient(ctx)
if err != nil { return err }
buckets := client.Buckets(ctx, "wrong-project-id-let-me-see")
for {
    b, err := buckets.Next()
    if err == iterator.Done { break }
    fmt.Println(b.Name)
}
```

Executor runs, gets a runtime error from the SDK:

```jsonc
{
  "stdout": "",
  "error": {
    "kind":    "eval",
    "message": "googleapi: Error 403: Caller does not have permission to act as project wrong-project-id-let-me-see, forbidden",
    "line":    4,
    "snippet": "  buckets := client.Buckets(ctx, \"wrong-project-id-let-me-see\")\n                                  ^"
  }
}
```

Model retries with the actual project ID, the call succeeds.
This is the loop that matters. The error is structured enough
for the model to identify the failure point (line 4, the project
ID arg) and unstructured enough (the underlying SDK error text)
that the model can read what the cloud said and react.

### Hint heuristics

For common mistakes we recognize, the executor populates the
`hint` field:

| Detected pattern                                | Hint                                                                 |
|-------------------------------------------------|----------------------------------------------------------------------|
| `googleapi: Error 401`                          | `auth: check that GOOGLE_APPLICATION_CREDENTIALS is set on the host` |
| `googleapi: Error 403`                          | (no hint — model usually figures out IAM from the message)           |
| `context deadline exceeded`                     | `script exceeded the timeout cap; consider raising timeout_seconds`  |
| `cannot use ... (type) as type ...`             | `Go type mismatch — see line N`                                      |
| `undefined: <symbol>` where symbol is in a known package | `did you forget to import "<package>"? Available packages: ...` |

These are cheap pattern matches; we ship a small starter set and
add to it as real failure modes emerge in production.

## Streaming and long-running calls

Some Go SDK calls are streaming or long-running by nature: Pub/Sub
`Subscription.Receive(...)` blocks indefinitely; BigQuery query
result iteration may page through millions of rows; Cloud Storage
object reads can be megabyte-scale.

**Phase 1 constraint: synchronous calls with a wall-clock budget.**
The contract is:

- The snippet runs in a goroutine wrapped in a
  `context.WithTimeout`. Default cap 30s; configurable up to 5min
  via `timeout_seconds` argument and `WithMaxTimeout` option.
- The injected `ctx` (a top-level variable in the script's
  namespace) carries the timeout. SDK calls that honor ctx — all
  modern GCP SDK calls do — cancel cleanly when the cap hits.
- The error shape on timeout is `kind: timeout` with the elapsed
  duration so the model knows whether to retry with a larger
  budget or break the work into chunks.

**What's explicitly out of scope for Phase 1:**

- Streaming responses to the model. The tool result is one JSON
  blob at the end; we don't dribble stdout to the model as it
  generates. (Adding this means rethinking how `functiontool`
  returns intermediate values to the agent loop. Possible later;
  not v1.)
- `Subscription.Receive` style indefinite blocking. Scripts that
  call it will hit the timeout and return; the model learns to
  use a finite-budget call instead (`subscription.Pull` for one
  message at a time, with explicit ack).
- Long-running operations (`compute.Operation.Wait` for a VM
  create). Scripts can call `Wait` with the script's ctx; if the
  operation outlasts the timeout, the script returns and the
  operation continues running on the cloud side. The model can
  poll for completion on a subsequent `execute_go` call with the
  operation's metadata it already captured.

The constraint is documented in the tool description so the model
knows the operational shape: "this tool runs a Go script with a
wall-clock cap. Use it for one-shot operations and short
iterations; for long-running operations, kick off the work and
poll for completion in a follow-up call."

## Result serialization

### `stdout` capture

The script's `os.Stdout` (and `fmt.Println` etc.) are redirected
to an in-memory buffer at executor construction time. After the
script returns, the buffer's contents become the response's
`stdout` field. Capped per `MaxBytesPerField` (default 16 KiB)
with truncation marker as described earlier.

### `result` capture

If the script declares `result := <expr>` at top level (i.e. the
identifier `result` is in scope after the script finishes), the
executor reads it back via Yaegi's `interp.Eval("result")` and
serializes it.

Serialization preference order:

1. `json.Marshal(result)` — works for most struct, map, slice,
   primitive return values. Most GCP SDK return types are
   JSON-friendly (proto-derived structs with json tags).
2. If `json.Marshal` errors (e.g. a value with cycles or a
   channel), fall back to `fmt.Sprintf("%+v", result)` and surface
   the result as a string under `result_text` instead of
   `result`. The model gets readable output even for unmarshalable
   types.
3. If both fail, omit `result` entirely and surface a warning in a
   new `warnings: []string` field on the response.

### Pretty-printing

`json.Marshal` produces compact JSON by default. We use
`json.MarshalIndent(..., "", "  ")` for `result` since the model is
better at reading it. `stdout` is preserved verbatim — the
script's print statements decide their own formatting.

### Truncation strategy

Same `tools/truncate.go` helper the file tools use, configured
per-field. The truncation point is the last well-formed JSON token
boundary when truncating `result` (so the model gets parseable
JSON even when truncated); arbitrary byte boundary for `stdout`
(which is freeform text anyway).

For very large results (full bucket listings, BigQuery row sets),
the right pattern is for the script to truncate / filter
client-side before assigning to `result`. The tool's description
nudges the model toward this: "If you expect a large result,
filter or aggregate in the script before assigning to `result`."

## Library API

```go
// tools/gocode/executor.go (sketch)

package gocode

import (
    "reflect"
    "time"

    "google.golang.org/adk/tool"

    "github.com/go-steer/core-agent/pkg/permissions"
    "github.com/go-steer/core-agent/pkg/tools/gocode/symbols"
)

// NewGoExecutor returns an ADK tool that runs Go snippets through
// an embedded Yaegi interpreter. The returned tool is registered
// like any other built-in: agent.WithTools([]tool.Tool{exec, ...}).
//
// gate must be non-nil; the tool refuses to construct otherwise.
// (Same rule as tools.Build.)
func NewGoExecutor(gate *permissions.Gate, opts ...Option) (tool.Tool, error) { /* ... */ }

type Option func(*options)

// WithProjectSymbols injects the consumer's domain packages into
// the interpreter's symbol map. THIS IS THE MOAT. Generated code
// can call these packages by their import paths exactly as the
// host's compiled code would.
//
// Each map key is the import path the model uses; each inner map
// is the package's exported symbols as reflect.Values. Use the
// Yaegi extract tool to generate these maps from your packages:
//
//   go run github.com/traefik/yaegi/cmd/yaegi extract ./internal/billing
//
// Project symbols win over any collision with stdlib or GCP
// packs — the consumer's namespace is authoritative.
func WithProjectSymbols(syms map[string]map[string]reflect.Value) Option { /* ... */ }

// WithExtraSymbols adds packages to the assembled symbol map on
// top of the curated defaults. Use for additional SDK packages
// (Datastore, KMS, AWS, etc.) or for stdlib packages we omitted
// from the safe pack.
func WithExtraSymbols(syms map[string]map[string]reflect.Value) Option { /* ... */ }

// WithDenyPackages removes the named packages from the assembled
// symbol map. Applies after all other Withs. Use to strip
// packages out of a default pack that this consumer specifically
// doesn't want exposed.
func WithDenyPackages(paths ...string) Option { /* ... */ }

// WithoutGCPDefaultPack omits the GCP starter pack from the
// assembled symbol map. Use when the consumer's host has no GCP
// dependencies and doesn't want them in the binary.
func WithoutGCPDefaultPack() Option { /* ... */ }

// WithMaxCodeBytes caps the size of the code argument the model
// can pass in one call. Default 32 KiB; max 256 KiB.
func WithMaxCodeBytes(n int) Option { /* ... */ }

// WithTimeout sets the default wall-clock cap for snippet
// execution. Default 30s. The model can override per-call up to
// WithMaxTimeout (default 5min).
func WithTimeout(d time.Duration) Option { /* ... */ }

// WithMaxTimeout sets the upper bound the model is allowed to
// request via timeout_seconds. Default 5min.
func WithMaxTimeout(d time.Duration) Option { /* ... */ }

// WithMaxBytesPerField caps stdout and result serialization at
// this many bytes per field. Default 16 KiB.
func WithMaxBytesPerField(n int) Option { /* ... */ }

// WithToolName overrides the tool name (default "execute_go").
// Useful when a consumer wants to expose multiple Code Mode
// instances with different symbol packs (e.g. "execute_go_safe"
// vs "execute_go_full").
func WithToolName(name string) Option { /* ... */ }
```

### Wiring into `tools.Build`

Code Mode is not added to the `tools.BuiltinTools` struct (which
governs the default suite) because:

1. **Different ergonomics.** It takes options the built-in suite
   doesn't (symbol packs).
2. **Different dep graph.** Yaegi pulls in a non-trivial graph
   (~50 transitive deps for the interpreter, plus whatever the
   GCP SDK adds). Consumers who don't want Code Mode shouldn't
   pay for the import cost.
3. **Different lifecycle.** `tools.Build` returns a `Registry`
   built from a static spec; Code Mode is constructed
   imperatively with options.

Instead, the consumer constructs it separately and adds it
alongside the built-in suite:

```go
gate := permissions.New(...)
reg, _ := tools.Build(cfg, gate, tools.Default())
codeMode, _ := gocode.NewGoExecutor(gate,
    gocode.WithProjectSymbols(billingSymbols),
    gocode.WithExtraSymbols(datastoreSymbols),
)
tools := append(reg.Tools, codeMode)
a, _ := agent.New(model, agent.WithTools(tools))
```

This mirrors how subagent tools, lifecycle tools, and ask_user
tools are wired today — constructed standalone, appended to the
slice.

### Where the code lives

```
tools/
  gocode/
    executor.go         # NewGoExecutor + Option, the public API
    executor_test.go
    eval.go             # Yaegi wrapping, ctx + stdout plumbing, panic recovery
    eval_test.go
    classify.go         # error shape: parse/import/eval/timeout/denied/panic
    classify_test.go
    truncate.go         # delegates to tools.truncate but with code-mode caps
    truncate_test.go
    symbols/
      stdlib_safe.go    # generated; curated stdlib subset
      gcp_pack.go       # generated; GCP SDK starter
      deny.go           # built-in deny defaults
      doc.go            # package doc + regen instructions
      gen/
        main.go         # tool to regenerate stdlib_safe.go and gcp_pack.go
      go.mod            # separate module so SDK churn is isolated
      go.sum
    examples_test.go    # godoc-renderable usage examples
```

The `tools/gocode/` subpackage convention follows
`tools/gocode/symbols/` being its own module; the rest of
core-agent doesn't import the symbol packs, only the executor.
This keeps the main `go.mod` clean of the GCP SDK's dep graph for
consumers who turn Code Mode off.

## Audit-log integration

Each `execute_go` call appears in the eventlog as a tool call
event the same way bash and the file tools do. The arguments
(crucially: the full code body) and the response (stdout, result,
errors) are persisted under the standard `tool` author with
`Content.Parts[].FunctionCall.Args` and
`Content.Parts[].FunctionResponse.Response`.

### What gets stored, what gets displayed

- **Stored verbatim:** the full code body, the full stdout, the
  full result (subject to the tool's own truncation caps, not an
  additional audit-log cap). The audit log is the forensic
  source of truth; truncating it would defeat the purpose.
- **Displayed in the chat-style stream:** the first 8 lines of
  the code body for `→ execute_go(code: ...)` formatting, with a
  `(... N more lines)` suffix when truncated. Full body is
  always visible in the `ask` mode prompt before approval. The
  display truncation is a cosmetic choice (long code blocks
  spam the terminal); the underlying audit row is intact.

### `ask` mode UX

Operator sees the full code body in the prompt:

```
core-agent (permissions): execute_go wants to run:
─────────────────────────────────────────────────────────
client, err := storage.NewClient(ctx)
if err != nil {
    return err
}
defer client.Close()

it := client.Buckets(ctx, "my-project")
for {
    b, err := it.Next()
    if err == iterator.Done { break }
    if err != nil { return err }
    fmt.Println(b.Name)
}
─────────────────────────────────────────────────────────
(y/s/t/a/N)
```

The full body is what makes `ask` mode usable for Code Mode. The
operator's mental model is "I'm approving this *specific* script",
not "I'm approving the abstract `execute_go` tool." Allow-session
(`s`) and allow-session-tool (`t`) decisions still work but
operators should be more reluctant to grant them than for the
file tools — a session-wide approval means subsequent
`execute_go` calls aren't reviewed for the rest of the run. The
prompter's existing wording for `t` ("trust this tool for the
rest of the session") gets a Code-Mode-specific addendum:
"Approving session-tool for execute_go means any future Go
snippet runs without review for the rest of this session."

### Recording infrastructure interaction

The `recording/` package wraps the LLM provider, not the tool
calls themselves; recordings replay the *model's* output
(including `execute_go` tool calls) against a scripted provider.
Tool execution at replay time still goes through the live
`execute_go` tool, which still runs the real Yaegi interpreter
against the live (or scripted) symbol pack.

The implication for hermetic testing: if a recording's
`execute_go` calls reach a real GCP API, replaying it later will
either re-hit the API or fail. The recommended pattern for
hermetic Code Mode tests is to use a mock symbol pack — the test
sets up `WithProjectSymbols(stubSymbols)` where `stubSymbols`
provides packages with the same import paths as the real ones
but with handler functions that return fixed responses. This is
the same pattern Go tests already use for stubbing internal
packages; the recording layer doesn't need to know.

(A future hardening could record tool outputs alongside LLM
turns to make replays fully hermetic without per-test symbol
stubbing. Tracked under the existing "tool environment isn't
recorded" caveat in `DESIGN.md`; not Code-Mode-specific.)

## Token/cost implications

Code Mode shifts work into the model's output channel. A
500-token Go snippet is comparable in cost to several JSON tool
calls but accomplishes substantially more per call. Notes:

- **Snippets tend to have a stable prefix.** "The model imports
  the same packages every time, declares the same `ctx`, opens
  the same client" — the script preamble is highly cacheable.
  Provider-side prompt caching (Gemini's implicit, Anthropic's
  `cache_control`) gives outsized wins here. We document the
  pattern and recommend consumers enable
  `models/anthropic.WithCacheSystem(true)` and equivalent for
  Code Mode-heavy workloads.
- **Audit log code bodies are larger than typical tool args.**
  A `bash` call's args might be 50 bytes; an `execute_go` call
  could be 2 KiB. The eventlog row size grows accordingly. For
  consumers using SQLite, this is fine (rows can be MB-scale).
  For Postgres / MySQL deployments where row size matters for
  index performance, consumers should monitor table size and
  consider pruning old sessions.
- **Stdout / result sizes are bounded** by the tool's truncation
  caps and the audit log inherits those caps. A
  10-bucket-list response is on the order of 1 KiB; a
  1000-row BigQuery dump should be filtered client-side in the
  script before being assigned to `result`.

The cost story is net-positive: replacing 10 round-tripped per-
service tool calls with one Code Mode call usually saves tokens
end-to-end. Direct measurement on representative workflows is a
follow-up after first deployment; we don't have it yet.

## Testing strategy

The test pyramid for Code Mode:

### Unit — `tools/gocode/*_test.go`

Yaegi can be tested directly with hand-written Go programs. No
LLM in the loop, no GCP creds needed. Coverage:

- **Symbol-pack assembly** — given a sequence of options
  (defaults, project, extras, deny), the assembled map is what
  we expect. Collision behavior (project wins over GCP wins over
  stdlib). Deny applies after merge.
- **Parse-error classification** — feed snippets with known
  parse errors; assert the `parse` error shape with correct
  line/column/snippet.
- **Import-error classification** — script imports `"os/exec"`;
  assert the `import` error shape with the deny breadcrumb.
- **Eval-error classification** — script returns an error; the
  error message round-trips; stdout up to the failure point is
  captured.
- **Timeout** — script with `for {}` hits the 1-second test
  timeout; `kind: timeout` returned with elapsed > 1s.
- **Panic recovery** — script `panic("oops")`; `kind: panic`
  with the recovered value; the host's test process survives.
- **Result serialization** — `result := struct{X int}{42}`
  round-trips as JSON; cycle case falls back to `result_text`;
  channel case surfaces a warning.
- **Truncation** — stdout > cap is truncated with marker;
  result > cap is truncated at JSON token boundary.

### Integration — `tools/gocode/integration_test.go`

Real `*permissions.Gate` (in yolo mode), real `*agent.Agent`
constructed against `models/mock/echo`. The echo provider
echoes the user's prompt back, including any tool calls in it,
so we can script a "model emits this `execute_go` call → tool
runs → response shape matches" round trip without burning API
quota.

The model-call simulation uses `models/mock/scripted` for richer
flows (model writes code, gets error back, retries with fix).
Fixture JSONL transcripts in `tools/gocode/testdata/` capture
the back-and-forth.

### Mock symbol pack for hermetic tests

A reusable `tools/gocode/symbols/testpack/` exposes a fake
`example.com/billing` package with deterministic responses. Tests
that want to exercise the "model writes code against a project
package" path use this pack instead of real GCP. Lets the test
assert on what the script called and with what arguments,
without any network or GCP credential.

### End-to-end smoke

`dev/smoke/08-code-mode.sh` (idempotent, GCP-credentialed):

1. Builds the binary.
2. Runs `core-agent --provider=gemini --yolo -p "use execute_go
   to list the first 3 buckets in project $TEST_GCP_PROJECT and
   print them"`.
3. Asserts the output contains the expected bucket names (from a
   pre-seeded fixture project).

The smoke is GCP-gated (requires ADC + a test project); CI
doesn't run it without those secrets. Local devs with GCP creds
run it on demand.

## Operator UX walkthroughs

### `ask` mode — review before approval

```
> count my GCP buckets

→ execute_go (awaiting approval)
─────────────────────────────────────────────────────────
buckets, err := storage.NewClient(ctx).Buckets(ctx, "my-project")
if err != nil { return err }
count := 0
for {
    if _, err := buckets.Next(); err == iterator.Done { break }
    if err != nil { return err }
    count++
}
result := count
─────────────────────────────────────────────────────────
(y) once  (s) session  (t) session-tool  (a) always  (N) refuse: y

← execute_go (843ms)
  result: 17

You have 17 buckets in my-project.
>
```

The operator reads the script before approving. The tool's
result-display ("17") is what the model used to construct its
final answer.

### `yolo` mode — autonomous run

```
core-agent --provider=vertex --yolo --session-db -p "audit my GCP \
storage for buckets without lifecycle policies"

→ execute_go (1.2s)
  result: [{"name": "logs-archive", "policies": []}, ...]
→ execute_go (912ms)
  result: 3 buckets are missing lifecycle policies. Here they are:
- logs-archive (created 2024-01-15)
- temp-uploads (created 2023-07-04)
- migration-backup (created 2022-11-30)
```

No prompt; the model iterates, calls Code Mode multiple times,
synthesizes the answer. The full code bodies live in the audit
log for forensic review.

### Debugging walkthrough — model fixes its own bad code

```
> list the first 5 BigQuery datasets in project foo

→ execute_go (failed: import)
  error: package "cloud.google.com/go/bigquery/v2" is not in the
         executor's symbol pack. Available packages: bigquery,
         compute, firestore, ... To request a new package, the
         consumer must add it via WithExtraSymbols(...).

→ execute_go (1.4s)
  // model retries with the correct import path
  result: ["dataset-a", "dataset-b", "dataset-c"]

The first 3 datasets in project foo are: dataset-a, dataset-b,
dataset-c. (There are only 3 in this project.)
```

The model adapts to the import error in one shot — the breadcrumb
in the error message ("Available packages: ...") gave it enough
signal to pick the right import on the retry.

## Critical files

**New:**

- `tools/gocode/executor.go` — public API: `NewGoExecutor`,
  `Option`, all the `With*` constructors
- `tools/gocode/executor_test.go`
- `tools/gocode/eval.go` — Yaegi interpreter wrapping, ctx +
  stdout plumbing, panic recovery, timeout
- `tools/gocode/eval_test.go`
- `tools/gocode/classify.go` — error shape classification
- `tools/gocode/classify_test.go`
- `tools/gocode/truncate.go`
- `tools/gocode/truncate_test.go`
- `tools/gocode/integration_test.go` — real gate + agent +
  scripted provider
- `tools/gocode/symbols/doc.go` — package doc + regen procedure
- `tools/gocode/symbols/deny.go` — built-in deny defaults
- `tools/gocode/symbols/stdlib_safe.go` (generated) — curated
  stdlib pack
- `tools/gocode/symbols/gcp_pack.go` (generated) — GCP SDK
  starter pack
- `tools/gocode/symbols/gen/main.go` — pack regeneration tool
- `tools/gocode/symbols/go.mod`, `go.sum` — isolated module for
  pack deps
- `tools/gocode/symbols/testpack/` — mock pack for hermetic tests
- `dev/smoke/08-code-mode.sh` — GCP-credentialed smoke
- `examples/code-mode/main.go` — minimal end-to-end example with
  `WithProjectSymbols`
- `.github/workflows/symbol-pack-bump.yml` — weekly GCP SDK
  freshness check

**Modified:**

- `CHANGELOG.md` — feature entry under the new version
- `README.md` — feature bullet in the built-ins / tool section
- `docs/DESIGN.md` — short Code Mode section cross-linking to
  this design doc (the *why*; the *how* lives here)
- `docs/site/content/docs/library-api.md` — Library API
  reference for the new `tools/gocode/` package
- `docs/site/content/docs/permissions.md` — note on Code Mode's
  permission posture and the `ask` mode UX

Nothing in `agent/`, `permissions/`, `eventlog/`, `runner/`, or
the other built-in tools needs to change. Code Mode is purely
additive — it plugs into the existing seams (Tool interface,
Gate, eventlog tool-call recording) without touching them.

## Phased delivery (single tag at the end)

### PR 1 — executor plumbing + permission gate integration with empty symbol pack

**Scope:** the executor wiring is fully real, but the assembled
pack contains only the bare `fmt`/`errors`/`context`/`time`
minimum needed for "hello world." No GCP, no extras. This lets
us verify the Yaegi integration, the error classification, the
gate integration, and the audit-log path end-to-end without
pulling the GCP SDK into the dep graph yet.

**Files:**

- `tools/gocode/executor.go`, `executor_test.go`
- `tools/gocode/eval.go`, `eval_test.go`
- `tools/gocode/classify.go`, `classify_test.go`
- `tools/gocode/truncate.go`, `truncate_test.go`
- `tools/gocode/integration_test.go`
- `tools/gocode/symbols/doc.go`, `deny.go`
- `tools/gocode/symbols/stdlib_safe.go` (generated, minimal)
- `tools/gocode/symbols/gen/main.go`
- `tools/gocode/symbols/go.mod`

**Verified:**

- Full unit suite green; integration tests pass against the echo
  + scripted providers.
- A small example using `tools.NewGoExecutor(gate)` runs a
  `fmt.Println("hello")` script end-to-end with no API key.

### PR 2 — default symbol pack + GCP SDK starter

**Scope:** generate the full curated stdlib pack and the GCP
starter pack. Wire `WithoutGCPDefaultPack` for opt-out. Update
the executor's tool description to list available packages.
Adds the GCP SDK to the symbols module's go.mod.

**Files:**

- `tools/gocode/symbols/stdlib_safe.go` (regenerated, full)
- `tools/gocode/symbols/gcp_pack.go` (generated, full)
- `tools/gocode/symbols/go.mod` (with GCP SDK deps)
- `dev/smoke/08-code-mode.sh`

**Verified:**

- The smoke script runs an `execute_go` call that lists buckets
  in a pre-seeded test project.
- Unit tests confirm the assembled pack contains the expected
  packages and that `WithoutGCPDefaultPack` removes them.

### PR 3 — project-symbol-injection seam + documentation

**Scope:** `WithProjectSymbols` is fully exposed and documented.
A worked example (`examples/code-mode/`) shows a fake billing
package being injected and called from a model-emitted snippet.
Site docs and DESIGN.md cross-references land.

**Files:**

- `examples/code-mode/main.go`
- `docs/site/content/docs/library-api.md` — Code Mode subsection
- `docs/site/content/docs/permissions.md` — Code Mode note
- `docs/DESIGN.md` — short rationale section pointing at this
  doc
- `README.md` — feature bullet
- `CHANGELOG.md` — version entry
- `tools/gocode/symbols/testpack/` — for the example's test

**Verified:**

- The example runs end-to-end with a scripted provider:
  model "emits" Go that calls the fake billing package, the
  executor runs it, the response shows the billing package's
  return value made it back.
- All three site doc pages render correctly under `hugo server`.
- Presubmits green; full `go test ./...` green.

## Verification

```bash
cd /home/user/projects/core-agent

# Per-PR unit tests
go test ./tools/gocode/... -v
go test ./tools/gocode/symbols/...

# Full
go vet ./... && go test ./...
for s in dev/ci/presubmits/*; do bash "$s"; done

# Hermetic integration (no GCP creds needed)
go test ./tools/gocode/ -run TestIntegration -v

# Real-LLM smoke (Phase 1+2):
source /home/user/scripts/gemini.sh && unset GEMINI_API_KEY GOOGLE_API_KEY
go build -o /tmp/core-agent ./cmd/core-agent
/tmp/core-agent --provider=vertex --yolo --session-db -p "
  Use execute_go to count the number of buckets in project
  ${TEST_GCP_PROJECT}. Print just the count.
"
# Expect: one execute_go tool call, exit 0, final text containing
# the bucket count.

# Real-LLM smoke for Phase 3 with the example's project pack:
go run ./examples/code-mode

# Symbol-pack regen
cd tools/gocode/symbols
go run ./gen --stdlib && git diff stdlib_safe.go
go run ./gen --gcp    && git diff gcp_pack.go
```

## Open questions and deferred items

- **Concurrent `execute_go` calls in one assistant turn.** The
  model may emit N parallel tool calls (the parallelism mandate
  in `DefaultInstruction` encourages this). Today the ADK runner
  dispatches them concurrently; the executor must therefore be
  goroutine-safe. Yaegi's `interp.Interpreter` is not
  goroutine-safe per its docs. Phase 1 ships with **one
  interpreter per call** (constructed fresh, used once, garbage-
  collected) — the per-call construction cost is real but
  bounded (~10-50ms on a modern box for the full stdlib + GCP
  pack), and the model's per-call latency is dominated by the
  LLM round-trip anyway. A future hardening could pool
  interpreters keyed by symbol-pack identity, eviction-on-error;
  not v1.
- **Stateful sessions across calls within a turn (declared vars
  persist).** Today, each call is independent. There's an
  alternative shape where the model gets a "REPL" — vars
  declared in one call are visible in the next, within the same
  turn. Cleaner narrative ("I set `client := storage.NewClient`
  once and reuse it"), but introduces new failure modes (what
  happens if the previous call panic'd? what if the model
  rebinds a var?). Defer; the stateless model is simpler and the
  model already adapts to it by hoisting setup into each call.
- **Composition with the `bash` tool — when to prefer which?**
  Documented in the `execute_go` tool description, but the
  decision boundary is fuzzy. Rough rule:
  - Cloud SDK calls → `execute_go`
  - OS-level / shell-native work (git, builds, file ops on
    workspace files) → `bash` or structured file tools
  - Tabular data manipulation → `execute_go` (Go has csv, json,
    sort; bash has awk, jq — both work; `execute_go` gives
    typed results)
  We'll refine the guidance based on real-world usage; not
  worth being precious in v1.
- **Multi-step compiled artifacts.** A natural extension: the
  model writes a function, the operator approves it, it becomes
  a *new tool* the model can invoke by name in later turns.
  This is the Voyager-style tool-synthesis pattern (eval-gated
  promotion to permanent skill). Requires the not-yet-built
  native eval framework to gate promotion — without
  measurable-quality eval, "promoted" tools accumulate
  unbounded and degrade rather than improve the agent. Phase 3+;
  this is the bridge to the self-learning thesis. Flag, don't
  design.
- **Yaegi version pinning.** Yaegi has historically lagged
  recent Go versions (generics support was incremental;
  reflect-representation differences are documented). We pin
  the Yaegi version in `tools/gocode/symbols/go.mod` and bump
  it deliberately, the same way we treat the GCP SDK pack
  versions. CHANGELOG entries note when the Yaegi pin changes.
- **`go` statement in scripts.** Scripts can `go func() { ... }()`.
  Goroutines spawned this way live on the host process and may
  outlive the script's ctx if they don't honor cancellation.
  Phase 1: documented as a footgun ("scripts should avoid
  `go`"); no enforcement. Future hardening: AST walk before
  Eval that refuses scripts containing `go` statements, with an
  opt-in flag for consumers who need them.
- **Per-pack permission scopes.** Today the permission gate
  treats `execute_go` as one allow/deny target. A richer model
  would allow per-package gating ("allow scripts that only
  touch the project's billing package, but ask before any GCP
  call"). This requires AST analysis of the script's imports
  and a gate API that accepts "this script touches packages
  X, Y, Z". Defer; the all-or-nothing gating is fine for v1.
- **Result streaming.** Today the response is one JSON blob at
  the end. For long-running scripts the operator gets no
  feedback until the script completes (or hits timeout). A
  streaming mode where stdout flushes mid-execution to the
  model would be nicer but requires rethinking the
  `functiontool` return path. Out of scope for v1.
- **WebAssembly as an alternative backend.** Yaegi works for
  Phase 1; if Yaegi's stability or coverage gaps bite us
  later, the same `NewGoExecutor` API could front a wasm-based
  evaluator (TinyGo-compiled scripts, wasmer/wazero runtime).
  The symbol-pack abstraction would change shape (wasm imports
  vs reflect.Value maps), but the *tool API the model sees*
  would not. Cross-reference for future-us.
- **`net` access from scripts via a vetted client.** Some
  consumers will want their scripts to make HTTP calls (not via
  the GCP SDK). The right answer is a vetted helper package
  exposed as a project symbol — e.g. `safehttp.Get(url)` with
  an allowlist of hosts — rather than exposing the raw `net/http`
  package. We document this pattern in the operator docs;
  no core-agent code required.

## Future work cross-references

- **Phase 2: standalone MCP server** that exposes the same Code
  Mode sandbox + GCP SDK pack to non-core-agent hosts (Claude
  Desktop, gemini-cli, generic MCP clients). Builds on the
  `docs/bidirectional-mcp-design.md` infrastructure — the same
  HTTP listener / stdio transport / auth plumbing serves both
  features. Phase 1 keeps `tools/gocode/` extraction-clean by:
  - Putting all Code Mode logic under `tools/gocode/` (no
    spillover into `agent/`, `permissions/`, `eventlog/`).
  - Treating `NewGoExecutor` as the public API surface; the
    Phase 2 MCP server wraps an executor instance behind the
    MCP tool schema we already designed.
  - Keeping the symbol packs as a separate module so the MCP
    server can pull them in without dragging core-agent's main
    module into its dep graph.
- **Phase 3: tool synthesis with eval-gated promotion.** Model
  writes a Go function inside Code Mode; an eval harness scores
  it against held-out tasks; passing functions get promoted to
  *named* tools the model can invoke directly in later turns.
  This is the Voyager-style self-learning bridge. Requires:
  - A native eval framework (not yet designed; tracked as a
    standalone future-work item).
  - A `tools.SkillBuilder` interface that accepts a function
    signature + reflect.Values and produces a `tool.Tool`.
  - Persistence — promoted tools survive across sessions. The
    `eventlog` backend is the obvious store; the design will
    fall out of Phase 2's MCP server work since persisted
    skills look a lot like an internal MCP server.
- **Relationship to bidirectional MCP design.** The
  `docs/bidirectional-mcp-design.md` design already plans to
  expose individual core-agent tools as MCP tools in
  tool-palette mode. Code Mode could be the *single* tool
  exposed in tool-palette mode (the model on the other side of
  the MCP boundary writes Go that runs inside ours). Two-way
  cross-pollination — they share the symbol-pack assembly
  logic, the permission gating, and the audit log shape.
- **Relationship to attach mode.** `docs/attach-mode-design.md`
  describes operator nudges into a running headless agent.
  Code Mode is orthogonal — the operator's nudge ("switch to
  read-only mode") is text; the agent's tools include Code
  Mode. The two features don't share infrastructure, just the
  underlying agent/event-log substrate they both depend on.
- **Relationship to skills.** Skills (`SKILL.md` bundles) are
  prompt-shaped capability extensions. Code Mode is a tool the
  model can invoke. A future "Go skill" format could let a
  consumer ship a `SKILL.md` plus a `*.go` file whose exported
  symbols get auto-added to `WithProjectSymbols` when the
  skill loads. Not designed; mentioned because the symmetry is
  obvious and we'll want to think about it before too many
  consumers grow their own conventions.
