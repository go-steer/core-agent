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

package attach

import (
	"context"
	"errors"
	"strings"
	"testing"

	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v2/pkg/models"
)

// #1206. A retry that did not rescue its call reaches the turn-error
// frame as a models.RetryError, whose text leads with the outcome. The
// classifier keys on substrings of that text, so the prefix must change
// nothing it decides — kind, code, retryable — and must survive the
// frame's first-sentence trim, which is the whole reason it is a prefix.
func TestClassifyTurnError_ARetryPrefixChangesNoClassification(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", 400)
	rejections := []error{
		genai.APIError{Code: 429, Message: "Resource exhausted. Please try again later. " + long, Status: "RESOURCE_EXHAUSTED"},
		genai.APIError{Code: 503, Message: "The model is overloaded.", Status: "UNAVAILABLE"},
		errors.New("Error 429: Rate exceeded."),
		errors.New("rpc error: code = ResourceExhausted desc = quota exceeded for tokens-per-minute"),
		errors.New("rpc error: code = Unavailable desc = connection reset"),
		// What a "failed" retry wraps: an answer that is not the rejection.
		errors.New("Error 400, Message: Request contains an invalid argument., Status: INVALID_ARGUMENT"),
		models.ErrEmptyResponse,
	}
	for _, rej := range rejections {
		want := ClassifyTurnError(rej)
		for _, outcome := range []string{"persisted", "abandoned", "skipped", "failed", "interrupted"} {
			wrapped := &models.RetryError{Outcome: outcome, Err: rej}
			got := ClassifyTurnError(wrapped)
			if got.Kind != want.Kind || got.Code != want.Code || got.Retryable != want.Retryable {
				t.Errorf("%s %q: classified %s/%s/%v, want the rejection's own %s/%s/%v",
					outcome, rej, got.Kind, got.Code, got.Retryable, want.Kind, want.Code, want.Retryable)
			}
			if !strings.HasPrefix(got.Message, "provider retry ") {
				t.Errorf("%s %q: frame message %q lost the retry prefix", outcome, rej, got.Message)
			}
		}
	}
}

// #1247. The Gemini adapter now retries a bare 400 INVALID_ARGUMENT
// once on a served session. What reaches the turn-error frame after
// that must be classified exactly as the bare 400 always was —
// config_error, code 400, not retryable — so the frame, auto-continue
// and the A2 counter see the same kind whether or not a retry ran.
func TestClassifyTurnError_Bare400IsClassifiedTheSameRetriedOrNot(t *testing.T) {
	t.Parallel()
	for _, bare := range []error{
		genai.APIError{Code: 400, Message: "Request contains an invalid argument.", Status: "INVALID_ARGUMENT"},
		errors.New("Error 400, Message: Request contains an invalid argument., Status: INVALID_ARGUMENT, Details: []"),
	} {
		plain := ClassifyTurnError(bare)
		if plain.Kind != TurnErrorConfig || plain.Code != "400" || plain.Retryable {
			t.Errorf("%v: classified %s/%s/%v, want config_error/400/false — the bare 400's classification moved",
				bare, plain.Kind, plain.Code, plain.Retryable)
		}
		for _, outcome := range []string{"persisted", "skipped", "abandoned"} {
			got := ClassifyTurnError(&models.RetryError{Outcome: outcome, Err: bare})
			if got.Kind != plain.Kind || got.Code != plain.Code || got.Retryable != plain.Retryable || got.Hint != plain.Hint {
				t.Errorf("%s %v: classified %s/%s/%v, want %s/%s/%v with the same hint",
					outcome, bare, got.Kind, got.Code, got.Retryable, plain.Kind, plain.Code, plain.Retryable)
			}
		}
	}
}

// #1206 review round 2. The context branches replace the message with
// fixed text; a retry that recovered and was then cut by a deadline
// must still lead with its outcome, or the transcript loses it.
func TestClassifyTurnError_ARetryPrefixSurvivesTheFixedMessages(t *testing.T) {
	t.Parallel()
	for _, inner := range []error{context.DeadlineExceeded, context.Canceled} {
		plain := ClassifyTurnError(inner)
		got := ClassifyTurnError(&models.RetryError{Outcome: "interrupted", Err: inner})
		if got.Kind != plain.Kind || got.Code != plain.Code || got.Retryable != plain.Retryable {
			t.Errorf("%v: classified %s/%s/%v, want %s/%s/%v", inner, got.Kind, got.Code, got.Retryable, plain.Kind, plain.Code, plain.Retryable)
		}
		if want := "provider retry interrupted after recovering: " + plain.Message; got.Message != want {
			t.Errorf("%v: message %q, want %q", inner, got.Message, want)
		}
	}
}
