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

package models

import (
	"context"

	"github.com/go-steer/core-models/callctx"
)

// PriorSuccess records that a model call in one agent session has
// already succeeded (#1247). It is the evidence that licenses
// RetryPolicy.IsTransientAfterSuccess: a rejection that is ambiguous on
// its own — Vertex's bare 400 INVALID_ARGUMENT with no details — is
// worth one retry only when the same session, model and configuration
// have already been served, because then the request cannot be
// malformed from the start.
//
// The agent owns one per session and marks it from its event loop when
// a model response arrives; the retry policy only reads it. The record
// travels on the call's context (WithPriorSuccess), the same way
// AsSideCall does. A nil *PriorSuccess is valid and never succeeded.
//
// It is core-models' callctx.PriorSuccess, so the library's HTTP-layer
// retry and this package's RetryPolicy read the same record.
type PriorSuccess = callctx.PriorSuccess

// NewPriorSuccess returns an unmarked record.
func NewPriorSuccess() *PriorSuccess { return callctx.NewPriorSuccess() }

// WithPriorSuccess puts rec on ctx for the model calls made under it.
//
// A nil rec SHADOWS any record ctx already carries. That is the form a
// nested run uses — a subtask or a sync subagent runs its own model,
// instruction and tools on a context derived from the parent's tool
// call, and the parent's success says nothing about whether the child's
// request is well formed.
func WithPriorSuccess(ctx context.Context, rec *PriorSuccess) context.Context {
	return callctx.WithPriorSuccess(ctx, rec)
}

// PriorSuccessFrom returns the record WithPriorSuccess put on ctx, or
// nil.
func PriorSuccessFrom(ctx context.Context) *PriorSuccess { return callctx.PriorSuccessFrom(ctx) }

// priorCallSucceeded reports whether a call under ctx may be judged by
// IsTransientAfterSuccess: its session has had a model call succeed,
// and it is not a side call. A side call — the approver, a title, the
// summarizer — sends its own instruction and no tools, so the session's
// success is no evidence about its request.
func priorCallSucceeded(ctx context.Context) bool { return callctx.PriorCallSucceeded(ctx) }
