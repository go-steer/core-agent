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

// Package coretuiremote adapts a remote core-agent (reached over the
// pkg/attach HTTP+SSE protocol via internal/attachclient) into a
// go-steer/core-tui Agent. cmd/core-agent-tui constructs an Adapter,
// hands it to coretui.Run, and from the operator's seat the result
// is indistinguishable from the in-process bubble-tea TUI — same
// status line, same slash dispatch, same chat view — driven by a
// remote agent reachable over HTTP/SSE.
//
// See docs/remote-tui-on-core-tui.md for the design rationale.
package coretuiremote

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"strings"
	"sync"
	"time"

	"google.golang.org/adk/session"
	"google.golang.org/genai"

	coretui "github.com/go-steer/core-tui/tui"

	"github.com/go-steer/core-agent/internal/attachclient"
)

// Adapter wraps an attachclient.Client to satisfy coretui.Agent and
// the capability interfaces the remote TUI can usefully implement.
// The adapter does NOT cache state — every method does the HTTP
// round-trip on demand so the operator sees fresh data after, e.g.,
// /pricing refresh on the server side.
//
// Construct via New(client, sessionPath). Pass the result to
// coretui.Run as Options.Agent + any optional capability fields.
type Adapter struct {
	client      *attachclient.Client
	sessionPath string // e.g., "/sessions/core-agent/abc123" or "/sessions/abc123"

	// connectedAt is set at construction time and used to filter out
	// the broadcaster's historical replay (since=0 streams every
	// frame from the start of the log). Events with Timestamp older
	// than connectedAt - replayGrace are discarded; the operator
	// sees an empty scrollback on attach + only NEW events from
	// that moment on. History remains queryable via sqlite or
	// `core-agent attach`; an explicit "show history" mode can come
	// later as a coretui option.
	connectedAt time.Time

	// lastSeq tracks the eventlog cursor across Run() invocations so
	// re-issuing Run() inside one Adapter doesn't re-yield earlier
	// turns' events. Protected by mu.
	mu      sync.Mutex
	lastSeq int64

	// usage caches the remote's totals (see capabilities.go).
	// coretui.UsageTracker is queried on every TUI render; the cache
	// keeps the network traffic bounded.
	usage usageCache

	// lastTurn snapshots the most recent turn's per-event
	// UsageMetadata so coretui.UsageTracker.LastTurn() has something
	// to surface in the per-turn footer's "Last turn" row. Captured
	// in Run() when we see a non-partial event carrying usage.
	// Protected by mu (the same one guarding lastSeq).
	lastTurn coretui.Usage

	// pricing cache — lazily populated from the /pricing attach
	// endpoint so we can compute per-event cost client-side. The
	// server-side pricing layer computes the same number into the
	// session tracker; we mirror it locally so the per-turn footer
	// shows "$X.XX" without a round-trip per event. Refreshed on
	// the first event with usage that arrives after a pricing
	// fetch attempt.
	pricingFetched bool
	pricingModel   string
	pricingIn      float64 // input $ per Mtok
	pricingOut     float64 // output $ per Mtok
}

// replayGrace is the slack we allow when discarding historical
// events by timestamp. Frames whose Timestamp is older than
// (connectedAt - replayGrace) are considered replay; newer ones
// pass through. The grace covers clock skew between server +
// client and any events that happened in the last second before
// connect (rare but plausible — a model turn was finishing as we
// attached).
const replayGrace = 2 * time.Second

// New returns an Adapter that drives sessionPath against client.
// sessionPath is the attach path prefix the operator picked at
// connect time — either /sessions/<sid> (shortcut form) or
// /sessions/<app>/<sid> (qualified). The adapter passes it verbatim
// to every attachclient call.
func New(client *attachclient.Client, sessionPath string) *Adapter {
	return &Adapter{
		client:      client,
		sessionPath: sessionPath,
		connectedAt: time.Now(),
	}
}

