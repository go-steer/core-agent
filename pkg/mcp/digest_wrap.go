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

package mcp

import (
	"context"
	"encoding/json"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/adk/agent"
	"google.golang.org/adk/model"
	"google.golang.org/adk/tool"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/pkg/digest"
	coretools "github.com/go-steer/core-agent/pkg/tools"
)

// tracer is the OTel tracer for the MCP wrap layer. The mcp.tool_call
// span groups the upstream HTTP call (mcp.http_call, contributed by
// otelhttp) and the digest.process child span emitted by pkg/digest.
// Resolved once — no-op when the global tracer provider is noop.
var tracer = otel.Tracer("core-agent/mcp")

// spanCtxToolContext overrides just the context.Context methods on a
// tool.Context so the inner tool's HTTP round-trip picks up our
// mcp.tool_call span as its parent (nesting mcp.http_call under
// mcp.tool_call in the trace tree). All other tool.Context methods
// (FunctionCallID, Actions, State, ...) delegate to the wrapped
// context so the inner tool sees identical behavior otherwise.
//
// Without this, ADK's tool.Context carries whatever context.Context
// the runner handed to it — likely the turn-level one — and
// otelhttp parents mcp.http_call off that instead of mcp.tool_call.
// The trace still records all the spans; they just don't nest.
type spanCtxToolContext struct {
	tool.Context
	span context.Context
}

func (s spanCtxToolContext) Deadline() (time.Time, bool) { return s.span.Deadline() }
func (s spanCtxToolContext) Done() <-chan struct{}       { return s.span.Done() }
func (s spanCtxToolContext) Err() error                  { return s.span.Err() }
func (s spanCtxToolContext) Value(key any) any           { return s.span.Value(key) }

// LLMFallbackResult is what an operator-supplied LLMFallback returns
// after running the raw MCP payload through a small-tier subagent.
// Text is the digest the model sees; SubagentModel + token counts
// feed digest.Savings.Subagent* for /stats + OTel display without
// pkg/mcp needing to import pkg/agent (which would create an import
// cycle down the line — pkg/agent lives above pkg/mcp in the layer
// hierarchy).
//
// The caller (cmd/core-agent, or any host code) constructs the
// closure supplying LLMFallback and captures the *Agent needed to
// invoke RunSubtask in that closure's environment.
type LLMFallbackResult struct {
	Text                 string
	SubagentModel        string
	SubagentInputTokens  int
	SubagentOutputTokens int
}

// DigestOptions configures how Build wraps MCP tool responses through
// pkg/digest. A nil *DigestOptions passed to Build disables wrapping
// entirely (existing behavior). A non-nil options struct wraps every
// tool from every server that isn't in NeverServers.
//
// LLMFallback opt-in (#223): a non-nil LLMFallback enables the LLM
// subagent digester for prose-shaped MCP responses that the
// structural pruner can't reduce below Threshold. Left nil, those
// responses take the bounded passthrough branch (#128's shipped
// default).
type DigestOptions struct {
	// Store is the CCR backing for retrieve_raw. When nil, digest
	// still runs but retrieve_raw returns "no raw payload" — matches
	// digest-design.md OQ1 default when --session-db is off.
	Store digest.Store

	// Threshold is the byte size below which responses bypass the
	// router entirely. Zero → DefaultAgenticWrapThreshold.
	Threshold int

	// NeverServers names MCP servers (by mcp.json key) that opt out
	// of digesting. Operator escape hatch for debug-sensitive or
	// known-tiny servers.
	NeverServers map[string]bool

	// LLMFallback, when non-nil, invokes a small-tier subagent to
	// digest MCP responses the structural JSON pruner can't reduce
	// (prose, malformed JSON, or JSON that's structurally minimal
	// and mostly-values). Callers pass a closure that owns a
	// reference to an Agent + a resolved small-model; the wrapper
	// invokes it with the raw payload and gets back a compressed
	// digest plus subagent usage numbers.
	//
	// Returned SubagentModel + token counts populate
	// digest.Savings.Subagent* on the resulting Result so /stats,
	// per-tool footer, and OTel span attributes have real cost
	// figures for the agentic path.
	//
	// A nil LLMFallback preserves the #128-shipped structural-only
	// behavior (bounded passthrough for anything structural can't
	// reduce). No opt-in from operators required to keep that.
	LLMFallback func(ctx context.Context, raw []byte) (LLMFallbackResult, error)

	// OnResult, when non-nil, fires after every successful Process
	// call with the (fully-decorated) Result. Callers use this to
	// aggregate per-call Savings into session-level counters — the
	// usage.Tracker sink in cmd/core-agent wires this to a cumulative
	// digest-savings block rendered by /context.
	//
	// Firing is best-effort: skipped on Process errors / marshal
	// failures where Result is undefined. Runs synchronously on the
	// wrapper's Run goroutine, so callers should keep the callback
	// fast (increment counters, don't do I/O).
	OnResult func(*digest.Result)
}

