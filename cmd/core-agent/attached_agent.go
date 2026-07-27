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

package main

import (
	"fmt"

	adkmodel "google.golang.org/adk/model"

	"github.com/go-steer/core-agent/v2/pkg/agent"
	"github.com/go-steer/core-agent/v2/pkg/attach"
	"github.com/go-steer/core-agent/v2/pkg/attachadapter"
)

// buildAttachedAgent is the one construction path every branch of
// run() (TUI, --no-repl wake loop, plain-REPL fallback, and the TUI's
// mid-session model switch) goes through since the pkg/agent split
// (#388 phase 4): construct the agent from core options, wrap it in
// the attach adapter that carries the operator-surface capability
// closures, and — when attach-mode is enabled — register the adapter
// so the agent is reachable over HTTP/SSE.
//
// reg may be nil (attach-mode disabled); the adapter is still built
// because the in-process TUI reads the same capability surface
// (AttachTools / AttachStatus / AttachUsage / AttachReload /
// AttachReplan) that remote operators do.
//
// Registration errors (typically attach.ErrSessionExists from a
// double-register) surface the same way agent.New used to surface
// them when registration lived inside construction.
func buildAttachedAgent(m adkmodel.LLM, agentOpts []agent.Option, adapterOpts []attachadapter.Option, reg *attach.SessionRegistry) (*agent.Agent, *attachadapter.Adapter, error) {
	a, err := agent.New(m, agentOpts...)
	if err != nil {
		return nil, nil, err
	}
	ad := attachadapter.New(a, adapterOpts...)
	if reg != nil {
		if _, err := reg.Register(ad); err != nil {
			return nil, nil, fmt.Errorf("agent: attach registry: %w", err)
		}
	}
	return a, ad, nil
}
