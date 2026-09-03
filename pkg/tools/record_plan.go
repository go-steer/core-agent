// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package tools

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"

	"github.com/go-steer/core-agent/v2/pkg/permissions"
)

// recordPlanDir is the subdirectory under agentsDir where plan
// artifacts are persisted. Per docs/plan-first-design.md Q4:
// `.agents/plans/` always, regardless of where the session DB lives.
const recordPlanDir = "plans"

// recordPlanFilenameRegex matches `plan-<seq>.md` and the revoked
// variant `plan-<seq>-revoked.md`. Capture group is the sequence
// number — used by nextPlanSeq to find max(seq) so a new plan
// gets <seq+1>.
var recordPlanFilenameRegex = regexp.MustCompile(`^plan-(\d+)(?:-revoked)?\.md$`)

type recordPlanArgs struct {
	Plan string `json:"plan" jsonschema:"the plan as markdown — required. free-form structure; the operator picks the shape via AGENTS.md prompting"`
}

type recordPlanResult struct {
	Path     string `json:"path"`
	Sequence int    `json:"sequence"`
	Message  string `json:"message"`
	// Outcome is the machine-readable form of what the call did:
	// planOutcomeRecorded (a new plan-<seq>.md), planOutcomeUpdated
	// (the plan this author already recorded this turn, overwritten in
	// place) or planOutcomeUnchanged (nothing written — the artifact
	// already holds this exact plan). A typed field rather than prose
	// only, because #857 established that prose alone does not stop a
	// loop: the field is what a behavioral detector can key on (#907).
	Outcome string `json:"outcome"`
	// Agent and Session echo the attribution written into the
	// artifact's frontmatter, so the model's own transcript records
	// which plan is its own. Empty when the host ran the handler
	// without an invocation context (library callers, tests).
	Agent   string `json:"agent,omitempty"`
	Session string `json:"session,omitempty"`
}

// Description prose, split so advisory mode doesn't tell the model
// its next call will be denied when nothing will deny it (#215). A
// model that believes a gate exists behaves as though it does —
// stalling for an approval that isn't coming — which is the same
// state-a-property-the-runtime-doesn't-enforce bug in reverse.
const (
	// The revision sentence is deliberately precise about what a repeat
	// call does (#906). It used to promise "each call writes a new plan
	// file with the next sequence number", which was both true and an
	// invitation: a model in completion-reporting mode called record_plan
	// eight times in one turn and minted plan-5 through plan-11, and the
	// description told it that was the intended way to revise.
	// The shape sentence is deliberately empty of slots (#909). It used
	// to read "typical shape: goal, files to change, approach, risks,
	// test plan, out of scope", which is a document template: a model
	// reads a slot list and fills the slots. An agent proposing an RBAC
	// change has no "files to change" and no "test plan", so it either
	// fabricates them or pads them by restating the incident. Worse, the
	// list contradicted this tool's own arg schema one screen up, which
	// already defers the shape to the recipe author — the description
	// was overriding the very person it defers to.
	recordPlanDescCommon = "Plan is free-form markdown — the operator's AGENTS.md picks the shape. The plan is persisted to .agents/plans/plan-<seq>.md and surfaced to any attached operator. Call this ONCE per plan: revising within the same turn updates that same artifact in place rather than filing a new one, and re-sending an unchanged plan writes nothing at all. The operator's /replan is the way to withdraw a plan and start over."

	recordPlanDescAdvisory = "Record your proposed course of action as a markdown artifact for the operator's audit trail. Plan-first gating is OFF — no tool call is blocked on this, so record the plan and then carry it out in the same turn rather than stopping to wait for approval. " + recordPlanDescCommon
)