// threshold returns the effective threshold (default when zero).
func (o *DigestOptions) threshold() int {
	if o == nil || o.Threshold <= 0 {
		return DefaultAgenticWrapThreshold
	}
	return o.Threshold
}

// digestingToolset composes name-prefixing (from namespacedToolset)
// with response digesting. Each Tool it returns is a digestingTool
// wrapping the underlying renamedTool.
//
// When the server is in DigestOptions.NeverServers, the composed
// wrapper falls back to plain namespacedToolset behavior — the
// per-server denylist is applied at Tools() time, not per Run(),
// so a denylisted tool's Declaration goes to the model unchanged.
type digestingToolset struct {
	inner  tool.Toolset
	prefix string
	server string // the mcp.json key; used for denylist check
	opts   *DigestOptions
}

// withNamespaceAndDigest wraps inner with name-prefixing AND digest
// routing. Passing nil opts (or an opts pointer with the denylist
// hit) yields the same behavior as plain withNamespace.
func withNamespaceAndDigest(inner tool.Toolset, prefix, server string, opts *DigestOptions) tool.Toolset {
	if inner == nil || prefix == "" {
		return inner
	}
	if opts == nil || opts.NeverServers[server] {
		return withNamespace(inner, prefix)
	}
	return &digestingToolset{
		inner:  inner,
		prefix: sanitizePrefix(prefix),
		server: server,
		opts:   opts,
	}
}

func (d *digestingToolset) Name() string {
	if base := d.inner.Name(); base != "" {
		return d.prefix + "_" + base
	}
	return d.prefix
}

func (d *digestingToolset) Tools(ctx agent.ReadonlyContext) ([]tool.Tool, error) {
	upstream, err := d.inner.Tools(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]tool.Tool, 0, len(upstream))
	for _, t := range upstream {
		// Compose renamedTool INSIDE digestingTool so the Declaration
		// (which the model sees) carries the prefixed name and the
		// Run wrapper handles digesting after the upstream call.
		out = append(out, digestingTool{
			inner: renamedTool{inner: t, prefix: d.prefix},
			opts:  d.opts,
		})
	}
	return out, nil
}

// digestingTool wraps a runnable (typically renamedTool) so its
// response goes through digest.Process before returning to the
// caller.
type digestingTool struct {
	inner renamedTool
	opts  *DigestOptions
}

func (d digestingTool) Name() string                            { return d.inner.Name() }
func (d digestingTool) Description() string                     { return d.inner.Description() }
func (d digestingTool) IsLongRunning() bool                     { return d.inner.IsLongRunning() }
func (d digestingTool) Declaration() *genai.FunctionDeclaration { return d.inner.Declaration() }

