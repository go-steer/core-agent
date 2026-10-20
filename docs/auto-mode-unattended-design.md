# Auto mode with nobody to ask

Status: implemented. Follows [`auto-mode-design.md`](auto-mode-design.md) (#1175).

## The problem

In auto mode, a call the approver can't decide goes to a person. That's the right default when a person is reachable. The self-dev soak's A7 runs (2026-10-20) showed what it costs when nobody is:

- **Run 1.** The agent's first `go -C <copy> test` was not on the eligible list. It went to a person, waited the full 30-minute `approval_timeout`, and expired. The expiry told the agent to end the turn and report the action as pending, and it did. The session ended with the fix written and nothing committed.
- **Run 2.** An unquoted `-run A|B` made a test command a pipeline, which never matches an eligible pattern: 30 minutes. The recipe's own `reviewer` subagent wasn't eligible either: another 30 minutes.

Two things made each of those expensive. The wait was for nobody. And the agent never learned *why* the call wasn't judged, so it couldn't tell "split this into simple commands" from "this needs a human". The reason is computed (`approverIneligible`) and thrown away.

A second defect showed up in the same run. The approver denied one `edit_file` on a path, correctly, because its `old_string` and `new_string` were identical. Every later `edit_file` on that path was then refused for the rest of the turn as "an identical request". The turn-refusal memory (#1074) keys on `tool|detail`. For bash the detail is the whole command, but for the file tools it's only the path.

## The change

1. **`permissions.auto.escalation`**, `"person"` (default) or `"refuse"`. With `"refuse"`, a call auto mode would hand to a person is refused at once: an ineligible call, and a call the approver passed on. That's the behaviour auto mode already has on a gate with no prompter. This makes it chosen in config, for a deployment that has a prompter but nobody behind it.
2. **The refusal says why.** It carries the ineligibility reason, or the approver's reason when the approver passed the call on. For bash it names the specific case: a pipe, redirection, chain or expansion never matches an eligible pattern, so the agent should run each command on its own. Its guidance says a different call can still be judged. It does not say to end the turn.
3. **An expiry in `"person"` mode carries the same reason.** An attended deployment's agent learns it too, after the wait.
4. **The turn-refusal memory keys a denial on the whole request.** A denial, by a person or the approver, is remembered under the tool, the detail and a fingerprint of the call's full arguments, for every tool except bash, when the gate holds them. A denied edit no longer blocks a different edit to the same file. An identical call is still refused without asking anyone.
5. **Control-plane writes are refused too.** They take their own elevated path, which never reaches the common prompt code. Under `"refuse"` they get the same immediate refusal, so no call is left waiting for a person.
6. **An approver failure is reported as one.** If the approver errors or returns no usable answer, the refusal says it "could not decide" the call, not that it passed the call on. That refusal is not remembered for the turn, because a retry may find the approver working.

## Settled decisions (do not relitigate)

- **A config field, not a mode bit.** Whether anybody will answer is a fact about the deployment, not something the gate can detect. A prompter that exists says nothing about a person behind it: the soak daemon has both an attach broker and an alert target, and nobody. So the operator states it. The repo rule is to branch on what's true about the build, and this is.
- **`"refuse"` covers approver pass-ons as well as ineligible calls.** Both would wait for the same absent person. Splitting them would leave a 30-minute stall in a mode chosen to have none.
- **Compound commands still never reach the approver.** `"refuse"` changes what happens to a call the approver may not judge, not which calls it judges. The prefix-rule safe-command guard is unchanged (auto-mode design decision 3, and the turn-refusal memory's rationale).
- **`approval_timeout` stays required for mode `auto`** (decision 12), even under `"refuse"`, where it's never used. Relaxing it is a separate question with its own failure mode, an auto gate that silently becomes an ask gate.
- **Bash keeps `tool|detail` as its refusal key.** Its detail is already the whole command. Adding the arguments would let a changed `timeout_seconds` launder a refused command past the memory, which is the storm #1074 exists to stop.
- **Only a denial is keyed on the arguments.** A denial is a verdict on the call's content. An expiry or a no-person refusal isn't: nobody is there whatever the arguments say. Keying those on the arguments would let a reworded call wait out another `approval_timeout`, so they keep `tool|detail`.
- **The denial key also applies in ask mode, deliberately.** After a person denies an edit, a *different* edit to the same file now prompts them, where before it was refused as a repeat. That is #1074's own rule: the memory suppresses "only the call the operator actually refused", and an agent may "fix its manifest and ask again". The bare key broke that rule for every tool whose detail is a target rather than the call.
- **A repeat under `"refuse"` doesn't say to end the turn.** It has its own refusal kind. Nothing is pending, so its guidance is the same as the first refusal's: do the work another way, or report what needs a person. The expiry guidance ("report that the action is still pending") is what made A7's first run quit.
- **Arguments go into the key only when the gate holds them.** A request without `Args` keeps the old key. That fails toward refusing the repeat, which is the memory's existing direction.

## Out of scope

- Refusing `..`, git's `-c` and `--git-dir`, or a second `-C` in prefix-matched eligibility. The soak design notes the gap. It needs its own design.
- Telling the agent which patterns are eligible. The reason says why a call wasn't eligible, not what would have been.
- What a person sees in a prompt. `PromptRequest.Ineligible` is available to prompters, but none shows it yet.
- Bounding the control-plane prompt by `approval_timeout` in `"person"` mode. It has never been bounded, and changing that is a separate question.
- Notification behaviour: `approval_notify` is unchanged, and under `"refuse"` nothing is announced because nothing waits.
