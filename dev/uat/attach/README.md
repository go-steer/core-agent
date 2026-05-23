# Attach-mode UAT (PRs #11 + #12 + #13)

Manual UAT walk-through for the attach-mode + peer-registration +
attach-config stack. Spins everything up in tmux panes under
`/tmp/core-agent-uat/` — no host pollution, single `clean`
command to wipe it all.

## Prerequisites

- `tmux`, `jq`, `go` on `$PATH`.
- No model creds needed — `run.sh` defaults to `--provider=echo`.
  Override with `MODEL=vertex` (or any other registered provider)
  plus the matching env if you want to see real LLM output.

## Layout

```
dev/uat/attach/
├── README.md          # this file
├── run.sh             # helper — spin up scenarios in tmux
└── fixtures/
    ├── hub-config.json    # all-config startup for the hub
    └── peer-config.json   # all-config startup for a peer
```

All persistent state lives under `/tmp/core-agent-uat/`:

```
/tmp/core-agent-uat/
├── bin/core-agent     # built once, reused across runs
├── db/<name>.db       # one session DB per agent
└── agents/<name>/     # per-agent CWD (where .agents/config.json lives)
```

## Cheat sheet

```bash
./run.sh build               # one-time: build the binary into /tmp
./run.sh solo                # Session A scenarios
./run.sh fleet 2             # Session B scenarios (hub + 2 peers)
./run.sh config-hub          # Session C scenarios
./run.sh ls                  # list sessions + peers on the hub
./run.sh tail <url>          # live-tail an attach endpoint
./run.sh inject <url> "msg"  # POST /inject (Bearer auth baked in)
./run.sh status              # what's running under tmux
./run.sh clean               # kill tmux + rm /tmp/core-agent-uat
tmux attach -t core-agent-uat  # watch the panes
```

Bearer token defaults to `uat-token-$(whoami)`, exported as
`ATTACH_TOKEN` for everything `run.sh` spawns. Override with
`ATTACH_TOKEN=…` in your shell.

---

## Session A — baseline attach (~15 min, PR #11)

**What you're validating:** the four-endpoint contract, auth
matrix, wake-on-inject seam.

### A1. Solo agent + ls + tail + inject

Spin up one agent with the attach listener on `:7777`:

```bash
./run.sh solo
tmux attach -t core-agent-uat   # split your shell so you can watch
```

From a separate shell:

```bash
./run.sh ls
# expect: one session under app=core-agent, user=local
```

Pick the session ID from the `ls` output, then live-tail:

```bash
./run.sh tail http://localhost:7777/sessions/<sid>
# OR the single-segment shortcut form:
./run.sh tail http://localhost:7777/sessions/<sid>
```

In a third shell, inject a message:

```bash
./run.sh inject http://localhost:7777/sessions/<sid>/inject "hello from the operator"
```

**Verify:**

- The `tail` shell shows a new `user` event with your message and
  a model `model` event with the echo provider's reply.
- The agent pane (`tmux attach -t core-agent-uat`) shows the same
  exchange via the REPL renderer.
- Both views agree — broadcaster and REPL are seeing the same
  stream out of the eventlog.

### A2. Wake-on-event seam

`run.sh solo` runs in REPL mode (no scheduler), so `/wake` is a
no-op there. To exercise it properly run the scheduler UAT
(`dev/uat/scheduled-monitor/`) with `--attach-listen=:7777` and
inject during a sleep — or, lightweight, just confirm
`POST /sessions/<sid>/wake` returns 204:

```bash
curl -i -H "Authorization: Bearer ${ATTACH_TOKEN}" \
    -X POST http://localhost:7777/sessions/<sid>/wake
# expect: HTTP/1.1 204 No Content
```

### A3. Auth matrix

Three quick curls against the running solo agent:

```bash
# (a) no token — expect 401
curl -i http://localhost:7777/sessions

# (b) wrong token — expect 401
curl -i -H "Authorization: Bearer not-the-token" http://localhost:7777/sessions

# (c) right token — expect 200 with JSON
curl -i -H "Authorization: Bearer ${ATTACH_TOKEN}" http://localhost:7777/sessions
```

### A4. ReadOnly mode

Stop the solo agent (`./run.sh clean`), restart with `readonly` on
via env (we don't have a CLI knob in `run.sh`, edit the spawn for
manual proof):

```bash
./run.sh clean
./run.sh build
ATTACH_TOKEN="${ATTACH_TOKEN}" /tmp/core-agent-uat/bin/core-agent \
    --provider=echo --session-db \
    --session-db-path=/tmp/core-agent-uat/db/ro.db \
    --attach-listen=:7777 --attach-token=ATTACH_TOKEN \
    --attach-readonly
```

