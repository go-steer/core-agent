---
title: Permissions
---


The permission gate is the central chokepoint consulted before every tool call. It enforces three things in order:

1. A built-in **bash denylist** — best-effort defense-in-depth, not a security boundary (see [below](#bash-denylist)). It catches the laziest `rm -rf /`-class mistakes even in `yolo` mode, but is trivially evadable.
2. A **path scope** check for file tools — out-of-scope reads/writes either prompt the user or fail.
3. The **mode + allow/deny patterns** from `.agents/config.json`.

---

## Modes

| Mode | Behavior |
|---|---|
| `ask` (default) | Allowlisted calls pass automatically; everything else prompts the user via the configured `Prompter`. With no `Prompter`, prompts fail closed with a clear error. |
| `allow` | Only allowlisted calls pass. Everything else is rejected without prompting — useful for headless / automated runs. |
| `yolo` | All calls pass except those caught by the bash denylist or a deny-pattern. Use with care; intended for trusted local dev. |
| `plan` | All tool execution is disabled — read-and-think sessions. The TUI's permission chip cycles out of it (Shift+Tab). |
| `acceptEdits` | File **writes** auto-allow without prompting — **including out-of-scope writes to any path** (see the caution below). Bash and every other tool kind still prompt as in `ask`. Used by the TUI's "acceptEdits" chip to stream a refactor without a diff modal per file. |

:::caution[acceptEdits is not a scoped-writes mode]
`acceptEdits` auto-allows **every** file write, in scope or not — the path scope is **not** a boundary in this mode. That includes `~/.bashrc`, `~/.ssh/authorized_keys`, cron files, systemd units: any path the process can reach. Treat it as **"trust this agent with your filesystem"** and use it only inside a sandbox/container or an equally disposable environment. If what you want is "auto-approve writes, but only under paths I declared", stay in `ask` mode and grant those paths via [`path_scope`](#path-scope) entries (`path_scope.allow`, typed `allow_paths`, or `--allow-path`) instead. Reads are unaffected — an out-of-scope read still prompts. Control-plane files remain the one exception: their elevated prompt fires before the mode is consulted.
:::

No mode — not even `yolo` or `acceptEdits` — auto-approves a write to a **control-plane file** (see below).

Set via `.agents/config.json`:

```json
{
  "permissions": {
    "mode": "ask",
    "allow": ["bash:git status", "bash:git log*"],
    "deny":  ["bash:sudo *"]
  }
}
```

Or programmatically when constructing the gate:

```go
gate := permissions.New(permissions.Options{
    Mode:     permissions.ModeAllow,
    Policy:   policy,
    Scope:    scope,
    Prompter: nil, // headless
})
```

---

## Pattern grammar

Patterns appear in `permissions.allow` and `permissions.deny`. Two forms:

```
<tool>:<glob>     applies only when the request is for <tool>
<glob>            applies to any tool (matched against the request key)
```

The `<glob>` uses `path/filepath.Match` semantics, so it understands `*`, `?`, and character classes. Two convenience extensions:

- **Exact match** comes first: a pattern with no wildcards matches the literal key only (so `bash:git status` matches the literal command, not `git statusabc`).
- **Open prefix** for trailing `*`: `bash:git diff*` matches `git diff`, `git diff main..HEAD`, etc.

### Safe-command guard on bash prefix rules

For **bash**, an open-prefix allow rule only auto-allows a command that is exactly **one simple command with a fully literal argv**. The command string is parsed with a real shell parser; any of the following disqualifies it from matching a `<verb> *`-style rule and sends it to the normal prompt flow instead:

- Chaining or composition: `;`, `&&`, `||`, pipes, background `&`, subshells.
- Redirections of any kind (`>`, `>>`, `<`, `2>&1`, heredocs).
- Any expansion: `$VAR`, `$(...)`, backticks, arithmetic, process substitution.

So `bash:cat *` auto-allows `cat notes.txt` but **not** `cat notes.txt; rm -rf ~` or `cat f > /etc/passwd`. Plain quoting of literal text is fine (`find . -name '*.go'` matches `bash:find *`). Leading `KEY=VAL` environment assignments are skipped when computing the argv, so `CGO_ENABLED=0 go build` matches a `bash:go *` rule.

A small set of **verb profiles** additionally withholds auto-allow when a dangerous predicate appears in the argv of an otherwise read-only verb: `find . -exec …`, `-execdir`, `-ok`, `-okdir`, `-delete`, `-fls`, `-fprint`, `-fprint0`, `-fprintf` all prompt even though `bash:find *` is in the default bundle. A profile hit never hard-denies — it only removes the auto-allow.

**Exact-match rules are exempt** from both guards: if you allowlist the literal string `bash:make build && make test`, that exact string passes. Deny rules are also unaffected — they stay as broad as written.

Examples:

| Pattern | Matches |
|---|---|
| `bash:git status` | exactly `git status` |
| `bash:git *` | any bash command starting with `git ` |
| `read_file:internal/**` | any read_file call with a key starting with `internal/` |
| `mcp:filesystem_read_file` | the namespaced MCP filesystem read tool |
| `skill:jira-triage` | invocation of the jira-triage skill |
| `*foo*` | anything (any tool) whose key contains `foo` |

**Deny always wins.** A deny pattern matched anywhere kills the call, even if an allow pattern also matches.

The "key" of a request is tool-specific:
- For `bash`: the trimmed command string.
- For file tools (`read_file`, `write_file`, `edit_file`, `list_dir`): the resolved absolute path.
- For MCP / skill calls: `<tool_name> <json-args>` (truncated at 200 chars).

The `bash`, `read_file`, `write_file`, `edit_file`, `list_dir`, and `todo` tool names refer to the [built-in tools](/embed/api/#built-in-tools) that ship with core-agent and are enabled by default in the bundled CLI. Use the same names in allow/deny patterns whether you keep the defaults or supply your own implementations under those names.

---

## Path scope

File tools may only touch paths inside the project root, the user-home root, or any explicit pattern in `path_scope.allow`. Out-of-scope access either prompts (in `ask` mode with a Prompter) or fails (everywhere else).

```json
{
  "path_scope": {
    "allow": [
      "/etc/myapp/...",
      "/var/log/myapp.log",
      "~/scratch/*.json"
    ]
  }
}
```

Pattern syntax:

| Form | Meaning |
|---|---|
| Exact absolute path | Only that file. |
| Directory tree ending `/...` | Anything at or under that root. |
| Standard `path/filepath.Match` glob | Glob match against absolute paths. |
| Leading `~` or `~/` | Expanded to `os.UserHomeDir()`. |

Symlinks are resolved (`filepath.EvalSymlinks`) before every scope check, with no opt-out. The file tools follow symlinks at the OS level, so a symlink inside the project root pointing at `/etc`, `~/.ssh`, or `~/.aws/credentials` is classified by its **real** target, not its in-scope name — it prompts (in `ask` mode) or is denied like any other out-of-scope path. New-file writes into a not-yet-existing path are classified by the deepest existing ancestor directory's real location; any resolution failure fails closed (treated as out-of-scope).

### Typed r/w/rw entries + CLI `--allow-path`

`path_scope.allow_paths` is the typed form of `allow` — each entry carries an explicit access mode:

```json
{
  "path_scope": {
    "allow_paths": [
      { "path": "/home/me/sibling-repo/...", "mode": "rw" },
      { "path": "/var/log/myapp.log",         "mode": "r"  }
    ]
  }
}
```

`mode` is one of `r` / `w` / `rw` (long forms `read` / `write` / `readwrite` also accepted). Read-only entries allow reads but still prompt on writes; write-only is uncommon but supported for tools that only append. Composes with the plain `allow` list (which grants both r+w unconditionally, matching the legacy shape).

The `--allow-path PATH:MODE` CLI flag adds one entry inline without touching `config.json`:

```bash
core-agent --allow-path /home/me/sibling-repo:rw --allow-path /var/log/myapp.log:r
```

Repeatable; entries are merged with anything in `path_scope.allow_paths`. Useful for one-off sessions (a sibling checkout, a scratch dir) where you don't want to commit the grant.

---

## Bash denylist

A small set of patterns are rejected for any `bash` call, in any mode, regardless of allow/deny config. These cover the most reliably destructive shell forms:

- `rm -r -f` (in any flag-order combination) targeting `/`, `~`, `$HOME`, etc.
- `dd if=… of=/dev/…`
- `mkfs.*`, `shred …`, `wipefs …`
- `chmod -R <mode> /` and `chown -R <user> /`
- `curl|wget … | sh|bash|zsh|ash|dash` (download-and-execute)
- The classic fork bomb `:(){ :|: & };:`

:::caution[Defense-in-depth, not a security boundary]
The bash denylist is **best-effort defense-in-depth, not a security boundary**. It's a small, pattern-based refusal list for the shell forms most likely to brick a machine by accident (or on a prompt-injected model's first, laziest attempt). It is **trivially evaded** and cannot be made complete — a regex denylist over the full shell grammar is a losing game by construction. Do not rely on it to contain a determined or adversarial command. Known bypass classes it does **not** catch:

- **Quoting / whitespace**: `rm -rf "$HOME"`, `rm -r${IFS}-f ~`.
- **Variable / expansion indirection**: `X=/ ; rm -rf "$X"`, `eval`, `base64 -d | sh`.
- **Staging**: `curl … -o /tmp/x; sh /tmp/x` — each command looks benign on its own.
- **Uncovered targets**: `rm -rf /etc`, `rm -rf ~/important` — the target list is a hard-coded handful, not "everything dangerous."

It is also the **only** bash protection left once a command reaches `yolo` mode or a session/verb grant.
:::

### Hardened posture: allowlist-based execution

For a real bash boundary, don't lean on the denylist — **allowlist** the commands you intend. Run in `allow` mode (or `ask` with a Prompter) and grant only specific commands via `permissions.allow`. Prefix allow rules (`bash:git *`) are guarded so they only auto-allow single literal simple commands — chaining, pipes, redirections, and command substitution fall through to prompting (see [Safe-command guard](#safe-command-guard-on-bash-prefix-rules)). An allowlist is an enumerated set of permitted actions; a denylist is a guess at the set of dangerous ones. The former is a boundary, the latter is a seatbelt.

---

## Control-plane files (elevated writes)

Two tiers of files live under `.agents/`:

- **Instruction-bearing** files — `AGENTS.md`, skills content — shape what the model is told. They stay normally writable; a change takes effect through the model, which is still gated.
- **Privilege-bearing** control-plane files — the agent config (`.agents/config.json`) and the MCP config (`.agents/mcp.json`), in both the project and `~/.agents/` trees — directly control the permission gate, the `hooks` commands, and the stdio MCP servers the daemon spawns. A write here is a self-escalation + persistence vector (add an `allow` entry, flip the mode, register a hook that runs arbitrary shell, add a malicious MCP server) whose effect lands out-of-band on the next session/restart.

Writes to the privilege-bearing files require a **new elevated interactive approval**. This approval is **never** satisfied by `yolo`/`acceptEdits` mode, a session/verb/tool grant, an allowlist entry, or a built-in bundle — only an explicit prompt approval passes. With no interactive prompter available (headless), the write is **denied** with a clear error. Classification runs on the symlink-resolved path, so a symlink pointing at a control-plane file is caught too. Each elevated approval authorizes exactly one write; it is not remembered.

## Hook command execution

Config-driven `hooks` (see [Hooks](/concepts/hooks/)) run shell commands on event boundaries. Because `.agents/config.json` is writable by the model, hook commands are **routed through the permission gate** (a bash-kind check: the built-in denylist, the configured mode, and any deny patterns apply) before they execute. A hook the gate refuses does not run. If the dispatcher has no gate wired, it fails closed and runs nothing.

## In-session decisions

When `ask` mode prompts the user, the `Prompter` returns one of:

| Decision | Effect |
|---|---|
| `DecisionDeny` | Reject this call. |
| `DecisionAllowOnce` | Allow this call; prompt again next time the same call is made. |
| `DecisionAllowSession` | Allow this exact request for the rest of the session — same `(tool, key)` pair won't re-prompt. |
| `DecisionAllowSessionTool` | Trust the specific **tool** for the rest of the session — every call to it passes regardless of args. For namespaced toolsets (MCP, skills) the grant is scoped **per underlying tool** (`mcp/<tool>`), so approving one MCP tool does not trust every tool from every server. |
| `DecisionAllowAlways` | Allow + the **gate** installs a permanent grant: non-path prompts become a live `"<tool>:<key>"` policy pattern, path prompts a subtree-expanded scope entry. When a `GrantStore` is wired (`permissions.Options.GrantStore` / `Gate.SetGrantStore`) the grant is also persisted — the bundled CLI wires the config-backed store, which writes `permissions.allow` patterns and typed `path_scope.allow_paths` entries (carrying the grant's `r`/`rw` access) into `.agents/config.json`. Persist failures surface to the gated call rather than silently downgrading to session-only. Without a store, the grant lasts the process lifetime. |

`DecisionAllowSessionTool` suppresses the mode prompt for **in-scope** operations, but it does **not** drop the path boundary: an out-of-scope read or write still escalates via the path-scope prompt every time, even for a session-trusted file tool. Trusting `read_file` for the session silences repeat prompts for files inside your scope; it does not grant the tool the whole filesystem.

---

## Background subagents and the gate (v1.2.0+)

When `agent.WithBackgroundManager` is wired, every spawned background subagent **inherits the parent's gate by reference**. That has three consequences worth knowing:

**The spawn itself is gated too (v2.9+).** `spawn_agent` and `spawn_remote_agent` call the gate before anything launches, so plan-first, the allow/deny policy, and the ask prompt all apply to *creating* a subagent — not just to what it does afterwards. Before v2.9 the gate governed only the second half, which under `plan_mode: "required"` meant a parent with no plan recorded was told `spawn_agent` would be denied, called it, and it ran.

The key a rule matches is **the subagent being launched**, not the goal (a goal is fresh prose every call, so no rule could match it twice):

```
spawn_agent:cluster       one preconfigured subagent
spawn_agent:ad-hoc:*      any inline-persona spawn, whatever the model names it
spawn_agent:*             all delegation
```

**One rule covers both doors.** A declarative subagent is reachable two ways — `spawn_agent {agent: "cluster"}` and, because `agent.WithSubagents` also registers it as a parent tool, a direct `cluster(request: …)` call. Both are matched under the `spawn_agent` bucket, so `deny: ["spawn_agent:cluster"]` means `cluster` does not run, not that it does not run asynchronously. (Library callers wiring `agent.NewSubagentTool` themselves pass the gate via `SubagentOptions.Gate`; `agent.WithSubagents` fills it in from `agent.WithGate`.)

`stop_agent` is deliberately **not** gated in any mode. It cancels, so every denial of it leaves running exactly what the model was trying to halt. To hand a subagent delegation without cancellation — or neither — withhold the tools rather than gating them (`subagents[].tools`, which already withholds both by default).

1. **Session-level approvals apply tree-wide.** If you approve `DecisionAllowSessionTool` for `bash` while a subagent is asking, every subagent (including future siblings) gets the same grant for the rest of the session. The gate has no per-subagent allow-state today; the whole tree shares one map.

2. **Prompts include source attribution.** `permissions.PromptRequest` carries a `Source` field that `StdinPrompter` renders in the heading: `[<subagent-name>] bash wants to run: ...`. So when a subagent triggers a prompt, you know which one is asking. Empty `Source` (the parent's own tool calls) renders unchanged. The gate populates `Source` from a context value `permissions.WithSubagentSource(ctx, name)` that the spawn machinery stamps on every subagent's ctx.

3. **Concurrent prompts serialize.** Multiple background subagents racing for `os.Stdin` would deadlock or interleave garbage. Wrap any interactive prompter in `permissions.Serialize(...)` before handing it to the gate:

   ```go
   prompter := permissions.Serialize(permissions.StdinPrompter(os.Stdin, os.Stderr))
   gate := permissions.New(permissions.Options{Prompter: prompter, Mode: permissions.ModeAsk})
   ```

   The bundled CLI does this automatically. Library callers using their own gate construction with background subagents should too.

**Deferred (v1.3+):** bounded permission subsets where the spawner grants the subagent only part of its own permissions and the spawner's *model* arbitrates out-of-subset requests via an injected synthetic prompt. Today, "inherit the parent's gate" is the only mode.

---

## Recommendations

After a session in `ask` mode, the gate exposes an audit log of every approval. `permissions.Recommend(approvals)` turns that log into a prioritized list of suggested permanent allowlist entries:

```go
recs := permissions.Recommend(gate.Approvals())
permissions.SortRecommendations(recs)
for _, r := range recs {
    fmt.Printf("%-40s  %s\n", r.Pattern, r.Reason)
}
```

Heuristics built in:

- A single approval becomes an exact pattern (`bash:git status`).
- Multiple bash approvals sharing a leading verb collapse to a verb-glob (`bash:git *`).
- Multiple file approvals sharing a directory prefix collapse to a directory glob (`read_file:internal/tui/**`).
- Otherwise, a tool-wide suggestion (`bash:*`) is offered as a fallback the user can opt out of.

`SortRecommendations` puts non-wildcard patterns above wildcards so the safer recommendations surface first.

---

## Implementing a `Prompter`

Hosts that can interact with the user implement the `Prompter` interface:

```go
type Prompter interface {
    AskApproval(ctx context.Context, req PromptRequest) (Decision, error)
}
```

`PromptRequest` carries everything needed to render a prompt — kind (bash / file write / path scope / generic), tool name, detail string, and the persistence keys the gate uses to build the grant if the user picks `DecisionAllowAlways`. A custom Prompter only returns the decision; installing and persisting the grant is the gate's job (via the wired `GrantStore`).

The bundled `cmd/core-agent` does not currently ship a Prompter — `ask` mode in the REPL fails closed. To use `ask` mode interactively, embed the library in your own host and supply a Prompter. See [Library API → Prompter](/embed/api/#prompter).

---

## Headless / CI use

For non-interactive runs (CI, batch jobs), use:

```json
{
  "permissions": {
    "mode": "allow",
    "allow": [
      "bash:go test ./...",
      "bash:go vet ./...",
      "read_file:**"
    ]
  }
}
```

`mode: allow` rejects anything not on the allowlist, which is what you want when there's no human in the loop.

### Autonomous runs and the gate

`autonomous.Run` would deadlock under `mode: ask` if your tools route through a gate without a `Prompter` — the model's first gated tool call would block waiting for human approval that's never going to arrive. Two options:

- Use `mode: yolo` (or `mode: allow` with an explicit allowlist) for unattended runs.
- Wire `permissions.RefusePrompter` so the agent gets a clean refusal instead of blocking, and pass `autonomous.WithPermissionsGate(g)` to enable the driver's startup deadlock guard. See [Autonomous runs → Permission modes](/run/autonomous/operations/#permission-modes).

---

## Bridging to ADK toolsets

Permission gating is bridged to ADK via the `tools.GateToolset` wrapper. It wraps any `adktool.Toolset` (an MCP server, a skills bundle, your own custom toolset) so each tool call goes through the gate before execution:

```go
import (
    coretools "github.com/go-steer/core-agent/v2/pkg/tools"
    "github.com/go-steer/core-agent/v2/pkg/permissions"
)

gated := coretools.GateToolset(myToolset, gate, "my-namespace")
```

The `namespace` argument is the policy bucket — it's what the allow/deny patterns use as the tool name (e.g. `mcp:`, `skill:`, or your own).

---

## Auditing

Every non-deny approval is recorded in the gate's session log:

```go
for _, a := range gate.Approvals() {
    fmt.Printf("%s  %s  %s  %s\n", a.At.Format(time.RFC3339), a.Tool, a.Decision, a.Key)
}
```

This is the data source for `Recommend()`. It's also useful for post-hoc auditing of what tool calls were approved during a run.
