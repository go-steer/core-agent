# Secrets — created out-of-band

This recipe deliberately does NOT ship a `Secret` manifest. Two
secrets are required at deploy time; both need to be created by the
operator with real values (never checked in to the repo):

## 1. `core-agent-users` (Opaque) — namespace `agent-triage`

Holds the `users.json` bearer-token table. Referenced by the
daemon's Deployment as a projected volume mounted at
`/etc/core-agent/users.json` (mode 0400).

```bash
# Generate three tokens (edit the identity list to suit your team):
cat > /tmp/users.json <<EOF
{
  "version": 1,
  "users": [
    { "identity": "sre-oncall@example.com", "token": "$(openssl rand -hex 32)" },
    { "identity": "sa:k8s-event-watcher",   "token": "$(openssl rand -hex 32)" }
  ]
}
EOF

chmod 0600 /tmp/users.json
kubectl -n agent-triage create secret generic core-agent-users \
    --from-file=users.json=/tmp/users.json
rm /tmp/users.json
```

The daemon's `attach.multi_session.enabled: true` + the identity list
above give you:
- `sre-oncall@example.com` — the admin identity that owns every
  incident session (via the sidecar's proxy assertion).
- `sa:k8s-event-watcher` — the sidecar's own identity. Listed in
  `attach.multi_session.proxy_identities` so it can assert
  `X-Asserted-Caller: sre-oncall@example.com` when creating sessions.

## 2. `k8s-event-watcher-token` (Opaque) — namespace `agent-triage`

Holds the sidecar's bearer token separately. It's the SAME token as
the `sa:k8s-event-watcher` entry in `users.json` above, mounted as an
env var into the sidecar container.

```bash
# Reuse the token from step 1 — either extract it, or generate a
# fresh one and update users.json to match.
WATCHER_TOKEN=$(jq -r '.users[] | select(.identity=="sa:k8s-event-watcher") | .token' /tmp/users.json)

kubectl -n agent-triage create secret generic k8s-event-watcher-token \
    --from-literal=token="${WATCHER_TOKEN}"
```

## Rotation

Both secrets are hand-managed. To rotate: regenerate the token(s),
update `users.json`, recreate both Secrets, then restart both pods
(`kubectl -n agent-triage rollout restart deployment core-agent
k8s-event-watcher`). No downtime beyond the rolling restart.

For real production, plug into your secret manager (External Secrets
Operator, GCP Secret Manager CSI driver, HashiCorp Vault, etc.) — this
recipe leaves the choice open because different orgs have different
posture.
