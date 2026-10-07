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
// The empty-Details 400 INVALID_ARGUMENT from #898 is NOT here. On its
// own it is indistinguishable from a request that is malformed, and
// INVALID_ARGUMENT is the most overloaded answer Vertex gives —
// vertexcache.IsCacheGone already has to carve one meaning out of it
// and warns about exactly this. It is retried only once the session has
// already been served, which this predicate cannot see; that is
// IsBareInvalidArgument's job, wired as the policy's
// IsTransientAfterSuccess (#1247).
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

// bareInvalidArgumentMessage is the whole message of the 400 that #898
// and #1247 recorded. Vertex sends it when it names nothing it could
// not parse.
const bareInvalidArgumentMessage = "Request contains an invalid argument."

// bareInvalidArgumentText is how genai.APIError renders that 400 with
// no details, for an error that reaches us without its type.
const bareInvalidArgumentText = "Error 400, Message: " + bareInvalidArgumentMessage + ", Status: INVALID_ARGUMENT, Details: []"

// IsBareInvalidArgument reports whether err is a Vertex 400
// INVALID_ARGUMENT that names nothing: the generic message and no
// details. Twice now — #898 on 2.9.0-dev.4 and #1247 on 2026-10-06 —
// it arrived on a session whose previous call, under the same config
// and model, had succeeded, and the session went on working afterwards.
//
// It is NOT a transient predicate on its own and must not be used as
// IsTransient: a request that is malformed from its first call gets the
// same answer. The policy consults it only as IsTransientAfterSuccess,
// for a call whose session has already been served
// (models.PriorSuccess), which is what rules out the request that was
// malformed from the start. A history made malformed since that success
// can still produce it, and then costs one retry before it surfaces.
//
// Three things must all hold, and each is a narrowing on purpose:
//
//   - code 400 with status INVALID_ARGUMENT, not just the code: a
//     plain-text 400 carries the HTTP status line ("400 Bad Request");
//   - no details: a 400 with field violations has said what is wrong;
//   - the generic message, exactly: a 400 whose message names a
//     parameter ("an empty text parameter", a function declaration's
//     name) is saying what is wrong too, and an expired cache ("Cache
//     content <id> is expired.") is vertexcache.IsCacheGone's.
//
// The typed check is tried first, and a typed error that fails it is a
// definite no, as in IsTransient. The string fallback requires the
// whole rendered error, "Details: []" included, so a 400 that carries
// details cannot match it.
func IsBareInvalidArgument(err error) bool {
	if err == nil {
		return false
	}
	var apiErr genai.APIError
	if errors.As(err, &apiErr) {
		return apiErr.Code == 400 &&
			apiErr.Status == "INVALID_ARGUMENT" &&
			len(apiErr.Details) == 0 &&
			strings.TrimSpace(apiErr.Message) == bareInvalidArgumentMessage
	}
	return strings.Contains(err.Error(), bareInvalidArgumentText)
}
