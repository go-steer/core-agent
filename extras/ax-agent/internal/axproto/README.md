# axproto — vendored snapshot of `github.com/google/ax/proto`

This directory holds a verbatim copy of the gRPC + protobuf definitions from
[Agent eXecutor (AX)](https://github.com/google/ax) — currently a private
GitHub repo, slated for a rewrite.

We snapshot the proto package directly (rather than vendoring the whole `ax`
module via `go mod vendor`) for two reasons:

1. **Auth-free CI.** `github.com/google/ax` is private; without this snapshot,
   `go mod download` would need a token and CI would break for outside
   contributors.
2. **Surface minimization.** The full `ax` module pulls in a long transitive
   dep tree (gRPC, sqlite, gemini libraries, an unresolvable local `replace`
   to `../SubstrATE`). The adapter only needs the wire definitions for
   `AgentService.Connect` + `HealthCheck` and the `Content` / `Message`
   types — copying those keeps our footprint tiny.

The wire-level protobuf names (`ax.AgentMessage`, `ax.Content`, etc.) are
preserved by keeping the original `package proto` declarations; the *Go*
import path is `github.com/go-steer/core-agent/extras/ax-agent/internal/axproto`,
typically aliased as `axproto` at the import site.

## Refresh

When the upstream proto schema changes (and the change is wire-relevant for
us), refresh by:

```bash
cp ../ax/proto/{ax.pb.go,ax_grpc.pb.go,content.pb.go,ax.proto,content.proto} \
   extras/ax-agent/internal/axproto/
go test ./extras/ax-agent/...
```

## Removal

When `github.com/google/ax` goes public, this directory disappears: switch
the adapter's imports to `github.com/google/ax/proto`, drop the `internal/`
package, drop this README. The `axplore` branch's reason for existing is
this snapshot — replacing it with a real dep is the merge-to-main signal.

## Source revision

Snapshot taken from `github.com/google/ax` at commit `d9821f6` (2026-05).
