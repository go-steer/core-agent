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

// Package wire is the attach protocol's vocabulary: the typed SSE event
// payloads and their event names, the durable eventlog rows the agent
// writes for things that happen TO a session (guardrail trips, turn
// errors, failed context reductions, audit rows), the turn-error
// classifier, and the small set of capability interfaces and context
// accessors pkg/agent implements or reads.
//
// It is the part of pkg/attach that pkg/agent EMITS, split out so the
// agent runtime can speak the protocol without linking the HTTP/SSE
// server (#1415). Everything here is plain types, constants and pure
// functions over an ADK session.Event; nothing here may import the
// server, the broadcaster, the session ACL store, gorm, or net/http
// handler types. Keeping it that way is what keeps pkg/attach's server
// — and pkg/auth and the other edges only the server needs — out of the
// slim embed graph (examples/embed/slim, dev/tools/verify-slim-deps).
//
// pkg/attach re-exports every name declared here under its original
// identifier (see pkg/attach/wire.go), so callers that already say
// attach.TurnError keep compiling unchanged. pkg/agent and
// pkg/agent/background import this package directly so that neither
// pulls the HTTP/SSE server into the slim embed graph
// (dev/tools/verify-slim-deps); dev/tools/verify-apidiff recognizes
// type-alias moves from pkg/attach.X to pkg/attach/wire.X as compatible
// when pkg/attach/wire.go re-exports X = wire.X. Packages outside the
// slim graph — pkg/attachadapter, pkg/compose — can keep saying attach.X,
// which is the same type.
package wire
