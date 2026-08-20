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
	"encoding/json"

	"google.golang.org/adk/agent"
	"google.golang.org/adk/model"
	adktool "google.golang.org/adk/tool"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v2/pkg/digest"
)

// DefaultDigestThreshold is the byte size a built-in response has to
// exceed before the wrap does anything at all. Matches
// mcp.DefaultAgenticWrapThreshold so an 8KB payload costs the same
// whether it came from a remote server or from the local filesystem
// — the asymmetry #706 is about is exactly that the two thresholds
// used to be "8000" and "infinity".
const DefaultDigestThreshold = 8000

// digestableBuiltins names the built-ins whose responses go through
// the digest wrap. Membership is a property of the tool, not an
// operator preference, so it lives here rather than in DigestOptions.
//
// The rule: a tool is in this set when its job is to SURVEY — to
// answer "what is out there" over a set the model named loosely (a
// glob, a directory, a pattern). A survey answer is useful in
// summary and the model has an obvious way to narrow it, so trading
// completeness for context is the right trade and the model can undo
// the trade cheaply.
//
// Deliberately NOT in the set:
//
//   - read_file — the narrowing move itself. A survey digest is only
//     safe because "then read the one file you actually want" is a
//     cheap next step; digesting that step too removes the floor.
//     read_file is also what precedes edit_file, and edit_file
//     matches old_string exactly — handing the model a truncated
//     copy of a file it is about to edit buys a few thousand tokens
//     and spends them again on a failed edit.
//   - bash — arbitrary output the model frequently needs verbatim
//     (a diff, a stack trace, an exit banner), with no structure the
//     JSON pruner can exploit and no narrowing argument to point at.
//   - json_query — the model already narrowed. Pruning a jq result
//     is second-guessing an extraction that was the whole point of
//     the call.
//   - write_file / edit_file / delete_file / record_plan / todo /
//     alert / ask_user / retrieve_raw — control and mutation
//     responses, small by construction, and in retrieve_raw's case
//     digesting its output would defeat the escape hatch that makes
//     the rest of this safe.
var digestableBuiltins = map[string]bool{
	"read_many_files": true,
	"grep":            true,
	"glob":            true,
	"list_dir":        true,
}

// DigestOptions configures the built-in digest wrap. A nil
// *DigestOptions handed to NewDigester disables wrapping entirely,
// which is the shape the --no-mcp-digest kill switch relies on.
type DigestOptions struct {
	// Store is the CCR backing behind retrieve_raw. Callers should
	// wire the same store retrieve_raw was constructed with —
	// digesting without it hands the model a call_id that resolves
	// to nothing.
	Store digest.Store

	// Threshold is the byte size below which responses are returned
	// verbatim. Zero → DefaultDigestThreshold.
	Threshold int

	// OnResult, when non-nil, fires after each digest with the
	// finished Result. Session-level savings are aggregated off the
	// `savings` sidecar by pkg/agent's event tap, so production
	// leaves this nil; it exists for tests and for hosts that embed
	// the wrap without an agent event loop.
	OnResult func(*digest.Result)
}

func (o *DigestOptions) threshold() int {
	if o == nil || o.Threshold <= 0 {
		return DefaultDigestThreshold
	}
	return o.Threshold
}

// Digester wraps built-in tools so oversize responses reach the model
// as a digest plus a retrievable call_id instead of inline
// (docs/digest-design.md; #706). Same shape as DurationInstrumenter:
// construct once, apply to the assembled tool slice.
//
// A nil *Digester passes tools through, so a caller that decided not
// to digest doesn't need a branch at the call site.
type Digester struct {
	opts *DigestOptions
}

// NewDigester returns a Digester for opts, or nil when opts is nil.
// The nil return is load-bearing: Wrap is nil-safe, so
// `NewDigester(nil).Wrap(ts)` is the identity.
func NewDigester(opts *DigestOptions) *Digester {
	if opts == nil {
		return nil
	}
	return &Digester{opts: opts}
}

// Wrap returns ts with every digestable built-in replaced by a
// digesting wrapper. Tools outside digestableBuiltins — and
// non-runnable tools, matching the posture of every other wrapper in
// this package — are returned untouched, by identity.
func (d *Digester) Wrap(ts []adktool.Tool) []adktool.Tool {
	if d == nil || d.opts == nil {
		return ts
	}
	out := make([]adktool.Tool, len(ts))
	for i, t := range ts {
		out[i] = d.wrapOne(t)
	}
	return out
}

// WrapToolset wraps a toolset so digestable tools it yields are
// digested. Provided for symmetry with InstrumentToolset; built-ins
// arrive as a slice today, but skills and rooted subagents assemble
// toolsets.
func (d *Digester) WrapToolset(ts adktool.Toolset) adktool.Toolset {
	if d == nil || d.opts == nil || ts == nil {
		return ts
	}
	return &digestingToolset{inner: ts, d: d}
}

