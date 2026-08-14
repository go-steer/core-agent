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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

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
	recordPlanDescCommon = "Plan is free-form markdown — typical shape: goal, files to change, approach, risks, test plan, out of scope. The plan is persisted to .agents/plans/plan-<seq>.md and visible to the operator in chat. To revise an existing plan, just call record_plan again — each call writes a new plan file with the next sequence number."

	recordPlanDescRequired = "Record the agent's implementation plan as a markdown artifact and unblock mutating tools. Plan-first gating is ON: call this BEFORE any write_file / edit_file / delete_file / bash / spawn_agent call, or those calls are denied with a 'plan required' error. " + recordPlanDescCommon

	recordPlanDescAdvisory = "Record the agent's implementation plan as a markdown artifact for the operator's audit trail. Plan-first gating is OFF — no tool call is blocked on this, so record the plan and then carry it out in the same turn rather than stopping to wait for approval. " + recordPlanDescCommon
)

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
		desc = recordPlanDescRequired
	}
	return functiontool.New(functiontool.Config{
		Name:        "record_plan",
		Description: desc,
	}, recordPlanFunc(gate, agentsDir))
}

func recordPlanFunc(gate *permissions.Gate, agentsDir string) functiontool.Func[recordPlanArgs, recordPlanResult] {
	return func(ctx tool.Context, in recordPlanArgs) (recordPlanResult, error) {
		body := strings.TrimSpace(in.Plan)
		if body == "" {
			return recordPlanResult{}, errors.New("record_plan: plan is required (non-empty markdown)")
		}
		plansDir := filepath.Join(agentsDir, recordPlanDir)
		if err := os.MkdirAll(plansDir, 0o755); err != nil {
			return recordPlanResult{}, fmt.Errorf("record_plan: create plans dir: %w", err)
		}
		seq, err := nextPlanSeq(plansDir)
		if err != nil {
			return recordPlanResult{}, fmt.Errorf("record_plan: compute next seq: %w", err)
		}
		name := fmt.Sprintf("plan-%d.md", seq)
		path := filepath.Join(plansDir, name)
		// Ensure trailing newline so the artifact is POSIX-clean.
		if !strings.HasSuffix(body, "\n") {
			body += "\n"
		}
		owner := planOwnerFromContext(ctx)
		artifact := planFrontmatter(seq, owner) + body
		if err := atomicWriteFile(path, []byte(artifact), 0o644); err != nil {
			return recordPlanResult{}, fmt.Errorf("record_plan: write %s: %w", path, err)
		}
		markPlanRecorded(ctx, gate)
		return recordPlanResult{
			Path:     path,
			Sequence: seq,
			Message:  planRecordedMessage(gate, path),
			Agent:    owner.Agent,
			Session:  owner.Session,
		}, nil
	}
}

// planRecordedMessage renders what record_plan tells the model it just
// did. Three things it must not do, all learned the hard way (#747):
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
func planRecordedMessage(gate *permissions.Gate, path string) string {
	const revoke = "The operator can revoke via /replan, which archives the artifact"
	if !gate.PlanRequired() {
		return fmt.Sprintf("Plan recorded at %s. plan_mode is advisory: no tool call was ever blocked on this plan and none becomes callable because of it — the artifact is the operator's audit trail, so carry the plan out in this turn rather than waiting for approval. %s and asks for a redraft.", path, revoke)
	}
	gated, known := gate.PlanGatedTools()
	switch {
	case !known:
		return fmt.Sprintf("Plan recorded at %s. Plan-first gating is on: the tool calls it was denying are now unblocked for this session. %s, clears the gate flag, and forces a redraft.", path, revoke)
	case len(gated) == 0:
		return fmt.Sprintf("Plan recorded at %s. Plan-first gating is on, but this build registered no plan-gated tools — nothing was blocked and nothing is unblocked; the artifact is the only effect. %s, clears the gate flag, and forces a redraft.", path, revoke)
	default:
		return fmt.Sprintf("Plan recorded at %s. Now unblocked for this session: %s. %s, clears the gate flag, and forces a redraft.", path, strings.Join(gated, ", "), revoke)
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
func markPlanRecorded(ctx context.Context, template *permissions.Gate) {
	if sg, ok := permissions.SessionGateFromContext(ctx); ok {
		sg.MarkPlanRecorded()
		return
	}
	if template != nil {
		template.MarkPlanRecorded()
	}
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
func RevokePlanBy(gate *permissions.Gate, agentsDir string, owner PlanOwner) (string, error) {
	defer gate.ClearPlanRecorded()
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
