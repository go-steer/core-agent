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

package attach

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/go-steer/core-agent/v2/pkg/permissions"
)

// PromptBroker bridges a permissions.Gate (which expects an in-process
// Prompter) with one or more remote attach subscribers. AskApproval
// generates a request_id, fans the request out to every active
// /perms/stream subscriber, then blocks until Respond delivers the
// operator's decision or ctx cancels.
//
// Headless safety: when no subscriber is attached at the moment of
// AskApproval, the request is still tracked so a subscriber that
// attaches mid-flight can drain the queue. Callers wanting fail-fast
// when no operator is around should cap ctx with a timeout — the
// gate's serializing wrapper already serializes prompts, so a hung
// AskApproval would block subsequent tool calls.
//
// One broker per daemon process. Wire via
// agent.WithAttachPromptBroker so the agent surfaces it through the
// PromptBrokerProvider capability the attach server consults.
type PromptBroker struct {
	mu      sync.Mutex
	pending map[string]*pendingPrompt
	subs    []*subscription
	closed  bool
}

// pendingPrompt is one in-flight AskApproval call. response is
// closed (or written to) when Respond delivers the operator's
// decision; ctx is the caller's context — used to drop the entry
// when the gate cancels.
type pendingPrompt struct {
	frame    PromptFrame
	response chan promptResponse
}

type promptResponse struct {
	decision permissions.Decision
	err      error
}

// subscription is one /perms/stream subscriber. The handler ranges
// over Frames; when ctx cancels (operator disconnects), the broker
// closes Frames so the handler's range loop exits.
type subscription struct {
	frames chan PromptFrame
	ctx    context.Context
}

// NewPromptBroker returns a fresh broker. Safe for concurrent use.
func NewPromptBroker() *PromptBroker {
	return &PromptBroker{pending: make(map[string]*pendingPrompt)}
}

// AskApproval implements permissions.Prompter by round-tripping the
// request through whichever subscribers are attached. Blocks until
// Respond is called or ctx cancels. Treats "no subscribers attached"
// as a queued state — the request waits until either a subscriber
// shows up or ctx expires; the gate's typical ctx is the per-tool-
// call context, so a stuck prompt fails the tool call cleanly.
func (b *PromptBroker) AskApproval(ctx context.Context, req permissions.PromptRequest) (permissions.Decision, error) {
	id := newRequestID()
	frame := PromptFrame{
		ID:          id,
		Kind:        kindToWire(req.Kind),
		ToolName:    req.ToolName,
		Detail:      req.Detail,
		Verb:        req.Verb,
		Source:      req.Source,
		PersistTool: req.PersistTool,
		PersistKey:  req.PersistKey,
		Access:      req.Access.String(),
		At:          time.Now().UTC(),
	}

	pending := &pendingPrompt{
		frame:    frame,
		response: make(chan promptResponse, 1),
	}

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return permissions.DecisionDeny, errors.New("attach: PromptBroker: closed")
	}
	b.pending[id] = pending
	subs := append([]*subscription(nil), b.subs...)
	b.mu.Unlock()

	// Best-effort fan-out. A subscriber that's already disconnected
	// (frames channel full because no goroutine is draining) is
	// skipped — the broker doesn't block on slow consumers. A
	// subscriber that subscribes AFTER this AskApproval call will
	// see the prompt via the initial-state snapshot Subscribe
	// returns.
	for _, s := range subs {
		select {
		case s.frames <- frame:
		default:
			// Slow / disconnected subscriber; the disconnect detector
			// in serveStream cleans them up.
		}
	}

	select {
	case resp := <-pending.response:
		b.mu.Lock()
		delete(b.pending, id)
		b.mu.Unlock()
		return resp.decision, resp.err
	case <-ctx.Done():
		b.mu.Lock()
		delete(b.pending, id)
		b.mu.Unlock()
		return permissions.DecisionDeny, ctx.Err()
	}
}