// recordPlanDescRequired names the tools the gate will actually deny.
// Listing one this build didn't register is the #215 bug in a third
// form: not a gate the model wrongly believes in, but a tool it
// wrongly believes it has. On a distroless deploy "call this BEFORE
// any ... bash call" describes a constraint on nothing.
//
// spawn_agent stays unconditional — it isn't part of tools.Build's
// catalog, so gate.HasTool cannot tell "absent" from "unknown" for it
// and must not be asked. Since #758 the claim is also true: the spawn
// door calls the gate, so on a build that registers it the denial this
// sentence promises is the denial the model gets. Until then this line
// was the #215 bug in its third form — the reason #758 was filed — and
// it is left unconditional only because the alternative is to say
// nothing about delegation in the mode where delegation matters most.
//
// Deliberately NOT gate.PlanGatedTools() (#747), which the result
// message uses: that set is registered after Build's ctor loop, and
// the MCP namespaces join it later still (GateToolset), so at the
// moment this description is baked it would name a set that is merely
// incomplete — a worse failure than the narrower claim made here.
func recordPlanDescRequired(gate *permissions.Gate) string {
	gated := make([]string, 0, 5)
	for _, n := range []string{"write_file", "edit_file", "delete_file", "bash"} {
		if gate.HasTool(n) {
			gated = append(gated, n)
		}
	}
	gated = append(gated, "spawn_agent")
	return "Record your proposed course of action as a markdown artifact and unblock mutating tools. " +
		"Plan-first gating is ON: call this BEFORE any " + strings.Join(gated, " / ") +
		" call, or those calls are denied with a 'plan required' error. " + recordPlanDescCommon
}

// RecordPlan returns the built-in record_plan tool. Calling it with
// a non-empty plan writes the plan to `<agentsDir>/plans/plan-<seq>.md`
// and flips the gate's `planRecorded` flag, which unblocks mutating
// tool calls when the gate is armed (permissions.plan_mode=required).
// In advisory mode the artifact is the whole point and the flag is
// inert — nothing consults it.
//
// The tool is ALWAYS allowed regardless of gate mode or
// planRecorded state — it's the escape valve from plan-first
// gating. It does not call the gate; it writes directly to
// agentsDir/plans/ via atomic-rename.
//
// Per docs/plan-first-design.md Q2 ("any non-empty string"), no
// schema validation beyond non-empty-after-trim. Plan quality is
// the operator's judgment, enforced via /replan when needed.
func RecordPlan(gate *permissions.Gate, agentsDir string) (tool.Tool, error) {
	if gate == nil {
		return nil, errors.New("tools.RecordPlan: gate is required")
	}
	if agentsDir == "" {
		return nil, errors.New("tools.RecordPlan: agentsDir is required (set --agents-dir or run inside an .agents/ workspace)")
	}
	desc := recordPlanDescAdvisory
	if gate.PlanRequired() {
		desc = recordPlanDescRequired(gate)
	}
	return functiontool.New(functiontool.Config{
		Name:        "record_plan",
		Description: desc,
	}, recordPlanFunc(gate, agentsDir))
}

func recordPlanFunc(gate *permissions.Gate, agentsDir string) functiontool.Func[recordPlanArgs, recordPlanResult] {
	// One memory per built tool. RecordPlan is called once per process
	// (tools.Build), so this closure spans every session the daemon
	// serves — which is why entries are keyed by author rather than
	// living in a single field. A host that rebuilds the tool per turn
	// degrades to the pre-#906 behavior rather than misfiring.
	turns := newPlanTurnMemory()
	return func(ctx tool.Context, in recordPlanArgs) (recordPlanResult, error) {
		body := strings.TrimSpace(in.Plan)
		if body == "" {
			return recordPlanResult{}, errors.New("record_plan: plan is required (non-empty markdown)")
		}
		plansDir := filepath.Join(agentsDir, recordPlanDir)
		if err := os.MkdirAll(plansDir, 0o755); err != nil {
			return recordPlanResult{}, fmt.Errorf("record_plan: create plans dir: %w", err)
		}
		// Ensure trailing newline so the artifact is POSIX-clean.
		if !strings.HasSuffix(body, "\n") {
			body += "\n"
		}
		owner := planOwnerFromContext(ctx)
		out, err := turns.record(planTurn{
			owner:      owner,
			invocation: planInvocationID(ctx),
			plansDir:   plansDir,
			body:       body,
		})
		if err != nil {
			return recordPlanResult{}, err
		}
		opened := markPlanRecorded(ctx, gate)
		return recordPlanResult{
			Path:     out.path,
			Sequence: out.seq,
			Outcome:  out.outcome,
			Message:  planResultMessage(gate, out, opened),
			Agent:    owner.Agent,
			Session:  owner.Session,
		}, nil
	}
}

