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
	"fmt"
	"strings"
	"time"

	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
)

// LifecycleEvent is the payload delivered to a LifecycleHandler each
// time the model calls a tool built by NewLifecycleTool. State is the
// value the model passed (e.g. "thinking", "blocked", "done"); Detail
// is the optional human-readable context. Time is the moment the
// handler received the call.
type LifecycleEvent struct {
	State  string
	Detail string
	Time   time.Time
}

// LifecycleHandler receives each lifecycle emit. Returning a non-nil
// error surfaces the error string back to the model as the tool's
// response — useful for "this state isn't valid right now" feedback.
// A nil return acks the call with a generic "ok" so the model can
// keep working.
type LifecycleHandler func(ctx context.Context, ev LifecycleEvent) error

// LifecycleOptions configures NewLifecycleTool.
type LifecycleOptions struct {
	// Handler is invoked once per emit. Required.
	Handler LifecycleHandler

	// Name overrides the tool's function name. Defaults to
	// "set_status" when empty.
	Name string

	// Description overrides the tool's description (the prose the
	// model sees in its function-decl list). Empty falls back to a
	// sensible default.
	Description string

	// AllowedStates, when non-empty, restricts the State values the
	// tool will accept. A call with a State outside the set is
	// rejected with an error result returned to the model — the
	// handler is not invoked for the rejected call.
	AllowedStates []string
}

const (
	defaultLifecycleName        = "set_status"
	defaultLifecycleDescription = "Emit a lifecycle status to signal what you are doing right now (e.g. \"thinking\", \"blocked\", \"done\"). Use this so the surrounding system can render your state. The state argument is required; detail is an optional short human-readable note."
)

type lifecycleArgs struct {
	State  string `json:"state" jsonschema:"a short keyword for the current state, e.g. thinking, blocked, done"`
	Detail string `json:"detail,omitempty" jsonschema:"optional one-sentence human-readable context"`
}

type lifecycleResult struct {
	Ack string `json:"ack"`
}

// NewLifecycleTool wraps a LifecycleHandler as an ADK tool the agent
// can call to signal state ("thinking", "blocked", "done", custom).
// The consumer's handler decides where the events go: stdout, a
// status file, an orchestrator's event log, a websocket, etc.
//
// Returns an error only if opts.Handler is nil or AllowedStates
// contains an empty entry.
func NewLifecycleTool(opts LifecycleOptions) (tool.Tool, error) {
	if opts.Handler == nil {
		return nil, fmt.Errorf("tools: NewLifecycleTool: Handler is required")
	}
	for _, s := range opts.AllowedStates {
		if strings.TrimSpace(s) == "" {
			return nil, fmt.Errorf("tools: NewLifecycleTool: AllowedStates contains an empty entry")
		}
	}
	name := opts.Name
	if name == "" {
		name = defaultLifecycleName
	}
	desc := opts.Description
	if desc == "" {
		desc = defaultLifecycleDescription
		if len(opts.AllowedStates) > 0 {
			desc = desc + " Allowed states: " + strings.Join(opts.AllowedStates, ", ") + "."
		}
	}
	return functiontool.New(
		functiontool.Config{Name: name, Description: desc},
		lifecycleFunc(opts.Handler, opts.AllowedStates),
	)
}

// lifecycleFunc builds the handler invoked by the wrapped function
// tool. Extracted so tests can drive it directly without going
// through ADK's functiontool wrapper.
func lifecycleFunc(handler LifecycleHandler, allowedStates []string) functiontool.Func[lifecycleArgs, lifecycleResult] {
	allowed := make(map[string]struct{}, len(allowedStates))
	for _, s := range allowedStates {
		allowed[strings.TrimSpace(s)] = struct{}{}
	}
	return func(ctx tool.Context, in lifecycleArgs) (lifecycleResult, error) {
		state := strings.TrimSpace(in.State)
		if state == "" {
			return lifecycleResult{Ack: "rejected: state is required"}, nil
		}
		if len(allowed) > 0 {
			if _, ok := allowed[state]; !ok {
				return lifecycleResult{Ack: fmt.Sprintf("rejected: state %q is not in the allowed set %v", state, allowedStates)}, nil
			}
		}
		ev := LifecycleEvent{
			State:  state,
			Detail: strings.TrimSpace(in.Detail),
			Time:   time.Now(),
		}
		// tool.Context embeds context.Context via CallbackContext;
		// fall back to Background when callers (typically tests)
		// pass an explicit nil interface.
		var hctx context.Context = ctx
		if hctx == nil {
			hctx = context.Background()
		}
		if err := handler(hctx, ev); err != nil {
			return lifecycleResult{Ack: fmt.Sprintf("rejected: %v", err)}, nil
		}
		return lifecycleResult{Ack: "ok"}, nil
	}
}
