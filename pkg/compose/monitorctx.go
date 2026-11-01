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

// Originally derived from go-steer/mast.

package compose

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"strings"
	"time"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/memory"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/toolconfirmation"
	"google.golang.org/genai"
)

// collectAgentName is the agent identity presented when the runtime executes a
// monitor.collect or monitor.ack tool on its own behalf outside any LLM turn.
const collectAgentName = "mast:monitor"

// collectContext implements google.golang.org/adk/v2/agent.Context for
// deterministic tool calls executed outside an LLM turn (#1406).
type collectContext struct {
	context.Context

	appName   string
	userID    string
	sessionID string
	actions   *session.EventActions
}

func newCollectContext(ctx context.Context, appName, userID, sessionID string) *collectContext {
	if ctx == nil {
		ctx = context.Background()
	}
	return &collectContext{
		Context:   ctx,
		appName:   appName,
		userID:    userID,
		sessionID: sessionID,
		actions: &session.EventActions{
			StateDelta:    map[string]any{},
			ArtifactDelta: map[string]int64{},
		},
	}
}

func (c *collectContext) AppName() string      { return c.appName }
func (c *collectContext) UserID() string       { return c.userID }
func (c *collectContext) SessionID() string    { return c.sessionID }
func (c *collectContext) AgentName() string    { return collectAgentName }
func (c *collectContext) InvocationID() string { return "" }

func (c *collectContext) UserContent() *genai.Content                          { return nil }
func (c *collectContext) Branch() string                                       { return "" }
func (c *collectContext) IsolationScope() string                               { return "" }
func (c *collectContext) ReadonlyState() session.ReadonlyState                 { return emptyState{} }
func (c *collectContext) State() session.State                                 { return emptyState{} }
func (c *collectContext) Artifacts() adkagent.Artifacts                        { return nil }
func (c *collectContext) Memory() adkagent.Memory                              { return nil }
func (c *collectContext) Session() session.Session                             { return nil }
func (c *collectContext) Agent() adkagent.Agent                                { return nil }
func (c *collectContext) RunConfig() *adkagent.RunConfig                       { return &adkagent.RunConfig{} }
func (c *collectContext) FunctionCallID() string                               { return "" }
func (c *collectContext) Actions() *session.EventActions                       { return c.actions }
func (c *collectContext) ToolConfirmation() *toolconfirmation.ToolConfirmation { return nil }
func (c *collectContext) ResumedInput(string) (any, bool)                      { return nil, false }
func (c *collectContext) Path() string                                         { return "" }
func (c *collectContext) RunID() string                                        { return "" }
func (c *collectContext) SubScheduler() adkagent.DynamicSubScheduler           { return nil }
func (c *collectContext) OutputForAncestors() []string                         { return nil }
func (c *collectContext) EndInvocation()                                       {}
func (c *collectContext) Ended() bool                                          { return false }

func (c *collectContext) RequestConfirmation(hint string, _ any) error {
	return errors.New("a tool run on the runtime's own behalf cannot ask for confirmation: it runs outside any turn, on no invocation, with nobody waiting to answer (hint was: " + hint + ")")
}

func (c *collectContext) SearchMemory(context.Context, string) (*memory.SearchResponse, error) {
	return nil, errors.New("a tool run on the runtime's own behalf has no memory service to search")
}

func (c *collectContext) WithContext(ctx context.Context) adkagent.InvocationContext {
	return c.with(ctx)
}

func (c *collectContext) WithAgentContext(ctx context.Context) adkagent.Context {
	return c.with(ctx)
}

func (c *collectContext) WithAgentTimeout(d time.Duration) (adkagent.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(c.Context, d)
	return c.with(ctx), cancel
}

func (c *collectContext) WithAgentCancel() (adkagent.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(c.Context)
	return c.with(ctx), cancel
}

func (c *collectContext) WithDelta(*adkagent.CommonContextDelta) adkagent.Context {
	return c.with(c.Context)
}

func (c *collectContext) WithICDelta(*adkagent.InvocationContextDelta) adkagent.InvocationContext {
	return c.with(c.Context)
}

func (c *collectContext) with(ctx context.Context) *collectContext {
	cp := *c
	cp.Context = ctx
	return &cp
}

var _ adkagent.Context = (*collectContext)(nil)

type emptyState struct{}

func (emptyState) Get(string) (any, error) { return nil, session.ErrStateKeyNotExist }

func (emptyState) Set(key string, _ any) error {
	return errors.New("a monitor.collect call cannot set session state (key " + key + "): it runs before the cycle's session is opened")
}

func (emptyState) All() iter.Seq2[string, any] {
	return func(func(string, any) bool) {}
}

var (
	_ session.State         = emptyState{}
	_ session.ReadonlyState = emptyState{}
)

// unwrapTool peels wrapper layers (such as pkg/mcp's digestingTool) so direct
// runtime callers outside the LLM dispatch path receive raw untruncated tool
// output (#1406).
func unwrapTool(t tool.Tool) tool.Tool {
	type unwrapper interface {
		Unwrap() tool.Tool
	}
	for i := 0; i < 8; i++ {
		u, ok := t.(unwrapper)
		if !ok {
			return t
		}
		next := u.Unwrap()
		if next == nil {
			return t
		}
		t = next
	}
	return t
}

type runnableTool interface {
	Run(ctx adkagent.Context, args any) (map[string]any, error)
}

// runOwnBehalf resolves name from toolsets, unwraps any model-facing digest
// wrapper, and executes the tool directly under cctx (#1406).
func runOwnBehalf(cctx adkagent.Context, toolsets []tool.Toolset, name string, args map[string]any) (map[string]any, error) {
	var found tool.Tool
	for _, ts := range toolsets {
		if ts == nil {
			continue
		}
		tools, err := ts.Tools(cctx)
		if err != nil {
			continue
		}
		for _, candidate := range tools {
			if candidate != nil && candidate.Name() == name {
				found = candidate
				break
			}
		}
		if found != nil {
			break
		}
	}
	if found == nil {
		return nil, fmt.Errorf("no tool named %q is wired into this deployment", name)
	}
	unwrapped := unwrapTool(found)
	runner, ok := unwrapped.(runnableTool)
	if !ok {
		return nil, fmt.Errorf("tool %q cannot be run directly on the runtime's own behalf", name)
	}
	if args == nil {
		args = map[string]any{}
	}
	return runner.Run(cctx, args)
}

// extractToolText extracts the textual payload from an ADK/MCP tool response
// map (#1406).
func extractToolText(res map[string]any) (string, error) {
	if res == nil {
		return "", errors.New("tool returned a nil result")
	}
	if rawOut, hasOutput := res["output"]; hasOutput {
		s, ok := rawOut.(string)
		if !ok {
			return "", fmt.Errorf("tool result has a non-text \"output\" field (%T); expected a text stream", rawOut)
		}
		return s, nil
	}
	for _, key := range []string{"text", "result"} {
		if s, ok := res[key].(string); ok {
			return s, nil
		}
	}
	if content, hasContent := res["content"]; hasContent {
		if s, ok := content.(string); ok {
			return s, nil
		}
		if items, ok := content.([]any); ok {
			var parts []string
			for _, item := range items {
				if m, ok := item.(map[string]any); ok {
					if t, ok := m["text"].(string); ok {
						parts = append(parts, t)
					}
				}
			}
			if len(parts) > 0 {
				return strings.Join(parts, "\n"), nil
			}
		}
	}
	b, err := json.Marshal(res)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
