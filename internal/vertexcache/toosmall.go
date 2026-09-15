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

package vertexcache

import "strings"

// IsBelowCacheMinimum reports whether err says the content we asked
// Vertex to cache is smaller than the model's minimum cacheable size.
//
// This is the one Create failure that time cannot fix. The cached
// content is the agent's system instruction plus its tool declarations
// — a property of the configuration, fixed for the life of the process
// — so attempt 6 carries exactly as many tokens as attempt 1 did, and
// the retry schedule the #707 carve-out exists to provide (~7.75
// minutes, for an IAM binding that has not propagated yet) buys nothing
// but six doomed RPCs and six alarming log lines (#1067).
//
// The shape, verbatim from `dev/uat/approval-gate/`'s deliberately
// minimal daemon — no persona, no skills, fifteen of sixteen built-in
// tools disabled, which is exactly what lands under the floor:
//
//	Error 400, Message: The cached content is of 2373 tokens. The minimum
//	token count to start explicit caching is 4096., Status: INVALID_ARGUMENT
//
// Matching is on the reason, never on the number. The minimum is
// per-model and Google has moved it before; a predicate that knows 4096
// is a predicate that stops working the day it changes. Pairing the
// phrase with INVALID_ARGUMENT keeps a future error that merely
// mentions token counts — a quota, an input-size ceiling — from being
// read as this.
func IsBelowCacheMinimum(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	if !strings.Contains(s, "INVALID_ARGUMENT") {
		return false
	}
	return strings.Contains(strings.ToLower(s), "minimum token count")
}
