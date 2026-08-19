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

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// The turn span — core-agent's own span around one Agent.Run turn.
//
// Two problems it solves, both visible on a live GKE run:
//
//  1. The turn had no root of our own. ADK emits `invoke_agent <name>`
//     from google.golang.org/adk/agent (via its internal/telemetry
//     package, which we cannot import), and it inherits whatever
//     parent is on the context handed to runner.Run. On the wake-loop
//     path that context is the daemon's long-lived loop context with
//     no span on it, so `invoke_agent` became a trace root and every
//     model call, tool call and MCP call hung off it in a trace that
//     started nowhere. Starting our own span on the per-turn context
//     just before runner.Run makes ADK's span our child, with no ADK
//     fork and no upstream change: we own the context, and
//     tracer.Start reads the parent off it.
//
//  2. There was nothing to attach the inject provenance to. The
//     watcher → daemon `traceparent` hop works (otelhttp extracts it
//     on POST /sessions/{sid}/inject), but the handler queues the
//     message and returns, ending its span long before a turn runs.
//     The turn span is the join point: one LINK per drained inject.
//
// Links, not parent-child, is the deliberate choice. Injects batch —
// one turn can answer several — so a parent edge would have to pick
// one inject and misattribute the turn to it. Asynchronous fan-in is
// exactly what OTel span links are specified for.
var turnTracer = otel.Tracer("core-agent/agent")

// turnSpanName is the span name for one Agent.Run turn. ADK's
// `invoke_agent <name>` sits directly underneath it.
const turnSpanName = "agent.turn"

// maxTurnSpanLinks bounds how many inject links one turn span carries.
//
// The inbox soft cap is 256 (defaultInboxCap) and a paused or wedged
// agent can accumulate the whole queue before a single turn drains it,
// so an unbounded link list is a real span-size hazard, not a
// theoretical one — links are exported in full on every span, and some
// backends drop oversized spans outright rather than truncating.
//
// The MOST RECENT injects are kept when a batch overflows: the turn
// originator is the last caller in the batch (see inboxDrain), so
// keeping the head would drop precisely the inject the turn is
// actually answering. The pre-cap total is preserved on the
// `core_agent.inbox.linked_injects` attribute so a truncated batch is
// still countable.
const maxTurnSpanLinks = 32

// inboxTraceLinks converts a drained inbox batch into turn-span links.
//
// Returns the (capped) links and the pre-cap count of messages that
// carried a valid span context. Messages with no trace context —
// CLI injects, tests, auto-continue notes, anything queued while
// tracing was off — are skipped, so the common untraced case allocates
// nothing and produces no links.
func inboxTraceLinks(msgs []inboxMessage) ([]trace.Link, int) {
	total := 0
	for _, m := range msgs {
		if m.spanCtx.IsValid() {
			total++
		}
	}
	if total == 0 {
		return nil, 0
	}
	// Keep the newest maxTurnSpanLinks. skip is how many of the valid
	// ones to walk past before collecting.
	skip := total - maxTurnSpanLinks
	if skip < 0 {
		skip = 0
	}
	links := make([]trace.Link, 0, total-skip)
	for _, m := range msgs {
		if !m.spanCtx.IsValid() {
			continue
		}
		if skip > 0 {
			skip--
			continue
		}
		links = append(links, trace.Link{SpanContext: m.spanCtx})
	}
	return links, total
}

// startTurnSpan opens the turn span on ctx and returns the derived
// context plus the function that closes the span.
//
// The returned context is what must be passed to runner.Run — that is
// the whole mechanism by which ADK's `invoke_agent` becomes a child.
//
// The end func takes the turn's terminal error so the span carries
// Error status and a recorded exception on a failed turn. It is safe
// to call exactly once, from the turn's cleanup hook.
func startTurnSpan(ctx context.Context, sessionID, agentName string, d inboxDrain) (context.Context, func(error)) {
	attrs := []attribute.KeyValue{
		// Matches the attribute ADK stamps on `invoke_agent`, which
		// is what makes `gen_ai.conversation.id:<sid>` a working
		// trace-backend filter for "every span of this session".
		attribute.String("gen_ai.conversation.id", sessionID),
		attribute.String("gen_ai.agent.name", agentName),
	}
	if d.linkedInjects > 0 {
		// Pre-cap count: len(links) can be smaller (maxTurnSpanLinks).
		attrs = append(attrs, attribute.Int("core_agent.inbox.linked_injects", d.linkedInjects))
	}
	opts := []trace.SpanStartOption{
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(attrs...),
	}
	if len(d.links) > 0 {
		opts = append(opts, trace.WithLinks(d.links...))
	}
	spanCtx, span := turnTracer.Start(ctx, turnSpanName, opts...)
	return spanCtx, func(err error) {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
		span.End()
	}
}
