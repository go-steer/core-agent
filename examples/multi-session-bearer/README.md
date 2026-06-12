# `multi-session-bearer` — two-user starter for the multi-session daemon

A **config-only** recipe that turns on the multi-session daemon
([`attach.multi_session.*`](../../docs/site/content/docs/reference/multi-session.md))
with a static bearer-token user table. Two users (alice + bob) attach
to the same daemon over the network; each sees only their own session;
cross-session isolation is enforced by the substrate.

Five minutes to run; no Cloud / IDP dependencies.

## What ships in this example

```
.agents/
├── config.json        # multi_session.enabled: true + bearer_table auth
└── AGENTS.md          # benign daemon-wide instructions
users/
├── users.json         # the bearer table — alice, bob, ops (admin)
└── README.md          # operator notes on token rotation, file mode
```

The substrate does the per-caller auth, ACL enforcement, and audit
threading; this recipe just wires it together with sample identities
so you can attach as alice, attach as bob, and see they're isolated.

## Run it (local, five-minute walkthrough)

The recipe defaults everything under `/tmp/multi-session-bearer/` so
nothing lands in your home directory. Adjust if you want persistence.

### 1. Stage the user table

```bash
sudo install -m 0600 -o "$USER" examples/multi-session-bearer/users/users.json \
  /tmp/multi-session-bearer/users.json
```

Or generate fresh tokens (the sample tokens are placeholders, fine for
local play but never push them anywhere shared):

```bash
mkdir -p /tmp/multi-session-bearer
cat > /tmp/multi-session-bearer/users.json <<EOF
{
  "version": 1,
  "users": [
    { "identity": "alice@example.com", "token": "$(openssl rand -hex 32)", "labels": {"team": "platform"} },
    { "identity": "bob@example.com",   "token": "$(openssl rand -hex 32)", "labels": {"team": "infra"}    },
    { "identity": "ops@example.com",   "token": "$(openssl rand -hex 32)", "labels": {"kind": "admin"}    }
  ]
}
EOF
chmod 0600 /tmp/multi-session-bearer/users.json
```

The loader **rejects** group- or world-readable users.json files at
startup — `0600` (or stricter) is required.

### 2. Start the daemon

```bash
cd examples/multi-session-bearer
core-agent --listen 127.0.0.1:7777 --session-db /tmp/multi-session-bearer/session.db
```

Look for these lines in the startup log:

```
core-agent: attach listener on 127.0.0.1:7777
core-agent: session db: /tmp/multi-session-bearer/session.db
```

### 3. Attach as alice (in another terminal)

```bash
ALICE_TOKEN=$(jq -r '.users[] | select(.identity == "alice@example.com") | .token' \
  /tmp/multi-session-bearer/users.json)

curl -s -H "Authorization: Bearer $ALICE_TOKEN" \
  http://127.0.0.1:7777/sessions
```

Expect a JSON response with whatever sessions alice has access to.

To inject a message into the agent's inbox **as alice**:

```bash
curl -s -X POST \
  -H "Authorization: Bearer $ALICE_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"message": "hello from alice"}' \
  http://127.0.0.1:7777/sessions/<sessionID>/inject
```

### 4. Attach as bob (in another terminal)

```bash
BOB_TOKEN=$(jq -r '.users[] | select(.identity == "bob@example.com") | .token' \
  /tmp/multi-session-bearer/users.json)

curl -s -H "Authorization: Bearer $BOB_TOKEN" \
  http://127.0.0.1:7777/sessions
```

Bob does **not** see alice's session (and vice versa) — the
`Registry.ListAuthorized` filter hides sessions the caller can't
read.

If bob tries to inject into alice's session ID directly, the response
is **404** (not 403, intentionally — hiding session existence prevents
activity enumeration).

### 5. Inspect the audit log

```bash
sqlite3 /tmp/multi-session-bearer/session.db \
  "SELECT seq, session_id, author, metadata FROM agent_eventlog ORDER BY seq;"
```

Every event row has a `metadata` JSON column carrying `{"caller": "<identity>"}`
(and `"proxy_by": "..."` when the event came via a proxy/asserted-caller
header). That's how "who did what" queries work in shared deployments.

## What this recipe *doesn't* show

- **OIDC / mTLS / K8s ServiceAccount auth.** Designed but deferred to
  v2.5+. Static bearer table is the shipped Authenticator.
- **Per-caller instruction overlays.** Configure `multi_session.users_dir`
  and drop a `<usersDir>/<identity>/.agents/` directory per caller.
  See the [Multi-session reference](../../docs/site/content/docs/reference/multi-session.md#per-caller-instruction-overlays).
- **Proxy / chat-bot integration pattern.** Bots authenticate as
  themselves and assert the user's identity via `X-Asserted-Caller`.
  See the [Shared-session pattern reference](../../docs/site/content/docs/reference/multi-session.md#shared-session-pattern-chat-bot-integration).
- **Per-session quotas.** Not in v2.4.

## Smoketest

For a scripted walk-through of the same flow (assertions and all), see
[`dev/smoke/multi-session-bearer.sh`](../../dev/smoke/multi-session-bearer.sh).
