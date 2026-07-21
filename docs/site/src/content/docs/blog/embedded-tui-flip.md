---
title: The embedded-TUI thesis flip
description: We argued the wrong position for a month, wrote a design doc committing to it, then flipped. This is what changed and why — with the receipts.
template: doc
tableOfContents: true
sidebar:
  order: 5
---

Most posts about working with a coding assistant celebrate the wins. This one is about a case where the assistant and I reinforced each other into the wrong architecture for about a month, wrote a design doc committing to it, shipped v1.8 on top of that decision, and then flipped.

The setting: **how the terminal UI relates to the agent runtime.** More specifically, whether `core-agent` — a bare invocation of the daemon binary — should drop the operator into a full-screen TUI, or whether the TUI should live in a *separate* binary that talks to the daemon over the SSE-based attach protocol.

We settled on the second option for weeks. Wrote it up. Shipped it. Then flipped to the first. The v2 architecture is captured in [`docs/embedded-tui-design-v2.md`](https://github.com/go-steer/core-agent/blob/main/docs/embedded-tui-design-v2.md); its opening line reads:

> Reverses the central decision of [embedded-tui-design.md](https://github.com/go-steer/core-agent/blob/main/docs/embedded-tui-design.md) (v1) after UAT showed the spawn-and-attach approach has the wrong shape for the "I just want to chat with an agent" case.

This post is about what happened between "settled" and "reversed," why the wrong answer held for so long, and what we changed in the loop to notice sooner next time.

## The context

core-agent was extracted from an earlier project we'd built in the weeks before — a Go-native terminal coding agent by the same pair. That project has an in-process bubble-tea TUI: one binary, which drops you into the TUI, everything running in one process. The extraction pulled the agent runtime — model providers, MCP, permissions, session, telemetry — into a reusable library, leaving the TUI behind in the earlier project.

Once the extraction was done, the question was: does core-agent get its own TUI, and if so, how does that TUI relate to the daemon?

## What v1 argued

The first design doc ([`docs/embedded-tui-design.md`](https://github.com/go-steer/core-agent/blob/main/docs/embedded-tui-design.md)) argued for **spawn-and-attach**: two binaries, one TUI codebase, one transport.

- `core-agent` is the daemon. Headless. Speaks the attach protocol on a Unix socket or TCP port.
- `core-agent-tui` is the TUI. Its only job is to render an attach session.
- Local use: `core-agent-tui --local` starts a fresh `core-agent` daemon in the background on a private Unix socket, then attaches to it.
- Remote use: `core-agent-tui --url https://daemon.example.com` attaches over the network.

The pitch was clean:

- **One TUI codebase.** No drift between "local TUI" and "remote TUI" code paths — they're the same code, exercising the same attach client, against the same wire protocol.
- **Same UX everywhere.** Whether the daemon is a local subprocess, a container in the next pod, or a cluster-hosted agent behind IAP, the operator sees the identical UI.
- **The daemon stays headless.** `core-agent` is a clean library-consuming CLI with no UI dependencies. Distroless images stay small; the daemon binary can be dropped into any environment without dragging bubble-tea's dependency tree along.
- **The attach protocol becomes load-bearing.** Every UI feature that works for local also works for remote, and vice versa. Interrupt, queue panel, slash commands, permissions modals — one transport, one implementation.

The doc had a "Settled decisions (do not relitigate)" section. It was, by our convention, done.

The assistant and I both liked the design. Neither of us pushed back on it seriously. When we noticed papercuts during development, we treated them as implementation bugs on the way to the shipped design rather than as evidence that the design was wrong.

## What UAT surfaced

We shipped v1.8 on the spawn-and-attach architecture. It worked. We used it. Then, on real UAT drives — sitting down with the tool and using it as an operator would — three specific things kept coming up:

**1. Two processes for one mental model.** Every `core-agent-tui --local` invocation spawned a `core-agent` daemon in the background. That meant:

- **Cold-start lag.** The daemon takes about a second to come up, wire the model provider, load AGENTS.md, initialize MCP servers. The TUI can't render anything meaningful until that's done. In practice: a second of "starting..." on every launch of what the operator thinks of as one command.
- **Doubled memory footprint.** Two Go processes, two ADK runtimes, two model-provider stacks. On a laptop it doesn't matter; in a resource-constrained container it does.
- **Split log streams.** The daemon logs go to `/tmp/<sock>.log`. The TUI logs go to stderr. Debugging any issue meant tailing two files, and cross-referencing timestamps by hand.
- **"Is this remote?"** Every operator we watched, seeing an SSE-driven UI, asked whether they were connected to something else. The answer was always "no, it's local, it just looks like it's remote because it *is* remote to a local subprocess." That is the wrong sentence to have to say.

**2. Indirection bugs.** Because the local TUI talked to the daemon through the same SSE-framed attach channel that the remote TUI used, bugs in the protocol showed up locally too:

- The SSE stream's default timeout hid model responses that took longer than expected. Model streamed a response, the SSE client timed out, the TUI showed nothing.
- OSC 11 (terminal color-query) sequences leaked through the input box because they got framed as data before being interpreted by the terminal.
- The spawned agent's `stdin` EOF killed the REPL side of the daemon.
- The queue panel's `[Inbox]` wrapper broke a text-matching check downstream.

Every one was real, and every one got fixed. But every one existed **only because we were routing local UI through an HTTP+SSE channel designed for remote control.** The bugs were not architectural mistakes; they were the natural consequence of running a local operation through a transport built to survive network flakes.

**3. The user-facing command was wrong.** `core-agent-tui --local` is ceremony. Every comparable tool — Claude Code, gemini-cli, Antigravity — treats "run the tool" and "get the interactive UI" as the same command. Making the operator remember which binary to run *and* which flag to pass is a paper cut that never goes away.

None of these three showed up in the v1 design doc. Not because we hadn't thought about them — we had thought about (1) and dismissed it, thought about (2) as a class of bug to be fixed rather than avoided, and hadn't taken (3) seriously at all. UAT is what promoted them from "considered and dismissed" to "kept happening every time we used the thing."

## The flip

On 2026-05-23 the user flipped the position. The new architecture: **`core-agent` runs the TUI in-process by default when stdin is a TTY.** The remote TUI binary survives — `core-agent-tui` becomes the *remote-only* client, with no agent runtime and no model providers of its own, just an attach consumer for connecting to a remote daemon.

The v2 doc lays out the differences:

| v1 (do-not-relitigate) | v2 replacement | Why |
|---|---|---|
| One TUI codebase under [`cmd/core-agent-tui/`](https://github.com/go-steer/core-agent/tree/main/cmd/core-agent-tui); `--local` is spawn-and-attach | Move (lifted) TUI code to `internal/tui/`; `core-agent` runs it in-process | Removes the IPC layer for local use; reuses the earlier project's mature UI work |
| Default `core-agent` keeps line-mode REPL | Default `core-agent` launches the TUI when stdin is a TTY | One command to remember; non-TTY paths still get headless behavior |
| Two distroless images, both bubble-tea-clean | `core-agent` carries bubble-tea by default; optional `no_tui` build tag strips it | Slightly bigger default binary; opt-out for size-sensitive deployments |
| `core-agent-tui` is the one-and-only TUI binary | `core-agent-tui` survives as the remote-only client | Different use case — attach to a *remote* agent without shipping a model runtime locally |

Two things about this that weren't obvious until they landed:

- **The good idea from v1 didn't die.** "One TUI codebase" is still true — the earlier project's `internal/tui/` gets lifted into core-agent's `internal/tui/` (Path A of a three-stage plan, [documented in the v2 doc](https://github.com/go-steer/core-agent/blob/main/docs/embedded-tui-design-v2.md#migration-path-a-now-c-eventually)). What died is the *deployment* assumption that "one codebase" required "one transport for both local and remote." The right frame is **one TUI, two transports**: local runs the TUI against an in-process agent (goroutines, `tea.Msg`); remote runs the TUI against a network agent (SSE, JSON). Same UI, different plumbing.
- **The remote-only binary got sharper by being remote-only.** `core-agent-tui` had been trying to be both things — spawn a local daemon *and* attach to a remote one. Now it does one thing. That means it can be shipped without model providers, without MCP infrastructure, without the entire agent runtime; a distroless image of the remote-only TUI is tiny and can live in a sidecar pod for `kubectl exec` debugging without carrying the daemon's dependencies along.

## Why the wrong conclusion held for so long

This is the part worth naming.

Working with a coding assistant introduces a specific failure mode that working alone doesn't have: **two agreeing parties can reinforce each other into a worse local optimum than either would have reached alone.** The mechanism is subtle. When I proposed the spawn-and-attach architecture, the assistant found real arguments for it (the ones listed in v1) and articulated them clearly. When the assistant elaborated on the design, I read those arguments as external validation — someone else agrees with me — rather than as an echo of my own framing. The doc's "Settled decisions" section closed the loop: we'd committed, so revisiting felt like relitigating.

None of the individual steps was wrong. The v1 arguments *were* real. The papercuts *were* fixable in principle. The design *did* have good properties. What was missing was any external signal saying "wait, the way you're both talking about this is wrong." Solo, you eventually notice you're the only one advocating for something and get suspicious; with an assistant, you never feel that alone.

The concrete countermeasure that finally broke the loop: **real UAT, not simulated UAT.** Sitting down and using v1.8 as an operator, on my own machine, doing the thing operators would do — that's what surfaced (1), (2), and (3). Not because I hadn't thought of them before, but because using the tool made me *feel* them at a rate no design review would have. The papercuts weren't intellectually surprising; they were experientially exhausting, and that exhaustion is what moved me from "these are fine" to "this is wrong."

We wrote the reversal into memory the same day:

> **Embedded TUI position flipped 2026-05-23** — embedded TUI is now wanted; architecture is `core-agent-tui --local` (single TUI codebase, spawn-and-attach locally). Don't re-propose putting bubble tea in default core-agent binary.

That memory entry has been load-bearing at least three times since — every time a fresh session picked up context from earlier work, saw the v1 doc, and would otherwise have started arguing for the v1 position again.

## What we changed in the loop

The v1 → v2 arc changed how we do a few things:

- **Design docs get a `## Reversal history` section, or none at all.** The v2 doc opens by naming the specific decision it reverses and pointing to the v1 doc. That means anyone reading v1 in isolation sees an out-of-date artifact and won't spend an afternoon reasoning from it. We now treat the pointer from old design docs to their reversal as a required piece of documentation hygiene, on par with links from a `CHANGELOG` entry to its PR.
- **UAT is a first-class step, not the last one.** On the earlier project we described the loop as plan → implement → smoke test → refine → memorize. The core-agent version treats "smoke test" as "run the thing end-to-end, as an operator would, against a real environment" and puts it *earlier* — before the design feels closed rather than after. Most of the correctness work in v2.7 (including this reversal) came out of UAT sessions rather than tests.
- **Memory entries for reversals are permanent.** They don't age out. The flip is a fact about the shape of the codebase, and every future session needs to know it happened, or they'll waste a day re-deriving the wrong position. See [Working with a coding assistant, one project later](/core-agent/blog/working-with-coding-assistants/) for the broader argument for memory-as-institutional-knowledge.
- **When the assistant and I both agree quickly, that's a signal to slow down.** Genuine agreement is fine. Rushed agreement — where neither party pushed on the other's premise — is a smell. The countermeasure we've adopted: when a design converges without friction, explicitly ask "what would have to be true for this to be wrong?" before writing it into a design doc. Sometimes nothing. But sometimes something.

## How to actually avoid it — a countermeasure playbook

Naming the failure mode ("two agreeing parties reinforce each other") is easy. Building habits that make it less likely is the harder work. Here's the playbook we've been evolving, in rough order of how often each item earns its keep:

1. **Separate the proposer conversation from the critic conversation.** The context that produced the design is the same context that will defend it — that's the whole mechanism. So when a design feels close to done, open a **fresh session** (no shared transcript, no shared framing) and ask the assistant, in that fresh session, to argue *against* the design as stated. What comes back is not automatically right, but it is genuinely independent, because the anchoring from the first session isn't there. The v1 embedded-TUI doc would have failed this test — a fresh session, reading only the pitch, would have surfaced the "is this remote?" cognitive load in the first three exchanges.

2. **Steel-man the alternative in writing before you close.** Before a design doc's "Settled decisions" section lands, spend 20-30 minutes drafting the strongest case for the option you're about to reject. If you can't make it compelling, one of two things is true: either the reasoning is genuinely one-sided (fine — write down why), or you haven't done the work to understand the alternative (dangerous — the design is closing prematurely). Attach the steel-man to the doc as an appendix so a future reader can see what was really on the table.

3. **Deliberately seed dissent as a distinct step.** Framing matters. An assistant asked to "poke holes in this" produces meaningfully different output than one asked to "check this looks right." Both are useful; only one is a check. Bake "poke-hole" into the workflow as a *separate* prompt, not as an afterthought to the design conversation. When we started doing this, we found the assistant was often eager to identify problems it hadn't raised while helping build the design — the reluctance to volunteer criticism mid-flow is real.

4. **Timebox the "we're aligned" phase before committing.** Rapid convergence with no friction is a signal. When human and assistant agree in the first pass, that either means the problem was easy (rare for design-shaped questions) or someone anchored the other. Our rule of thumb now: any design decision that feels obviously right in the first hour gets a night's sleep before it lands in a doc's "Settled decisions" section. If it still feels obviously right the next day, ship. If new arguments surface, the first-hour agreement was rushed.

5. **Force at least two considered options into any non-trivial design doc.** A doc that presents one option and pitches it is either not doing design work or hiding the design work it did. Every core-agent design doc now has a "Considered options" section that lists at least two, with the trade-offs for each. This makes the choice visible — and, more importantly, makes it visible to a future session picking up the doc, who might otherwise reason from the shipped option as if it were the only one that ever existed.

6. **Prototype the physically-distinguishable paths, don't just argue about them.** Some design questions are about semantics (what should X mean?) — those are settled by argument. Others are about experience (how does X feel to use?) — those are only really settled by trying both. The v1 doc argued about spawn-and-attach vs. in-process on paper for weeks; an afternoon each on a throwaway spike in a worktree would have made the two-processes-one-mental-model gap obvious. When the question has an experiential axis, cut the argument short and build both.

7. **Use UAT as a peer review, not just a bug hunt.** UAT surfaces bugs, yes. But its higher-order function is *forcing you to feel the design*, at a rate no design review reproduces. A design that reads well but exhausts you to use is a design worth revisiting. Our v1.8 UAT surfaced the embedded-TUI flip precisely because the design was defensible on paper but tiring in practice, and that gap only shows up when you're using the tool as an operator would.

8. **Watch for the sentences that normalize friction.** Phrases like *"operators will learn that..."*, *"once you get used to..."*, or *"it's a minor cold-start hit"* are usually true on any single day and cumulatively wrong across a design's life. When they start showing up in a doc, mark them and revisit whether the friction is really acceptable or just being written around. See item 2 in "What we'd do differently" below.

9. **Bring in someone who wasn't in the design conversation.** A colleague who reads the doc for the first time will spot assumptions the author + assistant both took for granted. This is the classic "fresh eyes" move; the specifically-with-a-coding-assistant version is item 1 above — a fresh session is the poor person's fresh eyes when there's no colleague available.

None of these is expensive. All of them together add maybe an hour to a design cycle. The v1 embedded-TUI doc cost us a month, and the flip cost us the reversal doc plus the migration. That's the ratio to keep in mind: an hour of dissent-seeding at design time is very cheap tuition against a month of committed-to-wrong-architecture time.

## What we'd do differently

Two things, both about the front of the process:

1. **Prototype both paths in parallel, briefly.** Instead of picking spawn-and-attach on paper and building it, we should have spent an afternoon each on a spawn-and-attach spike and an in-process spike, in throwaway worktrees. The comparison would have surfaced the "two processes for one mental model" problem before any real code committed us. Spikes are cheap; reversals are expensive. (This is item 6 of the playbook above — we didn't have it as a habit yet at the time.)
2. **Treat "the operator will get used to it" as a red flag.** In hindsight, several arguments for v1 rested on "operators will learn that `core-agent-tui --local` is the command" and "the doubled cold-start becomes background noise." Neither is wrong on any given day. But an accumulating count of "the operator will get used to it" claims in a design doc is a signal that you're normalizing friction rather than removing it. We now flag those phrases in design review. (This is item 8 of the playbook, promoted to a design-review lint.)

The through-line: **the coding assistant is not a checking authority; it is a collaborator who defaults to agreement.** That's most of the time exactly what you want, and it is the whole reason the loop works at all. But the specific failure mode where two people agree each other into a bad architecture is *amplified*, not dampened, by adding an assistant to the loop. The countermeasure isn't distrust; it's UAT, and it's a habit of asking "what would have to be true for this to be wrong?" before the design feels closed.

We got a month of wrong architecture and a reversal doc out of not doing that soon enough. That's the cost, and it's cheap tuition compared to what it would have been if we'd shipped v1 all the way to a GA before anyone flinched.
