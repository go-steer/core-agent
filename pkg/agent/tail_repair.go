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

// Tool-call tail repair (#537).
//
// Persistence is per-event: ADK's runner commits the model response
// carrying a functionCall, then executes the tool, then commits the
// functionResponse. Two failure modes leave the call durably
// unanswered:
//
//   - The process dies mid-tool (SIGKILL, OOMKill) — the response
//     event is never produced.
//   - The turn ctx is cancelled mid-tool (ESC, attach interrupt,
//     SIGTERM) — the flow does synthesize an error functionResponse,
//     but the runner appends it with the already-cancelled turn ctx,
//     so the write itself fails and only the call survives.
//
// ADK's contents processor re-pairs calls with responses but never
// synthesizes a missing one, and both Gemini and Anthropic reject a
// history containing an unanswered call — so without repair the
// session is poisoned for every subsequent turn until someone
// hand-edits the DB.
//
// The repair appends a real, durable functionResponse event (audit
// stays whole and replay/export see consistent history) rather than
// filtering on read. It runs at the top of every Run, which covers
// both crash-restart resume (lazy multi-session resume, single-session
// daemon restart, autonomous resume) and same-process interrupts.

package agent

import (
	"context"
	"fmt"
	"os"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/adk/session"
	"google.golang.org/genai"
)

// tailRepairScanWindow bounds the per-turn history fetch. A dangling
// call is only reachable near the tail: a history with an unanswered
// call cannot advance (every model call against it fails), so at most
// a handful of non-content events (interrupt audit rows, autonomous
// checkpoints/notes) can follow it before repair gets a chance to run.
// The window keeps the extra per-turn Get O(1) instead of O(session).
// Correctness is window-safe: a response always postdates its call, so
// any call inside the window has its response (if one exists) inside
// the window too.
const tailRepairScanWindow = 128

// tailRepairKind is the CustomMetadata "kind" stamped on synthesized
// response events so audit consumers can tell repair rows from real
// tool results. The Author must remain the call's author — a foreign
// author would be textified by ADK's contents processor and the pair
// would stay broken — so attribution lives in metadata instead.
const tailRepairKind = "tool_tail_repair"

// Control-flow pseudo-calls ADK excludes from LLM contents entirely
// (shouldExcludeEvent); an unanswered one cannot poison a request, and
// synthesizing a response for it could confuse the confirmation /
// credential flows.
const (
	confirmationCallName = "adk_request_confirmation"
	credentialCallName   = "adk_request_credential" //nolint:gosec // ADK pseudo-call name, not a credential
)

const interruptedToolResult = "tool execution was interrupted before a result was recorded (process restart or turn cancellation); the call may or may not have taken effect. Re-issue the tool call if its result is still needed."

// originalCallID extracts the parked original call's ID from an
// adk_request_confirmation pseudo-call's args. ADK stores the full
// original *genai.FunctionCall under "originalFunctionCall"; live
// in-memory it's the struct pointer, after a DB round-trip it's a
// nested map with the struct's lowercase JSON keys.
func originalCallID(args map[string]any) string {
	orig, ok := args["originalFunctionCall"]
	if !ok {
		return ""
	}
	switch v := orig.(type) {
	case *genai.FunctionCall:
		if v != nil {
			return v.ID
		}
	case map[string]any:
		if id, ok := v["id"].(string); ok {
			return id
		}
	}
	return ""
}