func (d *Digester) wrapOne(t adktool.Tool) adktool.Tool {
	if t == nil || !digestableBuiltins[t.Name()] {
		return t
	}
	if _, already := t.(*digestingTool); already {
		// Wrap is idempotent. A second layer would digest the first
		// layer's synthetic map — pruning `{digest, raw_bytes, ...}`
		// into a digest of a digest, and stamping a second call_id
		// over the one that pointed at the real payload.
		return t
	}
	if _, ok := t.(runnableTool); !ok {
		return t
	}
	return &digestingTool{inner: t, opts: d.opts}
}

type digestingToolset struct {
	inner adktool.Toolset
	d     *Digester
}

func (s *digestingToolset) Name() string { return s.inner.Name() }

func (s *digestingToolset) Tools(ctx agent.ReadonlyContext) ([]adktool.Tool, error) {
	upstream, err := s.inner.Tools(ctx)
	if err != nil {
		return nil, err
	}
	return s.d.Wrap(upstream), nil
}

// digestingTool routes one built-in's response through pkg/digest.
type digestingTool struct {
	inner adktool.Tool
	opts  *DigestOptions
}

func (dt *digestingTool) Name() string        { return dt.inner.Name() }
func (dt *digestingTool) Description() string { return dt.inner.Description() }
func (dt *digestingTool) IsLongRunning() bool { return dt.inner.IsLongRunning() }

// ReadOnlyHint forwards the wrapped tool's dispatch-class declaration
// (ReadOnlyHinter, #460), same as gatedTool and timedTool. Every tool
// in digestableBuiltins is read-only, so dropping the forward would
// reclassify the whole set as mutating.
func (dt *digestingTool) ReadOnlyHint() bool {
	if h, ok := dt.inner.(ReadOnlyHinter); ok {
		return h.ReadOnlyHint()
	}
	return false
}

func (dt *digestingTool) Declaration() *genai.FunctionDeclaration {
	if rn, ok := dt.inner.(runnableTool); ok {
		return rn.Declaration()
	}
	return nil
}

// ProcessRequest packs dt — the wrapper — so ADK dispatch routes
// through the digest instead of bypassing it. Same shape as
// timedTool.ProcessRequest.
func (dt *digestingTool) ProcessRequest(ctx adktool.Context, req *model.LLMRequest) error {
	return PackTool(req, dt)
}

// Run calls the wrapped tool and, when the response is over
// threshold, substitutes the same synthetic map pkg/mcp's wrap
// returns:
//
//	{
//	  "digest":    "<compressed payload>",
//	  "raw_bytes": N,
//	  "method":    "structural_json" | "passthrough" | "llm_fallback",
//	  "call_id":   "<tool call id>",   // only when a Store is wired
//	}
//
// Byte-for-byte the same wire contract, deliberately: core-tui's
// per-tool savings chip and pkg/agent's session-savings event tap
// both key off these names, so a built-in digest lights up both
// without either learning that built-ins exist.
//
// Under threshold the ORIGINAL response is returned unchanged, which
// is where this parts company with the MCP wrap. The MCP wrap always
// substitutes; doing that here would rewrite the response shape of
// every three-entry list_dir in every recipe and every test for no
// saving at all. Digesting is a cost intervention, so it should be
// invisible until there is a cost.
//
// Every failure path returns the original response. A digest that
// cannot be produced is not worth failing a tool call over, and a
// digest that came out no smaller than the payload it replaced is
// not worth the retrieve_raw round trip it would invite.
func (dt *digestingTool) Run(ctx adktool.Context, args any) (map[string]any, error) {
	rn, ok := dt.inner.(runnableTool)
	if !ok {
		return nil, nil
	}
	raw, err := rn.Run(ctx, args)
	if err != nil || raw == nil {
		return raw, err
	}

	rawBytes, marshalErr := json.Marshal(raw)
	if marshalErr != nil || len(rawBytes) <= dt.opts.threshold() {
		return raw, nil
	}

	callID := ""
	if ctx != nil {
		callID = ctx.FunctionCallID()
	}

	// Threshold is passed as 0, not dt.opts.threshold(): the size
	// gate already fired above, and re-applying it inside Process
	// would only re-derive the same answer. Passing the real
	// threshold would be harmless today and wrong the moment the
	// two checks disagree.
	res, procErr := digest.Process(ctx, rawBytes, digest.Options{
		Store:  dt.opts.Store,
		CallID: callID,
	})
	if procErr != nil {
		return raw, nil
	}
	if res.Savings != nil && res.Savings.DigestBytes >= res.Savings.OriginalBytes {
		// The pruner had nothing to work with. Handing back a
		// same-size "digest" would cost the model a call_id it has
		// no reason to use and lose it the response's real shape.
		return raw, nil
	}

	if dt.opts.OnResult != nil {
		dt.opts.OnResult(&res)
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
	if res.Savings != nil {
		out["savings"] = map[string]any{
			"path":                res.Savings.Path,
			"original_bytes":      res.Savings.OriginalBytes,
			"digest_bytes":        res.Savings.DigestBytes,
			"original_tokens_est": res.Savings.OriginalTokensEst,
			"digest_tokens_est":   res.Savings.DigestTokensEst,
		}
	}
	return out, nil
}