// Subscribe registers a /perms/stream listener. Returns a channel of
// PromptFrames + a cleanup func the caller must invoke when the
// subscription ends (typically deferred at the SSE handler). The
// returned channel is also seeded with every currently-pending
// frame so a late-attaching operator sees prompts that arrived
// before they connected.
func (b *PromptBroker) Subscribe(ctx context.Context) (<-chan PromptFrame, func()) {
	sub := &subscription{
		frames: make(chan PromptFrame, 16),
		ctx:    ctx,
	}

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		close(sub.frames)
		return sub.frames, func() {}
	}
	b.subs = append(b.subs, sub)
	// Snapshot all currently-pending prompts so the new subscriber
	// catches up on anything that arrived before they connected.
	for _, p := range b.pending {
		select {
		case sub.frames <- p.frame:
		default:
			// Buffer full at subscribe time — shouldn't happen in
			// Pattern A (gate serializes prompts), but defensively
			// drop rather than block.
		}
	}
	b.mu.Unlock()

	return sub.frames, func() { b.unsubscribe(sub) }
}

func (b *PromptBroker) unsubscribe(sub *subscription) {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := b.subs[:0]
	for _, s := range b.subs {
		if s != sub {
			out = append(out, s)
		}
	}
	b.subs = out
	close(sub.frames)
}

// Respond delivers the operator's decision to the blocked AskApproval
// call identified by id. Returns ErrPromptNotFound if the id doesn't
// match a live request (already responded, already cancelled, or
// never existed).
func (b *PromptBroker) Respond(id string, decision permissions.Decision) error {
	b.mu.Lock()
	pending, ok := b.pending[id]
	b.mu.Unlock()
	if !ok {
		return ErrPromptNotFound
	}
	select {
	case pending.response <- promptResponse{decision: decision}:
		return nil
	default:
		// AskApproval already drained the channel (concurrent
		// Respond race). Treat as not-found so the second caller
		// learns their decision was redundant.
		return ErrPromptNotFound
	}
}

// Pending returns a snapshot of currently-pending prompts. Useful
// for tests and for clients that want a poll-style fallback if SSE
// isn't available.
func (b *PromptBroker) Pending() []PromptFrame {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]PromptFrame, 0, len(b.pending))
	for _, p := range b.pending {
		out = append(out, p.frame)
	}
	return out
}

// Close unblocks every pending AskApproval with a closed-broker
// error and disconnects every active subscriber. Idempotent.
func (b *PromptBroker) Close() {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	pending := b.pending
	b.pending = make(map[string]*pendingPrompt)
	subs := b.subs
	b.subs = nil
	b.mu.Unlock()

	for _, p := range pending {
		select {
		case p.response <- promptResponse{decision: permissions.DecisionDeny, err: errors.New("attach: PromptBroker: closed")}:
		default:
		}
	}
	for _, s := range subs {
		close(s.frames)
	}
}

// ErrPromptNotFound is returned by Respond when the id doesn't match
// a live pending prompt.
var ErrPromptNotFound = errors.New("attach: prompt id not found (already responded, cancelled, or never issued)")

// PromptBrokerProvider is the optional capability for routes under
// /sessions/<sid>/perms/stream + /perms/respond. Agents that opted
// into prompt routing surface their broker via this interface; the
// attach server returns 501 / capability-not-registered otherwise.
type PromptBrokerProvider interface {
	AttachPromptBroker() *PromptBroker
}

// kindToWire maps the in-process PromptKind enum to its wire string.
// String form keeps the JSON stable across changes to the enum's
// underlying int values.
func kindToWire(k permissions.PromptKind) string {
	switch k {
	case permissions.PromptKindBash:
		return "bash"
	case permissions.PromptKindFileWrite:
		return "file_write"
	case permissions.PromptKindPathScope:
		return "path_scope"
	default:
		return "generic"
	}
}

// DecisionFromWire maps the wire-format decision string back to the
// permissions.Decision enum. Returns false if the string isn't one
// of the documented values.
func DecisionFromWire(s string) (permissions.Decision, bool) {
	switch s {
	case "deny":
		return permissions.DecisionDeny, true
	case "allow-once":
		return permissions.DecisionAllowOnce, true
	case "allow-session":
		return permissions.DecisionAllowSession, true
	case "allow-session-verb":
		return permissions.DecisionAllowSessionVerb, true
	case "allow-session-tool":
		return permissions.DecisionAllowSessionTool, true
	case "allow-always":
		return permissions.DecisionAllowAlways, true
	}
	return permissions.DecisionDeny, false
}

func newRequestID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand really shouldn't fail; if it does, fall back to
		// a time-based id so we don't panic mid-prompt.
		return fmt.Sprintf("ts-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