// Outcomes reported by planTurnMemory.record and echoed to the model in
// recordPlanResult.Outcome.
const (
	// planOutcomeRecorded: a new plan-<seq>.md was allocated and written.
	planOutcomeRecorded = "recorded"
	// planOutcomeUpdated: the author's current plan artifact was
	// overwritten in place because they already recorded one this turn.
	planOutcomeUpdated = "updated"
	// planOutcomeUnchanged: nothing was written — the current artifact
	// already holds this exact plan.
	planOutcomeUnchanged = "unchanged"
)

// planTurnMemoryLimit caps how many authors planTurnMemory remembers.
// One entry per (agent, session) pair, least-recently-used evicted
// first, so a long-lived multi-session daemon cannot grow this without
// bound. The cost of eviction is a false "recorded" — the pre-#906
// behavior — not a wrong file.
//
// LRU rather than insert-order matters more than it looks: the
// synchronous subagent door derives a fresh session ID per delegation
// (pkg/agent/subagent.go), so a parent that fans out produces a stream
// of single-use keys. Under insert-order eviction the busiest author —
// the primary session, first inserted and never re-inserted — would be
// the first thing that churn evicted, turning the guard off for
// exactly the session it was written for.
const planTurnMemoryLimit = 64

// planDirMu serializes every mutation of an agent's plans directory in
// this process: record_plan's allocate-then-write, its in-place update,
// and /replan's find-then-rename. Two things need it.
//
// nextPlanSeq-then-write is read-modify-write over a shared directory,
// and ADK dispatches a turn's function calls concurrently, so two
// authors could compute the same sequence number and clobber each
// other. And the repeat guard's "is the remembered artifact still on
// disk?" check would otherwise be a TOCTOU against /replan running on
// the operator's goroutine: revoke archives plan-N.md between the check
// and the write, and the update lands a live plan back at a path the
// operator just retired, leaving plan-N.md and plan-N-revoked.md both
// on disk. Pre-#906 that interleaving produced a harmless extra
// sequence, so closing it is not optional — the guard introduced it.
//
// Package-level rather than per-tool because the directory is
// process-global while nothing guarantees one tool instance per
// agentsDir. It does NOT reach across processes: two daemons sharing a
// plans volume still race, which is the pre-existing situation and out
// of scope here.
var planDirMu sync.Mutex

// planTurn is one record_plan call reduced to what the repeat guard
// needs: who is writing, which turn they are in, and what they wrote.
type planTurn struct {
	owner      PlanOwner
	invocation string
	plansDir   string
	body       string
}

// key identifies the author. Deliberately NOT keyed by invocation:
// the entry has to outlive the turn so a later turn can compare its
// plan against the one already on disk. The invocation is stored
// inside the entry and compared, not hashed into the key.
//
// Agent and session both participate because <agentsDir>/plans/ is
// process-global while the plan gate is per-session (#747): a parent
// and its declarative subagent, or two concurrent tenants, must each
// get their own plan file even when they interleave inside one turn.
func (t planTurn) key() string {
	return t.owner.Agent + "\x00" + t.owner.Session
}

type planTurnEntry struct {
	invocation string
	path       string
	seq        int
	// digest is sha256(body), not the body. The entry outlives the turn,
	// and holding every author's last plan text for the life of the
	// process buys nothing a comparison can't do with 32 bytes.
	digest [sha256.Size]byte
}

// planWriteOutcome is what record did: which artifact is now current,
// and whether writing it allocated a sequence number.
type planWriteOutcome struct {
	path    string
	seq     int
	outcome string
}

// planTurnMemory is the repeat guard for record_plan (#906).
//
// The problem it solves is not tidiness. Before it, every call ran
// nextPlanSeq and wrote a fresh file, so a model that called record_plan
// eight times in one turn got eight artifacts and eight cheerful
// successes — observed live on 2026-09-02, minting plan-5 through
// plan-11. #857 tried to stop the same shape of loop on mark_task_done
// with an honest status string alone and the model ignored it thirteen
// times, so the load-bearing part here is the behavior: the second call
// does not get a new file.
//
// Why the state lives here and not on the Agent: pkg/agent's
// checkpointer keys its in-turn repeat flag off an Agent field cleared
// by the post-turn hook, but pkg/tools has no Agent and no turn hook.
// What it does have is the invocation ID on tool.Context, which ADK
// mints per invocation and threads through every tool call in that
// turn — the same signal, read where the code already stands, and
// self-expiring (a new turn simply brings a new ID) rather than needing
// a reset callback that a library caller could forget to wire.
type planTurnMemory struct {
	mu      sync.Mutex
	entries map[string]planTurnEntry
	order   []string // least-recently-used first, for bounded eviction
}