From another shell:

```bash
./run.sh inject http://localhost:7777/sessions/<sid>/inject "should fail"
# expect: HTTP 403 — POST disabled in readonly mode
```

`GET /sessions` and `/events` still work.

### A5. Unix socket transport

```bash
./run.sh clean
ATTACH_TOKEN="${ATTACH_TOKEN}" /tmp/core-agent-uat/bin/core-agent \
    --provider=echo --session-db \
    --session-db-path=/tmp/core-agent-uat/db/uds.db \
    --attach-unix-socket=/tmp/core-agent-uat/sock/agent.sock \
    --attach-token=ATTACH_TOKEN

# In another shell:
./run.sh ls   "unix:///tmp/core-agent-uat/sock/agent.sock"
./run.sh tail "unix:///tmp/core-agent-uat/sock/agent.sock"
ls -l /tmp/core-agent-uat/sock/agent.sock   # expect 0600 perms
```

---

## Session B — fleet shape (~20 min, PR #12)

**What you're validating:** hub + peer registration, TTL/heartbeat,
name-upsert (no orphans), graceful hub-down behavior.

### B1. Hub + 2 peers

```bash
./run.sh clean
./run.sh fleet 2
sleep 2
./run.sh ls
```

**Verify:** the `ls` output has both a SESSIONS section (one per
agent) and a PEERS section listing `peer-1` at `:7780` and `peer-2`
at `:7781`. Each peer also shows its own session if you query
the peer's URL directly:

```bash
./run.sh ls http://localhost:7780
```

### B2. Lease expiry after peer death

```bash
./run.sh status
# find the window index for peer-2
tmux send-keys -t core-agent-uat:peer-2 C-c   # SIGINT the peer

# Watch the hub for ~60s (default TTL):
while sleep 5; do clear; ./run.sh ls; done
# expect: peer-2 disappears from the PEERS section
#         within (TTL=60s) + (prune-tick=5s) = ~65s
```

Ctrl-C the loop.

### B3. Name-upsert (no orphans on restart)

```bash
./run.sh peer peer-2 7781     # respawn under the same name
sleep 2
./run.sh ls
```

**Verify:** exactly **one** entry for `peer-2`. If you see two
(stale + fresh), the name-upsert path regressed.

### B4. Hub-down behavior

```bash
tmux send-keys -t core-agent-uat:hub C-c   # SIGINT the hub
sleep 2

# Peers should still be processing their own work; confirm by
# attaching to one directly:
./run.sh ls http://localhost:7780
# expect: peer's own session listing — no hub needed
```

Bring the hub back:

```bash
./run.sh hub
sleep 2
./run.sh ls
# expect: peers re-register on their next heartbeat (≤ TTL/3 = ~20s)
#         and reappear in the PEERS section
```

This is the "soft SPOF for discovery, not for agent work"
claim from the design doc.

---

## Session C — config UX (~10 min, PR #13)

**What you're validating:** all-config-no-flags startup, env-var
expansion, CLI-beats-config precedence, secret-stays-env.

### C1. All-config startup

```bash
./run.sh clean
./run.sh config-hub        # hub started from fixtures/hub-config.json
sleep 2
./run.sh ls
```

The agent CWD is `/tmp/core-agent-uat/agents/config-hub/`; its
`.agents/config.json` is a copy of `fixtures/hub-config.json`.
No `--attach-*` CLI flags were passed — every one was sourced
from the config file.

**Verify:**

- `ls` against `http://localhost:7777` works (listener up).
- `ls` headers carry `peer-hub: true` semantics (try
  `POST /peers/whatever/heartbeat` — should 404, not 405).

### C2. Env expansion

`fixtures/peer-config.json` carries:

```jsonc
"register_endpoint": "http://${POD_IP}:7780",
"register_name":     "monitor-${HOSTNAME_OVERRIDE}"
```

Start a peer that consumes it:

```bash
mkdir -p /tmp/core-agent-uat/agents/config-peer/.agents
cp dev/uat/attach/fixtures/peer-config.json \
    /tmp/core-agent-uat/agents/config-peer/.agents/config.json

(cd /tmp/core-agent-uat/agents/config-peer && \
    POD_IP=127.0.0.1 HOSTNAME_OVERRIDE=config-peer-pod \
    ATTACH_TOKEN="${ATTACH_TOKEN}" \
    /tmp/core-agent-uat/bin/core-agent \
        --session-db --session-db-path=/tmp/core-agent-uat/db/config-peer.db)
```

In another shell:

