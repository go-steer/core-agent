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

package attach

// AgentRegistrarAdapter satisfies agent.attachRegistrar (an
// interface defined in package agent with method set
// `Register(agent.Registrant) (agent.RegisterEntry, error)` + Unregister)
// using a *SessionRegistry from this package. We keep the indirection
// because agent/ cannot import attach/ (would create a cycle —
// attach/ depends on agent's *Agent via the Registrant shape).
//
// agent.Registrant and attach.Registrant have the same method set,
// so any *agent.Agent already satisfies the attach.Registrant we
// need internally. The adapter just relabels the registry's typed
// return for the agent package's RegisterEntry alias.
//
// Typical wiring in a consumer binary:
//
//	reg := attach.NewSessionRegistry()
//	ag, err := agent.New(m,
//	    agent.WithSessionRegistry(attach.NewAgentRegistrarAdapter(reg)),
//	    // ...
//	)
type AgentRegistrarAdapter struct {
	reg *SessionRegistry
}

// NewAgentRegistrarAdapter wraps reg so it satisfies
// agent.attachRegistrar.
func NewAgentRegistrarAdapter(reg *SessionRegistry) *AgentRegistrarAdapter {
	return &AgentRegistrarAdapter{reg: reg}
}

// ErrNotRegistrant is returned by Register when the supplied agent
// doesn't satisfy attach.Registrant. Practically this can't happen
// when *agent.Agent is the consumer (it implements all the
// methods); the check is defensive against future agent constructor
// refactors.
type ErrNotRegistrant struct{ Got any }

func (e *ErrNotRegistrant) Error() string {
	return "attach: AgentRegistrarAdapter.Register: argument does not implement attach.Registrant"
}

// Register accepts any (called from agent/, which can't import
// attach/ for Registrant), type-asserts to Registrant, and forwards
// to the wrapped registry. Returns ErrNotRegistrant if the type
// assertion fails.
func (a *AgentRegistrarAdapter) Register(ag any) (any, error) {
	r, ok := ag.(Registrant)
	if !ok {
		return nil, &ErrNotRegistrant{Got: ag}
	}
	return a.reg.Register(r)
}

// Unregister forwards to the registry.
func (a *AgentRegistrarAdapter) Unregister(appName, userID, sessionID string) {
	a.reg.Unregister(appName, userID, sessionID)
}
