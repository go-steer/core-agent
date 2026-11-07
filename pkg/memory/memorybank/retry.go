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

package memorybank

import (
	"context"
	"fmt"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	defaultRetryBackoff = 2 * time.Second
	retryCooldown       = 30 * time.Second
	retryBurst          = 3
)

func isTransient(err error) bool {
	switch status.Code(err) {
	case codes.ResourceExhausted, codes.Unavailable:
		return true
	default:
		return false
	}
}

type retryBudget struct {
	mu     sync.Mutex
	tokens float64
	last   time.Time
}

func newRetryBudget() *retryBudget {
	return &retryBudget{tokens: retryBurst, last: time.Now()}
}

func (b *retryBudget) take() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	b.tokens += now.Sub(b.last).Seconds() / retryCooldown.Seconds()
	if b.tokens > retryBurst {
		b.tokens = retryBurst
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// RetryError wraps a transient Memory Bank RPC error after a retry attempt was
// rejected, skipped due to an exhausted token budget, or abandoned when the
// caller's context expired during backoff.
type RetryError struct {
	Outcome string
	Err     error
}

func (e *RetryError) Error() string {
	if e.Outcome == "skipped" {
		return "memorybank: retry skipped, budget spent: " + e.Err.Error()
	}
	return fmt.Sprintf("memorybank: retry %s: %v", e.Outcome, e.Err)
}

func (e *RetryError) Unwrap() error { return e.Err }

// GRPCStatus preserves status.Code inspection through RetryError.
func (e *RetryError) GRPCStatus() *status.Status {
	s, _ := status.FromError(e.Err)
	return s
}

func (b *Backend) withRetry(ctx context.Context, call func() error) error {
	err := call()
	if err == nil || !isTransient(err) || b.retryBackoff < 0 {
		return err
	}
	if !b.retryBudget.take() {
		return &RetryError{Outcome: "skipped", Err: err}
	}
	timer := time.NewTimer(b.retryBackoff)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return &RetryError{Outcome: "abandoned", Err: err}
	case <-timer.C:
	}
	if err2 := call(); err2 != nil {
		if isTransient(err2) {
			return &RetryError{Outcome: "persisted", Err: err2}
		}
		return err2
	}
	return nil
}