func newPlanTurnMemory() *planTurnMemory {
	return &planTurnMemory{entries: make(map[string]planTurnEntry)}
}

// record persists t's plan and reports what that took.
//
// Three cases, in the order they are checked:
//
//   - The author's remembered artifact still exists and holds this exact
//     plan → write nothing. True whether or not it is the same turn: an
//     identical plan never earns a second file.
//   - It exists, the plan changed, and we are still in the same turn →
//     overwrite it. Minting a sibling would make the model's own
//     revision look like a second plan to every reader of the directory;
//     /replan is the explicit revoke-and-redraft path. The overwritten
//     draft is not archived: plan artifacts are the current plan, not a
//     version history, and an in-turn draft the model immediately
//     revised is not an operator decision worth preserving. The audit
//     trail that does matter — a plan the operator rejected — is what
//     /replan's -revoked.md rename keeps.
//   - Anything else — a new turn with a changed plan, an author we have
//     not seen, or a remembered path that is no longer on disk — →
//     allocate the next sequence number.
//
// That third clause is what keeps /replan honest: it renames the
// artifact to plan-<seq>-revoked.md, so the existence check fails and
// the redraft gets a fresh file instead of resurrecting a revoked
// sequence number.
//
// The whole body holds both m.mu (the author table) and planDirMu (the
// directory), file I/O included, and always in that order. See planDirMu
// for why the directory lock has to span the exists-check and the write
// rather than just the write.
func (m *planTurnMemory) record(t planTurn) (planWriteOutcome, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	planDirMu.Lock()
	defer planDirMu.Unlock()

	key := t.key()
	digest := sha256.Sum256([]byte(t.body))
	if prev, ok := m.entries[key]; ok && planArtifactExists(prev.path) {
		if prev.digest == digest {
			m.touch(key)
			return planWriteOutcome{path: prev.path, seq: prev.seq, outcome: planOutcomeUnchanged}, nil
		}
		if prev.invocation == t.invocation {
			if err := writePlanArtifact(prev.path, prev.seq, t.owner, t.body); err != nil {
				return planWriteOutcome{}, err
			}
			prev.digest = digest
			m.entries[key] = prev
			m.touch(key)
			return planWriteOutcome{path: prev.path, seq: prev.seq, outcome: planOutcomeUpdated}, nil
		}
	}

	seq, err := nextPlanSeq(t.plansDir)
	if err != nil {
		return planWriteOutcome{}, fmt.Errorf("record_plan: compute next seq: %w", err)
	}
	path := filepath.Join(t.plansDir, fmt.Sprintf("plan-%d.md", seq))
	if err := writePlanArtifact(path, seq, t.owner, t.body); err != nil {
		return planWriteOutcome{}, err
	}
	m.remember(key, planTurnEntry{invocation: t.invocation, path: path, seq: seq, digest: digest})
	return planWriteOutcome{path: path, seq: seq, outcome: planOutcomeRecorded}, nil
}

// remember stores an entry under key and marks it most-recently-used,
// evicting the least-recently-used entries past planTurnMemoryLimit.
// Caller holds m.mu.
func (m *planTurnMemory) remember(key string, e planTurnEntry) {
	m.entries[key] = e
	m.touch(key)
	for len(m.order) > planTurnMemoryLimit {
		delete(m.entries, m.order[0])
		m.order = m.order[1:]
	}
}

// touch moves key to the most-recently-used end of the eviction order.
// Caller holds m.mu.
func (m *planTurnMemory) touch(key string) {
	for i, k := range m.order {
		if k == key {
			m.order = append(m.order[:i], m.order[i+1:]...)
			break
		}
	}
	m.order = append(m.order, key)
}

