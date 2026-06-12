# users.json — operator notes

The `users.json` shipped in this directory is **example placeholder
data** — every token literal is `tok_<name>_PLACEHOLDER_REGENERATE_BEFORE_USE`.

## Before running the recipe

Either install the file to its runtime location with regenerated tokens:

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

Or copy the placeholder file with strict mode and edit in place:

```bash
install -m 0600 -o "$USER" users.json /tmp/multi-session-bearer/users.json
$EDITOR /tmp/multi-session-bearer/users.json
```

## File-mode requirement

`auth.LoadUsersFile` **rejects** group- or world-readable tables at
startup. The loader checks `mode & 0o077 == 0` — exactly mode `0600`
or stricter (e.g. `0400`). The reasoning is the same as for an SSH
private key: bearer tokens are credentials, not configuration.

Skipped on Windows (Unix mode bits don't map cleanly).

## Token rotation

Bearer tokens have no built-in rotation. To rotate:

1. Generate the new tokens, write them to the file.
2. Reload the daemon: `curl -X POST -H "Authorization: Bearer $OPS_TOKEN" \
   http://127.0.0.1:7777/sessions/<sid>/reload`.
3. Distribute the new tokens to operators.

A `core-agent users rotate` CLI is deferred to v2.5+.

## Where to NOT put this file

- **Anywhere world-readable.** Loader rejects.
- **Anywhere group-readable** that includes accounts who shouldn't
  hold every operator's credential. Loader rejects.
- **In a git repository.** This file IS the credentials — leakage
  exposure is high. Use a secret manager (Vault, Cloud Secret
  Manager, sealed-secret, etc.) and materialize on the host at
  process start.
