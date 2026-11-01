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

package evalcompose

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path"
	"strings"
	"sync"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/plugin"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v3/pkg/approval"
	"github.com/go-steer/core-agent/v3/pkg/watchdog"
)

const (
	// DefaultMaxModelCallsPerTurn is the per-turn model-call ceiling
	// when a bundle declares 0.
	DefaultMaxModelCallsPerTurn = 100

	// DefaultStopAfterNote is how many more identical calls a model gets
	// after the in-turn note before the turn ends.
	DefaultStopAfterNote = 3

	// LoopNoteKey is the field TurnLoop's note travels under in a tool result.
	LoopNoteKey = "watchdog"

	loopStoppedKey = "error"
	loopStopped    = "loop_stopped"
)

// LoopGuardConfig configures the in-turn runaway guard.
type LoopGuardConfig struct {
	MaxModelCallsPerTurn int
	RepeatThreshold      int
	RepeatStopAfter      int
	OnNote               func(sessionID, toolName string, count int)
	OnStop               func(sessionID, reason string)
}

// LoopGuard builds the in-turn runaway plugin for the judge eval rig.
func LoopGuard(cfg LoopGuardConfig) (*plugin.Plugin, error) {
	capCalls := DefaultMaxModelCallsPerTurn
	if cfg.MaxModelCallsPerTurn > 0 {
		capCalls = cfg.MaxModelCallsPerTurn
	} else if cfg.MaxModelCallsPerTurn < 0 {
		capCalls = 0
	}
	thresh := cfg.RepeatThreshold
	if thresh <= 0 {
		thresh = watchdog.DefaultRepeatThreshold
	}
	if thresh < 2 {
		thresh = 2
	}
	stopAfter := cfg.RepeatStopAfter
	if stopAfter <= 0 {
		stopAfter = DefaultStopAfterNote
	}

	g := &loopGuardState{
		cfg:       cfg,
		capCalls:  capCalls,
		thresh:    thresh,
		stopAfter: stopAfter,
		byInv:     map[string]*invLoopState{},
	}
	return plugin.New(plugin.Config{
		Name:                "eval_loop_guard",
		BeforeModelCallback: g.beforeModel,
		BeforeToolCallback:  g.beforeTool,
		AfterToolCallback:   g.afterTool,
		AfterRunCallback: func(ictx adkagent.InvocationContext) {
			g.forget(ictx.InvocationID())
		},
	})
}

type invLoopState struct {
	calls    int
	lastTool string
	lastArgs string
	result   string
	length   int
	notes    map[string]string
}

type loopGuardState struct {
	cfg       LoopGuardConfig
	capCalls  int
	thresh    int
	stopAfter int

	mu    sync.Mutex
	byInv map[string]*invLoopState
}

func (g *loopGuardState) stateFor(invID string) *invLoopState {
	s, ok := g.byInv[invID]
	if !ok {
		s = &invLoopState{notes: map[string]string{}}
		g.byInv[invID] = s
	}
	return s
}

func (g *loopGuardState) forget(invID string) {
	g.mu.Lock()
	delete(g.byInv, invID)
	g.mu.Unlock()
}

func (g *loopGuardState) beforeModel(ctx adkagent.Context, _ *model.LLMRequest) (*model.LLMResponse, error) {
	if g.capCalls <= 0 || ctx.InvocationID() == "" {
		return nil, nil
	}
	g.mu.Lock()
	st := g.stateFor(ctx.InvocationID())
	if st.calls >= g.capCalls {
		calls := st.calls
		g.mu.Unlock()
		stop := fmt.Sprintf(
			"turn ended: it made %d model calls, the per-turn cap (budget.max_model_calls_per_turn). "+
				"The next call was not made. Nothing is latched; the next turn starts clean.",
			calls,
		)
		if g.cfg.OnStop != nil {
			g.cfg.OnStop(ctx.SessionID(), stop)
		}
		if ts := approval.TurnStopFrom(ctx); ts != nil {
			ts.StopTurn(errors.New(stop))
		}
		return &model.LLMResponse{
			Content:      genai.NewContentFromText(stop, genai.RoleModel),
			TurnComplete: true,
			FinishReason: genai.FinishReasonStop,
		}, nil
	}
	st.calls++
	g.mu.Unlock()
	return nil, nil
}