// planArtifactExists reports whether the remembered plan is still on
// disk. False after /replan archived it, or after an operator deleted
// it by hand — both of which mean the next plan is a new plan.
func planArtifactExists(path string) bool {
	if path == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}

// writePlanArtifact renders frontmatter + body and lands it atomically.
func writePlanArtifact(path string, seq int, owner PlanOwner, body string) error {
	artifact := planFrontmatter(seq, owner) + body
	if err := atomicWriteFile(path, []byte(artifact), 0o644); err != nil {
		return fmt.Errorf("record_plan: write %s: %w", path, err)
	}
	return nil
}

// planInvocationID reads the per-turn identifier off the invocation
// context. ADK mints one ("e-<uuid>") per invocation and every tool
// call in that turn sees the same value, which is what makes it a
// usable turn boundary here.
//
// Empty for a nil ctx (library callers, unit tests driving the handler
// directly). An empty ID compares equal to the next empty ID, so such a
// caller is treated as one long turn: repeats update in place instead of
// minting siblings. That is the conservative direction for a guard whose
// entire job is to stop unbounded writes, and it costs a host that
// declines to identify its turns nothing it can't get from /replan.
func planInvocationID(ctx tool.Context) string {
	if ctx == nil {
		return ""
	}
	return ctx.InvocationID()
}

// planResultMessage is what record_plan tells the model it just did:
// one clause for the artifact, one for the gate. They are separate
// because they became independent in #906 — a repeat call can change
// the artifact without touching the gate, and a first call in a later
// turn can change the artifact when the gate is already open.
func planResultMessage(gate *permissions.Gate, out planWriteOutcome, opened bool) string {
	return planArtifactClause(out) + " " + planGateClause(gate, opened)
}

// planArtifactClause says what happened on disk, and for a repeat says
// plainly that no new file exists. The wording is not the defence —
// #857 proved prose alone does not stop a loop, and the defence here is
// that the file genuinely was not created — but a model told "recorded"
// eight times has no way to notice it is looping, so the message has to
// stop asserting a fresh artifact that isn't there.
//
// The unchanged clause is careful to describe what this tool did rather
// than what the file contains. The guard compares against the digest of
// the plan we last wrote, not against the bytes on disk, so an operator
// who hand-edits plan-N.md between calls would make any claim about the
// file's current contents a lie. "You already recorded this" stays true
// either way.
func planArtifactClause(out planWriteOutcome) string {
	switch out.outcome {
	case planOutcomeUnchanged:
		return fmt.Sprintf("No plan file was written: you already recorded this exact plan this session, and it is filed at %s. Recording an identical plan again cannot do anything further — get on with carrying it out, or ask the operator for /replan if it needs to be withdrawn.", out.path)
	case planOutcomeUpdated:
		return fmt.Sprintf("Plan updated in place at %s: you had already recorded a plan this turn, so this revision replaced it rather than filing a second plan file — the earlier draft is not kept. There is still exactly one plan (plan %d) for this task.", out.path, out.seq)
	default:
		return fmt.Sprintf("Plan recorded at %s.", out.path)
	}
}

