# Subagent wakes UAT (#1283)

Manual UAT for the subagent roster's live report and scheduled wakes, on
the real `core-agent` binary, over both TUI paths:

1. **local**: `core-agent` with its in-process TUI.
2. **headless**: `core-agent --no-repl --attach-listen …` with
   `core-agent-tui` attached over HTTP/SSE.

Both paths show the same running-tasks bar. A standing watcher sleeping
between turns reads `◷ cluster-watch-1 · wakes in 28s · watching
status.txt: all nodes Ready`. A bounded subagent shows a `▶` row with its
latest message while it works, then a `✓` row with its result for five
seconds.

## Prerequisites

- `tmux`, `curl`, `jq`, `go` on `$PATH`.
- **A real model.** The mock providers can't call `spawn_agent`, so there
  is nothing to see without one. The default is
  `MODEL_PROVIDER=vertex MODEL_NAME=gemini-3.7-flash`, which needs
  `GOOGLE_CLOUD_PROJECT` / `GOOGLE_CLOUD_LOCATION` and ADC. For Claude on
  Vertex, use `MODEL_PROVIDER=anthropic-vertex MODEL_NAME=claude-haiku-4-5`
  with `ANTHROPIC_VERTEX_PROJECT_ID` / `CLOUD_ML_REGION`. The config caps
  the session at $3, and a full walk-through of both paths on Haiku cost
  about $0.08.

## Layout

```
dev/uat/subagent-wakes/
├── README.md
├── run.sh
└── fixtures/
    ├── config.json.tmpl   # parent + two declared subagents
    ├── AGENTS.md          # parent persona: which subagent to spawn when
    └── cluster-watch.md   # the watcher's persona
```

The fixture declares two subagents:

- **`cluster-watch`**: `"scheduler": "sleep"`. A standing worker: each
  turn it reads `status.txt`, reports a line starting `ALERT` once, and
  calls `schedule_next_turn` for `WAKE_SECS` (default 40) with a `detail`
  quoting the line. This field is what makes the headless path possible:
  ad-hoc spawns are off on a `--no-repl` daemon, so a declared scheduler
  is the only way to get a standing worker there. Its `budgets` allow 30
  minutes and 40 turns. Without them the async defaults would stop it at
  10 minutes, sleep included.
- **`quick-check`**: no scheduler. A bounded delegation that reads
  `status.txt` once and returns.

The parent can't run `bash` or write files, and runs in `yolo` so nothing
waits on a permission prompt. Its only tools that matter are
`spawn_agent` and `stop_agent`.

All state lives under `/tmp/core-agent-uat-wakes/`, with one working
directory per path (`local/`, `headless/`), each holding its own
`.agents/config.json`, `status.txt`, session DB and `core-agent.log`.

## Cheat sheet

```bash
dev/uat/subagent-wakes/run.sh build      # build both binaries into /tmp
dev/uat/subagent-wakes/run.sh local      # path 1, tmux window :local
dev/uat/subagent-wakes/run.sh headless   # path 2, tmux windows :daemon + :tui
tmux attach -t core-agent-uat-wakes      # watch
dev/uat/subagent-wakes/run.sh poke "ALERT node-3 NotReady"   # change status.txt
dev/uat/subagent-wakes/run.sh agents     # raw GET /agents from the daemon
dev/uat/subagent-wakes/run.sh clean      # kill tmux, remove /tmp state
```

`poke` writes to every path that is set up. `agents` reads the headless
daemon only, since the local path has no attach listener.

## Scenarios

Run each against `local`, then against `headless`. Type the prompts into
the TUI.

### W1. A sleeping subagent counts down

Type `Start the watcher.`

**Verify:**

- The parent calls `spawn_agent cluster-watch` and replies in a sentence.
- Within a few seconds the bar shows `◷ cluster-watch-1 · wakes in Ns ·
  watching status.txt: all nodes Ready`, counting **down**, and the
  header ends with `1 subagent scheduled`.
- When the countdown runs out the row reads `waking`, never a negative
  time, and a fresh countdown follows the next turn.
- Headless only: `run.sh agents` shows the row with `"status": "running"`,
  an RFC 3339 `next_wake_at`, and `wake_detail` matching the bar.

### W2. Live report follows an alert

`run.sh poke "ALERT node-3 NotReady: kubelet stopped posting status"`

**Verify:**

- After the next wake, the watcher's `wake_detail` (the text on its bar
  row) quotes the ALERT line.
- Headless: `run.sh agents` shows `last_report` carrying the alert, not
  narration from an earlier turn.
- The parent receives the alert and takes a turn.

### W3. A working row next to a sleeping one

Type `Run a quick check.` while the watcher sleeps.

**Verify:**

- A `▶ quick-check-1 · Ns · <its latest message>` row appears under the
  `◷` row, and the header reads `1 subagent running · 1 scheduled`.
- When it finishes, the row turns into `✓ quick-check-1 · completed ·
  <its returned result>`, stays for about five seconds, then goes.

### W4. Stopping clears the wake

Type `Stop the watcher.`

**Verify:**

- The parent calls `stop_agent`, and the watcher's row shows its stopped
  outcome briefly, then goes.
- Headless: `run.sh agents` shows `"status": "stopped"` with **no**
  `next_wake_at` or `wake_detail` key.

A `STOP` first line (`run.sh poke STOP`) ends the watcher the other way:
it calls `return_result`, and the finished row carries the status lines
it saw.

### W5. An older daemon degrades quietly (optional)

Point a v0.31.0+ `core-agent-tui` at a daemon built before protocol
1.20.0. A sleeping subagent's row counts **up** like a working one, and
nothing errors.

## Known

- `core-agent-tui` caches the roster for 5s, so its bar can read `waking`
  or keep a just-stopped row a few seconds longer than the local TUI.
- The model chooses the wording of `detail` and of the alert, so expect
  the text, not the exact strings above.