// Run satisfies coretui.Agent. Sends prompt as an inject to the
// remote agent, then ranges over the SSE stream translating each
// session.Event into a coretui.Event until ev.TurnComplete fires
// (the remote agent's "I'm done with this turn" signal) or ctx is
// cancelled (operator hit Esc or Ctrl+C).
//
// The iterator's last yielded Event always carries the turn's final
// model output (Partial=false) and the cumulative usage if the
// remote emitted any. If the inject fails (network, 4xx) the
// iterator yields exactly one (zero-Event, error) pair and stops.
func (a *Adapter) Run(ctx context.Context, prompt string) iter.Seq2[coretui.Event, error] {
	return func(yield func(coretui.Event, error) bool) {
		// Open the stream FIRST so we don't miss the echo events
		// triggered by our own inject. Pass current lastSeq so a
		// reconnect after Ctrl+C replays from the right cursor.
		a.mu.Lock()
		since := a.lastSeq
		a.mu.Unlock()

		frames, err := a.client.Stream(ctx, a.sessionPath, since)
		if err != nil {
			yield(coretui.Event{}, fmt.Errorf("stream: %w", err))
			return
		}

		if err := a.client.Inject(ctx, a.sessionPath, prompt); err != nil {
			yield(coretui.Event{}, fmt.Errorf("inject: %w", err))
			return
		}

		for {
			select {
			case <-ctx.Done():
				yield(coretui.Event{}, ctx.Err())
				return

			case frame, ok := <-frames:
				if !ok {
					// Stream closed (network drop, server EOF).
					// Treat as end-of-turn — the caller's ctx-cancel
					// path handles "operator wanted to stop" cleanly.
					return
				}
				a.mu.Lock()
				if frame.Seq > a.lastSeq {
					a.lastSeq = frame.Seq
				}
				a.mu.Unlock()

				if frame.Event == nil {
					continue
				}
				// Drop historical replay: when an Adapter is fresh
				// the broadcaster streams every prior frame before
				// switching to live tail. Events older than
				// connectedAt - replayGrace are NOT for this turn
				// (they were here when we attached) so they don't
				// belong in the iterator's per-turn yield.
				if isReplay(frame.Event.Timestamp, a.connectedAt) {
					continue
				}
				ev := translateEvent(frame.Event)
				a.applyPricing(&ev)
				// Remember the per-event usage so LastTurn() can
				// surface it after the iterator ends. The final
				// non-partial event with UsageMetadata typically
				// carries the turn's cumulative tokens for Gemini.
				if ev.Usage != nil && !ev.Partial {
					a.mu.Lock()
					a.lastTurn = *ev.Usage
					a.mu.Unlock()
				}
				// Only yield non-empty events so the TUI doesn't see
				// echo frames for the inject itself or other no-op
				// events that don't carry text / tool calls / usage.
				if isEmptyEvent(ev) {
					if isTurnEnd(frame.Event, ev) {
						return
					}
					continue
				}
				if !yield(ev, nil) {
					// Consumer abandoned the iterator (TUI cleanup
					// or programmatic break).
					return
				}
				if isTurnEnd(frame.Event, ev) {
					return
				}
			}
		}
	}
}

// isTurnEnd decides whether the just-yielded event should close the
// per-turn iterator. Two signals, in order of preference:
//
//  1. session.Event.TurnComplete is the ADK-native end marker — used
//     when the eventlog projection preserves it.
//  2. Heuristic fallback: a final (Partial=false) model-authored event
//     that's neither a tool call nor a tool result. That's the
//     "model spoke and is done" pattern; subsequent injects start a
//     fresh Run() in coretui so we don't need to keep the iterator
//     open. Necessary because core-agent's eventlog projection
//     currently drops TurnComplete on stored events (the flag lives
//     on model.LLMResponse but isn't persisted as a session.Event
//     column the broadcaster re-emits with value=true).
func isTurnEnd(raw *session.Event, ev coretui.Event) bool {
	if raw != nil && raw.TurnComplete {
		return true
	}
	if ev.Partial || len(ev.ToolCalls) > 0 || len(ev.ToolResults) > 0 {
		return false
	}
	if raw == nil || raw.Author == "" || raw.Author == "user" {
		return false
	}
	// Final model event with no in-flight tool round-trip → turn done.
	return ev.Text != ""
}

