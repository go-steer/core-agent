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

package agentengine

import (
	"context"
	"fmt"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The append retry follows the shape of the model-call retry
// (pkg/models.RetryPolicy, #935), as the design's backend notes ask: a
// quota rejection gets one more attempt after a short backoff, drawn
// from a small shared budget that refills slowly, so a project that is
// genuinely over quota is not hammered by every resident session at
// once. The timings are the model policy's defaults.
const (
	defaultAppendRetryBackoff = 2 * time.Second
	appendRetryCooldown       = 30 * time.Second
	appendRetryBurst          = 3
)

// retryable reports whether an AppendEvent failure is worth one more
// attempt. Only ResourceExhausted qualifies: it is a definite rejection
// (nothing was written, so a retry cannot duplicate an event) and it is
// transient by nature. Unavailable and DeadlineExceeded are transient
// too, but their outcome is unknown — the event may have landed — so
// they take the invalidate path instead of a blind retry.
func retryable(err error) bool {
	return status.Code(err) == codes.ResourceExhausted
}

// retryBudget is a token bucket shared by every append this Transcript
// makes: appendRetryBurst tokens, one back per appendRetryCooldown.
type retryBudget struct {
	mu     sync.Mutex
	tokens float64
	last   time.Time
}

func newRetryBudget() *retryBudget {
	return &retryBudget{tokens: appendRetryBurst, last: time.Now()}
}

// take spends a token if one is available.
func (b *retryBudget) take() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	b.tokens += now.Sub(b.last).Seconds() / appendRetryCooldown.Seconds()
	if b.tokens > appendRetryBurst {
		b.tokens = appendRetryBurst
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// AppendRetryError is how an append whose quota retry did not rescue it
// reaches the caller, mirroring models.RetryError: the outcome first,
// then the service's error, which Unwrap keeps reachable for
// status.Code and errors.Is.
type AppendRetryError struct {
	// Outcome is "persisted" (the retry was rejected too), "skipped"
	// (the shared budget was spent) or "abandoned" (ctx ended during
	// the backoff).
	Outcome string
	Err     error
}

func (e *AppendRetryError) Error() string {
	if e.Outcome == "skipped" {
		return "agentengine: append retry skipped, budget spent: " + e.Err.Error()
	}
	return fmt.Sprintf("agentengine: append retry %s: %v", e.Outcome, e.Err)
}

func (e *AppendRetryError) Unwrap() error { return e.Err }

// GRPCStatus lets status.Code see the service's code through the
// wrapper, so a caller can still tell a quota failure apart.
func (e *AppendRetryError) GRPCStatus() *status.Status {
	s, _ := status.FromError(e.Err)
	return s
}

// withAppendRetry runs call, and once more after the backoff when the
// first attempt was a quota rejection and the budget allows.
func (t *Transcript) withAppendRetry(ctx context.Context, call func() error) error {
	err := call()
	if err == nil || !retryable(err) || t.retryBackoff < 0 {
		return err
	}
	if !t.retryBudget.take() {
		return &AppendRetryError{Outcome: "skipped", Err: err}
	}
	timer := time.NewTimer(t.retryBackoff)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return &AppendRetryError{Outcome: "abandoned", Err: err}
	case <-timer.C:
	}
	if err2 := call(); err2 != nil {
		if retryable(err2) {
			return &AppendRetryError{Outcome: "persisted", Err: err2}
		}
		return err2
	}
	return nil
}
