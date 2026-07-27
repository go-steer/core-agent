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

package recording

import (
	"context"
	"encoding/json"
	"io"
	"iter"
	"log"
	"sync"

	adkmodel "google.golang.org/adk/model"
)

// NewRecorder wraps inner so every GenerateContent turn is appended
// to w as a single JSONL line in RecordedTurn shape. The wrapper is
// transparent: callers see the inner LLM's responses unchanged. The
// caller owns w's lifecycle (open before, close after).
//
// Errors from the inner LLM pass through to the caller but are not
// recorded — replay can't reproduce a remote error meaningfully.
// Partial responses received before an error are still encoded.
func NewRecorder(inner adkmodel.LLM, w io.Writer) adkmodel.LLM {
	return &recorderLLM{inner: inner, enc: json.NewEncoder(w)}
}

type recorderLLM struct {
	inner adkmodel.LLM
	enc   *json.Encoder
	mu    sync.Mutex
}

func (l *recorderLLM) Name() string { return l.inner.Name() }

// recordedTurnWire is the encode-side twin of RecordedTurn: the
// request rides as pre-serialized JSON so the recorded form is fixed
// at snapshot time. Field names/order match RecordedTurn, so the
// emitted JSONL is byte-identical to encoding a RecordedTurn.
type recordedTurnWire struct {
	Request   json.RawMessage         `json:"request"`
	Responses []*adkmodel.LLMResponse `json:"responses"`
}

func (l *recorderLLM) GenerateContent(ctx context.Context, req *adkmodel.LLMRequest, stream bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		// Serialize the request to its recorded form BEFORE handing it
		// to the inner LLM. A shallow struct copy is not enough: it
		// shares the Config pointer, and the inner LLM may mutate
		// Config in place (e.g., the built-ins wrapper appends to
		// Config.Tools), which would retroactively rewrite what we
		// "snapshotted". Marshal errors leave capturedReq nil — the
		// turn is still recorded (request: null) so the response
		// stream isn't lost, and the failure is logged rather than
		// silently dropped.
		capturedReq, err := json.Marshal(req)
		if err != nil {
			log.Printf("recording: failed to marshal request snapshot (recording turn with null request): %v", err)
			capturedReq = nil
		}

		var responses []*adkmodel.LLMResponse
		stopped := false
		for resp, err := range l.inner.GenerateContent(ctx, req, stream) {
			if err == nil && resp != nil {
				// Stable copy — the caller may mutate the response after
				// receiving it, and we want what the inner LLM produced.
				snap := *resp
				responses = append(responses, &snap)
			}
			if !yield(resp, err) {
				stopped = true
				break
			}
		}
		_ = stopped // silence unused if a future change drops the early-break path

		l.mu.Lock()
		defer l.mu.Unlock()
		if err := l.enc.Encode(recordedTurnWire{Request: capturedReq, Responses: responses}); err != nil {
			log.Printf("recording: failed to encode turn: %v", err)
		}
	}
}
