# core-tui adapter smoke sweep — 2026-05-26

Smoke test for the `CORE_AGENT_TUI=core-tui` adapter against
core-tui **v0.4.1**. Walks the 19-item acceptance checklist from
[`core-tui-adapter-design.md §7`](core-tui-adapter-design.md)
plus the new UIConfig + permission-fix surfaces.

Tick `[x]` as you go; jot observations inline. Anything ✗ becomes
a follow-up issue / fix; anything ⚠ is "works but rough."

## Setup

- Binary: `/tmp/core-agent-smoke` (built from main at commit
  `8106de9` — includes gate fix + UIConfig + all 4 adapter
  tiers).
- core-tui: `v0.4.1`.
- Config: `.agents/config.json` with `gemini-3.5-flash` + Vertex
  creds (or whichever model your env is configured for).

```sh
CORE_AGENT_TUI=core-tui /tmp/core-agent-smoke
```

## 1 — Core navigation + history

- [✓] `/help` — merged built-in + adapter command list shows
- [✓] `/clear` — history resets
- [✓] `/quit` — exits cleanly; transcript saved to `<AgentsDir>/sessions/`

Notes:
/
## 2 — Info panels

- [✓] `/memory` — loaded memory files render with bytes / `(truncated)` annotation
- [✓] `/stats` — per-turn + session totals (after a turn lands); context window N/M shown
- [✓] `/model` — picker opens
- [✓] `/model <id>` — direct switch works (try `/model`)
- [✓] `/mcp` — configured servers listed (empty fine if `.agents/mcp.json` bare)
- [✓] `/skills` — skills listed
- [✓] `/tools` — tools listed with `Source` mix (builtin/mcp/skill) + per-tool `GateState`

Notes:

## 3 — Turn flow + cost

(Type any real prompt that produces text + at least one tool call.)

- [✓] Streaming: assistant text renders live via Glamour
- [✓] Per-turn footer after completion: `↑N in · ↓N out · $X · ◇ <model>` + elapsed
- [✓] `/interrupt` mid-turn cancels cleanly
- [✓] Esc on empty input does the same as `/interrupt`
- [⚠] Type a 2nd prompt **during** streaming → queue panel shows ⏳Python
- [✗] On turn-end, queued prompt auto-continues with ↻ marker

Notes:

## 4 — Permissions

- [✓] `Shift+Tab` cycles chip Ask → AcceptEdits → Plan → Yolo (and back)
- [ ] Plan mode: any bash/edit refused with "tool execution disabled in 'plan' mode"
- [ ] AcceptEdits mode: `write_file` / `edit_file` auto-allow; bash still prompts
- [ ] `/permissions list` shows current allow/deny patterns
- [ ] `/allow bash:date` — auto-allowed, persisted to `.agents/config.json`
- [ ] Always-allow write on `/tmp/foo` → subsequent reads of `/tmp/...` siblings stay silent (already verified earlier)

Notes:

## 5 — Pricing + reload

- [✓] `/pricing refresh` — returns "updated N models" or "upstream unchanged" summary
- [✓] `/pricing set <model> 0.10 0.30` — round-trips (try a model not in the built-in table)
- [✓] `/reload` — *expected*: "not yet wired" message (known v0.4.1 adapter limitation, not a regression)

Notes:

## 6 — Side query + subagent

- [ ] `/btw what was the last assistant message about?` — opens dismissible overlay
- [✓] Space/Enter/Esc dismisses the `/btw` overlay
- [] `/btw` content never enters main conversation history
- [✗] `/subagent --name=ping echo done` — spawns successfully

  Result:
  ℹ  /subagent requires the internal/tui flag parser — not yet lifted into the core-tui adapter. Use
CORE_AGENT_TUI=internal to drive subagent spawn for now.

- [ ] `/subagents` lists the spawned subagent with real status (not hardcoded "running")
- [ ] Subagent's `report_completed` alert lands as a system message in the chat

Notes:

## 7 — Layout + mouse

- [✓] `Ctrl+B` toggles `StatusHeader` ↔ `StatusSidebar`
- [✗] Layout choice persists across restart (host-supplied `PersistStatusLayout` writes it)
- [✓] `/mouse off` toggles capture; plain drag now selects text
- [✓] `/mouse on` re-enables; wheel scrolls chat again
- [ ] Set `ui.mouse: false` in `.agents/config.json` → relaunch → boots with mouse-off (new UIConfig path)
- [ ] Set `ui.theme: "light"` in `.agents/config.json` → relaunch → Glamour uses light style (skips OSC-11 query)

Notes:

## What this sweep does NOT cover

**core-tui v0.5.0 status (shipped 2026-05-27):**

- ✅ `Options.ForceTheme` + `Options.Mouse` — landed in v0.5.0 and
  wired in the adapter via `uiThemeToCoreTui` / `uiMouseToCoreTui`
  (commit `d4b00a2`).
- ❌ Misleading "Wake signal received" message — open as
  [core-tui#7](https://github.com/go-steer/core-tui/issues/7).
- ❌ Queue panel done-state TTL too short — open as
  [core-tui#8](https://github.com/go-steer/core-tui/issues/8).
- ❌ `AutoContinueFromInbox` mode for ADK-shaped hosts — open as
  [core-tui#9](https://github.com/go-steer/core-tui/issues/9).
  Stopgap on our side: this branch flips
  `MidTurnInjectionMode` from `InjectIntoCurrent` to
  `QueueForNext` so queued prompts at least fire as separate
  follow-up turns. Bundled-with-framing + ↻ marker still pending
  core-tui#9.

**Punted to follow-on PRs:**

- `/reload` adapter lift (currently surfaces "not yet wired") —
  punted until the host-side closure can be reached cleanly.
- Attach-mode parity (R4 from the design doc) — separate follow-up.

## Summary

Fill in after walking the checklist:

- **Items passing:** _ / 38
- **Items failing (with notes):**
- **Items rough but functional (⚠):**
- **Verdict on flipping launcher default to `launchTUIv2`:** ☐ go / ☐ wait

Once the verdict is "go," PRs queued behind this:

1. Flip launcher default (`cmd/core-agent/main.go:639`).
2. Hugo site docs sweep (`docs/site/content/docs/*`).
3. core-tui v0.5 wiring PR when v0.5 lands.
