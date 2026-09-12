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
	"fmt"
	"testing"

	"google.golang.org/genai"
)

// archived429 is the error text recorded verbatim in all four GKE drill
// runs that lost a subagent delegation (#935). If the predicate does
// not match this exact string it does not do the job it was written
// for.
const archived429 = "Error 429, Message: Resource exhausted. Please try again later. " +
	"Please refer to https://cloud.google.com/vertex-ai/generative-ai/docs/error-code-429 " +
	"for more details., Status: RESOURCE_EXHAUSTED, Details: []"

func TestIsTransient(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},

		// The case this exists for, in both the shape the SDK
		// produces and the shape that survives re-wrapping.
		{"archived 429, verbatim", errors.New(archived429), true},
		{"archived 429, wrapped by ADK", fmt.Errorf("generate content: %w", errors.New(archived429)), true},
		{"typed 429", genai.APIError{Code: 429, Status: "RESOURCE_EXHAUSTED"}, true},
		{"typed 429, wrapped", fmt.Errorf("gemini: %w", genai.APIError{Code: 429}), true},
		{"typed 503", genai.APIError{Code: 503, Status: "UNAVAILABLE"}, true},
		{"untyped 503", errors.New("Error 503, Status: UNAVAILABLE, Details: []"), true},

		// Permanent. Retrying these spends a request to be told the
		// same thing.
		{"typed 400", genai.APIError{Code: 400, Status: "INVALID_ARGUMENT"}, false},
		{"typed 404", genai.APIError{Code: 404, Status: "NOT_FOUND"}, false},
		{"typed 500", genai.APIError{Code: 500, Status: "INTERNAL"}, false},

		// #898's empty-Details 400 is still a single occurrence across
		// 38 archived runs and deliberately stays out. INVALID_ARGUMENT
		// is the most overloaded answer Vertex gives.
		{"the #898 400", errors.New("Error 400, Message: Request contains an invalid argument., Status: INVALID_ARGUMENT, Details: []"), false},

		// A status code on its own is not a discriminator. The drill
		// reads live cluster state, so provider-shaped numbers and
		// words turn up in model prose and in resource names.
		{"model prose quoting a 429", errors.New("the deployment reported 429 failed probes"), false},
		{"a pod named unavailable", errors.New("pods/svc-unavailable-7d9: CrashLoopBackOff"), false},
		{"UNAVAILABLE without a 503", errors.New("Status: UNAVAILABLE on a cached handle"), false},

		// IsCacheGone owns these; reading one as transient would retry
		// the identical cached request instead of dropping the handle.
		{"cache reaped", errors.New("Error 404, Message: Cached content x is not found., Status: NOT_FOUND"), false},
		{"cache expired", errors.New("Error 400, Message: Cache content x is expired., Status: INVALID_ARGUMENT"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsTransient(tc.err); got != tc.want {
				t.Errorf("IsTransient(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// A typed error that is not transient must not fall through to the
// substring path, where its message body could re-open the door.
func TestIsTransient_TypedNonTransientIsFinal(t *testing.T) {
	err := genai.APIError{
		Code:    400,
		Status:  "INVALID_ARGUMENT",
		Message: "your previous request returned 429 RESOURCE_EXHAUSTED",
	}
	if IsTransient(err) {
		t.Error("a typed 400 quoting a 429 in its message was read as transient")
	}
}
