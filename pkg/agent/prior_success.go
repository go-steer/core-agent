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

package agent

import (
	"google.golang.org/adk/session"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v2/pkg/models"
)

// The record of a served model call (#1247,
// docs/retry-bare-invalid-argument-design.md).
//
// The provider's retry policy may retry Vertex's bare 400
// INVALID_ARGUMENT once, but only on a run whose requests have already
// been accepted, because a request malformed from its first call gets
// the same answer. The policy is one process-wide instance with no
// session, so the evidence rides the call's context as a
// models.PriorSuccess, and this package — which knows which run a call
// belongs to — is what marks it.
//
// Three runs carry a record, each its own:
//
//   - Agent.Run: the Agent's, created at New and kept for the session's
//     life. An Agent's model, persona and tool registrations are fixed
//     at New, so one accepted request is evidence about the config of
//     every later one. (Not about every byte: the history grows, and
//     context caching swaps the instruction for a cache reference after
//     the first turn. The design doc states that cost.)
//   - A sync subagent delegation and a RunSubtask: a fresh record per
//     delegation. Each runs its own instruction and tools on a context
//     derived from the parent's tool call, so the parent's record must
//     not reach it; and a sync subagent's inner agent is shared by every
//     tenant of a daemon (#741), so nothing session-shaped may live on
//     it. A delegation of several model calls still gets the retry from
//     its second call on. (Both in-tree RunSubtask callers mark their
//     context as a side call, which never qualifies.)
//
// RunWithContents shadows any inherited record with nil: it creates its
// session on the spot, so nothing precedes its first call.
//
// Marked from the event stream, never from inside the provider, so it
// holds for every provider and no side call can mark it.

// markIfServed marks rec when ev is a model response that arrived
// whole: not partial, no error, model role, at least one part — text, a
// function call, anything. Nil-safe in rec.
func markIfServed(rec *models.PriorSuccess, ev *session.Event, err error) {
	if err != nil || ev == nil || ev.Partial || ev.ErrorCode != "" {
		return
	}
	if ev.Content == nil || ev.Content.Role != genai.RoleModel || len(ev.Content.Parts) == 0 {
		return
	}
	rec.Mark()
}
