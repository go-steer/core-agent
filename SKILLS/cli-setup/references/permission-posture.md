# Permission posture

Reference for the `cli-setup` skill. Fetch when the user is choosing between modes, writing allow patterns, or hitting too many (or too few) prompts.

## The three modes

| Mode | When the agent asks | When you'd use it |
|---|---|---|
| `ask` (default) | Before every gated tool call | Interactive use, your terminal, first-time setup |
| `allow` | Only when a call falls outside your `allow` patterns | Daily driving with trust built up |
| `yolo` | Never (the gate is bypassed for non-denylist calls) | CI, scripted runs, known-safe tool surfaces |

Set in `.agents/config.json` under `permissions.mode`. CLI flag `--yolo` is equivalent to `mode=yolo`.

**Recommendation:** start with `ask`. Switch to `allow` once the user has approved enough common operations interactively (the in-session "always allow" choice adds entries to the allowlist automatically). Skip `yolo` unless you have a specific reason.

## Pattern grammar for allow / deny

Patterns scope to a specific tool via `<tool>:<pattern>` syntax. For path-scoped tools (`read_file`, `write_file`, etc.) the pattern matches against the file path; for `bash`, the pattern matches against the command string.

| Token | Meaning |
|---|---|
| `*` | Matches one path segment (no `/`) or one shell token |
| `**` | Recursive match (any depth) |
| `<tool>:<pattern>` | Restricts the rule to that tool |
| `<pattern>` (no tool prefix) | Applies to all tools that take a path arg |

Examples:

```json
{
  "permissions": {
    "mode": "allow",
    "allow": [
      "bash:git status",
      "bash:git diff*",
      "bash:go vet ./...",
      "bash:go build ./...",
      "read_file:internal/**",
      "read_file:cmd/**",
      "grep:**",
      "glob:**",
      "list_dir:**"
    ],
    "deny": [
      "bash:rm -rf *",
      "write_file:internal/billing/**"
    ]
  }
}
```

**Deny always wins.** A `deny` entry overrides any `allow` entry on the same call. Don't rely on `deny` for security — see "Bash denylist" below for the immutable floor.

## In-session "always allow"

When `mode=ask`, every gated call prompts. The prompt offers four buttons:

- **Allow once** — proceeds with this single call; doesn't persist.
- **Always allow this tool** — adds `<tool>:*` to the allowlist for the session AND persists to `.agents/config.json` if writable.
- **Always allow this exact call** — adds the specific call as an allow pattern.
- **Deny** — refuses; the model sees a permission-denied response.

This is the recommended way to build the allowlist. You don't write patterns by hand; the agent gradually trains its own posture.

## Bash denylist

A handful of bash commands are on a **non-overridable** denylist. You can't allowlist past them, you can't disable the deny via `mode=yolo`:

- `rm -rf /` and close variants
- `dd if=/dev/zero of=/dev/...`
- `mkfs.*`, `fdisk` against real devices
- `curl | sh` (any pipe to shell)
- Direct fork-bomb patterns

This is a substrate-level safety floor. If the model needs one of these, the operator runs it manually outside the agent.

## Path scope (the broader safety floor)

By default, file tools (`read_file`, `write_file`, `edit_file`, `delete_file`, `list_dir`, `glob`, `grep`) can only operate inside two roots:

- The directory tree containing `.agents/` (i.e., the project root)
- The user's home directory (`$HOME`)

Outside those, operations are refused regardless of mode. The path scope is enforced even in `yolo` mode — it's a safety floor below the permission gate.

To expand:

```json
{
  "path_scope": {
    "allow": [
      "/var/log/myapp/...",      // tree (the ... is required)
      "/etc/myapp/config.yaml"   // exact path
    ]
  }
}
```

CLI equivalent: `--allow-path /var/log/myapp:r` (read-only), `--allow-path /var/log/myapp:rw` (read-write). The CLI flag is repeatable.

## Common configurations

### "I'm exploring; want to see what the agent does"

```json
{ "permissions": { "mode": "ask" } }
```

Default. Every gated call prompts. Highest friction but maximum visibility.

### "I'm using this agent regularly; reasonable trust"

```json
{
  "permissions": {
    "mode": "allow",
    "allow": [
      "bash:git status",
      "bash:git diff*",
      "bash:go vet ./...",
      "read_file:**",
      "read_many_files:**",
      "grep:**",
      "glob:**",
      "list_dir:**"
    ]
  }
}
```

Read-side runs without prompting. Writes + broader shell still gate. Build the allowlist via in-session "always allow" prompts; the entries land here automatically.

### "Read-only agent — docs writer, code explainer"

```json
{
  "tools": {
    "disable": ["bash", "write_file", "edit_file", "delete_file"]
  },
  "permissions": {
    "mode": "allow",
    "allow": [
      "read_file:**",
      "read_many_files:**",
      "grep:**",
      "glob:**",
      "list_dir:**"
    ]
  }
}
```

The write tools are removed from the agent's tool list entirely — it can't even propose a write call. Combine with `mode=allow` for the read tools so nothing prompts.

### "CI run — fully automated, known-safe tool surface"

```bash
core-agent --yolo --no-checkpoint --no-compact -p "..."
```

Or in config:

```json
{ "permissions": { "mode": "yolo" } }
```

Use only when the prompt + tool surface are constrained. The bash denylist + path scope still apply.

## Diagnosing "too many prompts"

| Symptom | Likely cause | Fix |
|---|---|---|
| Prompting on every `read_file` | `mode=ask` with no allowlist | Use "always allow" buttons to build allowlist OR switch to `mode=allow` with `read_file:**` |
| Prompting on every `git` invocation | `bash:*` not allowlisted | Add `bash:git *` or specific subcommands |
| Prompting on `read_file` to home directory | Path-scope mismatch | Add `path_scope.allow` entry for the relevant subtree |
| Prompting on a tool the agent should NEVER call | (You may want to deny) | Add `<tool>:*` to `deny` list |

## Diagnosing "agent did something I didn't approve"

The agent should NEVER take an action that bypasses the gate. If it did:

1. Check `permissions.mode` — is it `yolo`? Yolo bypasses everything except the denylist + path scope.
2. Check the allowlist — did a pattern match more broadly than expected? `bash:*` matches everything.
3. Check the audit log (`--session-db`) for the exact request. If the gate genuinely allowed something it shouldn't have, file an issue with the audit-log entry.

The gate is the audit trail. If the model did X, the audit log shows the gate decision that permitted X.
