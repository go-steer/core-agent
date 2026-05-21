# Peer registration: hub-and-spoke discovery for multi-agent deployments

Design doc for the fast-follow-on PR after `attach-mode`. Untracked
sibling to [`attach-mode-design.md`](attach-mode-design.md),
[`scheduled-monitoring-design.md`](scheduled-monitoring-design.md),
[`bidirectional-mcp-design.md`](bidirectional-mcp-design.md),
[`code-mode-design.md`](code-mode-design.md),
[`ax-integration-audit.md`](ax-integration-audit.md).

## Context

The K8s + Scion + AX deployment story has N agent pods running
independently — supervisor + monitors per the scheduled-monitoring
shape, plus AX-side multi-agent topologies. Each pod can expose
attach-mode (HTTP/SSE) for operator observability. But the operator
needs to know **which addresses to attach to** — and they don't
want to maintain that list by hand.

Three normal answers to "discover all agent endpoints":

1. **K8s Service + label selector.** `kubectl get pods -l role=core-agent`
   plus the attach port gives you everything reachable. Works
   today, no new code. **Sufficient for the simple case.**
2. **External service registry** (Consul, etcd, k8s API). Pulls
   in infrastructure that the agent fleet otherwise wouldn't need.
3. **Hub-and-spoke registration over attach-mode itself.** One
   agent listens for peers to register; others POST themselves on
   startup. Operator hits the hub's `GET /peers` to enumerate. This
   doc.

Option 3 is appealing because it's self-contained — no external
infra dependency — and it composes cleanly with attach-mode v1
(reuses the same `attach/` package, auth, HTTP server). The cost is
running one agent as the "hub" (a soft single point of failure for
discovery, not for actual agent work — peer agents keep functioning
even if the hub falls over).

This doc records the design so PR #2 has a concrete spec; PR #1
(attach-mode core) ships first without it.

### Settled decisions (do not relitigate)

- **Ships as PR #2**, after attach-mode core lands. Same `attach/`
  package, additive only. No attach-mode v1 surface changes.
- **Hub-and-spoke topology, not full mesh.** One designated hub
  agent holds the peer list; others register with it. Full mesh
  is N² registrations and has no clear win for the operator-
  discovery use case.
- **Soft single-point-of-failure is acceptable.** If the hub dies,
  *discovery* breaks but the peer agents keep working — operator
  falls back to k8s `get pods` to find them and reconnects when
  the hub recovers. Hard HA (leader election, gossip) is out of
  scope for v1; revisit if a consumer asks.
- **Authentication reuses attach-mode's mTLS + bearer.** Only
  trusted peers can register. Hub validates registrant TLS certs
  against the same CA as attach clients.
- **Registration is opt-in per agent.** A binary built without any
  `--attach-register-to` flag never registers anywhere; behaves
  identically to today's binary plus the bare attach-mode listener.

## Endpoints (additive on `attach/`)

| Endpoint | Purpose |
|---|---|
| `POST /peers` | Register self with the hub. Body: `{name, endpoint, labels, heartbeat_ttl_sec}`. Returns: `{registration_id, lease_expires_at}`. |
| `GET /peers` | List all live registrations on the hub. Filterable by label via `?label=cluster=prod` query params. |
| `POST /peers/<id>/heartbeat` | Extend the registration lease. Idempotent. |
| `DELETE /peers/<id>` | Explicit deregistration (peer shutdown). Hub also prunes expired leases automatically; this is the graceful-shutdown fast path. |

### Request bodies + responses

```jsonc
// POST /peers
{
  "name":             "monitor-cluster-a",       // unique per hub
  "endpoint":         "https://10.0.4.7:7777",  // peer's attach-mode URL
  "labels": {                                    // opaque to the hub
    "role":    "monitor",
    "cluster": "cluster-a",
    "version": "v1.7.0"
  },
  "heartbeat_ttl_sec": 60                        // peer's chosen cadence
}

// 201 Created
{
  "registration_id":   "reg-7f4e2a",
  "lease_expires_at":  "2026-05-22T15:30:00Z"    // now + ttl
}
```

```jsonc
// GET /peers
{
  "peers": [
    {
      "registration_id":   "reg-7f4e2a",
      "name":              "monitor-cluster-a",
      "endpoint":          "https://10.0.4.7:7777",
      "labels": { "role": "monitor", "cluster": "cluster-a", "version": "v1.7.0" },
      "registered_at":     "2026-05-22T15:25:00Z",
      "last_heartbeat":    "2026-05-22T15:28:30Z",
      "lease_expires_at":  "2026-05-22T15:30:00Z"
    },
    ...
  ]
}
```

## TTL + heartbeat policy

- **Default TTL: 60 seconds.** Peers re-POST `/peers/<id>/heartbeat`
  every 20-30s (third-of-TTL convention). Hub prunes any
  registration whose `lease_expires_at < now()` on a tick (every
  5 seconds inside the hub goroutine).
- **TTL is the peer's choice**, capped server-side at 5 minutes
  (configurable via `--peer-max-ttl`). A peer that wants a
  longer lease can ask for it but the hub clamps.
- **Heartbeat failures** are a peer concern, not the hub's. A
  peer that loses its registration just re-POSTs `/peers` and
  gets a fresh `registration_id`. Idempotent on name — re-using a
  name overwrites the prior registration rather than creating a
  ghost (avoid orphaned entries when peers restart).
- **Clock skew tolerance** — server uses its own clock for
  `lease_expires_at`; the peer's clock is irrelevant. Drift only
  matters at the heartbeat-cadence level (peer should heartbeat
  earlier than its own ttl-clamped deadline).

