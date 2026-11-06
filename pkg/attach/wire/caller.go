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

package wire

import "context"

// DirectCaller returns the identity the attach server verified from the
// request's own credential, or "" when it verified none (an anonymous
// request carries a placeholder identity, not a verified one) or when a
// proxy asserted the identity on someone's behalf (X-Asserted-Caller).
//
// It is how an inject tells an operator's own message from one a relay
// sent under the operator's name: k8s-lookout and chat gateways inject
// that way, and their text is relayed event or chat content, not the
// operator's instruction (#1175 decision 6). The agent records it on
// the queued message and checks it against permissions.auto.task_from.
//
// The auth source /whoami reports is not enough to decide this. A
// listener's transport bearer token or a verified client certificate
// upgrades the source, but the identity is then the configured
// default (multi_session.default_identity, or "anon"), which every
// holder of that token shares — the operator's TUI and a watcher
// alike. Only an identity the per-caller authenticator derived from
// the credential is one caller's.
func DirectCaller(ctx context.Context) string {
	id, _ := ctx.Value(directCallerCtxKey{}).(string)
	return id
}

// WithDirectCaller stamps DirectCaller's answer onto ctx. Only the
// attach server's caller middleware writes it; it is exported because
// the key lives here and the writer lives in pkg/attach.
func WithDirectCaller(ctx context.Context, identity string) context.Context {
	return context.WithValue(ctx, directCallerCtxKey{}, identity)
}

// directCallerCtxKey carries DirectCaller's answer, which only
// WithDirectCaller writes.
type directCallerCtxKey struct{}

// InjectTask returns the part of an injected message its client says
// the operator wrote (InjectRequest.TaskBytes), and whether the client
// said anything. Without it, the whole message is the operator's. Like
// DirectCaller, only the inject handler sets it; the agent reads it
// when it queues the message (#1230).
func InjectTask(ctx context.Context) (string, bool) {
	t, ok := ctx.Value(injectTaskCtxKey{}).(string)
	return t, ok
}

// WithInjectTask stamps InjectTask's answer onto ctx. Only the attach
// server's inject handler writes it.
func WithInjectTask(ctx context.Context, task string) context.Context {
	return context.WithValue(ctx, injectTaskCtxKey{}, task)
}

type injectTaskCtxKey struct{}
