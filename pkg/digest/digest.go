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

// Package digest consolidates the digesting primitives core-agent
// uses to keep large tool responses out of the parent context.
// Inspired by Headroom (Netflix, Apache 2.0), which ships the same
// idea as a Python library.
//
// Three primitives, each independently useful and testable:
//
//   - Content router — sniff the payload shape and dispatch
//     (passthrough / structural JSON / LLM fallback).
//   - Structural JSON pruner — preserve identifier-shaped keys,
//     collapse long strings and arrays, recurse with a depth cap.
//     Deterministic, no API call.
//   - CCR store — keep the raw payload locally keyed by tool-call
//     ID so the model can fetch it back via a retrieve_raw built-in
//     tool. (Skeleton PR: store interface + implementations land in
//     the follow-up per docs/digest-design.md sequencing.)
//
// LLM-agnostic: this package digests payloads. It does not import
// pkg/agent, does not know what an MCP tool is, does not reach for
// the model loop. Callers pass an LLMFallback function if they want
// one.
//
// Full design: docs/digest-design.md. Tracking issue: #128.
package digest

import (
	"context"
	"errors"
)

// Method values populated on Result.Method — the observable dispatch
// decision the router made. Callers surface these in telemetry
// (per-tool method distribution → drives the decision on whether to
// add tool-specific pruners).
const (
	MethodPassthrough    = "passthrough"
	MethodStructuralJSON = "structural_json"
	MethodLLMFallback    = "llm_fallback"
)

// Result is what Process returns to the caller. RawBytes is the
// serialized size of the original payload — useful for telemetry
// even when Method is passthrough. CallID is populated when a Store
// is wired (follow-up PR); the skeleton always leaves it empty.
type Result struct {
	Digest   string         // compressed payload (caller hands this to the model)
	Method   string         // one of the Method* constants above
	RawBytes int            // serialized size of the original
	CallID   string         // opaque ID for CCR retrieval (empty until Store lands)
	Metadata map[string]any // pruner-specific stats (e.g. {"arrays_collapsed": 3})
}

// Options configure a single Process call. All fields are optional;
// a zero Options passes payloads through verbatim (which is useful
// for telemetry-only wiring where the caller wants byte counts but
// not compression).
type Options struct {
	// Threshold: payloads smaller than this bypass digesting entirely.
	// Zero = 0 bytes = always digest; callers typically want a
	// meaningful value (e.g. 4096) so tiny responses skip the router
	// overhead.
	Threshold int

	// Store: optional CCR backing. When non-nil AND CallID is
	// non-empty, Process writes the raw payload to the store before
	// returning and populates Result.CallID so the caller can weave
	// the ID into the synthetic map handed to the model. When nil or
	// CallID is empty, no retrieval is possible and CallID stays
	// empty on the way back.
	//
	// Store errors are surfaced in Result.Metadata["store_err"] but
	// don't fail Process — losing retrieval capability shouldn't
	// break the primary digest path.
	Store Store

	// LLMFallback: optional prose digester. Called when the router
	// cannot dispatch to a structural pruner. When nil, payloads that
	// would fall through return Method == passthrough with Digest
	// truncated to a safe upper bound (see MaxPassthroughBytes) so we
	// never silently dump megabytes into the model's context.
	LLMFallback func(ctx context.Context, raw []byte) (string, error)

	// CallID: caller-provided identifier (e.g. tool-call ID). When
	// empty, Process leaves Result.CallID empty and skips the Store
	// write even when Store is non-nil.
	CallID string
}

// MaxPassthroughBytes bounds how much prose data is returned verbatim
// when neither a structural pruner nor an LLMFallback is available.
// Payloads over this cap are truncated with a "…<N more bytes>" suffix
// so a caller who forgot to wire an LLMFallback still doesn't slam
// the model with a megabyte of raw text.
const MaxPassthroughBytes = 64 * 1024

// Process digests payload according to opts. It never returns an
// error for content-shape reasons — pruner failures fall through to
// the LLM fallback or passthrough. The only error path is a caller
// mistake (nil ctx) or an LLMFallback that errors out; even the
// latter degrades to a truncated-passthrough Result so the caller
// still has *something* to hand to the model.
//
// When opts.Store is wired AND opts.CallID is set, the raw payload
// is persisted to the store before the dispatch decision is made
// (so retrieve_raw works even when the router chose passthrough).
// Store failures degrade to a Result with Metadata["store_err"] set
// — losing retrieval capability shouldn't break the primary digest
// path.
func Process(ctx context.Context, payload []byte, opts Options) (Result, error) {
	if ctx == nil {
		return Result{}, errors.New("digest: nil context")
	}
	rawBytes := len(payload)

	// Persist to the CCR store BEFORE routing. If the write fails,
	// we still process — the caller gets a digest, just no retrieval
	// backdoor for this payload. The error surfaces in Metadata so
	// telemetry can catch it.
	var storeErr error
	if opts.Store != nil && opts.CallID != "" {
		storeErr = opts.Store.Put(ctx, opts.CallID, payload)
	}

	// Route on payload shape. The router owns the "which method" call;
	// each branch below owns the actual compression work.
	method := route(payload, opts.Threshold, opts.LLMFallback != nil)

	res := Result{
		RawBytes: rawBytes,
		CallID:   opts.CallID,
	}
	switch method {
	case MethodPassthrough:
		// truncatePassthrough is a no-op when payload fits under
		// MaxPassthroughBytes (the common under-threshold case), and
		// bounds oversize prose that reached here because no
		// LLMFallback was wired. Either way, the model never sees an
		// unbounded blob.
		res.Digest = truncatePassthrough(payload)
		res.Method = MethodPassthrough

	case MethodStructuralJSON:
		digest, meta := PruneJSON(payload)
		res.Digest = digest
		res.Method = MethodStructuralJSON
		res.Metadata = meta

	case MethodLLMFallback:
		digest, err := opts.LLMFallback(ctx, payload)
		if err != nil {
			// The LLM path errored — fall back to a bounded passthrough
			// so the caller still gets a usable Result. Callers who want
			// to surface the error can inspect Result.Metadata["llm_err"].
			res.Digest = truncatePassthrough(payload)
			res.Method = MethodPassthrough
			res.Metadata = map[string]any{"llm_err": err.Error()}
			break
		}
		res.Digest = digest
		res.Method = MethodLLMFallback

	default:
		// Unreachable: route() returns one of the three consts above.
		res.Digest = truncatePassthrough(payload)
		res.Method = MethodPassthrough
	}

	if storeErr != nil {
		// Losing retrieval capability shouldn't invalidate the digest
		// itself, but operators need to see the failure. Retrieval is
		// silently broken for this CallID going forward.
		if res.Metadata == nil {
			res.Metadata = map[string]any{}
		}
		res.Metadata["store_err"] = storeErr.Error()
	}
	return res, nil
}

// truncatePassthrough returns payload verbatim if it fits under
// MaxPassthroughBytes, or a truncated form with a size marker
// otherwise. Prevents a caller who forgot to wire LLMFallback from
// silently dumping megabytes into the model context.
func truncatePassthrough(payload []byte) string {
	if len(payload) <= MaxPassthroughBytes {
		return string(payload)
	}
	head := payload[:MaxPassthroughBytes]
	dropped := len(payload) - MaxPassthroughBytes
	return string(head) + truncationSuffix(dropped)
}
