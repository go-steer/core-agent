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
)

func TestClassifyTurnError_Kinds(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		err         error
		wantKind    string
		wantRetry   bool
		wantHintHas string // substring of expected hint, empty means don't check
	}{
		{
			name:        "model_not_found from Vertex 404",
			err:         errors.New(`Error 404, Message: Publisher Model "gemini-x" was not found or your project does not have access to it. Status: NOT_FOUND`),
			wantKind:    TurnErrorModelNotFound,
			wantRetry:   false,
			wantHintHas: "global-only",
		},
		{
			name:      "model_not_found from gRPC name",
			err:       errors.New("rpc error: code = NotFound desc = model not found"),
			wantKind:  TurnErrorModelNotFound,
			wantRetry: false,
		},
		{
			name:        "auth_error from permission denied",
			err:         errors.New("rpc error: code = PermissionDenied desc = caller lacks aiplatform.user"),
			wantKind:    TurnErrorAuth,
			wantRetry:   false,
			wantHintHas: "aiplatform.user",
		},
		{
			name:      "auth_error from 401",
			err:       errors.New("HTTP 401 Unauthorized — invalid credentials"),
			wantKind:  TurnErrorAuth,
			wantRetry: false,
		},
		{
			name:      "rate_limited from 429",
			err:       errors.New("Error 429: Rate exceeded."),
			wantKind:  TurnErrorRateLimited,
			wantRetry: true,
		},
		{
			name:      "rate_limited from gRPC ResourceExhausted",
			err:       errors.New("rpc error: code = ResourceExhausted desc = quota exceeded for tokens-per-minute"),
			wantKind:  TurnErrorRateLimited,
			wantRetry: true,
		},
		{
			name:      "transient_network from gRPC Unavailable",
			err:       errors.New("rpc error: code = Unavailable desc = upstream connect reset"),
			wantKind:  TurnErrorTransientNet,
			wantRetry: true,
		},
		{
			name:      "transient_network from 503",
			err:       errors.New("HTTP 503 Service Unavailable"),
			wantKind:  TurnErrorTransientNet,
			wantRetry: true,
		},
		{
			name:        "config_error from URL parse",
			err:         errors.New(`createAPIURL: error parsing base URL: parse "https://${GOOGLE_CLOUD_LOCATION}-aiplatform.googleapis.com/": invalid character "{" in host name`),
			wantKind:    TurnErrorConfig,
			wantRetry:   false,
			wantHintHas: "GOOGLE_CLOUD_LOCATION",
		},
		{
			name:      "config_error from gRPC InvalidArgument",
			err:       errors.New("rpc error: code = InvalidArgument desc = bad request"),
			wantKind:  TurnErrorConfig,
			wantRetry: false,
		},
		{
			name:      "transient_network from context deadline",
			err:       context.DeadlineExceeded,
			wantKind:  TurnErrorTransientNet,
			wantRetry: true,
		},
		{
			name:      "transient_network from context canceled",
			err:       context.Canceled,
			wantKind:  TurnErrorTransientNet,
			wantRetry: true,
		},
		{
			name:      "unknown for novel errors",
			err:       errors.New("something nobody planned for"),
			wantKind:  TurnErrorUnknown,
			wantRetry: false,
		},
		{
			name:     "unknown for nil error (defensive)",
			err:      nil,
			wantKind: TurnErrorUnknown,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := ClassifyTurnError(tc.err)
			if got.Kind != tc.wantKind {
				t.Errorf("Kind = %q, want %q (full: %+v)", got.Kind, tc.wantKind, got)
			}
			if got.Retryable != tc.wantRetry {
				t.Errorf("Retryable = %v, want %v (kind=%s)", got.Retryable, tc.wantRetry, got.Kind)
			}
			if tc.wantHintHas != "" && !strings.Contains(got.Hint, tc.wantHintHas) {
				t.Errorf("Hint = %q, want substring %q", got.Hint, tc.wantHintHas)
			}
			if got.Kind != TurnErrorUnknown && tc.err != nil && got.Message == "" {
				t.Errorf("Message should be non-empty for classified errors; got %+v", got)
			}
		})
	}
}

func TestClassifyTurnError_FirstSentenceTrim(t *testing.T) {
	t.Parallel()
	// Multi-line error message should be trimmed to first line.
	err := errors.New("line one says it all\nline two adds stack trace\nline three has another stack frame")
	got := ClassifyTurnError(err)
	if strings.Contains(got.Message, "\n") {
		t.Errorf("Message should be single line; got %q", got.Message)
	}
	if !strings.HasPrefix(got.Message, "line one") {
		t.Errorf("Message should start with first line; got %q", got.Message)
	}
}

func TestClassifyTurnError_LongMessageCapped(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", 1000)
	got := ClassifyTurnError(errors.New(long))
	if len(got.Message) > 240 {
		t.Errorf("Message length = %d, want <= 240 (was capped)", len(got.Message))
	}
	if !strings.HasSuffix(got.Message, "...") {
		t.Errorf("Capped message should end with ellipsis; got %q", got.Message)
	}
}
