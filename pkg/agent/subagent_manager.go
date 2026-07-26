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

package agent

import (
	"context"

	"github.com/go-steer/core-agent/v2/pkg/attach"
)

// SubagentManager is the narrow seam the core Agent uses to talk to a
// subagent/background manager without importing the background package
// (which imports agent, and would otherwise form an import cycle). The
// concrete implementation lives in pkg/agent/background; wire one via
// WithBackgroundManager.
//
// Every method is expressed in core/attach/primitive types only — no
// background-package types leak across this boundary. Callers that need
// the richer *background.Manager surface (repl, coretui) recover it with
// background.ManagerOf(agent).
type SubagentManager interface {
	// AttachParent sets the manager's parent back-reference so its
	// spawn calls can read the agent's session triple + session.Service
	// without the consumer plumbing them twice. Called once during
	// Agent construction.
	AttachParent(*Agent)

	// PrependPendingAlerts drains any pending background alerts
	// (non-blocking) and prepends them to the prompt the underlying ADK
	// runner sees, returning the augmented prompt. Called each turn of
	// Agent.Run.
	PrependPendingAlerts(prompt string) string

	// ListSubagents returns attach-facing metadata for the manager's
	// live subagents. Backs Agent.AttachAgents (attach.AgentLister).
	ListSubagents() []attach.AgentInfo

	// SpawnSubagent spawns a subagent from an attach spec. Backs
	// Agent.AttachSpawnSubagent (attach.SubagentSpawner).
	SpawnSubagent(ctx context.Context, spec attach.SubagentSpec) (attach.SubagentSpawnResponse, error)
}