// planGateClause reports the permission effect, and only when there was
// one. Three things it must not do, all learned the hard way (#747):
//
//   - Claim an unblock in advisory mode. Nothing was ever blocked, and
//     a model that believes a gate exists behaves as though it does.
//     This is the result-path half of the description split above
//     (#215), which fixed the same bug one surface earlier.
//   - Name a category instead of the tools. The 2026-08-14 GKE recipe
//     disabled bash / write_file / edit_file / delete_file, so "mutating
//     tools are now unblocked" named an empty set while saying nothing
//     about what the plan really unblocked: the whole `gke` MCP read
//     surface, which is deliberately not plan-exempt.
//   - Enumerate a set it doesn't have. A host that wires tools by hand
//     never calls RegisterPlanGatedTools, and inventing "nothing is
//     gated" for it would be the same unenforceable claim pointed the
//     other way. Unknown gets prose that doesn't enumerate.
//
// And since #906, a fourth: don't announce a transition that didn't
// happen. `opened` is false when the gate was already satisfied — every
// repeat, and every plan after the first in a session — and re-reading
// "Now unblocked for this session: mcp, spawn_agent, wait_and_verify"
// seven times was the loop telling itself it was making progress.
func planGateClause(gate *permissions.Gate, opened bool) string {
	const revoke = "The operator can revoke via /replan, which archives the artifact"
	if !gate.PlanRequired() {
		return fmt.Sprintf("plan_mode is advisory: no tool call was ever blocked on this plan and none becomes callable because of it — the artifact is the operator's audit trail, so carry the plan out in this turn rather than waiting for approval. %s and asks for a redraft.", revoke)
	}
	if !opened {
		return fmt.Sprintf("Plan-first gating is on and was already satisfied for this session before this call — no tool became callable that wasn't already. %s, clears the gate flag, and forces a redraft.", revoke)
	}
	gated, known := gate.PlanGatedTools()
	switch {
	case !known:
		return fmt.Sprintf("Plan-first gating is on: the tool calls it was denying are now unblocked for this session. %s, clears the gate flag, and forces a redraft.", revoke)
	case len(gated) == 0:
		return fmt.Sprintf("Plan-first gating is on, but this build registered no plan-gated tools — nothing was blocked and nothing is unblocked; the artifact is the only effect. %s, clears the gate flag, and forces a redraft.", revoke)
	default:
		return fmt.Sprintf("Now unblocked for this session: %s. %s, clears the gate flag, and forces a redraft.", strings.Join(gated, ", "), revoke)
	}
}

// planFrontmatter is the YAML block prepended to every plan artifact.
//
// Without it a plans directory is a pile of anonymous markdown: the
// 2026-08-14 UAT had a parent and its declarative subagent write plan-1
// and plan-2 into one directory in one incident, and nothing on disk
// said which was which. Worse in multi-session, where the gate flag is
// per-session but <agentsDir>/plans/ is process-global, so concurrent
// tenants interleave into one sequence.
//
// Values are emitted as double-quoted YAML scalars because agent names
// come from operator config and a bare `agent: foo: bar` would not
// parse. Keys are omitted rather than emitted empty so a reader can
// tell "no attribution recorded" from "recorded as empty". No
// timestamp: the file's mtime already carries it, and a clock in here
// would make every artifact test non-deterministic.
func planFrontmatter(seq int, owner PlanOwner) string {
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "plan: %d\n", seq)
	if owner.Agent != "" {
		fmt.Fprintf(&b, "agent: %q\n", owner.Agent)
	}
	if owner.Session != "" {
		fmt.Fprintf(&b, "session: %q\n", owner.Session)
	}
	b.WriteString("---\n\n")
	return b.String()
}

// planOwnerFromContext reads the recording agent and session off the
// invocation context. A nil ctx (library callers, unit tests that drive
// the handler directly) yields the zero owner, which planFrontmatter
// renders as no attribution — same guard rationale as
// permissions.SessionGateFromContext.
func planOwnerFromContext(ctx tool.Context) PlanOwner {
	if ctx == nil {
		return PlanOwner{}
	}
	return PlanOwner{Agent: ctx.AgentName(), Session: ctx.SessionID()}
}

// markPlanRecorded routes the plan-recorded flip through the per-
// session sub-gate when one is threaded on ctx (multi-session mode).
// Falls back to the template gate for single-session paths (direct-
// attach without DeriveForSession, mock runs, tests) where no session
// gate is set on ctx.
//
// Why this indirection: the captured `template` closure variable in
// recordPlanFunc is the daemon-wide template gate; every permission
// check consults resolveSessionGate(ctx) which returns the per-
// session sub-gate (see pkg/permissions/gate.go). Marking the
// template WITHOUT marking the session sub-gate produced an infinite
// loop during the v2.6 GKE-troubleshoot demo drive: record_plan
// "succeeded" on every call but list_skills / write_file / bash
// stayed denied because the check-side gate never saw
// planRecorded=true. Filed as #214.
//
// Extracted as its own helper so unit tests can exercise both paths
// without stubbing the full tool.Context interface.
//
// Returns whether this call actually opened the gate. False means the
// flag was already set — which is every repeat within a turn and every
// plan after the first in a session — and the caller must not then
// announce an unblock that already happened (#906). Read on the same
// gate it marks, so the answer is about the gate the model's next call
// will be checked against, not the template it may not be using.
func markPlanRecorded(ctx context.Context, template *permissions.Gate) bool {
	if sg, ok := permissions.SessionGateFromContext(ctx); ok {
		return sg.MarkPlanRecordedOnce()
	}
	if template != nil {
		return template.MarkPlanRecordedOnce()
	}
	return false
}