```bash
./run.sh ls   # against the hub from C1
# expect: a peer named "monitor-config-peer-pod" at "http://127.0.0.1:7780"
```

The literal `${POD_IP}` and `${HOSTNAME_OVERRIDE}` should be
resolved — if you see them unexpanded, `os.ExpandEnv` regressed.

### C3. CLI beats config (bool override)

Edit `fixtures/hub-config.json` temporarily to set
`"readonly": true`, then start the hub with an explicit CLI
override that *un*sets it:

```bash
jq '.attach.readonly = true' dev/uat/attach/fixtures/hub-config.json \
    > /tmp/core-agent-uat/agents/config-hub/.agents/config.json

(cd /tmp/core-agent-uat/agents/config-hub && \
    ATTACH_TOKEN="${ATTACH_TOKEN}" \
    /tmp/core-agent-uat/bin/core-agent \
        --session-db --session-db-path=/tmp/core-agent-uat/db/cli-vs-cfg.db \
        --attach-readonly=false) &

sleep 2
./run.sh inject http://localhost:7777/sessions/<sid>/inject "should succeed"
# expect: HTTP 200 — CLI false beats config true
```

This exercises the `flag.Visit` path that distinguishes
"explicitly set to false" from "not set at all".

### C4. Secret stays env-only

There is no `token` (vs. `token_env`) field in `AttachConfig` by
design. Try adding one and confirm nothing changes:

```bash
jq '.attach.token = "this-should-be-ignored"' \
    dev/uat/attach/fixtures/hub-config.json \
    > /tmp/core-agent-uat/agents/config-hub/.agents/config.json

# Restart hub — confirm bearer auth still rejects "this-should-be-ignored":
curl -i -H "Authorization: Bearer this-should-be-ignored" \
    http://localhost:7777/sessions
# expect: 401
```

Anything other than `${ATTACH_TOKEN}` should be a 401. Secrets do
not live in committed config.

---

## Session D — read-only state endpoints (~10 min, PR A: attach-tui-endpoints)

**What you're validating:** the three new GET endpoints that feed the
TUI's `/tools`, `/subagents`, `/status` slash commands. Pure
read-only projections — `--attach-readonly` doesn't block them.

### D1. `/tools` — full tool catalog + per-tool gate state

Spin up a single agent with a couple of permission rules so the
`gate_state` field has something to surface:

```bash
./run.sh clean
mkdir -p /tmp/core-agent-uat/agents/state-test/.agents

cat > /tmp/core-agent-uat/agents/state-test/.agents/config.json <<'EOF'
{
  "version": 1,
  "model": { "provider": "echo", "name": "echo" },
  "permissions": {
    "mode":  "ask",
    "allow": ["read_file:**", "fetch_url:github.com/*"],
    "deny":  ["bash:sudo *"]
  }
}
EOF

(cd /tmp/core-agent-uat/agents/state-test && \
    ATTACH_TOKEN="${ATTACH_TOKEN}" /tmp/core-agent-uat/bin/core-agent \
        --session-db --session-db-path=/tmp/core-agent-uat/db/state-test.db \
        --attach-listen=:7777 --attach-token=ATTACH_TOKEN) &
sleep 2
```

Pick the session ID from `./run.sh ls`, then:

```bash
curl -s -H "Authorization: Bearer ${ATTACH_TOKEN}" \
    "http://localhost:7777/sessions/core-agent/<sid>/tools" | jq
```

**Verify:**

- Top-level shape is `{"tools": [...]}`.
- Each entry has `name`, `description`, `source`, `gate_state`.
- `read_file` shows `source: "builtin"` and `gate_state: "allowed"` (from the allow pattern).
- `bash` shows `gate_state: "prompted"` (no allow pattern matches the bare name; the `sudo *` deny needs a key).
- Tools not configured at all (e.g. `glob`) show `gate_state: "prompted"` (ask mode default).

Try the shortcut form:

```bash
curl -s -H "Authorization: Bearer ${ATTACH_TOKEN}" \
    "http://localhost:7777/sessions/<sid>/tools" | jq '.tools | length'
```

Should return the same count.

### D2. `/status` — model name + state

```bash
curl -s -H "Authorization: Bearer ${ATTACH_TOKEN}" \
    "http://localhost:7777/sessions/core-agent/<sid>/status" | jq
```

**Verify:** `state: "idle"`, `model_name: "echo"` (or your `MODEL=` override). State is hardcoded `idle` in v1 — finer run-loop instrumentation is design-doc'd as v3 work.

### D3. `/agents` — background subagents

```bash
curl -s -H "Authorization: Bearer ${ATTACH_TOKEN}" \
    "http://localhost:7777/sessions/core-agent/<sid>/agents" | jq
```

