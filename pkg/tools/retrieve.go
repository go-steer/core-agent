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
	"errors"
	"fmt"

	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"

	"github.com/go-steer/core-agent/pkg/digest"
)

const (
	defaultRetrieveRawName        = "retrieve_raw"
	defaultRetrieveRawDescription = "Fetch the raw, un-digested payload for a prior tool call whose response arrived digested. The call_id is what the digest wrapper stamped onto the compressed response you saw. Treat the digest as authoritative by default — DO NOT call retrieve_raw to spot-check, cross-verify, or 'see what was truncated' when the digest already answers your question. Every call re-inflates the full payload back into your context, undoing the wrap's savings (which is the point: a call that would have burned 12k tokens uncached still burns those 12k when you retrieve_raw the same content). Only call when the digest itself signals a load-bearing field was dropped (metadata will include a truncated-field marker) AND you need that specific truncated content to proceed. When the digest is ambiguous but the raw isn't obviously needed, prefer a narrower follow-up call to the underlying tool over re-inflating the whole payload. Returns the raw text and its byte size."
)

// RetrieveRawOptions configures NewRetrieveRawTool.
type RetrieveRawOptions struct {
	// Store is the CCR backing populated by digest.Process. Required.
	// When the operator hasn't wired a store (e.g. --session-db is
	// off and no explicit FilesystemStore was configured), consumers
	// should skip registering the tool rather than passing nil here
	// — the tool would refuse every call and only confuse the model.
	Store digest.Store

	// Name overrides the tool's function name. Defaults to
	// "retrieve_raw" when empty.
	Name string

	// Description overrides the tool's description (the prose the
	// model sees in its function-decl list). Empty falls back to a
	// sensible default that explains when NOT to use it.
	Description string
}

type retrieveRawArgs struct {
	CallID string `json:"call_id" jsonschema:"the call_id stamped onto the digest by the digest wrapper — usually surfaced as call_id in the synthetic tool response"`
}

// retrieveRawResult is what the tool returns to the model. Raw is
// the un-digested payload as a string. Bytes is the size — useful
// when Raw is truncated in transit or when the model needs to
// decide whether to slice further before shipping to the next tool.
type retrieveRawResult struct {
	Raw   string `json:"raw"`
	Bytes int    `json:"bytes"`
}

// NewRetrieveRawTool exposes digest.Store.Get as an ADK tool the
// agent can call during a turn. The tool returns the raw payload
// verbatim; the caller / model is responsible for deciding what to
// slice or extract before feeding it to the next step.
//
// Design intent: aggressive digesting is only safe when the model
// has an escape hatch to fetch back the raw payload. This tool is
// that hatch. Register it alongside any consumer wiring
// digest.Process with a Store.
//
// Error handling: the tool NEVER returns a Go error from its
// handler — unknown call_ids and store failures surface as a
// tool-response with Raw carrying a "(error: ...)" prefix. That
// keeps the model in the loop (it can retry, ask the user, or
// pick a different call_id) rather than aborting the whole turn.
//
// Returns an error only if opts.Store is nil at construction time.
// Consumers who don't have a Store should not call this — see the
// Store field docstring on RetrieveRawOptions.
func NewRetrieveRawTool(opts RetrieveRawOptions) (tool.Tool, error) {
	if opts.Store == nil {
		return nil, fmt.Errorf("tools: NewRetrieveRawTool: Store is required")
	}
	name := opts.Name
	if name == "" {
		name = defaultRetrieveRawName
	}
	desc := opts.Description
	if desc == "" {
		desc = defaultRetrieveRawDescription
	}
	return functiontool.New(
		functiontool.Config{Name: name, Description: desc},
		retrieveRawFunc(opts.Store),
	)
}

// retrieveRawFunc returns the tool handler as a plain function so
// tests can exercise the logic without going through functiontool's
// reflection layer. Matches the same factory-returning-handler
// pattern used by fetch.go / bash.go.
func retrieveRawFunc(store digest.Store) func(tool.Context, retrieveRawArgs) (retrieveRawResult, error) {
	return func(ctx tool.Context, in retrieveRawArgs) (retrieveRawResult, error) {
		if in.CallID == "" {
			return retrieveRawResult{
				Raw: "(error: retrieve_raw requires a non-empty call_id)",
			}, nil
		}
		raw, err := store.Get(ctx, in.CallID)
		if err != nil {
			// Distinguish "unknown key" from other failures so
			// the model can react appropriately: unknown → try
			// a different call_id or admit it doesn't have the
			// data; store failure → maybe retry, maybe give up.
			if errors.Is(err, digest.ErrNotFound) {
				return retrieveRawResult{
					Raw: fmt.Sprintf("(error: no raw payload stored for call_id %q — the digest may pre-date store wiring, or the id may be typo'd)", in.CallID),
				}, nil
			}
			return retrieveRawResult{
				Raw: fmt.Sprintf("(error: store failed to fetch %q: %v)", in.CallID, err),
			}, nil
		}
		return retrieveRawResult{
			Raw:   string(raw),
			Bytes: len(raw),
		}, nil
	}
}
