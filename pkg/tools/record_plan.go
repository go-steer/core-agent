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

	"github.com/go-steer/core-agent/pkg/permissions"
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
}

// RecordPlan returns the built-in record_plan tool. Calling it with
// a non-empty plan writes the plan to `<agentsDir>/plans/plan-<seq>.md`
// and flips the gate's `planRecorded` flag, which unblocks mutating
// tool calls when RequirePlanArtifact is set.
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
	return functiontool.New(functiontool.Config{
		Name:        "record_plan",
		Description: "Record the agent's implementation plan as a markdown artifact and unblock mutating tools when plan-first gating is enabled. Call this BEFORE any write_file / edit_file / delete_file / bash / spawn_agent call when require_plan_artifact is on; otherwise those calls are denied with a 'plan required' error. Plan is free-form markdown — typical shape: goal, files to change, approach, risks, test plan, out of scope. The plan is persisted to .agents/plans/plan-<seq>.md and visible to the operator in chat. To revise an existing plan, just call record_plan again — each call writes a new plan file with the next sequence number.",
	}, recordPlanFunc(gate, agentsDir))
}

func recordPlanFunc(gate *permissions.Gate, agentsDir string) functiontool.Func[recordPlanArgs, recordPlanResult] {
	return func(_ tool.Context, in recordPlanArgs) (recordPlanResult, error) {
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
		if err := atomicWriteFile(path, []byte(body), 0o644); err != nil {
			return recordPlanResult{}, fmt.Errorf("record_plan: write %s: %w", path, err)
		}
		gate.MarkPlanRecorded()
		return recordPlanResult{
			Path:     path,
			Sequence: seq,
			Message:  fmt.Sprintf("Plan recorded at %s. Mutating tools are now unblocked for this session. The operator can revoke via /replan, which clears the gate flag and forces a redraft.", path),
		}, nil
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

// LatestActivePlan returns the path of the highest-sequence non-
// revoked plan in <agentsDir>/plans/, or empty string if none.
// Used by /replan to find what to archive.
func LatestActivePlan(agentsDir string) string {
	if agentsDir == "" {
		return ""
	}
	plansDir := filepath.Join(agentsDir, recordPlanDir)
	entries, err := os.ReadDir(plansDir)
	if err != nil {
		return ""
	}
	type p struct {
		seq  int
		name string
	}
	var actives []p
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
		actives = append(actives, p{seq: n, name: name})
	}
	if len(actives) == 0 {
		return ""
	}
	sort.Slice(actives, func(i, j int) bool { return actives[i].seq > actives[j].seq })
	return filepath.Join(plansDir, actives[0].name)
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
	defer gate.ClearPlanRecorded()
	latest := LatestActivePlan(agentsDir)
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
