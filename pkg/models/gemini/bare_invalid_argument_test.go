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

// #1247: Vertex's bare 400 INVALID_ARGUMENT is retried once, and only
// on a session that has already been served.

package gemini

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v2/pkg/models"
)

// recorded1247 is the turn error from #1247 verbatim, which is #898's
// too. If the predicate does not match this exact string it does not do
// the job it was written for.
const recorded1247 = "Error 400, Message: Request contains an invalid argument., Status: INVALID_ARGUMENT, Details: []"

func TestIsBareInvalidArgument(t *testing.T) {
	typedBare := genai.APIError{Code: 400, Message: "Request contains an invalid argument.", Status: "INVALID_ARGUMENT"}
	withDetails := typedBare
	withDetails.Details = []map[string]any{{
		"@type":           "type.googleapis.com/google.rpc.BadRequest",
		"fieldViolations": []any{map[string]any{"field": "contents[3].parts[0]"}},
	}}
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},

		// The shape this exists for.
		{"#1247 verbatim", errors.New(recorded1247), true},
		{"#1247 wrapped", fmt.Errorf("generate content: %w", errors.New(recorded1247)), true},
		{"typed", typedBare, true},
		{"typed, wrapped", fmt.Errorf("gemini: %w", typedBare), true},
		{"typed, empty non-nil details", genai.APIError{Code: 400, Message: typedBare.Message, Status: "INVALID_ARGUMENT", Details: []map[string]any{}}, true},
		// What genai renders that typed error as is what the string
		// fallback matches: the two paths agree.
		{"typed error's own rendering, untyped", errors.New(typedBare.Error()), true},

		// A 400 that says what is wrong has said it; a retry is told
		// the same thing again.
		{"typed, with details", withDetails, false},
		{"untyped, with details", errors.New(withDetails.Error()), false},
		{"typed, names a parameter", genai.APIError{Code: 400, Status: "INVALID_ARGUMENT",
			Message: "Unable to submit request because it has an empty text parameter. Add a value to the parameter and try again."}, false},
		{"untyped, names a parameter", errors.New("Error 400, Message: Unable to submit request because it has an empty text parameter., Status: INVALID_ARGUMENT, Details: []"), false},
		{"cache expired is IsCacheGone's", errors.New("Error 400, Message: Cache content 123 is expired., Status: INVALID_ARGUMENT, Details: []"), false},

		// The status word is required, not just the code.
		{"typed 400, FAILED_PRECONDITION", genai.APIError{Code: 400, Message: typedBare.Message, Status: "FAILED_PRECONDITION"}, false},
		{"typed 400, HTTP status line", genai.APIError{Code: 400, Message: typedBare.Message, Status: "400 Bad Request"}, false},
		{"untyped 400, FAILED_PRECONDITION", errors.New("Error 400, Message: Request contains an invalid argument., Status: FAILED_PRECONDITION, Details: []"), false},
		{"typed 500 INVALID_ARGUMENT", genai.APIError{Code: 500, Message: typedBare.Message, Status: "INVALID_ARGUMENT"}, false},

		// Not a provider rejection at all.
		{"model prose quoting the message", errors.New("the API said: Request contains an invalid argument."), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsBareInvalidArgument(tc.err); got != tc.want {
				t.Errorf("IsBareInvalidArgument(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// The predicate is never a transient one on its own: IsTransient must
// keep refusing the bare 400, or it would be retried on a session's
// very first call.
func TestIsTransient_StillRefusesTheBare400(t *testing.T) {
	if IsTransient(errors.New(recorded1247)) {
		t.Error("IsTransient accepted the bare 400 — it would be retried with no prior success")
	}
}

func servedCtx() context.Context {
	rec := models.NewPriorSuccess()
	rec.Mark()
	return models.WithPriorSuccess(context.Background(), rec)
}

// a2RetryRE is dev/uat/gke-drill/a2_count.py's RETRY_RE. Box A2 counts
// a retry by this line; a bare-400 retry that it missed would be a
// transcript-only retry and fail the comparison.
var a2RetryRE = regexp.MustCompile(`transient provider error \(.*?\) (— retrying once after|NOT retried)`)

// The #1247 turn, driven through the real composed stack: the session
// has been served, Vertex answers the next call with the bare 400, the
// retry recovers, and the recovery is stamped like any other.
func TestGenerateContent_RetriesBare400AfterSuccess(t *testing.T) {
	useFastRetryPolicy(t)
	lines := captureLogf(t)
	inner := &scriptedLLM{script: [][]fakeEvent{
		{{nil, errors.New(recorded1247)}},
		{{modelText("the answer"), nil}},
	}}

	var stamped bool
	req := &adkmodel.LLMRequest{
		Contents: []*genai.Content{{Role: genai.RoleUser, Parts: []*genai.Part{{Text: "hi"}}}},
		Config:   &genai.GenerateContentConfig{},
	}
	for resp, err := range (retrying{inner: inner}).GenerateContent(servedCtx(), req, false) {
		if err != nil {
			t.Fatalf("caller saw %v, want the retry to recover", err)
		}
		if _, ok := resp.CustomMetadata[models.ProviderRetryMetadataKey]; ok {
			stamped = true
		}
	}

	if inner.calls != 2 {
		t.Errorf("inner calls = %d, want 2", inner.calls)
	}
	if !stamped {
		t.Error("the recovered response carries no provider_retry stamp")
	}
	var counted bool
	for _, l := range *lines {
		if a2RetryRE.MatchString(l) && strings.Contains(l, "INVALID_ARGUMENT") {
			counted = true
		}
	}
	if !counted {
		t.Errorf("no log line a2_count's RETRY_RE counts: %v", *lines)
	}
}

func TestGenerateContent_Bare400PersistingIsARetryError(t *testing.T) {
	useFastRetryPolicy(t)
	inner := &scriptedLLM{script: [][]fakeEvent{
		{{nil, errors.New(recorded1247)}},
		{{nil, errors.New(recorded1247)}},
		{{modelText("unreached"), nil}},
	}}

	_, errs := drainLLMCtx(servedCtx(), retrying{inner: inner})

	if inner.calls != 2 {
		t.Errorf("inner calls = %d, want exactly 2", inner.calls)
	}
	var re *models.RetryError
	if len(errs) != 1 || !errors.As(errs[0], &re) || re.Outcome != "persisted" {
		t.Fatalf("errs = %v, want one RetryError{persisted}", errs)
	}
	if want := "provider retry persisted: " + recorded1247; errs[0].Error() != want {
		t.Errorf("error = %q, want %q", errs[0], want)
	}
}

// Every way the bare 400 must surface exactly as it did before #1247:
// one call, the provider's error untouched.
func TestGenerateContent_400NotRetriedOutsideThePolicy(t *testing.T) {
	unmarked := models.WithPriorSuccess(context.Background(), models.NewPriorSuccess())
	for _, tc := range []struct {
		name string
		ctx  context.Context
		err  error
	}{
		{"bare 400, no record (the session's first call)", context.Background(), errors.New(recorded1247)},
		{"bare 400, record not yet marked", unmarked, errors.New(recorded1247)},
		{"bare 400, side call in a served session", models.AsSideCall(servedCtx(), "summarizer"), errors.New(recorded1247)},
		{"400 with details, served", servedCtx(), genai.APIError{Code: 400, Message: "Request contains an invalid argument.", Status: "INVALID_ARGUMENT",
			Details: []map[string]any{{"@type": "type.googleapis.com/google.rpc.BadRequest"}}}},
		{"400 naming a parameter, served", servedCtx(), errors.New("Error 400, Message: Unable to submit request because it has an empty text parameter., Status: INVALID_ARGUMENT, Details: []")},
		{"400 FAILED_PRECONDITION, served", servedCtx(), genai.APIError{Code: 400, Message: "Request contains an invalid argument.", Status: "FAILED_PRECONDITION"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			useFastRetryPolicy(t)
			inner := &scriptedLLM{script: [][]fakeEvent{
				{{nil, tc.err}},
				{{modelText("unreached"), nil}},
			}}

			_, errs := drainLLMCtx(tc.ctx, retrying{inner: inner})

			if inner.calls != 1 {
				t.Errorf("inner calls = %d, want 1", inner.calls)
			}
			// Compared as text: a genai.APIError with Details is not
			// comparable, so errors.Is cannot match it.
			if len(errs) != 1 || errs[0].Error() != tc.err.Error() {
				t.Fatalf("errs = %v, want the 400 itself", errs)
			}
			var re *models.RetryError
			if errors.As(errs[0], &re) {
				t.Errorf("error %q claims a retry that never fired", errs[0])
			}
		})
	}
}
