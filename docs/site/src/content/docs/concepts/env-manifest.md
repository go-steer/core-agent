---
title: Environment variables (env.yaml)
---


Agent bundles that ship in containers, pods, or systemd units frequently need deployment-specific values — GCP project, cluster name, oncall address, ticket prefix, API tokens. `.agents/env.yaml` (or `.env.json`) is core-agent's declaration file for those values: the recipe author lists which env vars the bundle expects, the daemon validates them at boot, and `${env:VAR}` references throughout AGENTS.md, skill files, and `mcp.json` get resolved to the actual environment values.

Bundles without a manifest keep working unchanged — the mechanism is opt-in. Only bundles that ship an `env.yaml` (or `env.json`) get manifest-driven validation and interpolation.

**In `AGENTS.md` and skill files, `${env:VAR}` does nothing without a manifest.** There is no ambient-env fallback there for a bundle that ships no `env.yaml`: the placeholder is handed to the model as literal text. (`mcp.json` is the exception — its `env` / `headers` values have always interpolated straight from the process env, with or without a manifest.) The instruction-side behaviour is the opt-in working as designed, but it is easy to walk into by accident, so the daemon [says so at boot](#surviving-placeholders) whenever loaded content still contains `${env:…}` placeholders after the load.

---

## Quick start

Drop a manifest next to `AGENTS.md`:

```yaml
# .agents/env.yaml
version: 1
env:
  - name: GCP_PROJECT
    required: true
    description: GCP project ID this daemon operates in
    used_by: [AGENTS.md]
  - name: ONCALL_EMAIL
    required: false
    default: unassigned@example.com
    description: CC address for INCIDENT SUMMARY escalation blocks
  - name: SLACK_TOKEN
    required: true
    sensitive: true
    description: Bearer token for the Slack MCP
    used_by: [mcp.json]
```

Reference them in any instruction file with the `${env:VAR}` syntax:

```markdown
# .agents/AGENTS.md
You are the on-call agent for `${env:GCP_PROJECT}`.
When escalating, CC `${env:ONCALL_EMAIL}`.
```

Set the env vars via whatever mechanism your runtime provides:

- **Kubernetes**: `env:` on the container, or `envFrom:` a ConfigMap.
- **Docker**: `-e GCP_PROJECT=...` or `--env-file`.
- **systemd**: `Environment=GCP_PROJECT=...`.
- **Local dev**: shell `export` or a `.env` file.

The daemon reads env at process start, validates required vars, and fails loud with a clear error if anything's missing.

---

## Schema

Both `env.yaml` and `env.json` share this shape. Shipping both files in the same `.agents/` directory is an error (ambiguous which one wins).

| Field | Type | Purpose |
|---|---|---|
| `version` | int | Currently `1`. Future breaking changes bump this and old versions get rejected with a clear upgrade path. |
| `env` | list | Env-var entries; order irrelevant. |

Each entry:

| Field | Type | Purpose |
|---|---|---|
| `name` | string | Env var name. Must be a valid identifier (letters, digits, underscore; not starting with a digit) — the same shape as `${env:NAME}` accepts. |
| `required` | bool | If true and the env var is unset at boot, daemon fails to start with a clear error. Default false. |
| `default` | string | Value used when `required: false` and the env var is unset. Ignored when the env var IS set. |
| `sensitive` | bool | Marks the resolved value as sensitive — redacted in verbose logs, eventlog, and `/stats`-style diagnostic surfaces. Set true for tokens, passwords, API keys. |
| `description` | string | Free-text explanation of what the var is for. Surfaces in the "required var missing" error message so operators see context without hunting through the manifest. |
| `used_by` | list of strings | Optional grep-friendly hint about which files reference this var. The loader doesn't validate the entries. |

---

## Interpolation

`${env:VAR}` in any of these gets substituted at load time:

- `.agents/AGENTS.md` (and everything under `AGENTS.d/`, and `@include`d files).
- `.agents/skills/**/SKILL.md` and skill reference files under `references/`.
- `.agents/mcp.json` values (Env, Headers) — same syntax that shipped with mcp.json originally.

Syntax rules:

- `${env:NAME}` — matches when `NAME` starts with a letter or underscore, followed by letters/digits/underscores.
- Unset non-declared vars fall through to the ambient process env (via `os.Getenv`), then resolve to empty string. Undeclared references surface as drift warnings at boot (see below).
- Anything that doesn't match the syntax (`${envFOO}`, `$env:FOO}`, `${env :FOO}`) passes through as literal text. A near-miss that still opens with `${env:` — `${env:my-var}`, `${env:2FA_TOKEN}` — is reported at boot (see below), because a hyphen or a leading digit in the name is far more likely to be a typo than a deliberate literal.

Interpolation runs once per file at daemon startup (or `/reload`). It's not dynamic — changing an env var while the daemon is running has no effect on already-loaded prompts until the daemon restarts.

---

## Boot-time validation

The daemon runs the manifest through four phases at startup:

1. **Schema validation** — parses the file, rejects malformed entries (empty names, duplicates, invalid identifiers).
2. **Required-var check** — every entry with `required: true` must have a value in the process env. Missing → fatal error, daemon exits with `ExitConfigError`. Errors are batched (all missing vars listed at once), not fail-first, so operators see everything to fix in one round-trip.
3. **Drift diagnostics (warn only)** — after all bundle files have been loaded and interpolated:
   - Names referenced via `${env:NAME}` but not declared in the manifest surface as `"${env:NAME} is referenced but not declared in the manifest"`.
   - Names named by a `*_env` config field but not declared in the manifest surface as `` "config names env var \"X\" (a *_env field) but it is not declared in the manifest" ``.
   - Names declared in the manifest but reached by neither route surface as `"manifest declares X but nothing in the bundle references it"`.
4. **Surviving placeholders (warn only)** — see below. This one runs even when there is no manifest at all, because that is the case it exists for.

Drift diagnostics and the surviving-placeholder check are advisory. The daemon keeps running; the recipe author sees the warnings and cleans up on their next iteration.

### Surviving placeholders

Once every instruction file, skill, and subagent content root has loaded, the daemon checks the loaded text for `${env:…}` placeholders that are still there. (`mcp.json` is not in scope: its values interpolate unconditionally, so nothing can survive that pass for lack of a manifest.) Anything it finds would reach the model verbatim, so it is named on stderr with the rest of the startup lines:

```
core-agent: agentenv: 2 ${env:...} placeholder(s) survived loading and will reach the
  model as literal text: ${env:GKE_CLUSTER}, ${env:GOOGLE_CLOUD_PROJECT} — no env.yaml
  or env.json in /app/.agents, so interpolation is OFF
```

Two situations produce it:

- **No manifest.** Interpolation never ran. The remedy is to add `env.yaml` declaring those names — or to delete the placeholders if the bundle wasn't meant to use the mechanism.
- **A manifest is active, but a placeholder doesn't match the `${env:NAME}` syntax.** Interpolation ran and skipped it. The warning says so, and the remedy is to fix the name.

The check reads the loaded *content* rather than inferring from the manifest's absence, so a bundle that ships no manifest and references nothing stays silent — which is most bundles.

It is a warning, not a fatal error, because `${env:...}` shows up legitimately as literal text in content that documents the feature — a skill whose reference file quotes an example `mcp.json` is a real and correct thing to ship. If your persona is one of those, the line is noise you can ignore; there is no suppression flag, and adding one would recreate the silence this exists to break.

### The two ways a bundle consumes an env var

`${env:NAME}` is not the only reference the drift check counts, because it is not the only way the bundle reaches the environment:

| Convention | Where | What it does | When it resolves |
| --- | --- | --- | --- |
| `${env:NAME}` | AGENTS.md, skills, `mcp.json` values | splices the **value** into text the model reads | once, at load |
| a `*_env` config field | `config.json` | hands the **name** to the component that owns the field | late, at use |

The second exists because some values must not be spliced. `alerts.targets[].url_env` is re-read on every fire, so rotating the Secret needs no restart, and the webhook URL never lands in the in-memory config that log and status surfaces can echo. Config is parsed by `pkg/config` and never flows through the interpolator, so writing `"url": "${env:MY_WEBHOOK}"` does not work — it isn't expanded, and the URL validator rejects the literal for having no scheme.

The fields that name an env var are, today:

- `alerts.targets[].url_env`
- `alerts.targets[].auth.bearer_env`
- `alerts.targets[].auth.basic_env_user` / `basic_env_pass`
- `attach.token_env`
- `tools.call_peer.token_env`

Both routes count as a reference, so declaring a var in `env.yaml` and using it only from config is not drift. Before this landed it was reported as unreferenced — a live `kube-platform-native` deployment warned that nothing referenced `PLATFORM_AGENT_ALERT_WEBHOOK` while an alert target was reading it by name.

Discovery is by JSON-tag convention (`*_env`), not a hand-maintained list, so a new `*_env` field is picked up with no wiring. The corollary: an env-name field tagged something else — `webhook_secret`, `token_var` — is invisible to the check. Name new fields with the suffix.

---

## Sensitive values

Setting `sensitive: true` doesn't change where the value ends up (it still gets interpolated into the prompt, which the model sees) — it flags the value for redaction in log paths that already sanitize secrets:

- Verbose daemon logs.
- Eventlog transcripts.
- `/stats` and `/mcp` diagnostic renderers.

For MCP header values that carry tokens, the existing `mcp.json` redaction already applies — `sensitive: true` on a manifest entry adds a second layer for the same value.

---

## Backwards compatibility

- Bundles without `env.yaml` / `env.json` behave exactly as before #322 landed: no interpolation happens, no validation runs, no drift warnings. Existing operators are unaffected. The one thing that changed since is the [surviving-placeholder warning](#surviving-placeholders) — a boot line only, and only for bundles whose content actually contains `${env:` markers.
- `mcp.json`'s pre-existing `${env:VAR}` support in Env / Headers keeps working identically. The regex + resolver moved to `pkg/agentenv` internally but the semantics are preserved.
- Adopting the mechanism is a per-bundle opt-in: drop a manifest, replace literals with `${env:VAR}` references, populate env at deploy time.

---

## Migration recipe

Migrating a bundle that currently uses sed-based placeholders (e.g. the `gke-troubleshoot-agent` recipe used `__GCP_PROJECT__` before #322):

1. Add `.agents/env.yaml` declaring the three variables (`required: true`).
2. Replace `__GCP_PROJECT__` → `${env:GCP_PROJECT}` throughout the bundle files.
3. Remove any operator-facing sed step from setup scripts / docs.
4. Populate the env vars via ConfigMap `envFrom` / Deployment `env:` / whatever the runtime already uses.
5. Deploy and rely on fail-loud validation to catch missed values.

See `examples/gke-troubleshoot-agent/deploy/base/config/env.yaml` for the canonical shape.
