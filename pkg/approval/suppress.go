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

package approval

import (
	"context"
	"fmt"
	"sync"
)

const suppressedCallsEndTurn = 3

type refusals struct {
	mu           sync.Mutex
	byInvocation map[string]*turnRefusals
}

type turnRefusals struct {
	refused    map[string]struct{}
	suppressed int
}

func newRefusals() *refusals {
	return &refusals{byInvocation: map[string]*turnRefusals{}}
}

func (r *refusals) remember(invocationID, key string) {
	if invocationID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	t := r.byInvocation[invocationID]
	if t == nil {
		t = &turnRefusals{refused: map[string]struct{}{}}
		r.byInvocation[invocationID] = t
	}
	t.refused[key] = struct{}{}
}

func (r *refusals) suppress(invocationID, key string) (suppressed, endTurn bool, count int) {
	if invocationID == "" {
		return false, false, 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	t := r.byInvocation[invocationID]
	if t == nil {
		return false, false, 0
	}
	if _, ok := t.refused[key]; !ok {
		return false, false, 0
	}
	t.suppressed++
	return true, t.suppressed >= suppressedCallsEndTurn, t.suppressed
}

func (r *refusals) forget(invocationID string) {
	if invocationID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.byInvocation, invocationID)
}

// TurnStop ends the turn the gate is running in.
type TurnStop interface {
	StopTurn(reason error)
}

type turnStopKey struct{}

// WithTurnStop returns ctx carrying stop, for the write gate to find.
func WithTurnStop(ctx context.Context, stop TurnStop) context.Context {
	if stop == nil {
		return ctx
	}
	return context.WithValue(ctx, turnStopKey{}, stop)
}

// TurnStopFrom returns the stop on ctx, or nil if this turn cannot be
// ended from inside.
func TurnStopFrom(ctx context.Context) TurnStop {
	stop, _ := ctx.Value(turnStopKey{}).(TurnStop)
	return stop
}

// RefusalLoopError is the reason a turn ended in which the model kept
// re-proposing a call an operator had already refused.
type RefusalLoopError struct {
	Tool  string
	Key   string
	Count int
}

func (e *RefusalLoopError) Error() string {
	return fmt.Sprintf(
		"turn ended: the model re-proposed a call an operator refused %d times (tool=%s call=%q). "+
			"Nothing was executed and no approval is pending. The refusal stands; the next turn starts clean.",
		e.Count, e.Tool, e.Key)
}

// TurnErrorKind implements the attach package's turn-error classification
// contract without importing pkg/attach.
func (e *RefusalLoopError) TurnErrorKind() string { return "refusal_loop" }
