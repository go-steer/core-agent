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
	"fmt"
	"slices"
	"strconv"
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
			name:        "config_error from FAILED_PRECONDITION",
			err:         errors.New("Error 400, Message: Vertex AI API has not been used in project 12345 before or it is disabled., Status: FAILED_PRECONDITION"),
			wantKind:    TurnErrorConfig,
			wantRetry:   false,
			wantHintHas: "GOOGLE_CLOUD_PROJECT",
		},
		{
			name:      "transient_network from context deadline",
			err:       context.DeadlineExceeded,
			wantKind:  TurnErrorTransientNet,
			wantRetry: true,
		},
		{
			name:      "canceled from context canceled",
			err:       context.Canceled,
			wantKind:  TurnErrorCanceled,
			wantRetry: false,
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

// TestClassifyTurnError_BareInvalidArgumentHintNamesBothReadings is the
// #898 regression pin.
//
// A bare 400 INVALID_ARGUMENT is the most overloaded answer Vertex
// gives. During the #799 UAT one arrived in a window where the same
// session was also getting 429s, and the hint told the operator to go
// check model.vertex.location — a config that was correct, and that
// two probes on the same session and transcript proved correct
// seconds later. The classification is defensible; the hint asserting
// which of the two readings applies was not.
//
// Structural failures in the same arm keep the unhedged hint: a URL
// that will not parse is wrong on every attempt, and telling that
// operator the provider might just be busy would be the same defect
// pointed the other way.
func TestClassifyTurnError_BareInvalidArgumentHintNamesBothReadings(t *testing.T) {
	t.Parallel()

	// Verbatim from the #898 report, not a paraphrase — a hint keyed
	// on a remembered shape is how #902 got written twice.
	observed := errors.New("Error 400, Message: Request contains an invalid argument., Status: INVALID_ARGUMENT, Details: []")
	got := ClassifyTurnError(observed)

	if got.Kind != TurnErrorConfig {
		t.Errorf("Kind = %q, want %q — the classification is unchanged by this fix", got.Kind, TurnErrorConfig)
	}
	if got.Retryable {
		t.Error("Retryable = true; the runtime does not retry this yet (#935) and must not claim it does")
	}
	for _, want := range []string{"ambiguous", "transiently", "reproduces on every attempt"} {
		if !strings.Contains(got.Hint, want) {
			t.Errorf("Hint = %q, want substring %q — it must name the transient reading too", got.Hint, want)
		}
	}
	if !strings.Contains(got.Hint, "model.vertex.location") {
		t.Errorf("Hint = %q, still needs to name the config to check", got.Hint)
	}

	// Structural failures keep the flat "your config is wrong" hint,
	// including the ones that also carry a 400 and so match both arms.
	// Ordering is what decides those, so pin one.
	for _, structural := range []error{
		errors.New(`createAPIURL: error parsing base URL: parse "https://${GOOGLE_CLOUD_LOCATION}-aiplatform.googleapis.com/": invalid character "{" in host name`),
		errors.New("Error 400, Message: Vertex AI API has not been used in project 12345 before or it is disabled., Status: FAILED_PRECONDITION"),
		errors.New(`Error 400: could not parse request body, Status: INVALID_ARGUMENT`),
	} {
		h := ClassifyTurnError(structural).Hint
		if strings.Contains(h, "ambiguous") {
			t.Errorf("Hint = %q for %v; a structural failure is not ambiguous and hedging it is noise", h, structural)
		}
		if !strings.Contains(h, "model.vertex.location") {
			t.Errorf("Hint = %q for %v, want the provider-config hint", h, structural)
		}
	}
}

// TestClassifyTurnError_CanceledIsNotRetryable is the #816 regression
// pin. A cancelled turn is a deliberate stop — an operator interrupt,
// a shutdown, a guardrail cutting the turn short — and `retryable` is
// the one decision the protocol asks a client to make off this
// payload, so answering "yes, try again" is the wrong answer in the
// case where trying again undoes what was asked for. The same value
// rides `error.type` on gen_ai.agent.invocation.duration, where it
// used to inflate the transient-network rate with deliberate stops.
//
// The wrapped case is covered because a cancel does not necessarily
// arrive bare — pkg/runner/repl.go and pkg/agent/autonomous both
// errors.Is-check the error a turn hands back rather than comparing
// it, which is the in-tree evidence that a wrapper can sit in front
// of it. A classifier keyed on == would miss those. Fails on pre-fix
// code, which returned transient_network / retryable=true for both.
// selfKindedErr is a stand-in for the guardrail refusals in pkg/agent,
// which this package cannot import (they import it).
type selfKindedErr struct{ msg string }

func (e *selfKindedErr) Error() string { return e.msg }
func (e *selfKindedErr) AsTurnError() TurnError {
	return TurnError{Kind: TurnErrorCostCeiling, Code: "cost_ceiling", Message: e.msg, Retryable: false}
}

// TestClassifyTurnError_SelfClassifyingWins pins the precedence (#818).
// The declared kind must beat both the substring heuristics and the
// context checks, or an error that knows its own answer still gets
// guessed at.
func TestClassifyTurnError_SelfClassifyingWins(t *testing.T) {
	t.Parallel()

	// Prose that the heuristics WOULD classify as transient_network /
	// retryable. Guardrail reasons carry arbitrary operator- and
	// trigger-supplied text, so this collision is not hypothetical.
	trap := &selfKindedErr{msg: "halted: upstream unavailable, connection reset while looping"}
	if got := ClassifyTurnError(trap); got.Kind != TurnErrorCostCeiling || got.Retryable {
		t.Errorf("ClassifyTurnError = %+v, want kind=%s retryable=false — the substring scan overrode a declared kind",
			got, TurnErrorCostCeiling)
	}

	// Wrapped, and wrapped around a context error whose branch runs
	// before the string switch.
	wrapped := fmt.Errorf("agent: run turn: %w", trap)
	if got := ClassifyTurnError(wrapped); got.Kind != TurnErrorCostCeiling {
		t.Errorf("wrapped: Kind = %q, want %q", got.Kind, TurnErrorCostCeiling)
	}

	// The interface must not swallow ordinary errors: anything that
	// doesn't implement it still goes through the heuristics.
	if got := ClassifyTurnError(errors.New("503 unavailable")); got.Kind != TurnErrorTransientNet {
		t.Errorf("plain error: Kind = %q, want %q — the new branch must not capture non-implementers",
			got.Kind, TurnErrorTransientNet)
	}
}

func TestClassifyTurnError_CanceledIsNotRetryable(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
	}{
		{name: "bare", err: context.Canceled},
		{name: "wrapped", err: fmt.Errorf("agent: run turn: %w", context.Canceled)},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := ClassifyTurnError(tc.err)
			if got.Kind != TurnErrorCanceled {
				t.Errorf("Kind = %q, want %q (full: %+v)", got.Kind, TurnErrorCanceled, got)
			}
			if got.Retryable {
				t.Errorf("Retryable = true, want false — a client keying a retry prompt off this would offer to re-run work the operator stopped (full: %+v)", got)
			}
			if got.Code != "CANCELED" {
				t.Errorf("Code = %q, want %q", got.Code, "CANCELED")
			}
			// The cancel is not necessarily at the model call — the
			// turn's own context can surface it — so the message must
			// not name one.
			if strings.Contains(strings.ToLower(got.Message), "model call") {
				t.Errorf("Message = %q, should not attribute the cancel to the model call", got.Message)
			}
		})
	}

	// The neighbouring branch must NOT move: a turn that ran out of
	// time is retryable, and a fix that flipped every context error to
	// non-retryable would pass the assertions above while breaking the
	// case they were meant to leave alone.
	if got := ClassifyTurnError(context.DeadlineExceeded); got.Kind != TurnErrorTransientNet || !got.Retryable {
		t.Errorf("DeadlineExceeded = %+v, want kind=%s retryable=true", got, TurnErrorTransientNet)
	}
}