// repairDanglingToolCalls scans recent history for functionCall parts
// authored by this agent that have no matching functionResponse and
// appends one synthesized error-response event per damaged call event.
//
// Scope guards:
//   - Only calls whose event Author equals this agent's name. Foreign
//     authors (background subagents sharing the session row) are both
//     harmless — ADK textifies their call/response parts, so they
//     never poison this agent's requests — and dangerous to touch: a
//     live subagent's in-flight call looks identical to a dangling one.
//   - One synthetic event per damaged call event, answering only that
//     event's unanswered calls. A single merged event answering calls
//     from several call events would be relocated after EACH of them
//     by ADK's history rearrangement, duplicating responses.
//   - Long-running tool IDs are skipped: their responses legitimately
//     arrive in a later user turn.
//
// Failures are logged and swallowed — if the tail really is dangling,
// the turn was going to fail anyway, and if it isn't, the turn should
// proceed untouched.
func (a *Agent) repairDanglingToolCalls(ctx context.Context) {
	resp, err := a.sessionService.Get(ctx, &session.GetRequest{
		AppName:         a.appName,
		UserID:          a.userID,
		SessionID:       a.sessionID,
		NumRecentEvents: tailRepairScanWindow,
	})
	if err != nil || resp == nil || resp.Session == nil {
		// No session yet (first turn — AutoCreateSession will make
		// one) or a transient read failure. Nothing to repair.
		return
	}

	// Pass 1: every answered call ID in the window, regardless of who
	// answered (async long-running responses are user-authored), plus
	// every call ID parked behind ADK's tool-confirmation flow: a
	// RequireConfirmation tool's original call legitimately sits
	// unanswered (and not long-running-marked) while its
	// adk_request_confirmation pseudo-call awaits the user, so
	// answering it here would break the flow. core-agent itself never
	// sets RequireConfirmation, but pkg/agent is consumed as a
	// library. Skipping unconditionally (even for an already-answered
	// confirmation) means a crash inside the confirmation gap leaves
	// the tail unrepaired — the pre-repair status quo — rather than
	// risking a wrong guess about an in-flight approved tool.
	answered := map[string]bool{}
	confirmationParked := map[string]bool{}
	var callEvents []*session.Event
	for ev := range resp.Session.Events().All() {
		if ev == nil || ev.Content == nil {
			continue
		}
		hasCall := false
		for _, p := range ev.Content.Parts {
			if p == nil {
				continue
			}
			if p.FunctionResponse != nil {
				answered[p.FunctionResponse.ID] = true
			}
			if p.FunctionCall != nil {
				hasCall = true
				if p.FunctionCall.Name == confirmationCallName {
					if id := originalCallID(p.FunctionCall.Args); id != "" {
						confirmationParked[id] = true
					}
				}
			}
		}
		if hasCall {
			callEvents = append(callEvents, ev)
		}
	}

	// Pass 2: synthesize per damaged call event.
	for _, cev := range callEvents {
		if cev.Author != a.agentName {
			continue
		}
		longRunning := map[string]bool{}
		for _, id := range cev.LongRunningToolIDs {
			longRunning[id] = true
		}
		var parts []*genai.Part
		var repairedIDs []any
		for _, p := range cev.Content.Parts {
			if p == nil || p.FunctionCall == nil {
				continue
			}
			fc := p.FunctionCall
			if answered[fc.ID] || longRunning[fc.ID] || confirmationParked[fc.ID] {
				continue
			}
			if fc.Name == confirmationCallName || fc.Name == credentialCallName {
				continue
			}
			parts = append(parts, &genai.Part{FunctionResponse: &genai.FunctionResponse{
				ID:       fc.ID,
				Name:     fc.Name,
				Response: map[string]any{"error": interruptedToolResult},
			}})
			repairedIDs = append(repairedIDs, fc.ID)
			// Mark synthesized IDs answered so a duplicate call ID in a
			// later event (impossible with today's ADK — the streaming
			// aggregator's duplicate emits are partial-only and never
			// committed — but cheap to be invariant against) can't
			// collect a second response.
			answered[fc.ID] = true
		}
		if len(parts) == 0 {
			continue
		}
		ev := session.NewEvent(cev.InvocationID)
		ev.Author = cev.Author
		ev.Branch = cev.Branch
		ev.LLMResponse = adkmodel.LLMResponse{
			Content: &genai.Content{Role: genai.RoleUser, Parts: parts},
			CustomMetadata: map[string]any{
				"kind":              tailRepairKind,
				"repaired_call_ids": repairedIDs,
			},
		}
		if err := a.sessionService.AppendEvent(ctx, resp.Session, ev); err != nil {
			fmt.Fprintf(os.Stderr, "core-agent: tail repair: append synthesized response for session %s: %v\n", a.sessionID, err)
			return
		}
	}
}
