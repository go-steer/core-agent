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

import "context"

// Grant is one "allow always" decision the gate resolved and wants
// persisted beyond the current session.
type Grant struct {
	// Kind mirrors the PromptRequest.Kind that produced the grant.
	Kind PromptKind

	// Tool and Key are the persistence coordinates carried on the
	// PromptRequest (PersistTool / PersistKey).
	Tool string
	Key  string

	// Pattern is the fully-expanded entry the gate installed
	// in-memory: the "<Tool>:<Key>" policy pattern, or the
	// subtree-expanded path pattern from expandAlwaysAllowPattern.
	// Persist this verbatim so a restart reloads the identical grant.
	Pattern string

	// Access is the resolved file access for PromptKindPathScope
	// grants, after the read→r / write→rw promotion the gate applies
	// (see the DecisionAllowAlways branch). Zero (AccessNone) for
	// non-path grants.
	Access Access
}

// GrantStore persists "allow always" grants so they survive process
// restart. The gate calls Persist from its DecisionAllowAlways path,
// immediately after installing the grant in-memory.
//
// Persist must be idempotent (re-granting an existing pattern is a
// no-op) and safe for concurrent use. A nil GrantStore disables
// persistence — the grant still applies for the current session.
//
// The bundled reference implementation writes grants into
// .agents/config.json; library consumers can back this with whatever
// their deployment persists config in. Persist errors surface to the
// caller of the gated tool (the operator asked for a durable grant
// and it failed — silently downgrading to session-only would violate
// the DecisionAllowAlways contract).
type GrantStore interface {
	Persist(ctx context.Context, g Grant) error
}
