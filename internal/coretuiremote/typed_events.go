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

// Typed event-stream subscriber for SSE protocol v1.1.0 — projects
// attach.* payloads carried on attach.Frame.TypedData into
// coretui.Event's push-mode fields. See
// docs/site/content/docs/reference/attach-events.md and core-tui's
// docs/sse-event-stream-protocol.md for the wire contract.
//
// Layering: attachclient is the transport layer (unmarshal SSE
// frames into attach.* types); this file is the projection layer
// (attach.* → coretui.* with identical-shape struct copies). Keeping
// the two separate means attachclient stays UI-library-agnostic
// (other consumers — web TUI, IDE plugin — could reuse it without
// pulling core-tui).
//
// Forward compatibility: unknown event types and decode errors are
// dropped silently (per spec §3) rather than crashing the stream.
// Adding a new typed event = adding its case in client.go's
// parseStreamFrame + a new case in translateTypedFrame.

package coretuiremote

import (
	coretui "github.com/go-steer/core-tui/tui"

	"github.com/go-steer/core-agent/pkg/attach"
)

// translateTypedFrame projects an attach.Frame carrying a typed
// SSE payload (StatusUpdate / UsageUpdate / InboxEvent /
// TurnComplete / TurnError / Capabilities) into a coretui.Event
// with the matching push-mode field set. Returns false when the
// frame's Type is unrecognized, TypedData is nil, or the payload
// is of an unexpected concrete type — caller skips the emit.
//
// Capabilities is intentionally a no-op for v1: it's a handshake
// frame consumed only by the future RemoteTransport=Auto
// negotiation (Phase 2 in core-tui's design). Ignoring it now
// keeps the stream flowing without crashing on the legitimately-
// expected first frame of every session.
func translateTypedFrame(frame attach.Frame) (coretui.Event, bool) {
	switch frame.Type {
	case attach.EventStatusUpdate:
		p, ok := frame.TypedData.(*attach.StatusUpdate)
		if !ok || p == nil {
			return coretui.Event{}, false
		}
		return coretui.Event{StatusUpdate: statusUpdateToCoreTui(p)}, true
	case attach.EventUsageUpdate:
		p, ok := frame.TypedData.(*attach.UsageUpdate)
		if !ok || p == nil {
			return coretui.Event{}, false
		}
		ev := coretui.Event{UsageUpdate: usageUpdateToCoreTui(p)}
		// Per-turn info piggybacks on the same Event so core-tui's
		// per-turn footer updates from the authoritative server-side
		// number instead of the adapter's applyPricing estimate. The
		// framework's emitEvent fans out both usageMsg (for
		// currentCost) and usageUpdateMsg (for sessionUsage) from a
		// single Event that has both fields set.
		if p.LastTurn != nil {
			ev.Usage = &coretui.Usage{
				InputTokens:  p.LastTurn.TokensIn,
				OutputTokens: p.LastTurn.TokensOut,
			}
			ev.CostUSD = p.LastTurn.CostUSD
			ev.Model = p.LastTurn.Model
		}
		return ev, true
	case attach.EventInbox:
		p, ok := frame.TypedData.(*attach.InboxEvent)
		if !ok || p == nil {
			return coretui.Event{}, false
		}
		return coretui.Event{Inbox: inboxEventToCoreTui(p)}, true
	case attach.EventTurnComplete:
		p, ok := frame.TypedData.(*attach.TurnComplete)
		if !ok || p == nil {
			return coretui.Event{}, false
		}
		return coretui.Event{TurnComplete: turnCompleteToCoreTui(p)}, true
	case attach.EventTurnError:
		p, ok := frame.TypedData.(*attach.TurnError)
		if !ok || p == nil {
			return coretui.Event{}, false
		}
		return coretui.Event{TurnError: turnErrorToCoreTui(p)}, true
	case attach.EventCapabilities:
		// Phase 2 will read this to negotiate poll-vs-push. For now,
		// acknowledge the frame and drop it so the stream stays
		// flowing without emitting a no-op coretui.Event.
		return coretui.Event{}, false
	default:
		return coretui.Event{}, false
	}
}

func statusUpdateToCoreTui(p *attach.StatusUpdate) *coretui.StatusUpdate {
	return &coretui.StatusUpdate{
		Model:      p.Model,
		Provider:   p.Provider,
		PermMode:   p.PermMode,
		TurnState:  p.TurnState,
		ContextPct: p.ContextPct,
	}
}

func usageUpdateToCoreTui(p *attach.UsageUpdate) *coretui.UsageUpdate {
	out := &coretui.UsageUpdate{
		TokensInTotal:  p.TokensInTotal,
		TokensOutTotal: p.TokensOutTotal,
		CostUSDTotal:   p.CostUSDTotal,
		TurnsTotal:     p.TurnsTotal,
	}
	if len(p.ByModel) > 0 {
		out.ByModel = make(map[string]coretui.UsageByModel, len(p.ByModel))
		for model, bucket := range p.ByModel {
			out.ByModel[model] = coretui.UsageByModel{
				TokensIn:  bucket.TokensIn,
				TokensOut: bucket.TokensOut,
				CostUSD:   bucket.CostUSD,
				Turns:     bucket.Turns,
			}
		}
	}
	return out
}

func inboxEventToCoreTui(p *attach.InboxEvent) *coretui.InboxEvent {
	return &coretui.InboxEvent{
		State:    p.State,
		PromptID: p.PromptID,
		QueuedAt: p.QueuedAt,
	}
}

// turnCompleteToCoreTui projects the optional-cost attach.TurnComplete
// into coretui.TurnSummary. attach uses *float64 for cost (nil =
// "cost deferred, see next usage-update" per spec v1.1.0 §2.5);
// coretui uses float64 with the same 0-means-deferred contract.
// nil → 0 collapses to the documented deferred signal; populated
// values pass through.
func turnCompleteToCoreTui(p *attach.TurnComplete) *coretui.TurnSummary {
	out := &coretui.TurnSummary{
		PromptID:  p.PromptID,
		Model:     p.Model,
		TokensIn:  p.TokensIn,
		TokensOut: p.TokensOut,
		LatencyMs: p.LatencyMs,
	}
	if p.CostUSD != nil {
		out.CostUSD = *p.CostUSD
	}
	return out
}

func turnErrorToCoreTui(p *attach.TurnError) *coretui.TurnError {
	return &coretui.TurnError{
		Kind:      p.Kind,
		Code:      p.Code,
		Message:   p.Message,
		Retryable: p.Retryable,
		Hint:      p.Hint,
	}
}