**Verify:** `{"agents": []}` since nothing's been spawned. If you've been driving the agent (`./run.sh inject ...`) and triggered `spawn_agent`, those would appear with `id`, `name`, `status`, `started_at`, `parent_session_id`.

### D4. ReadOnly doesn't block reads

```bash
# Restart with --attach-readonly:
./run.sh clean
(cd /tmp/core-agent-uat/agents/state-test && \
    ATTACH_TOKEN="${ATTACH_TOKEN}" /tmp/core-agent-uat/bin/core-agent \
        --session-db --session-db-path=/tmp/core-agent-uat/db/state-test.db \
        --attach-listen=:7777 --attach-token=ATTACH_TOKEN \
        --attach-readonly) &
sleep 2

# All three reads still succeed:
curl -i -H "Authorization: Bearer ${ATTACH_TOKEN}" \
    "http://localhost:7777/sessions/core-agent/<sid>/tools" | head -1
curl -i -H "Authorization: Bearer ${ATTACH_TOKEN}" \
    "http://localhost:7777/sessions/core-agent/<sid>/status" | head -1

# But /inject is still 403:
./run.sh inject http://localhost:7777/sessions/core-agent/<sid>/inject "blocked" || true
```

**Verify:** GETs 200; the POST 403. ReadOnly gates writes only.

### D5. Empty-handler graceful behavior

This is implicit but worth knowing: an agent that doesn't implement the provider interfaces (e.g. an older binary, a library consumer that built `*agent.Agent` without one of the providers) returns the empty shape — `{"tools":[]}`, `{"agents":[]}`, `StatusInfo{state:"idle"}` — never 501. The TUI fans these calls at startup against potentially-mixed-vintage fleets, so empty-is-OK is structural.

---

## After UAT — cleanup

```bash
./run.sh clean
```

Removes the tmux session + `/tmp/core-agent-uat/`. Nothing leaks.

## When something fails

- Capture: `./run.sh status` (window list), `cat /tmp/core-agent-uat/db/*.db | strings | head` (or sqlite query the eventlog), and the failing curl with `-v`.
- The agent panes inside `tmux attach -t core-agent-uat` show stderr live — the attach server logs every bound listener and every auth failure to stderr.
- Common gotchas:
  - Port already in use → previous run not cleaned (`./run.sh clean`).
  - `ls` returns 412 → `--session-db` wasn't passed; attach server requires the broadcaster's eventlog.
  - Peer registered but `endpoint` is `0.0.0.0:...` → forgot `--attach-register-endpoint` (the design doc warns about this exact case).

## Coverage matrix

| Scenario | PR # | Covers |
|---|---|---|
| A1 ls / tail / inject  | #11 | 4-endpoint contract, broadcaster, session URL forms |
| A2 wake                | #11 | wake-on-event seam (basic) |
| A3 auth matrix         | #11 | bearer 401, ReadOnly |
| A4 readonly            | #11 | `POST /inject` 403 |
| A5 unix socket         | #11 | transport variant, 0600 perms |
| B1 fleet ls            | #12 | hub + peer registration round trip |
| B2 lease expiry        | #12 | TTL pruning |
| B3 name-upsert         | #12 | no orphan on restart |
| B4 hub-down            | #12 | soft-SPOF + re-register on recovery |
| C1 all-config          | #13 | flag-less startup |
| C2 env expansion       | #13 | `os.ExpandEnv` on config values |
| C3 CLI beats config    | #13 | `flag.Visit` precedence (bool false case) |
| C4 secret stays env    | #13 | no token field in config |
| D1 /tools + gate_state | TUI-A | full catalog, source classification, per-tool gate state |
| D2 /status             | TUI-A | model name + state |
| D3 /agents             | TUI-A | background subagent listing |
| D4 read-only + reads   | TUI-A | reads bypass --attach-readonly |
| D5 empty-handler graceful | TUI-A | no 501 when provider not implemented |

Not covered here (deferred until needed):

- **mTLS end-to-end** (client cert verification) — needs cert
  generation; manual smoke is `openssl req`/`openssl x509` plus
  passing `--attach-tls-cert` / `--attach-tls-key` /
  `--attach-client-ca`. Out-of-band when a consumer asks.
- **Reconnect + `?since=N` replay** — exercised by the unit
  tests (`attach/integration_test.go`); manual is "kill tail,
  re-tail with `?since=<N>`, confirm events ≥ N stream first."
- **Real LLM runs** — set `MODEL=vertex` (or anthropic-vertex)
  and the matching env. The echo provider exercises every
  attach/peer path; the model choice only affects what the
  events *say*, not whether they flow.
