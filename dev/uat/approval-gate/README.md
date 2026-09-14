# The gated-daemon UAT

Can a core-agent daemon run with the permission gate **on**, unattended,
and still get work done?

That question is [#647](https://github.com/go-steer/core-agent/issues/647)'s
whole premise. Its opening line is the indictment: there was no async
approval path, *"which is why every shipped autonomous recipe is forced
into `mode: yolo`"*. Two halves have since shipped —
`permissions.approval_timeout` ([PR #1057](https://github.com/go-steer/core-agent/pull/1057))
so a gated call with nobody watching eventually stops waiting, and
`permissions.approval_notify` ([PR #1059](https://github.com/go-steer/core-agent/pull/1059))
so a prompt nobody is attached to tells somebody. Both have unit tests.
Neither had ever run against a cluster, and the acceptance criterion left
open on the issue is exactly that: **a daemon can run gated (not yolo)
end-to-end.**

This rig is that run.

## What it does

`run.sh` deploys two short-lived pods into the demo namespace, drives one
turn per leg against the gated daemon, asserts, and tears everything
down.

```
  ┌──────────────────────┐   /approval      ┌───────────────┐
  │  approval-gate-uat   │ ───────────────▶ │ approval-sink │
  │  permissions.mode    │   (the GATE      │               │
  │      = "ask"         │    notifying)    │  logs one     │
  │  approval_timeout    │                  │  JSON line    │
  │  approval_notify     │   /agent-alert   │  per request  │
  │      = "oncall"      │ ───────────────▶ │               │
  └──────────────────────┘   (the AGENT's   └───────────────┘
           ▲                  gated action           │
           │                  actually running)      │ kubectl logs
           │ POST perms/respond                      ▼
      run.sh (you)                            the assertions
```

Three legs, each a fresh turn on the same session:

| leg | what happens | what must be true |
|---|---|---|
| **approve** | the gate opens a prompt; `run.sh` answers `allow-once` out of band | the notification was *delivered*, it carries a `request_id`, it names the waiting tool, it says how to answer — and the released call then actually runs |
| **deny** | every prompt the leg opens is answered `deny` | the call never runs, **and** the agent is told it was refused rather than left hanging |
| **expire** | nobody answers | `approval_timeout` fires, the call never runs, the daemon survives, and a late approver gets **410 Gone** with "the action was not taken" — not 404 unknown-id |

"Every prompt the leg opens", not "the prompt", because a refused call
does not stay refused in the model's head: it re-issues it, and each
re-issue is a new prompt. Denying only the first is both unrealistic —
an operator who said no would say no again — and a way for a retry to
slip through the gate while the assertion reads the original. The run
prints how many prompts it opened for exactly this reason; three is one
per leg, more is the model arguing. See
[#1068](https://github.com/go-steer/core-agent/issues/1068).

## Why there is a sink

Because the daemon's own log is not evidence.

The daemon logs `approval notification sent` when `alert.Sender.Send`
returns `nil`. That is a claim about a function call, not about a
delivery. #647's premise is that an operator who cannot watch the console
must be **told**, and "we called Send and it did not error" is the system
reporting on itself. Silence is zero deliveries, and the only witness
competent to report a delivery is the thing that received one.

This is the same rule the [#652](https://github.com/go-steer/core-agent/issues/652)
evals apply when they grade the world instead of the transcript, and it
is why every assertion here except one reads `kubectl logs
deploy/approval-sink`. The exception is leg 2's *"the agent was told it
was refused"*, which necessarily reads the daemon's own event stream —
the claim there is about what the daemon told its model, and nothing
outside the process can witness a tool result.

Two sink paths, not one. `/approval` is where the **gate's** notification
lands; `/agent-alert` is where the **agent's own** `alert` tool lands.
Keeping them apart is what makes "the operator was told" and "the action
ran" two distinguishable lines in one log rather than one ambiguous one.

## Why the gated action is `alert`

It needs to be something the gate actually stops, and something visible
from outside the pod.

`alert` is not in `readOnlyBuiltins` (`pkg/tools/serialize.go`), so
`pkg/tools/gate.go` routes it through `CheckToolCall` — the prompting
path — like any other mutation. And a webhook target points at a Service
in the same namespace, so its effect is a line in a log rather than a
state change nobody can see.

Each leg's prompt carries a token (`uat-approve-<runid>`, …) that must
appear verbatim in the alert. Without it, leg 3's *"no action arrived"*
assertion would be reading a log that still contains leg 1's successful
action, and would pass only by accident of ordering.

## What this does **not** prove

**It does not apply anything to the cluster.** Box A3 of
[#1042](https://github.com/go-steer/core-agent/issues/1042) wants
*propose → approve out-of-band → **apply***, and the `gke-platform-agent`
recipe has no path to mutate a cluster — distroless image, mutating file
and bash tools disabled, a read-only GKE MCP endpoint on a read-only
OAuth scope, and no Role or RoleBinding for the daemon's ServiceAccount.
The recipe says so in its own words and its UAT expects "apply that fix"
to *refuse*. Reaching A3 is therefore a decision about a shipped demo's
blast radius, not a missing script; the options are costed in the A3
comment on #1042. **Do not record A3 as met on the strength of this
rig** — it meets #647's criterion, which is the smaller claim.

## Running it

Needs a cluster with the `gke-platform-agent` recipe already deployed:
the rig borrows that recipe's ServiceAccount (for the Workload Identity
binding) and its env ConfigMap (for the Vertex project and location), so
there is no IAM setup of its own. It borrows nothing else — its own
config, session DB, Deployment and Service — so tearing it down cannot
take the recipe with it, and running it cannot change the recipe's
behaviour.

```bash
dev/uat/approval-gate/run.sh
```

Cluster coordinates come from `~/.gke-platform-agent.env`, the same file
the [GKE drill](../gke-drill/README.md) uses. Expect roughly 6–8 minutes:
most of it is leg 3 waiting out `approval_timeout` in real time, twice
over — once for the deadline, once for the grace window that proves
nothing fired late.

Artifacts land in `~/.gke-drill/approval-gate/<runid>/`: the rendered
manifests, each leg's captured notification, a `legN-status.jsonl`
timeline sampled from the daemon's own `/status` while that leg waited,
the `legN-guardrail-reset.json` the leg's pre-flight got back, and — captured *before* teardown, because on a failed run they are the
only evidence — the full sink and daemon logs plus
`daemon-pod.describe`, which is the only witness for a pod that never
got far enough to log anything.

| variable | default | |
|---|---|---|
| `UAT_APPROVAL_TIMEOUT` | `90s` | the single biggest term in the wall clock |
| `UAT_NOTIFY_WAIT` | `300` | seconds to wait for a leg's prompt to open |
| `UAT_IDLE_WAIT` | `240` | seconds a leg waits for the previous leg's turn to end; must exceed `UAT_APPROVAL_TIMEOUT` |
| `UAT_DAEMON_IMAGE` | borrowed from the running `core-agent` Deployment | so the UAT tests the build that is actually deployed — **set it explicitly whenever the cluster has not been redeployed since #1059**, see below |
| `UAT_MODEL` | `gemini-3.7-flash` | matches the recipe |
| `UAT_KEEP` | unset | `1` leaves the rig up for poking; prints the cleanup command |
| `DEMO_NS`, `KUBE_CONTEXT`, `UAT_KSA`, `UAT_ENV_CM` | from `~/.gke-platform-agent.env` | |

Assertions accumulate rather than exiting on the first failure — the rig
takes minutes to stand up and nobody re-runs it three times to collect
three failures. Exit 0 means every leg passed; exit 1 prints the failed
assertions and points at the sink log.

**The image default is a trap this rig has already sprung once.**
Borrowing the running Deployment's image is the right default for the
GKE drill, which measures the build the demo is actually serving. Here
the thing under test *is* the build: a daemon older than
[#1059](https://github.com/go-steer/core-agent/pull/1059) parses
`approval_notify`, validates it against `alerts.targets`, starts clean —
and then notifies nobody, because the Notifier that does the work does
not exist in it. Every leg fails on "no notification arrived" and no log
anywhere says why. The rig now refuses to start in that state (it checks
for the daemon's own startup announcement of the target), but the fix is
to pass the image: `UAT_DAEMON_IMAGE=ghcr.io/go-steer/core-agent:main-<sha>`.

## Reading a failure

**"the gated daemon never became ready", and `daemon.log` is empty** —
then it is not the config, because a rejected config is a process that
started and said why. A zero-byte log means no process ran at all. Read
`daemon-pod.describe`: the first run of this rig died exactly here, with
`CreateContainerConfigError — container has runAsNonRoot and image has
non-numeric user (nonroot)`, because the pod spec asked for non-root
without naming a UID and the distroless image names its user rather than
numbering it. The kubelet cannot verify a name, so it refuses before the
container exists.

**"the gated daemon never became ready", with a log** — that one is the
config. `approval_notify` must name a target that exists in
`alerts.targets`, and a mismatch is deliberately fatal at startup rather
than a warning. The daemon's log says which.

**"the gate's notification was DELIVERED to the receiver" fails, and
nothing else ran** — check whether the model reached for `alert` at all.
Every other tool is disabled precisely so it cannot, but a turn that ends
with prose instead of a tool call produces this exact symptom, and the
daemon log distinguishes the two in one line.

**Everything passes but leg 3's 410** — the prompt expired and the
responder was told *404 unknown id*. That is the regression #1059 exists
to prevent: 404 sends an operator hunting for a typo in the request id
they were handed, when the truth is that they were too slow and the
action was never taken. Read `leg3-late-respond.status` and
`leg3-late-respond.body`, which are two files on purpose — see below.

**"a second/third prompt opened and was notified" fails, and the sink
log shows the notification arriving anyway, moments later** — the leg
gave up before its own turn started. Read that leg's
`legN-status.jsonl`: `turn_in_flight` false throughout means the wake
was queued and the turn had not begun (the previous leg's turn was
still running — a leg's wake during an in-flight turn is injected and
driven when that turn ends), while `turn_in_flight` true throughout
means the turn was running and the model was slow. Raise
`UAT_NOTIFY_WAIT` for the second; for the first, the leg boundary is
what failed, not the budget.

This is the failure that cost run 6 two assertions, and it does not stop
where it starts: the gate's notification carries no leg token, so a
notification the rig attributes to the wrong leg makes it answer the
wrong request id, and every assertion after that is reading someone
else's prompt. The rig now waits for the daemon to go idle before each
leg — which, when a prompt went unanswered, means waiting out
`approval_timeout` — and takes its baseline count only once the sink has
stopped moving.

**"a third prompt opened and was notified" fails, and `leg3-status.jsonl`
says the daemon was `idle` the whole time** — the wake was accepted and
the turn was then refused, which the API cannot tell you because the
refusal happens after the POST returns. The daemon log has it:
`turn refused (guardrail still tripped, not reset)`. Run 7 got here from
one denial — the model re-issued the refused call, the retries went
unanswered, three failed tool calls and three inert `mark_task_done`
calls tripped `watchdog: enforce`, and the session was halted before
leg 3 ever started. Each leg now clears a tripped watchdog before it
begins and says so (`legN-guardrail-reset.json`, and a warning naming
what it cleared); the behaviour that caused it is
[#1068](https://github.com/go-steer/core-agent/issues/1068). A run that
prints that warning is telling you something real about the daemon under
test, not about the rig.

**One correlation that is not causation.** `core-agent-vertexcache:
Caches.Create failed` appears in `daemon.log` a second or two before
each leg's notification, because the cache init fires when a turn starts
its first model request. It is a goroutine (`manager.go`'s `go
m.doInit`) and it is not on the turn's critical path. On this rig it
always fails — the daemon's prompt is far below the provider's minimum
cacheable size — and it is filed as
[#1067](https://github.com/go-steer/core-agent/issues/1067). Read it as
a turn-start timestamp, not as a cause.

## A note on curl, because this rig has already been lied to by it

`leg3-late-respond` is two files — `.status` and `.body` — and the
reason is worth writing down. The compact spelling
`curl -o /dev/stdout -w '\n%{http_code}'`, with the caller redirecting
to a file, opens two file descriptions onto that file with two offsets,
both starting at zero. Whichever flushes second overwrites the other.
Run 6 recorded the daemon's perfectly correct answer as
`410ch: approval arrived after the prompt expired…` — the status at byte
zero, eating the `attach:` off the front of the body — and then failed
its own `grep -qx '410'`. The daemon was right and the rig destroyed the
proof. Anything that captures a status and a body now keeps them apart.