func (g *loopGuardState) beforeTool(ctx adkagent.Context, t tool.Tool, args map[string]any) (map[string]any, error) {
	invID := ctx.InvocationID()
	if invID == "" {
		return nil, nil
	}
	callID := ctx.FunctionCallID()
	name := t.Name()
	ser := serializeLoopArgs(args)

	g.mu.Lock()
	st := g.stateFor(invID)
	if st.length > 0 && st.lastTool == name && argsEquivalent(st.lastArgs, ser) {
		st.length++
	} else {
		st.lastTool = name
		st.lastArgs = ser
		st.result = ""
		st.length = 1
	}
	length := st.length
	if length < g.thresh {
		g.mu.Unlock()
		return nil, nil
	}
	left := g.thresh + g.stopAfter - length
	if left > 0 {
		st.notes[callID] = loopNote(name, length, left)
		g.mu.Unlock()
		return nil, nil
	}
	g.mu.Unlock()

	stop := fmt.Sprintf(
		"turn ended: the model called %s with identical arguments %d times in a row and kept going after being told the result would not change (args=%s). "+
			"The last call was not made. Nothing is latched; the next turn starts clean.",
		name, length, ser,
	)
	if g.cfg.OnStop != nil {
		g.cfg.OnStop(ctx.SessionID(), stop)
	}
	if ts := approval.TurnStopFrom(ctx); ts != nil {
		ts.StopTurn(errors.New(stop))
	}
	return map[string]any{
		loopStoppedKey: loopStopped,
		"detail":       "This call was not made. It repeats the same call with the same arguments and the same result as the calls before it, after you were told the result would not change. The turn is ending.",
	}, nil
}

func (g *loopGuardState) afterTool(ctx adkagent.Context, t tool.Tool, _ map[string]any, result map[string]any, err error) (map[string]any, error) {
	if err != nil {
		return nil, nil
	}
	if result[loopStoppedKey] == loopStopped {
		return nil, nil
	}
	invID := ctx.InvocationID()
	callID := ctx.FunctionCallID()
	digest := serializeLoopArgs(result)

	g.mu.Lock()
	st, ok := g.byInv[invID]
	if !ok {
		g.mu.Unlock()
		return nil, nil
	}
	note := st.notes[callID]
	delete(st.notes, callID)
	var streak int
	switch {
	case st.result == "":
		st.result = digest
		streak = st.length
	case st.result != digest:
		st.result = digest
		st.length = 1
		g.mu.Unlock()
		return nil, nil
	default:
		streak = st.length
	}
	g.mu.Unlock()

	if note == "" {
		return nil, nil
	}
	if g.cfg.OnNote != nil {
		g.cfg.OnNote(ctx.SessionID(), t.Name(), streak)
	}
	out := make(map[string]any, len(result)+1)
	maps.Copy(out, result)
	out[LoopNoteKey] = note
	return out, nil
}

func loopNote(name string, n, left int) string {
	warn := fmt.Sprintf("Calling it %d more times with the same arguments ends this turn.", left)
	if left == 1 {
		warn = "Calling it again with the same arguments ends this turn."
	}
	return fmt.Sprintf(
		"Automated observation, not part of the tool result: you have called %s with the same arguments %d times in a row in this turn, and the result will not change. "+
			"Use what it already returned, try a different tool or different arguments, or report what is blocking you. "+
			"%s",
		name, n, warn,
	)
}

func serializeLoopArgs(m map[string]any) string {
	if len(m) == 0 {
		return "{}"
	}
	b, err := json.Marshal(m)
	if err != nil {
		return fmt.Sprintf("%v", m)
	}
	return string(b)
}

func canonicalArgs(args string) string {
	if args == "" {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(args), &m); err != nil || m == nil {
		return args
	}
	changed := false
	for k, v := range m {
		s, ok := v.(string)
		if !ok || !pathishKey(k) {
			continue
		}
		if c := path.Clean(strings.TrimSpace(s)); c != s {
			m[k] = c
			changed = true
		}
	}
	if !changed {
		return args
	}
	b, err := json.Marshal(m)
	if err != nil {
		return args
	}
	return string(b)
}

func pathishKey(k string) bool {
	k = strings.ToLower(k)
	switch k {
	case "target", "source", "destination", "src", "dst", "cwd":
		return true
	}
	return strings.Contains(k, "path") ||
		strings.Contains(k, "file") ||
		strings.Contains(k, "dir") ||
		strings.Contains(k, "folder")
}

func argsEquivalent(a, b string) bool {
	if a == b {
		return true
	}
	ca, cb := canonicalArgs(a), canonicalArgs(b)
	if ca == cb {
		return true
	}
	var ma, mb map[string]any
	if json.Unmarshal([]byte(ca), &ma) != nil || json.Unmarshal([]byte(cb), &mb) != nil {
		return false
	}
	if len(ma) != len(mb) || ma == nil || mb == nil {
		return false
	}
	for k, va := range ma {
		vb, ok := mb[k]
		if !ok {
			return false
		}
		sa, aIsStr := va.(string)
		sb, bIsStr := vb.(string)
		if aIsStr && bIsStr && pathishKey(k) {
			if !pathSuffixEquivalent(sa, sb) {
				return false
			}
			continue
		}
		ja, ea := json.Marshal(va)
		jb, eb := json.Marshal(vb)
		if ea != nil || eb != nil || string(ja) != string(jb) {
			return false
		}
	}
	return true
}

func pathSuffixEquivalent(a, b string) bool {
	a, b = path.Clean(a), path.Clean(b)
	if a == b {
		return true
	}
	if a == "" || b == "" || a == "." || b == "." {
		return false
	}
	long, short := a, b
	if len(short) > len(long) {
		long, short = short, long
	}
	return strings.HasSuffix(long, "/"+short)
}
