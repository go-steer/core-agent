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

package mock

import (
	"context"
	"encoding/json"
	"io"
	"iter"
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

func (l *recorderLLM) GenerateContent(ctx context.Context, req *adkmodel.LLMRequest, stream bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		// Snapshot the request before handing it to the inner LLM, which
		// may mutate Config (e.g., the Gemini built-ins wrapper appends
		// to Config.Tools).
		captured := *req

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
		_ = l.enc.Encode(RecordedTurn{Request: &captured, Responses: responses})
	}
}
