# Local attach listeners need a credential, and tokens can come from files

Status: implemented ([#1201](https://github.com/go-steer/core-agent/issues/1201) items 2 and 3).
Precedent: [#1157](https://github.com/go-steer/core-agent/issues/1157) (the daemon withholds its credentials from children) and [PR #1210](https://github.com/go-steer/core-agent/pull/1210) (attach clients *take* `--token-env`).

## Problem

The attach token answers the agent's own permission prompts. #1157 and #1210 keep it out of the agent's reach. Two routes stayed open:

1. **No credential at all.** `pkg/attach` refuses an unauthenticated *non-loopback* bind (#376) but lets a loopback one start with a log warning. A Unix socket's only auth is its 0600 file mode. Both rest on "local means this machine's user", which is exactly the agent once it has the `bash` tool: `curl 127.0.0.1:7777/sessions/<id>/perms/respond`, or `curl --unix-socket`, needs nothing to steal.
2. **The token sits in some process's environment.** `--attach-token` and `--token-env` name an env var. The daemon withholds it from children and goes non-dumpable, and clients unset it and go non-dumpable, but neither can touch the exported variable in the shell that launched the process, nor the milliseconds between a client's exec and its read. In the self-dev rig the agent is already running when the operator starts the TUI.

## Design

### Item 2: refuse a token-less local listener when the agent has a shell

`cmd/core-agent` checks after `attach.NewServer` and before `Bind`:

- The listener has **no credential gate**. That is `attach.Options.Authenticated()` returning false, the same predicate #376 uses. A gate means a bearer token, an mTLS client CA *with* a server cert and key, or multi-session auth with `allow_anonymous: false`.
- The listener is **not read-only**.
- **The `bash` tool is registered**: `hasToolNamed(builtinTools, "bash")`. That is the catalog the agent and every `POST /sessions` session are built from.
- **No `--attach-allow-unauthenticated-local`.**

When all four hold, the daemon exits 2 with an error that names the route and the ways out: a credential the agent cannot read (`--attach-token`, which #1157 withholds from bash, or `--attach-token-file` naming a pipe or an unreadable path), mTLS or multi-session auth, `--attach-readonly`, removing bash, or the opt-out flag. With the opt-out it starts and prints a warning saying what the agent can now do.

### Item 3: tokens from files

- Daemon: `--attach-token-file=<path>` / `attach.token_file`.
- Clients: `--token-file=<path>` on `core-agent-tui`, `core-agent attach` and `core-agent ls`.

All of them go through `childenv.TakeFile`: read once at startup (the daemon only when a listener or hub registration will use it), validate, then `Protect()` (non-dumpable), because the token now lives in process memory and `/proc/<pid>/mem` is as open to a same-user child as `environ`. The daemon resolves the token once in `resolveAttachToken`, right after the credentials are withheld. The listener and hub registration share that value, so nothing re-reads the environment later. The old code read `os.Getenv` twice.

## Settled decisions (do not relitigate)

1. **Refuse, not warn.** #376 has warned about a token-less loopback listener since v2.8, and #1201 exists because a warning is not a gate. Nothing in this repository runs a token-less local listener with bash. The `dev/uat/attach` and `dev/uat/self-dev` rigs, smoke tests 09/10 and every example deployment set a token or multi-session auth (checked by grep while writing this). The only users broken are out-of-tree ones, and their failure is loud: exit 2, a message naming the flag to set, and a `Breaking Changes` entry. A silent break is not possible.
2. **The opt-out is a CLI flag only, with no config field.** Accepting the risk is a per-invocation operator decision. A config field would live in `.agents/config.json`, a file the agent itself can edit with `write_file`, and the change would apply on the next restart.
3. **The condition is the `bash` tool, not "any way to exec".** An MCP server that exposes a shell, a hook, or a skill script can also open a socket, and this check does not see them. Bash is the one exec route this build both owns and can detect from its catalog, so the policy branches on what is true about this build (AGENTS.md's tool-description rule applied to startup). The docs still say to give a listener a token whenever the agent has a shell.
4. **Unix sockets get the same rule as loopback.** Socket file permissions stop other users, never the socket owner's own processes.
5. **The policy lives in `cmd/core-agent`, not in `pkg/attach`.** The library cannot see the tool catalog, and changing `NewServer`'s default would break every embedder and test that runs a token-less server over a trusted transport. `Options.Authenticated()` is exported so an embedder can apply the same rule with the same definition of "authenticated".
6. **The check runs after `NewServer`.** A non-loopback unauthenticated bind is therefore refused by #376's own error, and the opt-out flag can never weaken #376.
7. **Naming both sources is an error, not a precedence rule.** That covers a token file together with a token env var, on either side. Two sources for one secret is a misconfiguration. Picking one silently is how an operator ends up authenticating with the stale token they meant to replace. On the daemon, CLI-beats-config treats the two sources as **one setting**: naming either on the CLI drops both config values, so a CLI `--attach-token-file` overrides a config `token_env` instead of colliding with it.
8. **The file is kept.** It is never deleted, because a Kubernetes secret volume is read-only and deleting an operator's file is surprising. The consequence is stated wherever the flag is documented, including the refusal message: a regular file is out of the agent's reach only if the agent cannot read that path, and against the agent a 0600 file it can `cat` is weaker than `--attach-token`, which is withheld from its bash. A FIFO or bash process substitution (`--token-file <(pass show attach | head -n1)`) is the posture that leaves nothing behind, and `TakeFile` accepts pipes for that reason. A process substitution's fd (`/dev/fd/63`) is inherited without close-on-exec, so the daemon's children inherit the pipe's read end too. By then the daemon has read the pipe to EOF and the writer has exited, so nothing can be read from it. That was observed in the e2e test, which found zero bytes.
9. **Mode check.** A regular file with any "other" bit or group write is refused. Group read is allowed, because a Kubernetes secret with `fsGroup` and `defaultMode: 0440` is the standard way to hand a token to a non-root container. The Kubernetes default `0644` is refused, with a message naming `defaultMode`. Pipes, character devices and sockets are not mode-checked: `/dev/fd/N` is a pipe and its mode says nothing about who can read it.
10. **Content rules.** Surrounding whitespace, including the trailing newline, is trimmed. The rest must be non-empty, with no internal whitespace or control characters (RFC 6750 `b64token`). Files over 64 KiB are refused. Errors never echo content. A file that cannot be read is a startup error. An empty `--token-env` stays the supported no-token posture, but nobody names a file to mean "no token".
11. **Hardening failure is a warning.** If `Protect()` fails, the process warns and keeps the token, the same contract as `Take` and `withholdDaemonCredentials`. `TakeFile` returns that error separately from the read error, so a caller cannot turn it into an outage by checking one value.
12. **A read-only listener is exempt.** `--attach-readonly` returns 403 for every write in the transport middleware, before any handler runs and whatever the credentials. So a read-only listener cannot answer a prompt, add a rule or reset a guardrail. What it still serves is the agent's own transcript, which is not this threat.
13. **A client CA without TLS is not a gate.** `LoadTLSConfig` never reads `ClientCAFile` without a server cert, and `Serve` then speaks plain HTTP, but `listenerAuthenticated` counted the CA alone as authentication. That let `--attach-client-ca` by itself satisfy #376 on a non-loopback bind, and it would have satisfied this check too. The predicate now needs the cert and key as well. This tightens #376, and it is called out as a security fix.
14. **The daemon reads the token only when something uses it.** That means a listener or hub registration. A config `attach.token_file` must not make every `core-agent -p` fail on the file, or block forever opening a FIFO nobody writes to. The same goes for `--attach-token` with no listener, which has always been ignored.
15. **No attach protocol change.** The refusal happens at startup and token files only change where the daemon and clients read the token. The wire is untouched, so there is no version bump.

## Out of scope

- **#1201 item 4** (the multi-session bearer table file, `pkg/auth/users.go`) and **item 5** (the root/`CAP_SYS_PTRACE` startup warning). Another change covers those.
- **Detecting other exec routes** (MCP shell servers, hooks, skill scripts) for the item 2 policy. See decision 3.
- **The webhook sink** (`dev/webhook-sink`). It has its own `--bearer-env` flag, does not share the client flag code, and is a dev tool. #1210 already makes it take its token.
- **`tools.call_peer.token_env`**, the token this agent presents to *other* daemons. It is not this daemon's attach credential.
- **Library embedders** (`examples/attach-daemon`, `examples/compose-multi-session`) keep #376's posture. They build their own tool sets, and the exported predicate is how they opt in.
- **Making a regular token file unreadable to the agent.** Same user, same file. That takes a different user or a FIFO, as the docs say.
