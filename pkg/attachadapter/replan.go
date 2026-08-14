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

package attachadapter

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/go-steer/core-agent/v2/pkg/attach"
	"github.com/go-steer/core-agent/v2/pkg/permissions"
	"github.com/go-steer/core-agent/v2/pkg/tools"
)

// ReplanHandler builds the closure WithReplanner wants: archive the
// caller's own active plan artifact, clear the gate's planRecorded
// flag, and describe what actually happened in terms the operator can
// act on.
//
// It lives here rather than in a host because there are two hosts. The
// single-session CLI wired this inline; the multi-session hub left
// /replan returning 501, which meant a recipe running plan_mode
// "required" under the hub could arm the plan-first gate and never
// revoke it from the session that owned the plan (#763). Duplicating
// the closure would have duplicated the parts #747 proved are
// load-bearing: the owner scoping and the three-way message.
//
// gate is required — clearing the flag is half the contract, and a
// handler that silently skipped it would report a revocation that
// didn't happen; a nil one degrades to an honest error rather than
// panicking a tenant's request. agentsDir may be empty; the closure
// then reports that plan artifacts have nowhere to live instead of
// pretending success.
//
// owner is a func, not a value, because both hosts learn the agent's
// identity after the adapter options are built (the CLI late-binds
// agentRef via WithPostConstruct). A nil owner, or one returning the
// zero PlanOwner, degrades to newest-wins — the pre-#747 behavior,
// which is the right fallback for a /replan that beat construction.
func ReplanHandler(gate *permissions.Gate, agentsDir string, owner func() tools.PlanOwner) func(context.Context, attach.ReplanRequest) (attach.ReplanResponse, error) {
	return func(_ context.Context, _ attach.ReplanRequest) (attach.ReplanResponse, error) {
		if gate == nil {
			// A wiring bug rather than a configuration, but a daemon
			// serving many tenants should say so on one request rather
			// than panic out of an HTTP handler.
			return attach.ReplanResponse{
				Message: "/replan unavailable: no permissions gate wired for this session (report this — it is a host wiring bug, not a configuration)",
			}, nil
		}
		// Hosts wire this unconditionally; with plan_mode off,
		// RevokePlanBy returns "" with no error and the gate flag was
		// never set, so the response just says "no plan to revoke".
		if agentsDir == "" {
			return attach.ReplanResponse{
				Message: "/replan unavailable: no .agents/ directory resolved (plan artifacts have nowhere to live)",
			}, nil
		}
		// Scope the revocation to this agent's own plan (#747). A
		// background subagent that recorded a plan holds the highest
		// sequence number, so the owner-blind spelling archived the
		// specialist's notes and left the plan the operator was
		// rejecting active.
		var who tools.PlanOwner
		if owner != nil {
			who = owner()
		}
		archived, err := tools.RevokePlanBy(gate, agentsDir, who)
		if err != nil {
			return attach.ReplanResponse{}, err
		}
		resp := attach.ReplanResponse{
			ArchivedPath:  archived,
			PlanWasActive: archived != "",
		}
		actives := tools.ActivePlans(agentsDir)
		switch {
		case archived == "" && len(actives) > 0:
			// Somebody's plan is active, just not this agent's. Saying
			// "no active plan" here would read as "the directory is
			// empty" and hide a plan the operator may well want to look
			// at. Report the artifact's own attribution rather than
			// asserting "another agent" — the commonest way to land
			// here is the same agent in an earlier session, after a
			// daemon restart.
			resp.Message = fmt.Sprintf("/replan: this agent has no active plan to revoke — %s was left alone (%s). The gate flag is clear.",
				filepath.Base(actives[0].Path), DescribePlanOwner(actives[0]))
		case archived == "":
			resp.Message = "/replan: no active plan to revoke (gate flag is clear)."
		case gate.PlanRequired():
			resp.Message = fmt.Sprintf("Plan revoked. Archived to %s. The next mutating tool call will be denied until the agent calls record_plan again.", archived)
		default:
			// Advisory mode: the artifact is archived, but promising a
			// denial that will never come is the claim-the-runtime-
			// doesn't-enforce bug this mode exists to avoid.
			resp.Message = fmt.Sprintf("Plan revoked. Archived to %s. plan_mode is advisory, so no tool call is blocked — ask the agent to record a new plan if you want a fresh artifact.", archived)
		}
		return resp, nil
	}
}

// DescribePlanOwner renders a plan artifact's frontmatter attribution
// for the /replan message that reports a plan it declined to archive
// (#747). It states what the file says rather than inferring: "another
// agent" would be a guess, and the likeliest way to reach this branch
// is the same agent in an earlier session after a daemon restart.
func DescribePlanOwner(p tools.PlanInfo) string {
	switch {
	case p.Agent != "" && p.Session != "":
		return fmt.Sprintf("recorded by %q in session %s", p.Agent, p.Session)
	case p.Agent != "":
		return fmt.Sprintf("recorded by %q", p.Agent)
	case p.Session != "":
		return fmt.Sprintf("recorded in session %s", p.Session)
	default:
		return "it records no author, though another plan here does"
	}
}