// nextPlanSeq returns max(seq)+1 over every `plan-<seq>.md` and
// `plan-<seq>-revoked.md` in plansDir. Missing directory returns
// 1. Names that don't match the pattern are ignored.
func nextPlanSeq(plansDir string) (int, error) {
	entries, err := os.ReadDir(plansDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 1, nil
		}
		return 0, err
	}
	max := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := recordPlanFilenameRegex.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		n, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		if n > max {
			max = n
		}
	}
	return max + 1, nil
}

// PlanOwner identifies who recorded a plan. It is both what
// record_plan stamps into the artifact's frontmatter and the selector
// /replan uses to find the plan the operator actually meant.
//
// The zero value matches everything, which is what keeps
// RevokeLatestPlan's historical "newest wins" semantics intact for
// callers that don't care.
type PlanOwner struct {
	// Agent is the recording agent's name (tool.Context.AgentName) —
	// "core_agent" for the root, the subagent's declared name for a
	// background subagent. This is the field that separates a parent's
	// plan from its specialist's.
	Agent string
	// Session is the recording agent's session ID. Separates concurrent
	// tenants in a multi-session daemon, where the plan gate is per-
	// session but <agentsDir>/plans/ is process-global.
	Session string
}

func (o PlanOwner) empty() bool { return o.Agent == "" && o.Session == "" }

// matches reports whether p was recorded by this owner. An empty field
// on the owner is a wildcard: {Agent: "x"} matches every plan agent "x"
// recorded regardless of session.
func (o PlanOwner) matches(p PlanInfo) bool {
	if o.Agent != "" && p.Agent != o.Agent {
		return false
	}
	if o.Session != "" && p.Session != o.Session {
		return false
	}
	return true
}

// PlanInfo is one active plan artifact plus whatever attribution its
// frontmatter carried. Agent and Session are empty for artifacts
// written before frontmatter existed (#747) — see ActivePlans.
type PlanInfo struct {
	Path     string
	Sequence int
	Agent    string
	Session  string
}

// ActivePlans returns every non-revoked plan in <agentsDir>/plans/,
// newest sequence first, with frontmatter attribution parsed. Exposed
// so a host can render "who planned what" without re-deriving the
// naming scheme.
func ActivePlans(agentsDir string) []PlanInfo {
	if agentsDir == "" {
		return nil
	}
	plansDir := filepath.Join(agentsDir, recordPlanDir)
	entries, err := os.ReadDir(plansDir)
	if err != nil {
		return nil
	}
	var actives []PlanInfo
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		m := recordPlanFilenameRegex.FindStringSubmatch(name)
		if m == nil {
			continue
		}
		// Skip the -revoked variant; we only want active plans.
		if strings.Contains(name, "-revoked.md") {
			continue
		}
		n, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		path := filepath.Join(plansDir, name)
		info := PlanInfo{Path: path, Sequence: n}
		// An unreadable artifact is still an active plan — losing it
		// from the list would make /replan skip past it to someone
		// else's, which is the bug this whole change is about.
		if raw, err := os.ReadFile(path); err == nil {
			info.Agent, info.Session = parsePlanFrontmatter(raw)
		}
		actives = append(actives, info)
	}
	sort.Slice(actives, func(i, j int) bool { return actives[i].Sequence > actives[j].Sequence })
	return actives
}

// parsePlanFrontmatter pulls agent + session out of the leading YAML
// block planFrontmatter wrote. Deliberately not a YAML parser: the
// block is machine-written and single-level, and pulling in a parser
// to read two strings would be the larger risk. Anything that doesn't
// look like our block yields empty strings, which callers read as "no
// attribution" — the pre-#747 artifact shape.
func parsePlanFrontmatter(raw []byte) (agent, session string) {
	text := string(raw)
	if !strings.HasPrefix(text, "---\n") {
		return "", ""
	}
	rest := text[len("---\n"):]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return "", ""
	}
	for _, line := range strings.Split(rest[:end], "\n") {
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		val = strings.TrimSpace(val)
		if unquoted, err := strconv.Unquote(val); err == nil {
			val = unquoted
		}
		switch strings.TrimSpace(key) {
		case "agent":
			agent = val
		case "session":
			session = val
		}
	}
	return agent, session
}

