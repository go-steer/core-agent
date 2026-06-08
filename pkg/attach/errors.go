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
	"regexp"
	"strings"
)

// ClassifyTurnError maps a raw error from the model-call path to a
// TurnError payload conforming to the SSE event-stream protocol's
// kind enum (spec section 2.6).
//
// Classification is string-based rather than type-based because the
// genai / ADK / Vertex / Anthropic clients each wrap upstream
// errors differently and changing wrapper layers would silently
// regress a type-switch. String matching against the canonical
// status names (gRPC code names, HTTP status numbers, well-known
// substrings) survives wrapper churn.
//
// The hint field is populated with the most actionable next step
// when one is obvious — operators reading these in a chat-bubble
// shouldn't need to leave the TUI to know what to try.
func ClassifyTurnError(err error) TurnError {
	if err == nil {
		return TurnError{Kind: TurnErrorUnknown, Message: "nil error", Retryable: false}
	}

	// Context errors come through unwrapped from cancellation /
	// deadline plumbing; check before string matching since
	// errors.Is is more reliable than substring scan for these.
	if errors.Is(err, context.DeadlineExceeded) {
		return TurnError{
			Kind:      TurnErrorTransientNet,
			Code:      "DEADLINE_EXCEEDED",
			Message:   "model call timed out",
			Retryable: true,
		}
	}
	if errors.Is(err, context.Canceled) {
		return TurnError{
			Kind:      TurnErrorTransientNet,
			Code:      "CANCELED",
			Message:   "model call canceled",
			Retryable: true,
		}
	}

	msg := err.Error()
	lower := strings.ToLower(msg)
	code := extractStatusCode(msg)

	switch {
	// NotFound — typically a model name / location mismatch
	// (e.g. global-only model requested at a regional endpoint).
	case containsAny(lower, "not_found", "not found") || code == "404":
		return TurnError{
			Kind:      TurnErrorModelNotFound,
			Code:      coalesce(code, "NOT_FOUND"),
			Message:   firstSentence(msg),
			Retryable: false,
			Hint:      "Check the model name and vertex.location (some models are global-only).",
		}

	// Auth — IAM / credentials / OAuth failures. Match both the
	// underscored ("permission_denied") and CamelCase-as-one-word
	// ("permissiondenied") forms — gRPC error strings emit the
	// latter after the lowercase pass.
	case containsAny(lower, "permission_denied", "permissiondenied", "unauthenticated",
		"permission denied", "unauthorized", "invalid credentials",
		"could not find default credentials", "forbidden") || code == "401" || code == "403":
		return TurnError{
			Kind:      TurnErrorAuth,
			Code:      coalesce(code, "PERMISSION_DENIED"),
			Message:   firstSentence(msg),
			Retryable: false,
			Hint:      "Verify the runtime service account has roles/aiplatform.user (or the provider-equivalent role) and that GOOGLE_APPLICATION_CREDENTIALS / ADC is set.",
		}

	// Rate-limit / quota — retryable with backoff.
	case containsAny(lower, "resource_exhausted", "resourceexhausted", "rate exceeded",
		"rate limit", "quota exceeded", "too many requests") || code == "429":
		return TurnError{
			Kind:      TurnErrorRateLimited,
			Code:      coalesce(code, "RESOURCE_EXHAUSTED"),
			Message:   firstSentence(msg),
			Retryable: true,
		}

	// Transient network — usually retryable.
	case containsAny(lower, "deadline_exceeded", "deadlineexceeded", "unavailable",
		"connection refused", "connection reset", "no such host",
		"temporary failure", "i/o timeout") ||
		code == "503" || code == "504" || code == "502":
		return TurnError{
			Kind:      TurnErrorTransientNet,
			Code:      coalesce(code, "UNAVAILABLE"),
			Message:   firstSentence(msg),
			Retryable: true,
		}

	// Config — URL parse failures, missing required values, malformed
	// inputs caught client-side before any RPC fires. These don't
	// retry on their own.
	case containsAny(lower, "invalid_argument", "invalidargument", "failed_precondition",
		"failedprecondition", "invalid character", "parse", "createapiurl") || code == "400":
		return TurnError{
			Kind:      TurnErrorConfig,
			Code:      coalesce(code, "INVALID_ARGUMENT"),
			Message:   firstSentence(msg),
			Retryable: false,
			Hint:      "Check the model provider config (model.vertex.location, model.name, GOOGLE_CLOUD_PROJECT, GOOGLE_CLOUD_LOCATION).",
		}
	}

	// Unknown — preserve the message so operators can still see what
	// happened, even though we couldn't categorize it.
	return TurnError{
		Kind:      TurnErrorUnknown,
		Code:      code,
		Message:   firstSentence(msg),
		Retryable: false,
	}
}

// containsAny returns true if s contains any of needles.
func containsAny(s string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}

// coalesce returns the first non-empty argument.
func coalesce(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// statusCodeRE matches HTTP status numbers in common error message
// formats: "Error 404,", "status: 404", "404 Not Found", etc. The
// extractStatusCode below uses this to pull a code even when the
// upstream library doesn't expose a structured status.
var statusCodeRE = regexp.MustCompile(`\b(?:Error |status[: ]+|code[: ]+|HTTP )(\d{3})\b`)

// extractStatusCode pulls an HTTP-style status number out of an
// error message if one is present. Returns "" if not found.
func extractStatusCode(msg string) string {
	m := statusCodeRE.FindStringSubmatch(msg)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

// firstSentence trims the message to a single user-readable line,
// capped at a length that fits comfortably in a chat-bubble render.
// Multi-line errors from upstream APIs often include a stack trace
// or URL dump; surfacing the whole block in the TUI's chat window
// crowds out everything else.
func firstSentence(msg string) string {
	if i := strings.IndexAny(msg, "\n"); i >= 0 {
		msg = msg[:i]
	}
	msg = strings.TrimSpace(msg)
	const cap = 240
	if len(msg) > cap {
		msg = msg[:cap-3] + "..."
	}
	return msg
}
