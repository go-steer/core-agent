---
title: Permissions
---


The permission gate is the central chokepoint consulted before every tool call. It enforces three things in order:

1. A built-in **bash denylist** that's non-overridable (even `yolo` mode can't run `rm -rf /`).
2. A **path scope** check for file tools — out-of-scope reads/writes either prompt the user or fail.
3. The **mode + allow/deny patterns** from `.agents/config.json`.

---

## Modes

| Mode | Behavior |
|---|---|
| `ask` (default) | Allowlisted calls pass automatically; everything else prompts the user via the configured `Prompter`. With no `Prompter`, prompts fail closed with a clear error. |
| `allow` | Only allowlisted calls pass. Everything else is rejected without prompting — useful for headless / automated runs. |
| `yolo` | All calls pass except those caught by the bash denylist or a deny-pattern. Use with care; intended for trusted local dev. |

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

Symlinks are not followed — the input path is trusted as-is.

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

This list is intentionally conservative — it's not a complete bash sandbox, just a refusal list for the patterns most likely to brick a system by accident.

---

## In-session decisions

When `ask` mode prompts the user, the `Prompter` returns one of:

| Decision | Effect |
|---|---|
| `DecisionDeny` | Reject this call. |
| `DecisionAllowOnce` | Allow this call; prompt again next time the same call is made. |
| `DecisionAllowSession` | Allow this exact request for the rest of the session — same `(tool, key)` pair won't re-prompt. |
| `DecisionAllowSessionTool` | Trust the entire **tool** for the rest of the session — every call to it passes regardless of args. |
| `DecisionAllowAlways` | Allow + caller persists a permanent allowlist entry. The gate also remembers it for the rest of the session so persistence latency doesn't cause a re-prompt. |

`DecisionAllowSessionTool` short-circuits the path-scope check too — once you trust `read_file` for the session, even out-of-scope reads pass without re-prompting. This is the affordance that prevents the "modal-soup" anti-pattern from wide-ranging tool use.

---

## Background subagents and the gate (v1.2.0+)

When `agent.WithBackgroundManager` is wired, every spawned background subagent **inherits the parent's gate by reference**. That has three consequences worth knowing:

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

`PromptRequest` carries everything needed to render a prompt — kind (bash / file write / path scope / generic), tool name, detail string, and the persistence keys to write back if the user picks `DecisionAllowAlways`.

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

`agent.RunAutonomous` would deadlock under `mode: ask` if your tools route through a gate without a `Prompter` — the model's first gated tool call would block waiting for human approval that's never going to arrive. Two options:

- Use `mode: yolo` (or `mode: allow` with an explicit allowlist) for unattended runs.
- Wire `permissions.RefusePrompter` so the agent gets a clean refusal instead of blocking, and pass `agent.WithPermissionsGate(g)` to enable the driver's startup deadlock guard. See [Autonomous runs → Permission modes](/run/autonomous/operations/#permission-modes).

---

## Bridging to ADK toolsets

Permission gating is bridged to ADK via the `tools.GateToolset` wrapper. It wraps any `adktool.Toolset` (an MCP server, a skills bundle, your own custom toolset) so each tool call goes through the gate before execution:

```go
import (
    coretools "github.com/go-steer/core-agent/pkg/tools"
    "github.com/go-steer/core-agent/pkg/permissions"
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