// Events satisfies coretui.LiveAgent (issue #22). Continuously drains
// the remote eventlog and yields every event into the TUI's chat
// view — autonomous turns, subagent activity, MCP-server-triggered
// events, other attached operators' injects, and our own. Unlike
// Run (Pattern A), Events does NOT filter historical replay (the
// operator attached because they want to see what they walked into)
// and does NOT close on turn-end (observer mode is continuous).
//
// When core-tui detects LiveAgent, Run is silently skipped — Inject
// remains the path for operator-typed prompts (already implemented
// on the adapter). The result: same chat, populated end-to-end from
// the remote stream regardless of whether the operator typed.
//
// Lifecycle: the iterator returns when the underlying stream closes
// (network drop, server EOF, or ctx cancel). core-tui then renders a
// "Disconnected" system row and keeps the program alive so the
// operator can read scrollback or quit cleanly. Reconnection is the
// caller's responsibility per the LiveAgent contract; today the
// program restart is the simplest path. Auto-reconnect can layer in
// later without an interface change.
func (a *Adapter) Events(ctx context.Context) iter.Seq2[coretui.Event, error] {
	return func(yield func(coretui.Event, error) bool) {
		frames, err := a.client.Stream(ctx, a.sessionPath, 0)
		if err != nil {
			yield(coretui.Event{}, fmt.Errorf("stream: %w", err))
			return
		}
		for {
			select {
			case <-ctx.Done():
				return
			case frame, ok := <-frames:
				if !ok {
					// Stream closed — coretui surfaces the
					// "Disconnected" banner via liveStreamEndedMsg.
					return
				}
				a.mu.Lock()
				if frame.Seq > a.lastSeq {
					a.lastSeq = frame.Seq
				}
				a.mu.Unlock()
				if frame.Event == nil {
					continue
				}
				ev := translateEvent(frame.Event)
				a.applyPricing(&ev)
				if ev.Usage != nil && !ev.Partial {
					a.mu.Lock()
					a.lastTurn = *ev.Usage
					a.mu.Unlock()
				}
				if isEmptyEvent(ev) {
					continue
				}
				if !yield(ev, nil) {
					return
				}
			}
		}
	}
}

// Inject satisfies coretui.InjectableAgent. Operator-typed messages
// during a streaming turn route through here when the host opts in
// to MidTurnInjectionMode=InjectIntoCurrent.
func (a *Adapter) Inject(message string) error {
	// coretui's InjectableAgent.Inject is sync; the network call
	// here is short (the body is an 8 KiB cap). Use context.TODO
	// since coretui doesn't thread a context through this surface.
	return a.client.Inject(context.TODO(), a.sessionPath, message)
}

// RequestWake satisfies coretui.WakeRequester. Wired so the
// operator's /wake slash works.
func (a *Adapter) RequestWake() {
	// Fire-and-forget — wake doesn't return useful state and
	// coretui's interface is void.
	_ = a.client.Wake(context.TODO(), a.sessionPath)
}

// SessionPath returns the configured attach session path (mostly
// for diagnostics / tests).
func (a *Adapter) SessionPath() string { return a.sessionPath }

// translateEvent projects one session.Event into a coretui.Event.
// Pulls text + function calls + function responses out of the
// event's Content.Parts; pulls usage from the embedded LLMResponse's
// UsageMetadata (Gemini-shaped) with a CustomMetadata["usage"]
// fallback for adapters that route usage through metadata instead.
//
// CostUSD and Model are NOT populated per-event for remote — the
// remote server holds the pricing layer and the model identity;
// they surface through the UsageTracker (/stats) and StatusReporter
// (status header) instead of the per-turn footer.
func translateEvent(ev *session.Event) coretui.Event {
	out := coretui.Event{Partial: ev.Partial}

	if ev.Content != nil {
		var sb strings.Builder
		for _, p := range ev.Content.Parts {
			if p == nil {
				continue
			}
			if p.Text != "" {
				sb.WriteString(p.Text)
			}
			if p.FunctionCall != nil {
				out.ToolCalls = append(out.ToolCalls, toolCallFromPart(p))
			}
			if p.FunctionResponse != nil {
				out.ToolResults = append(out.ToolResults, toolResultFromPart(p))
			}
		}
		out.Text = sb.String()
	}

	// Prefer the genai UsageMetadata on the embedded LLMResponse —
	// that's where Gemini-provider events carry per-event token
	// counts. Fall back to CustomMetadata["usage"] for adapters that
	// stash usage there (the in-process tracker's pricing layer
	// projects through this shape).
	if u := usageFromGenai(ev.UsageMetadata); u != nil {
		out.Usage = u
	} else if usage, cost, model := usageFromMetadata(ev.CustomMetadata); usage != nil {
		out.Usage = usage
		out.CostUSD = cost
		out.Model = model
	}

	return out
}