// LatestActivePlan returns the path of the highest-sequence non-
// revoked plan in <agentsDir>/plans/, or empty string if none.
//
// Owner-blind: with a parent and a subagent both planning, this is the
// subagent's. Use LatestPlanBy when you mean a particular author —
// this spelling is kept for callers that genuinely want "the newest
// artifact on disk", such as reporting one in a message.
func LatestActivePlan(agentsDir string) string {
	actives := ActivePlans(agentsDir)
	if len(actives) == 0 {
		return ""
	}
	return actives[0].Path
}

// LatestPlanBy returns the highest-sequence active plan recorded by
// owner, or empty string if that author has none.
//
// The zero owner means "any", and returns LatestActivePlan.
//
// Back-compat: when no active plan carries attribution at all — a
// plans directory written before #747 — the newest is returned even
// for a non-empty owner, because "the operator upgraded mid-incident"
// should not read as "you have no plan". A directory with a MIX of
// attributed and anonymous plans gets the strict answer: once some
// artifact can say who wrote it, silently revoking one that can't is
// the guess this function exists to stop making.
func LatestPlanBy(agentsDir string, owner PlanOwner) string {
	actives := ActivePlans(agentsDir)
	if len(actives) == 0 {
		return ""
	}
	if owner.empty() {
		return actives[0].Path
	}
	attributed := false
	for _, p := range actives {
		if p.Agent != "" || p.Session != "" {
			attributed = true
		}
		if owner.matches(p) {
			return p.Path
		}
	}
	if !attributed {
		return actives[0].Path
	}
	return ""
}

// RevokeLatestPlan renames `<agentsDir>/plans/plan-<seq>.md` to
// `plan-<seq>-revoked.md` (preserving the audit trail) and clears
// the gate's planRecorded flag. Called by /replan.
//
// Returns the path of the file that was archived (empty if no
// active plan existed). An empty path with no error means there
// was nothing to revoke; the gate flag is still cleared in case it
// was set out of band.
func RevokeLatestPlan(gate *permissions.Gate, agentsDir string) (string, error) {
	return RevokePlanBy(gate, agentsDir, PlanOwner{})
}

// RevokePlanBy is RevokeLatestPlan scoped to an author. It archives
// the highest-sequence active plan that owner recorded and clears the
// gate flag; the zero owner reproduces RevokeLatestPlan exactly.
//
// This exists because "latest" is the wrong plan as soon as anything
// else in the process plans too (#747). With a parent and a background
// subagent both recording, the newest artifact is the subagent's, so an
// operator rejecting the parent's approach was archiving the
// specialist's investigation notes and leaving the plan they meant to
// reject in place.
//
// An empty return with a nil error means owner had no active plan. The
// gate flag is cleared either way — /replan's contract is "the next
// mutating call needs a fresh plan", and that must hold whether or not
// there was an artifact to file.
//
// Holds planDirMu across the find-then-rename so it cannot interleave
// with a concurrent record_plan; see planDirMu (#906).
func RevokePlanBy(gate *permissions.Gate, agentsDir string, owner PlanOwner) (string, error) {
	defer gate.ClearPlanRecorded()
	planDirMu.Lock()
	defer planDirMu.Unlock()
	latest := LatestPlanBy(agentsDir, owner)
	if latest == "" {
		return "", nil
	}
	dir, name := filepath.Split(latest)
	revoked := filepath.Join(dir, strings.TrimSuffix(name, ".md")+"-revoked.md")
	if err := os.Rename(latest, revoked); err != nil {
		return "", fmt.Errorf("revoke %s: %w", latest, err)
	}
	return revoked, nil
}

// atomicWriteFile writes via temp file + rename so a partial write
// can never leave a corrupt plan-<seq>.md on disk. Used because the
// plan artifact is read by /memory-style provenance commands and a
// half-written file would surface as a corrupted plan.
func atomicWriteFile(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".plan-")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		cleanup()
		return err
	}
	return nil
}
