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

package mcp

import (
	"context"
	"iter"

	"google.golang.org/adk/agent"
	"google.golang.org/adk/session"
	"google.golang.org/genai"
)

// listCtx is a read-only stub satisfying agent.ReadonlyContext, used
// only to call Toolset.Tools(...) at startup so we can enumerate the
// tool names a server exposes.
//
// The mcptoolset implementation reads ctx solely as a context.Context
// for cancellation — none of the agent-specific methods are invoked
// when no ToolFilter is configured. Returning zero values is safe.
type listCtx struct{ context.Context }

func (listCtx) UserContent() *genai.Content          { return nil }
func (listCtx) InvocationID() string                 { return "" }
func (listCtx) AgentName() string                    { return "" }
func (listCtx) UserID() string                       { return "" }
func (listCtx) AppName() string                      { return "" }
func (listCtx) SessionID() string                    { return "" }
func (listCtx) Branch() string                       { return "" }
func (listCtx) ReadonlyState() session.ReadonlyState { return emptyState{} }

type emptyState struct{}

func (emptyState) Get(string) (any, error) { return nil, session.ErrStateKeyNotExist }
func (emptyState) All() iter.Seq2[string, any] {
	return func(yield func(string, any) bool) {}
}

// asReadonly wraps ctx so it can be passed where agent.ReadonlyContext
// is expected.
func asReadonly(ctx context.Context) agent.ReadonlyContext { return listCtx{Context: ctx} }
