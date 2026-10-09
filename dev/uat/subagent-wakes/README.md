# Subagent wakes UAT (#1283)

Manual UAT for the subagent roster's live report and scheduled wakes, on
the real binaries, in both TUIs:

- **`run.sh local`**: the embedded TUI. Runs `core-agent` in your
  terminal.
- **`run.sh headless`**: the remote TUI. Starts `core-agent --no-repl
  --attach-listen …` in the background and runs `core-agent-tui`
  attached to it in your terminal. Quitting the TUI stops the daemon.

Both show the same running-tasks bar. A standing watcher sleeping between
turns reads `◷ cluster-watch-1 · wakes in 28s · watching status.txt: all
nodes Ready`. A bounded subagent shows a `▶` row with its latest message
while it works, then a `✓` row with its result for five seconds.

## Setup (once)

You need `go`, `curl` and `jq`, plus a real model: the mock providers
can't call `spawn_agent`, so there is nothing to see without one.

```bash
cd dev/uat/subagent-wakes
cp env.example .env      # git-ignored
$EDITOR .env             # set your project
```

`env.example` defaults to Claude Haiku 4.5 on Vertex, authenticated with
gcloud application-default credentials. Gemini on Vertex is the commented
alternative. Anything exported in your shell overrides `.env`. `run.sh`
refuses to start if the chosen provider's project or key is missing. The
config caps the session at $3; walking through both TUIs on Haiku cost
about $0.10.

## Run it

Embedded TUI:

```bash
dev/uat/subagent-wakes/run.sh local
```

Remote TUI:

```bash
dev/uat/subagent-wakes/run.sh headless
```

Quit either TUI with **Ctrl-D** (or Ctrl-C twice). While one is running,
use a second terminal for:

```bash
dev/uat/subagent-wakes/run.sh poke "ALERT node-3 NotReady"   # change the watched file
dev/uat/subagent-wakes/run.sh agents                         # raw GET /agents (headless only)
```

`run.sh clean` removes `/tmp/core-agent-uat-wakes/` and stops a daemon
left behind if the script was killed. Each mode starts from a fresh
working directory under `/tmp/core-agent-uat-wakes/{local,headless}/`
holding its config, `status.txt`, session DB and `core-agent.log`.

## What the fixture declares

- **`cluster-watch`**, with `"scheduler": "sleep"`. A standing worker:
  each turn it reads `status.txt`, reports a line starting `ALERT` once,
  and calls `schedule_next_turn` for `WAKE_SECS` (default 40) with a
  `detail` quoting the line. The declared scheduler is what makes the
  headless path possible: ad-hoc spawns are off on a `--no-repl` daemon.
  Its `budgets` allow 30 minutes and 40 turns; without them the async
  defaults would stop it at 10 minutes, sleep included.
- **`quick-check`**: no scheduler. Reads `status.txt` once and returns.

The parent can't run `bash` or write files, and runs in `yolo` so nothing
waits on a permission prompt.

## Scenarios

Run each in both TUIs. Type the prompts into the TUI.

### W1. A sleeping subagent counts down

Type `Start the watcher.`

**Verify:**

- The parent calls `spawn_agent cluster-watch` and replies in a sentence.
- The bar shows `◷ cluster-watch-1 · wakes in Ns · watching status.txt:
  all nodes Ready`, counting **down**, and the header ends with `1
  subagent scheduled`.
- When the countdown runs out the row reads `waking`, never a negative
  time, then a fresh countdown follows.
- Headless: `run.sh agents` shows `"status": "running"`, an RFC 3339
  `next_wake_at`, and `wake_detail` matching the bar.

### W2. Live report follows an alert

`run.sh poke "ALERT node-3 NotReady: kubelet stopped posting status"`

**Verify:**

- After the next wake, the watcher's row quotes the ALERT line.
- Headless: `last_report` in `run.sh agents` carries the alert.
- The parent receives the alert and takes a turn.

### W3. A working row next to a sleeping one

Type `Run a quick check.` while the watcher sleeps.

**Verify:**

- A `▶ quick-check-1 · Ns · <its latest message>` row appears under the
  `◷` row, and the header reads `1 subagent running · 1 scheduled`.
- It becomes `✓ quick-check-1 · completed · <its result>`, stays about
  five seconds, then goes.

### W4. Stopping clears the wake

Type `Stop the watcher.`

**Verify:**

- The parent calls `stop_agent`; the row shows its stopped outcome
  briefly, then goes.
- Headless: `run.sh agents` shows `"status": "stopped"` and **no**
  `next_wake_at` or `wake_detail` key.

`run.sh poke STOP` ends the watcher the other way: it returns, and the
finished row carries the status lines it saw.

### W5. An older daemon degrades quietly (optional)

Point a v0.31.0+ `core-agent-tui` at a daemon built before protocol
1.20.0. A sleeping subagent's row counts **up** like a working one, and
nothing errors.

## Known

- `core-agent-tui` caches the roster for 5s, so its bar can read
  `waking` or keep a just-stopped row a few seconds longer than the
  embedded TUI.
- The model chooses the wording of `detail` and of the alert, so check
  the substance, not the exact strings above.
