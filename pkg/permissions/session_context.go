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
)

// WithSessionGate stores the per-session sub-gate on ctx so
// downstream tool wrappers can opt into routing permission checks
// through THIS session's gate (and therefore THIS session's
// prompter) instead of the gate captured at tool-construction time.
//
// The multi-session daemon needs this because MCP toolsets +
// built-in tool registries are constructed once at startup (the
// template gate). Without a session-aware override, every tool
// call's permission prompt would land on the daemon's
// startup-time PromptBroker — which isn't subscribed to by the
// per-session attach client. Result: alice's tool prompts go
// nowhere; the tool hangs forever waiting for an approval that
// never arrives.
//
// agent.Run wraps runCtx with WithSessionGate(a.gate) so any tool
// wrapper that propagates ctx through to its gate check (notably
// pkg/tools.gatedTool used by both built-in tools and MCP toolsets
// via GateToolset) sees the per-session sub-gate via
// SessionGateFromContext and prefers it.
//
// Nil gate is a no-op (returns the input ctx unchanged) so call
// sites don't need a guard.
func WithSessionGate(ctx context.Context, g *Gate) context.Context {
	if g == nil {
		return ctx
	}
	return context.WithValue(ctx, sessionGateKey{}, g)
}

// SessionGateFromContext returns the per-session sub-gate previously
// stamped on ctx by WithSessionGate. ok is false (and the returned
// gate is nil) when no session gate is on the context — typically
// the single-user / pre-multi-session code paths. Callers fall back
// to their constructor-time gate in that case.
func SessionGateFromContext(ctx context.Context) (g *Gate, ok bool) {
	if ctx == nil {
		// Tests sometimes pass a nil tool.Context that fronts a nil
		// context.Context. ctx.Value(...) on a nil context panics —
		// guard so the gate methods stay safe to call from those
		// paths (where there's no session gate to find anyway).
		return nil, false
	}
	g, ok = ctx.Value(sessionGateKey{}).(*Gate)
	if !ok || g == nil {
		return nil, false
	}
	return g, true
}

type sessionGateKey struct{}