// usageFromGenai projects genai's per-event UsageMetadata into a
// coretui.Usage. Returns nil when the metadata is absent or carries
// no token counts (which it does for non-final partials).
func usageFromGenai(meta *genai.GenerateContentResponseUsageMetadata) *coretui.Usage {
	if meta == nil {
		return nil
	}
	in := int(meta.PromptTokenCount)
	out := int(meta.CandidatesTokenCount)
	if in == 0 && out == 0 {
		return nil
	}
	return &coretui.Usage{InputTokens: in, OutputTokens: out}
}

// toolCallFromPart projects a genai function-call into a
// coretui.ToolCall. ID is the function-call ID the model emits
// (used by core-tui to dedup partial + final echoes of the same
// call across streamed events).
func toolCallFromPart(p *genai.Part) coretui.ToolCall {
	tc := coretui.ToolCall{
		ID:   p.FunctionCall.ID,
		Name: p.FunctionCall.Name,
	}
	if len(p.FunctionCall.Args) > 0 {
		tc.Args = make(map[string]any, len(p.FunctionCall.Args))
		for k, v := range p.FunctionCall.Args {
			tc.Args[k] = v
		}
	}
	return tc
}

// toolResultFromPart projects a genai function-response. Error
// strings come from a conventional "error" key in the response map;
// everything else is preserved verbatim so core-tui's per-tool
// renderers can pick the relevant fields (`content` for read_file,
// `stdout`/`stderr` for bash, etc.).
func toolResultFromPart(p *genai.Part) coretui.ToolResult {
	tr := coretui.ToolResult{
		ID:   p.FunctionResponse.ID,
		Name: p.FunctionResponse.Name,
	}
	if p.FunctionResponse.Response == nil {
		return tr
	}
	tr.Response = make(map[string]any, len(p.FunctionResponse.Response))
	for k, v := range p.FunctionResponse.Response {
		tr.Response[k] = v
		if k == "error" {
			if s, ok := v.(string); ok {
				tr.Error = s
			}
		}
	}
	return tr
}

// usageFromMetadata extracts the per-event usage delta (when the
// remote stamped it) into a coretui.Usage plus cost+model. Returns
// (nil, 0, "") when the metadata doesn't carry usage — events with
// no usage have no per-turn footer contribution.
func usageFromMetadata(meta map[string]any) (*coretui.Usage, float64, string) {
	if len(meta) == 0 {
		return nil, 0, ""
	}
	usageRaw, ok := meta["usage"]
	if !ok {
		return nil, 0, ""
	}
	// usage is stored as a JSON-shaped map by core-agent's
	// telemetry layer. Re-marshal+unmarshal is the cheapest way
	// to cope with the map[string]any → typed-struct hop without
	// hand-walking every field.
	raw, err := json.Marshal(usageRaw)
	if err != nil {
		return nil, 0, ""
	}
	var u struct {
		InputTokens  int     `json:"input_tokens"`
		OutputTokens int     `json:"output_tokens"`
		CostUSD      float64 `json:"cost_usd"`
		Model        string  `json:"model"`
	}
	if err := json.Unmarshal(raw, &u); err != nil {
		return nil, 0, ""
	}
	if u.InputTokens == 0 && u.OutputTokens == 0 && u.CostUSD == 0 && u.Model == "" {
		return nil, 0, ""
	}
	return &coretui.Usage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens}, u.CostUSD, u.Model
}

// isEmptyEvent reports whether the event would render as a no-op
// in the TUI. Used to skip frames that don't move the chat forward
// — e.g., the inject's own echo before the model starts speaking.
func isEmptyEvent(ev coretui.Event) bool {
	return ev.Text == "" && len(ev.ToolCalls) == 0 && len(ev.ToolResults) == 0 && ev.Usage == nil
}

// isReplay reports whether an event's timestamp marks it as part of
// the broadcaster's historical replay rather than a live event for
// the current Adapter session. Frames stamped before
// (connectedAt - replayGrace) are replay. A zero ts (never set) is
// treated as live — we'd rather show a wrongly-attributed event
// than swallow a real one.
func isReplay(ts, connectedAt time.Time) bool {
	if ts.IsZero() {
		return false
	}
	return ts.Before(connectedAt.Add(-replayGrace))
}
