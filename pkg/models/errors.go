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

package models

import "errors"

// ErrEmptyResponse is the provider-agnostic sentinel for "the call
// succeeded and the model produced nothing usable". Provider adapters
// that synthesize such an error wrap this so callers can recognize the
// condition without importing the adapter — gemini.ErrEmptyResponse
// does, and anything added later should.
//
// It exists because "no content" needs opposite handling in the two
// places it shows up. Inside the agentic loop it is a fault: a turn
// that produces nothing leaves the loop with no next action and the
// session goes idle forever, so the Gemini adapter retries it once and
// then surfaces it as an error (#220). For a one-shot side question
// (/btw) the same condition is simply the answer — the model declined,
// and the operator should be told that rather than shown a stack of
// provider prose. AskSideQuestion converts it; the loop doesn't.
//
// Match with errors.Is. Never compare messages: the adapters wrap this
// with their own diagnostic text, which is the part that changes.
var ErrEmptyResponse = errors.New("model returned no usable content")
