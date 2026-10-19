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
- The reply arrives without anyone touching the agent. Every agent
  this script starts is an attach-only daemon (`--no-repl`), so its
  wake loop takes the turn, and the agent pane
  (`tmux attach -t core-agent-uat`) shows only the daemon's log.

### A2. Wake-on-event seam

`run.sh solo` has no scheduler, so a `/wake` with nothing queued
does nothing visible: the wake loop looks, finds no input, and goes
back to waiting. To exercise it properly run the scheduler UAT
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
    --attach-readonly --no-repl
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
    --attach-token=ATTACH_TOKEN --no-repl

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
        --session-db --session-db-path=/tmp/core-agent-uat/db/config-peer.db --no-repl)
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

**What you're validating:** three read-only GET endpoints:
`/tools`, `/agents` and `/status`. The first two feed the TUI's
`/tools` and `/subagents` slash commands. `/status` has no slash
command in the current TUI, but the endpoint is still served. Pure
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

## Session E — TUI (~15 min)

**What you're validating:** `core-agent-tui` as an end-to-end consumer of the attach-mode protocol: the picker, chat, slash commands and the usage readout. The TUI is built on go-steer/core-tui. This session was rewritten on 2026-10-19 against core-tui v0.31.0; the commands and keys of the older standalone TUI (`/status`, `/peers`, `/wake`, `/reconnect`, `/transcript`, `/theme dark`, Ctrl+E) are gone.

### E1. Build + launch

```bash
./run.sh clean
./run.sh build
./run.sh hub                                  # standalone hub on :7777
sleep 1
./run.sh tui http://localhost:7777
```

**Verify on launch:**

- Picker appears with the hub's session listed.
- Header reads `core-agent-tui  ●  http://localhost:7777  ·  session picker`.
- Footer shows `↑/↓ navigate · Enter attach · r refresh · q quit`.

### E2. Picker + enter session

- Use ↑/↓ (or j/k) to navigate.
- Hit Enter to attach. A standalone hub has exactly one session, `default`; pick that.
- An `ℹ Attached to live session` line appears.
- The header now carries the session and the usage readout, e.g.
  `core-agent-tui · default · ◇ echo · … · default · 0 in · 0 out · $0.0000`.
- The footer shows the keys: `enter submit · alt+enter newline · ctrl+c quit · ? for more`.

### E3. Inject a message via the input box

- Type `what is 2+2?` + Enter.
- **Verify:** your text appears as `❯ what is 2+2?`, and the echo provider's reply appears below it within a second or two, with nobody touching the hub.
- If the spinner just runs and no reply comes, the hub is not running `--no-repl` (an old `run.sh`, or a hub
  started by hand). Its embedded TUI is holding the session, and the message waits in its inbox.
  go-steer/core-tui#379 covers the spinner never stopping.
- The header's usage readout updates (the echo provider may report 0 tokens; with `MODEL=vertex` you'll see real numbers).

### E4. Slash commands

Type `/` to see the palette. Run each of these; each should open its view or print a line, and none should say `unknown command`:

```
/help         — commands + keybindings (also `?`)
/tools        — the agent's tools with source + gate state
/subagents    — the roster (empty unless you've spawned any)
/stats        — the session's usage
/pricing      — the model's rate and where it came from
/keys         — keybindings
/theme        — lists themes; then `/theme gopher` (or google, gke, anthropic, gemini, openai, matrix, …)
/clear        — clears the local scrollback (the server log is unchanged)
```

`/transcripts` doesn't work in attach mode yet: the transcripts live on the daemon's machine (#1331).

### E5. Multi-session picker against a fleet

```bash
./run.sh clean
./run.sh fleet 2                              # hub + 2 peers
sleep 2
./run.sh tui http://localhost:7777
```

**Verify:** the picker shows entries from the hub AND from each peer (Origin column shows `local` vs the peer name). Pick a peer session and type; the reply comes from that peer.

### E6. Direct-jump shortcut URL

```bash
./run.sh ls                                   # grab a session ID
./run.sh tui "http://localhost:7777/sessions/core-agent/<sid>"
```

**Verify:** the picker is skipped; the chat opens directly on that session.

### E7. Theme switch

- In a running TUI: `/theme gopher`, then `/theme default`. **Verify:** existing messages re-render in the new palette, and an unknown name (`/theme nope`) falls back to default with a pointer to `/theme`.
- There is no input-box `$EDITOR` handoff in the current TUI (the old Ctrl+E); go-steer/core-tui#380 tracks bringing it back.

### E8. Read-only listener

```bash
# Restart the hub in read-only mode and try the TUI:
./run.sh clean
ATTACH_TOKEN="${ATTACH_TOKEN}" /tmp/core-agent-uat/bin/core-agent \
    --provider=echo --session-db \
    --session-db-path=/tmp/core-agent-uat/db/ro.db \
    --attach-listen=:7777 --attach-token=ATTACH_TOKEN \
    --attach-readonly --no-repl &
sleep 2
./run.sh tui http://localhost:7777
```

**Verify:** the picker and chat work for reads; sending a message is refused by the server (403), and the TUI shows that as an error line rather than a spinner that never ends. Note the exact wording for the record.

### E9. Hub restart mid-session

While attached, in another shell:

```bash
tmux send-keys -t core-agent-uat:hub C-c    # kill the hub
sleep 5
./run.sh hub                                 # restart it
```

There is no `/reconnect` command in the current TUI. **Observe and record:** does the TUI show the drop, and does it pick the stream back up on its own once the hub is back? If it doesn't recover, quit and re-run `./run.sh tui`; a session's history replays from the eventlog either way.

### E10. Saved transcripts

`/transcripts` is the current command (the old `/transcript` that wrote `/tmp/<sid>.md` is gone). In attach mode it currently reports that it isn't wired (#1331). Check it in the embedded TUI instead: run `core-agent` in a terminal, have a short exchange, then `/transcripts`.

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
| E1 TUI build + launch   | TUI-B | binary builds and connects to hub |
| E2 picker → chat         | TUI-B | session list, navigate, attach |
| E3 inject via input      | TUI-B | your message + echo reply + header usage readout update |
| E4 slash commands        | TUI-B | core-tui's current commands all functional |
| E5 fleet picker          | TUI-B | hub + peer sessions enumerated |
| E6 direct-jump URL       | TUI-B | skip picker via /sessions/<sid> suffix |
| E7 theme switch          | TUI-B | palette re-render; unknown name falls back (editor handoff: core-tui#380) |
| E8 read-only listener    | TUI-B | reads work, writes 403 with clear error line |
| E9 hub restart           | TUI-B | behaviour on a dropped stream (observed, no /reconnect) |
| E10 /transcripts         | TUI-B | embedded TUI lists saved transcripts (attach mode: #1331) |

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
