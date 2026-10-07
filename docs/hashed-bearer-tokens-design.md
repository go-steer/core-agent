# Hashed bearer tokens (#1213 P2)

Prerequisite P2 of the self-development soak
([`selfdev-soak-design.md`](selfdev-soak-design.md)): the agent must not
be able to forge its own task.

## The problem

In multi-session mode the daemon authenticates callers against
`users.json` (`attach.multi_session.auth.table_file`). Each row's
`token` is the plaintext bearer credential. The table is mounted in the
pod whose `bash` the agent drives, and the agent runs as the daemon's
user, so file modes don't keep it out. An agent that reads the table
learns the dispatcher's token and can `POST /inject` a "task" of its own
as `sa:selfdev-dispatcher`, which `task_from` trusts.

#1267 refused the table to every agent file tool, to `@include`, and to
a `bash` command that names it. The `bash` check is a seatbelt: a glob,
a variable or `cd` plus a bare name still reach a readable file. The
gate sees a command string, not the files the shell opens.

## The fix

A row stores `token_sha256`, the hex SHA-256 of the token, instead of
the token. The plaintext lives only with its holder. The daemon hashes
each presented token and compares digests. An agent that reads the whole
table learns nothing it can authenticate with. That closes the read
route completely and needs no extra privilege in the pod.

```json
{ "identity": "sa:selfdev-dispatcher", "token_sha256": "<64 hex>" }
```

Operators produce the value with `core-agent auth hash-token`, which
reads the token from stdin.

## Settled decisions (do not relitigate)

1. **Unkeyed SHA-256, not bcrypt / scrypt / argon2.** Password hashes
   exist to slow down guessing a low-entropy secret a human chose. A
   bearer token is minted random: `openssl rand -hex 32` is 256 bits.
   There is no dictionary to slow down, and a preimage search on
   SHA-256 is the whole attack. A slow hash would add its cost (tens of
   milliseconds by design, plus memory for argon2) to *every*
   authenticated request. That includes each SSE reconnect, every
   `/perms/respond` and every webhook inject, because the daemon has no
   session cookie to amortise it over. And it would buy nothing against
   a random token. The cost of this choice is stated plainly in the
   docs: a digest only hides a token too random to guess. So
   `hash-token` warns below 32 characters, and every generator in the
   tree mints 32 random bytes.
2. **No key (HMAC) and no salt.** A key has to live where the daemon can
   read it, which is where the agent can read it too, so it adds a
   second file to protect and no protection. A per-row salt defends
   against precomputed tables of *likely* secrets. Random 256-bit tokens
   have no likely values to precompute.
3. **On-disk field: `token_sha256`, alongside the legacy `token`; a row
   sets exactly one.** Both set, neither set, or a `token_sha256` that
   isn't 64 hex characters is a load error. Upper-case hex is accepted
   because it names the same digest. The algorithm is in the field name,
   so a future algorithm is a new field, not a format to sniff.
4. **One comparison path.** At construction the authenticator reduces
   every row to a 32-byte digest, including a plaintext row, which it
   hashes then. A request is hashed once and compared against every row
   with `subtle.ConstantTimeCompare`, with no early exit. Time depends
   on the table size, which the operator sets and isn't secret, and not
   on which row matched. The authenticator keeps no plaintext, whichever
   shape the table used. Comparing fixed-size digests also drops the
   length leak that comparing raw tokens of different lengths had.
5. **Collisions are checked on digests.** The loader computes every
   row's digest and rejects two equal ones, so a plaintext row and a
   hashed row for the same token collide too. The error names both
   identities and neither value.
6. **Plaintext rows keep working, with a loud startup warning.** They
   are not refused. Every table deployed today is plaintext, including
   the GKE recipes. Those pin a *released* image (2.9.0), which would
   itself reject `token_sha256` as an unknown field, so they can't
   migrate until their pin moves. Refusing would break every upgrade
   for a property only some deployments need. The warning goes to
   stderr once per boot. It names each plaintext identity (never a
   token) and gives the `hash-token` command, and the startup summary
   counts the plaintext rows. A deployment that needs the guarantee,
   like the soak, gets it by hashing every row. `PlaintextIdentities()`
   being empty is the testable statement of "reading this table yields
   nothing".
7. **Schema version stays 1.** The field is additive. A loader that
   predates it rejects a `token_sha256` row as an unknown field
   (`DisallowUnknownFields`). That's the fail-closed answer a downgrade
   needs: refuse to start, rather than drop the row or authenticate
   something else. Bumping the version would instead break every
   existing plaintext table on older daemons for no gain.
8. **`core-agent auth hash-token` reads stdin, never argv.** A token on
   the command line lands in shell history and `/proc/<pid>/cmdline`.
   Any argument is refused, and the refusal doesn't echo it, because the
   argument is probably the token. One trailing `\n` / `\r\n` is
   dropped (`echo`, a file's last line). Any other whitespace or control
   character is refused rather than hashed: that digest would match
   nothing a client sends, and the failure would surface at request
   time with nothing to say why. On a terminal it prompts without echo
   (`term.ReadPassword`). It's peeled off in `main()` before
   `flag.Parse` like `attach` and `ls`, reads no config, and is exempt
   from the harness config-pin gate for the same reason.
9. **No credential value appears in any log or error.** That covers load
   errors (missing, both, malformed, collision), the startup warning,
   the summary line and the CLI's errors. A malformed `token_sha256`
   isn't quoted, because the likeliest malformed value is a plaintext
   token pasted into the wrong field.
10. **Generators in the tree emit hashed rows where the daemon reading
    them is built from this tree.** `dev/tools/gen-users-json` writes
    `token_sha256` (via `openssl dgst -sha256`, byte-identical to
    `hash-token`) and prints the tokens once on stderr.
    `examples/compose-multi-session` uses `auth.HashToken`.
    `dev/smoke/09` runs a mixed table and asserts the warning names
    exactly the plaintext rows and that no token reaches the log. The
    GKE recipes' `gen-tokens.sh` and the static
    `examples/multi-session-bearer/users/users.json` stay plaintext
    (decision 6). The latter's walkthrough reads tokens back out of
    the file with `jq`, and its README now shows the hashed form.

## Out of scope

- **The write route.** Hashing stops an agent *reading* a credential
  out of the table. An agent that can *write* the table can add its own
  row for the next boot. Mount the table read-only (a Kubernetes Secret
  volume already is). The soak does.
- **Running the agent's tools as a different user** from the daemon
  (#1201's broader answer). It protects every credential file, not just
  this one, but needs `CAP_SETUID` or a tool sidecar. It's deferred
  unless the soak finds a route hashing doesn't close.
- **The single-user attach token** (`--attach-token-file` /
  `attach.token_file`). It's one token, and the daemon holds it in
  plaintext to compare. It could take the same treatment later. Its
  exposure is covered by #1266/#1267.
- **The readable-credential-file startup warning** (`cmd/core-agent`
  `credentialFileExposure`) still fires for a fully hashed table. Making
  it hash-aware would mean threading the parsed table into the
  credential-file guard. The cost of leaving it is one extra warning
  line, and the docs say so.
- **Refusing plaintext rows**, by default or behind a config knob.
  That's revisitable at the next major version, once the released
  images the recipes pin can read `token_sha256`.
- **A `core-agent users` management CLI** (add / rotate / remove).
  It's deferred as before. `hash-token` is the one primitive operators
  need to produce a row.
- **Clients.** `core-agent-tui`, `core-agent attach` / `ls`, the
  webhook injectors and the dispatcher hold the plaintext token they
  present. Nothing changes for them.
