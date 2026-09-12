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

package gemini

import (
	"errors"
	"strings"

	"google.golang.org/genai"
)

// IsTransient reports whether err is Vertex declining to serve this
// request right now, as opposed to declining to serve it at all. A true
// answer licenses exactly one retry; see models.RetryPolicy for what
// bounds that.
//
// The shape this was written for, recorded verbatim in four GKE drill
// runs (#935), is the whole of the evidence:
//
//	Error 429, Message: Resource exhausted. Please try again later. …,
//	Status: RESOURCE_EXHAUSTED, Details: []
//
// Two codes qualify and no others:
//
//	429  RESOURCE_EXHAUSTED  quota or per-minute rate limit
//	503  UNAVAILABLE         backend briefly not serving
//
// # What is deliberately excluded
//
// The empty-Details 400 INVALID_ARGUMENT from #898 is NOT here. It is
// still a single observed occurrence across 38 archived runs, its cause
// is unknown, and INVALID_ARGUMENT is the most overloaded answer Vertex
// gives — vertexcache.IsCacheGone already has to carve one meaning out
// of it and warns about exactly this. Retrying an unexplained 400 would
// re-send a request the server has already said it cannot parse.
//
// 500 INTERNAL is also excluded. It is retryable in principle, but it
// has not appeared in the archive, and #935 asked for a conservative
// predicate rather than a complete one. Add codes when a run produces
// them.
//
// # Not the same question as attach.ClassifyTurnError
//
// That one also has a Retryable bit, also recognises RESOURCE_EXHAUSTED
// and UNAVAILABLE, and is deliberately NOT reused here. It answers a
// different question at a different moment: given a turn that has
// already failed, may the auto-continue gate re-drive it
// (autocontinue.go's transientTurnError). Its input is a committed
// turn-error, so breadth is cheap and a false positive costs one extra
// turn.
//
// This predicate decides whether to re-issue a request that is still in
// flight, from inside the model adapter, against error text that may
// have the model's own words in it. A false positive here re-sends a
// request the provider rejected on purpose. So it is narrower on
// purpose — ClassifyTurnError matches a bare "unavailable" substring,
// which in a run that reads live cluster state is a pod name as often
// as a provider status. Unifying them would have to widen this one or
// narrow that one, and neither is an improvement.
//
// # Why both a typed check and a substring check
//
// genai.APIError carries Code and Status, so the typed path is exact
// and is tried first. It is not sufficient on its own: the error
// crosses the ADK model layer before reaching the wrapper, and whether
// the chain preserves errors.As is a property of a dependency rather
// than of this package. The substring fallback pairs the numeric code
// with its status word — the discriminator convention IsCacheGone
// established — so a 429 appearing in a model's own prose, or a pod
// named "unavailable", cannot be read as a provider rejection.
func IsTransient(err error) bool {
	if err == nil {
		return false
	}

	var apiErr genai.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.Code {
		case 429, 503:
			return true
		}
		// A typed error that is not one of those is a definite no.
		// Falling through to substrings here would let the message
		// body of, say, a 400 quoting a prior 429 re-open the door.
		return false
	}

	s := err.Error()
	return (strings.Contains(s, "429") && strings.Contains(s, "RESOURCE_EXHAUSTED")) ||
		(strings.Contains(s, "503") && strings.Contains(s, "UNAVAILABLE"))
}