// Run calls the wrapped tool, marshals the response, runs it through
// digest.Process, and returns a synthetic map:
//
//	{
//	  "digest":     "<compressed payload>",
//	  "raw_bytes":  N,
//	  "call_id":    "<toolCallID>",  // present only when Store wired
//	  "method":     "structural_json" | "passthrough" | "llm_fallback",
//	}
//
// The model sees the digest as the tool response, plus the call_id it
// can pass to retrieve_raw when the digest looks suspicious.
//
// Digest failures (which shouldn't happen — digest.Process never
// returns a content-shape error) degrade to a bounded passthrough of
// the marshaled raw response, so the caller always gets *something*
// they can hand to the model.
func (d digestingTool) Run(ctx tool.Context, args any) (map[string]any, error) {
	// mcp.tool_call span groups the upstream HTTP round-trip
	// (already otelhttp-instrumented via #237) with the digest
	// child span pkg/digest.Process emits below. Attribute names
	// use core_agent.* namespace per docs/agentic-mcp-design.md.
	// Span kind: internal — the wrap layer isn't itself a network
	// egress; the underlying HTTP call is a child span in its own
	// right.
	toolName := d.Name()
	spanCtx, span := tracer.Start(ctx, "mcp.tool_call", trace.WithSpanKind(trace.SpanKindInternal))
	span.SetAttributes(attribute.String("core_agent.mcp.tool_name", toolName))
	defer span.End()

	// Bracket wall-clock latency around the upstream tool call so
	// operators can see per-call timing without hand-scraping the
	// eventlog (#277). Stamped as `latency_ms` on the returned map
	// alongside the digest keys — travels the wire through both
	// remote (internal/coretuiremote/adapter.go:toolResultFromPart)
	// and embedded (cmd/core-agent/coretui_enabled.go:splitFunctionResponse)
	// TUI paths without any adapter-side plumbing, because both
	// projections copy the whole FunctionResponse.Response map
	// through to coretui.ToolResult.Response verbatim.
	//
	// Also stamped on the error / marshal-fallback paths so slow
	// failing calls are still visible (a 30-second MCP timeout is
	// exactly the case operators need to see).
	// Swap the context.Context inside tool.Context to spanCtx so
	// otelhttp on the inner MCP call picks up mcp.tool_call as the
	// parent span. Delegates non-context methods (FunctionCallID,
	// State, ...) to the original tool.Context so the inner tool
	// sees the same behavior otherwise.
	innerCtx := spanCtxToolContext{Context: ctx, span: spanCtx}
	start := time.Now()
	raw, err := d.inner.Run(innerCtx, args)
	latencyMS := time.Since(start).Milliseconds()
	if err != nil {
		// Upstream tool errored — return the error verbatim + inject
		// latency into a shallow-copied map so the caller gets *some*
		// timing signal on the failing call. If raw is nil we can't
		// carry the sidecar (nothing to attach it to); accept that
		// edge as unmeasured.
		return withLatency(raw, latencyMS), err
	}

	rawBytes, marshalErr := json.Marshal(raw)
	if marshalErr != nil {
		// Fallback: hand back the un-digested map so the tool call
		// still completes. digest_method telemetry will not capture
		// this case, which is acceptable for what should be a
		// vanishingly rare path.
		return withLatency(raw, latencyMS), nil
	}

	callID := ""
	if ctx != nil {
		callID = ctx.FunctionCallID()
	}

	// Adapt operator-supplied LLMFallback to digest.Options's simpler
	// signature. We need the subagent's usage numbers to populate
	// Savings.Subagent* AFTER Process returns, so capture them in a
	// closure here rather than threading a new return through pkg/digest.
	// Zero-valued when LLMFallback is nil OR the router doesn't take
	// the fallback path.
	var (
		fbModel string
		fbIn    int
		fbOut   int
	)
	var digestLLM func(context.Context, []byte) (string, error)
	if d.opts.LLMFallback != nil {
		digestLLM = func(ctx context.Context, raw []byte) (string, error) {
			r, err := d.opts.LLMFallback(ctx, raw)
			if err != nil {
				return "", err
			}
			fbModel = r.SubagentModel
			fbIn = r.SubagentInputTokens
			fbOut = r.SubagentOutputTokens
			return r.Text, nil
		}
	}

	// Pass spanCtx (not ctx) so digest.process nests under mcp.tool_call
	// in the trace. The LLMFallback closure sees the same spanCtx and
	// stamps subagent.llm_call as a grandchild.
	res, procErr := digest.Process(spanCtx, rawBytes, digest.Options{
		Threshold:   d.opts.threshold(),
		Store:       d.opts.Store,
		CallID:      callID,
		LLMFallback: digestLLM,
	})
	if procErr != nil {
		// Same fallback rationale as marshal error.
		return withLatency(raw, latencyMS), nil
	}

	// If the LLM fallback fired, decorate Savings with the subagent's
	// usage — pkg/digest can't populate this itself (it doesn't own
	// the subagent). Only touch Savings on the fallback path; on
	// structural / passthrough the Subagent* fields correctly stay
	// zero.
	if res.Savings != nil && res.Method == digest.MethodLLMFallback {
		res.Savings.SubagentModel = fbModel
		res.Savings.SubagentInputTokens = fbIn
		res.Savings.SubagentOutputTokens = fbOut
	}

	// Fire the aggregator callback (best-effort — a nil callback is
	// the shipped default). Called AFTER Subagent* decoration so the
	// sink sees the fully-populated Savings.
	if d.opts.OnResult != nil {
		d.opts.OnResult(&res)
	}

	out := map[string]any{
		"digest":     res.Digest,
		"raw_bytes":  res.RawBytes,
		"method":     res.Method,
		"latency_ms": latencyMS,
	}
	if res.CallID != "" {
		out["call_id"] = res.CallID
	}
	if len(res.Metadata) > 0 {
		out["digest_meta"] = res.Metadata
	}
	// Surface Savings on the returned map so the runner + TUI adapters
	// (which already forward the whole FunctionResponse map through)
	// can render per-tool-call footers without a new plumbing layer.
	// Same forward-compat rationale as latency_ms above.
	if res.Savings != nil {
		sv := map[string]any{
			"path":                res.Savings.Path,
			"original_bytes":      res.Savings.OriginalBytes,
			"digest_bytes":        res.Savings.DigestBytes,
			"original_tokens_est": res.Savings.OriginalTokensEst,
			"digest_tokens_est":   res.Savings.DigestTokensEst,
		}
		if res.Savings.SubagentModel != "" {
			sv["subagent_model"] = res.Savings.SubagentModel
			sv["subagent_input_tokens"] = res.Savings.SubagentInputTokens
			sv["subagent_output_tokens"] = res.Savings.SubagentOutputTokens
		}
		out["savings"] = sv
	}
	return out, nil
}

// withLatency returns a shallow-copied response map with a
// `latency_ms` sidecar stamped on top. Used on the digest wrap's
// error + fallback paths where we return the upstream MCP server's
// map verbatim (no synthetic wrapping) — copying rather than
// mutating avoids surprising a caller that might reuse the raw
// map. Returns nil unchanged: some error paths from ADK/MCP can
// produce (nil, err), and we can't attach a sidecar to nothing.
func withLatency(raw map[string]any, latencyMS int64) map[string]any {
	if raw == nil {
		return nil
	}
	out := make(map[string]any, len(raw)+1)
	for k, v := range raw {
		out[k] = v
	}
	out["latency_ms"] = latencyMS
	return out
}

// ProcessRequest satisfies ADK's internal RequestProcessor interface
// — same reasoning as renamedTool.ProcessRequest. Packs the outer
// wrapper (d), not the inner, so the model-visible function name
// stays the prefixed one and dispatch routes back through digesting.
func (d digestingTool) ProcessRequest(ctx tool.Context, req *model.LLMRequest) error {
	return coretools.PackTool(req, d)
}