## Hub-side architecture (additive `attach/peers.go`)

```go
// PeerRegistry is the hub-side registry. Lives alongside SessionRegistry
// in the attach package. Independent — sessions and peers are orthogonal
// concerns; a peer's endpoint may itself host sessions.
type PeerRegistry struct {
    mu     sync.RWMutex
    byID   map[string]*Peer
    byName map[string]*Peer  // for name-based upsert
    maxTTL time.Duration
}

type Peer struct {
    RegistrationID  string
    Name            string
    Endpoint        string
    Labels          map[string]string
    RegisteredAt    time.Time
    LastHeartbeat   time.Time
    LeaseExpiresAt  time.Time
}

func (r *PeerRegistry) Register(req RegisterRequest) (*Peer, error)
func (r *PeerRegistry) Heartbeat(regID string) (time.Time, error)
func (r *PeerRegistry) Deregister(regID string) error
func (r *PeerRegistry) List(labelMatch map[string]string) []*Peer
func (r *PeerRegistry) Prune() int  // returns count pruned; called from a ticker
```

A background goroutine inside the registry runs `Prune()` every 5s
to drop expired leases.

## Peer-side wiring

New `attach.Client` (the same client used by `core-agent attach`)
gains a `RegisterTo(hubURL, opts...) error` method. On startup, a
peer binary calls it once; behind the scenes a goroutine handles
heartbeats. On graceful shutdown the binary calls `Deregister()`.

```go
// In a consumer binary or the bundled CLI when --attach-register-to is set:
client := attach.NewClient(hubURL,
    attach.WithBearerToken(os.Getenv("ATTACH_TOKEN")),
    attach.WithClientCert(certPath, keyPath))

stop, err := client.RegisterAndHeartbeat(ctx, attach.RegisterRequest{
    Name:            os.Getenv("HOSTNAME"),  // pod name
    Endpoint:        myAttachListenURL,       // e.g. https://${POD_IP}:7777
    Labels:          map[string]string{
        "role":    "monitor",
        "cluster": kubeContext,
    },
    HeartbeatTTL:    60 * time.Second,
})
// stop() blocks until ctx is cancelled or Deregister succeeds.
```

## CLI surface

- **`core-agent ls <hub-url>`** — already in attach-mode v1. PR #2
  extends it: if the URL responds to `GET /peers`, list peers
  instead of (or in addition to) sessions. The output adds a column
  showing each peer's endpoint so the operator can `core-agent
  attach <peer-endpoint>` next.
- **`--attach-register-to <hub-url>`** — new agent-side flag (PR
  #2). When set, the agent registers with the named hub on startup
  and heartbeats until shutdown. Implies `--attach-listen` so the
  hub has a real endpoint to record.

## Out of scope

- **Hub HA / leader election.** A single hub is a soft SPOF for
  *discovery*. If you need redundancy, run two hubs and have peers
  register with both, or use a real service registry. Mesh-style
  gossip protocols (Serf, Memberlist) are out of scope.
- **Cross-hub federation.** Hubs don't know about each other in
  v1. Two pods running attach-mode + receiving registrations are
  independent registries.
- **Endpoint health checks.** The hub doesn't probe peer endpoints
  for liveness — heartbeat is the only signal. A peer that crashes
  hard (no graceful shutdown) sits in the registry until the
  lease expires. Operator polling `GET /peers` may see stale
  endpoints for up to the TTL.
- **Capabilities advertisement.** Labels can carry whatever the
  peer wants ("supports_mcp": "true", etc.), but there's no
  structured capability model. If a consumer needs that (e.g. for
  A2A interop), they can encode it in labels or add a
  `capabilities` field later.
- **TLS material distribution.** Same as attach-mode core: certs
  and CAs are mounted from disk by the operator. The hub validates
  registrant client certs against the same CA as attach clients.

## Implementation sketch

About **300 LoC + tests** on top of PR #1. Five new files:

- `attach/peers.go` — `PeerRegistry`, `Peer`, `RegisterRequest`,
  pruner goroutine. (~150 LoC.)
- `attach/peers_handlers.go` — HTTP handlers for the four endpoints.
  (~80 LoC.)
- `attach/client_register.go` — peer-side `RegisterAndHeartbeat`
  helper on `attach.Client`. (~60 LoC.)
- `attach/peers_test.go` — TTL pruning, name-upsert idempotency,
  label-filter `List`, registration validation. (~150 LoC.)
- `cmd/core-agent/main.go` — wire `--attach-register-to`. (~30 LoC.)
- `dev/smoke/11-peer-registration.sh` — local smoke: hub agent +
  peer agent, peer registers, `ls` shows it, peer deregisters,
  next `ls` is empty. (~80 LoC.)

CHANGELOG entry under `[Unreleased]` at PR #2 time.

## Open questions

1. **Hub address discovery for the first peer.** Same chicken-and-egg
   as any registry. Three answers: hardcode an endpoint in the
   peer config, use a k8s DNS name (`core-agent-hub.default.svc`),
   or use an env var the deployment manifest sets. All work; pick
   per deployment.
2. **Should peers register their session IDs too?** Currently the
   peer registers itself (one endpoint per peer). The operator
   then attaches to that endpoint and uses `GET /sessions` to see
   what's running. Alternative: peer pushes its session list on
   registration + heartbeat. *Lean:* don't push sessions — keeps
   the registry small + freshness comes from going to the peer
   directly. Revisit if a consumer asks.
3. **Hub-of-hubs / hierarchical.** One hub at the cluster level,
   one supervisor-hub per K8s namespace? Out of scope for v1 but
   worth thinking about. Probably solved by labels + the operator
   choosing which hub to query.