// TestProtocolVersion_CoversCanceledKind pins the version half of the
// change. A new `kind` value is not a new event type, so
// supportedEventTypes is deliberately unchanged and there is nothing on
// the capabilities frame a consumer could feature-detect — the only
// signal that a daemon may emit `canceled` is protocol_version, which
// makes forgetting the bump silent. Asserted as a floor rather than an
// equality so a later additive minor doesn't have to edit this test to
// stay green.
func TestProtocolVersion_CoversCanceledKind(t *testing.T) {
	t.Parallel()
	major, minor, ok := protocolMajorMinor(protocolVersion)
	if !ok {
		t.Fatalf("protocolVersion = %q, not parseable as major.minor", protocolVersion)
	}
	if major != 1 || minor < 8 {
		t.Errorf("protocolVersion = %q, want >= 1.8.0 (the canceled turn-error kind, #816)", protocolVersion)
	}
	if slices.Contains(supportedEventTypes, TurnErrorCanceled) {
		t.Errorf("supportedEventTypes = %v: %q is a turn-error kind, not an event type", supportedEventTypes, TurnErrorCanceled)
	}
}

// protocolMajorMinor parses "1.8.0" into (1, 8). Test-local: the
// server's own negotiation only ever compares majors (minors are
// additive by contract), so there is no production need for this.
func protocolMajorMinor(version string) (int, int, bool) {
	major, ok := protocolMajor(version)
	if !ok {
		return 0, 0, false
	}
	v := strings.TrimPrefix(strings.TrimSpace(version), "v")
	dot := strings.Index(v, ".")
	if dot < 0 {
		return major, 0, true
	}
	rest := v[dot+1:]
	if end := strings.IndexAny(rest, ".-+"); end >= 0 {
		rest = rest[:end]
	}
	minor, err := strconv.Atoi(rest)
	if err != nil || minor < 0 {
		return 0, 0, false
	}
	return major, minor, true
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
