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

package permissions

import (
	"context"
	"sync"
)

// Serialize returns a Prompter that wraps inner with a mutex so
// concurrent AskApproval calls run one at a time. Necessary when the
// gate is shared across multiple goroutines that might prompt the
// same underlying medium (e.g. several background subagents racing
// for os.Stdin via the inherited StdinPrompter).
//
// Cancellation: a blocked caller's ctx is honored — once the mutex
// is acquired, the underlying inner.AskApproval sees the original
// ctx and can fail fast on ctx.Done(). Callers waiting in line just
// block on the mutex until their turn or ctx error during the
// inner call.
//
// Passing nil returns nil so callers can chain Serialize(nil)
// without a guard.
func Serialize(inner Prompter) Prompter {
	if inner == nil {
		return nil
	}
	return &serializingPrompter{inner: inner}
}

type serializingPrompter struct {
	mu    sync.Mutex
	inner Prompter
}

func (p *serializingPrompter) AskApproval(ctx context.Context, req PromptRequest) (Decision, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.inner.AskApproval(ctx, req)
}
