# Secrets — created out-of-band

This recipe deliberately does NOT ship a `Secret` manifest. Two secrets
are required at deploy time; both need real values created by the
operator (never checked in). Namespace: `kube-platform`.

## 1. `core-agent-users` (Opaque)

Holds the `users.json` bearer-token table. Referenced by the daemon
Deployment, staged by an initContainer into an emptyDir at
`/etc/core-agent/users.json` (mode 0400). The identities must match
`attach.multi_session` in `.agents/config.hub.json`:
`admin_identities: ["platform-oncall@example.com"]` and
`proxy_identities: ["sa:kube-agents-chat", "sa:lookout-watch"]`.

```bash
cat > /tmp/users.json <<EOF
{
  "version": 1,
  "users": [
    { "identity": "platform-oncall@example.com", "token": "$(openssl rand -hex 32)" },
    { "identity": "sa:lookout-watch",        "token": "$(openssl rand -hex 32)" }
  ]
}
EOF

chmod 0600 /tmp/users.json
kubectl -n kube-platform create secret generic core-agent-users \
    --from-file=users.json=/tmp/users.json
```

Identities:
- `platform-oncall@example.com` — the admin identity that owns every
  incident session (asserted by the watcher's proxy identity).
- `sa:lookout-watch` — the watcher's own identity. Listed in
  `proxy_identities` so it can assert
  `X-Asserted-Caller: platform-oncall@example.com` when creating
  incident sessions from watcher injects.
- `sa:kube-agents-chat` — the chat gateway companion
  (go-steer/switchboard, out of scope for this deploy). Add a token
  entry when you wire up chat; the config already lists it as a proxy
  identity.

## 2. `lookout-watch-token` (Opaque)

The watcher's bearer token, mounted as an env var. It is the SAME token
as the `sa:lookout-watch` entry in `users.json` above.

```bash
WATCHER_TOKEN=$(jq -r '.users[] | select(.identity=="sa:lookout-watch") | .token' /tmp/users.json)

kubectl -n kube-platform create secret generic lookout-watch-token \
    --from-literal=token="${WATCHER_TOKEN}"

rm /tmp/users.json
```

## Rotation

Both secrets are hand-managed. To rotate: regenerate the token(s),
update `users.json`, recreate both Secrets, then restart both pods
(`kubectl -n kube-platform rollout restart deployment core-agent
lookout-watch`).

For production, plug into your secret manager (External Secrets
Operator, GCP Secret Manager CSI driver, Vault, etc.) — this recipe
leaves the choice open.
