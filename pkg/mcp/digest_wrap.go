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
	"encoding/json"

	"google.golang.org/adk/agent"
	"google.golang.org/adk/model"
	"google.golang.org/adk/tool"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/pkg/digest"
	coretools "github.com/go-steer/core-agent/pkg/tools"
)

// DigestOptions configures how Build wraps MCP tool responses through
// pkg/digest. A nil *DigestOptions passed to Build disables wrapping
// entirely (existing behavior). A non-nil options struct wraps every
// tool from every server that isn't in NeverServers.
//
// LLMFallback is intentionally omitted from v1 — per the 2026-07-14
// cost-stack plan (docs/backlog-cost-stack-2026-07-14.md), the LLM
// subagent path (#124) is parked. Prose-shaped MCP responses take the
// bounded passthrough branch, not a fallback subtask.
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
	raw, err := d.inner.Run(ctx, args)
	if err != nil {
		// Upstream tool errored — return the error verbatim, no
		// digesting. The model needs to see the raw failure to
		// decide whether to retry / adapt.
		return raw, err
	}

	rawBytes, marshalErr := json.Marshal(raw)
	if marshalErr != nil {
		// Fallback: hand back the un-digested map so the tool call
		// still completes. digest_method telemetry will not capture
		// this case, which is acceptable for what should be a
		// vanishingly rare path.
		return raw, nil
	}

	callID := ""
	if ctx != nil {
		callID = ctx.FunctionCallID()
	}

	res, procErr := digest.Process(ctx, rawBytes, digest.Options{
		Threshold: d.opts.threshold(),
		Store:     d.opts.Store,
		CallID:    callID,
	})
	if procErr != nil {
		// Same fallback rationale as marshal error.
		return raw, nil
	}

	out := map[string]any{
		"digest":    res.Digest,
		"raw_bytes": res.RawBytes,
		"method":    res.Method,
	}
	if res.CallID != "" {
		out["call_id"] = res.CallID
	}
	if len(res.Metadata) > 0 {
		out["digest_meta"] = res.Metadata
	}
	return out, nil
}

// ProcessRequest satisfies ADK's internal RequestProcessor interface
// — same reasoning as renamedTool.ProcessRequest. Packs the outer
// wrapper (d), not the inner, so the model-visible function name
// stays the prefixed one and dispatch routes back through digesting.
func (d digestingTool) ProcessRequest(ctx tool.Context, req *model.LLMRequest) error {
	return coretools.PackTool(req, d)
}
