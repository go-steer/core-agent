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
	adkmodel "google.golang.org/adk/model"
)

// RecordedTurn is the on-disk shape of a single LLM turn captured by
// the recording wrapper and consumed by the scripted provider.
//
// One RecordedTurn is written per JSONL line. Request is a snapshot
// taken before the inner LLM may have mutated it (Config.Tools is
// commonly appended to). Responses is the full ordered stream of
// LLMResponse values yielded for that turn — typically zero or more
// Partial: true chunks followed by exactly one TurnComplete: true.
//
// Note that adkmodel.LLMRequest.Tools is tagged json:"-" upstream and
// will silently drop on serialization. That's intentional: the inner
// LLM provides tool declarations on replay; recorded Tools would be
// dead weight.
type RecordedTurn struct {
	Request   *adkmodel.LLMRequest    `json:"request"`
	Responses []*adkmodel.LLMResponse `json:"responses"`
}
