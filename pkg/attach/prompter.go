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
// attachadapter.WithPromptBroker so the agent surfaces it through the
// PromptBrokerProvider capability the attach server consults.
type PromptBroker struct {
	mu      sync.Mutex
	pending map[string]*pendingPrompt
	subs    []*subscription
	closed  bool

	// gone is a bounded tombstone ring of prompts this broker stopped
	// waiting on, so a late answer gets an accurate reason instead of
	// "not found". Each entry carries the sentinel RespondAs should
	// return for it, because "the clock ran out" and "the turn ended
	// under you" are different facts. See rememberGone.
	gone []gonePrompt

	// unwatched, when set, is called for a prompt the fan-out reached
	// nobody with. See SetUnwatchedNotifier.
	unwatched func(context.Context, UnwatchedPrompt)
}

// UnwatchedPrompt describes a prompt that opened with nobody listening,
// handed to the callback SetUnwatchedNotifier installed.
type UnwatchedPrompt struct {
	// Frame is the prompt itself, including the request ID a responder
	// needs to answer it.
	Frame PromptFrame

	// Deadline is when the prompt expires unanswered, zero when the
	// gate imposed no approval timeout and it will wait forever.
	//
	// Load-bearing for the notification's wording rather than
	// decorative: "this expires in nine minutes" and "this will block
	// the agent until somebody answers" ask a human for two different
	// responses, and the zero case is the one that actually needs
	// them, because nothing else will ever escalate it.
	Deadline time.Time
}

// SetUnwatchedNotifier installs fn as the out-of-band escalation for
// prompts that open with nobody to see them. Pass nil to remove it. One
// notifier per broker; the last call wins.
//
// fn is invoked on its own goroutine, exactly once per such prompt, with
// a context that does NOT inherit the prompt's cancellation — only a
// bound of its own (notifyTimeout). That detachment is the point rather
// than a convenience: the moment a notification is most worth sending is
// the moment the prompt is about to expire, and a send parented on the
// prompt's context would be killed by the very expiry it is reporting.
//
// The broker does not wait for fn and does not surface its error; a
// notifier that wants those seen must log them itself. A prompt must
// remain answerable while its notification is still in flight, so the
// gate is never made to depend on a webhook.
func (b *PromptBroker) SetUnwatchedNotifier(fn func(context.Context, UnwatchedPrompt)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.unwatched = fn
}

// notifyTimeout bounds one out-of-band notification. Generous next to
// the alert sender's own 10s HTTP timeout, because this budget also
// covers DNS and connection setup on a daemon that may not have talked
// to the destination since it started.
const notifyTimeout = 30 * time.Second

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
	by       string
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
	a, err := b.AskApprovalAttributed(ctx, req)
	return a.Decision, err
}

// AskApprovalAttributed implements permissions.AttributingPrompter:
// same round trip as AskApproval, but it also reports which principal
// answered, so the gate's approval log can name the human behind an
// approved write rather than the bearer token that carried it (#830).
//
// The identity comes from RespondAs, i.e. from the server's own
// caller-resolution middleware — never from the response body. See
// permissions.Approval.By for why that distinction is the whole point.
func (b *PromptBroker) AskApprovalAttributed(ctx context.Context, req permissions.PromptRequest) (permissions.Approval, error) {
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
		return permissions.Approval{Decision: permissions.DecisionDeny}, errors.New("attach: PromptBroker: closed")
	}
	b.pending[id] = pending
	unwatched := b.unwatched

	// Best-effort fan-out, and it happens UNDER b.mu rather than off a
	// snapshot taken under it.
	//
	// The snapshot version raced with unsubscribe, which closes
	// sub.frames while holding the same lock the fan-out had already
	// released — so an operator's SSE handler returning at the moment a
	// prompt opened could land a send on a closed channel and panic the
	// daemon. Rare, and rare in the worst way: the two events are an
	// operator disconnecting and the agent asking for permission, which
	// on an unattended deployment happen together by construction.
	//
	// Holding the lock is cheap here precisely because every send is
	// non-blocking: a subscriber that's already disconnected (frames
	// channel full because no goroutine is draining) is skipped rather
	// than waited on, so the critical section is bounded by the number
	// of subscribers, not by the slowest one. A subscriber that
	// subscribes AFTER this call sees the prompt via the initial-state
	// snapshot Subscribe returns.
	delivered := 0
	for _, s := range b.subs {
		select {
		case s.frames <- frame:
			delivered++
		default:
			// Slow / disconnected subscriber; the disconnect detector
			// in serveStream cleans them up.
		}
	}
	b.mu.Unlock()

	// Nobody saw it. Count DELIVERIES, not registered subscribers: a
	// subscriber whose buffer is full is one whose reader stopped
	// draining, which from the operator's side is the same silence as
	// no subscriber at all — and it is the worse case, because
	// len(b.subs) says somebody is watching. That is the shape this
	// whole path exists to break: a gate waiting on an audience that
	// is not there, indistinguishable from one waiting on a human who
	// is thinking.
	if delivered == 0 && unwatched != nil {
		info := UnwatchedPrompt{Frame: frame}
		if dl, ok := ctx.Deadline(); ok {
			info.Deadline = dl
		}
		// Detached from ctx on purpose — see SetUnwatchedNotifier.
		nctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), notifyTimeout)
		go func() {
			defer cancel()
			unwatched(nctx, info)
		}()
	}

	select {
	case resp := <-pending.response:
		b.mu.Lock()
		delete(b.pending, id)
		b.mu.Unlock()
		return permissions.Approval{Decision: resp.decision, By: resp.by}, resp.err
	case <-ctx.Done():
		b.mu.Lock()
		delete(b.pending, id)
		// Every departure through this branch is remembered, with the
		// reason (#1088). Out-of-band approval means SLOW humans:
		// somebody reads the notification, thinks about it, and posts an
		// approval at minute eleven of a ten-minute window. Answering
		// that with "unknown request id" tells them nothing about
		// whether the write happened, and whether it happened is the
		// only question they have. It did not — not for an expiry and
		// not for a cancellation — and the broker is the last thing that
		// still knows the prompt was ever here.
		//
		// The reason is not interchangeable, which is why the tombstone
		// carries it rather than the ring meaning one thing. The gate
		// marks the timeout it imposed as the context's cause (see
		// permissions.ErrPromptExpired); anything else that closes this
		// context ended the turn out from under a prompt nobody had
		// answered yet, and telling that operator the clock ran out
		// would be a guess dressed as a fact.
		b.rememberGone(id, context.Cause(ctx))
		b.mu.Unlock()
		return permissions.Approval{Decision: permissions.DecisionDeny}, ctx.Err()
	}
}

// gonePrompt is the tombstone left behind by a prompt whose wait ended
// without an answer, so a late RespondAs can be answered accurately.
// err is the sentinel RespondAs returns for this id.
type gonePrompt struct {
	id  string
	at  time.Time
	err error
}

// maxGoneRemembered caps the tombstone ring. A bound rather than a
// TTL sweep because the memory is a courtesy to a late operator, not a
// record: the eventlog and the approval log are where an unanswered
// prompt is durably accounted for. Sized so a daemon prompting on a
// cycle keeps roughly a shift's worth of them without anything to
// sweep it.
//
// Callers hold b.mu.
const maxGoneRemembered = 64

// goneReason maps the cause that ended a wait to the sentinel a late
// responder should see. Only the gate's own timeout reads as an expiry;
// everything else — an operator's stop, a guardrail cutting the turn, a
// shutting-down request context — is the turn ending under a prompt
// that was still open.
func goneReason(cause error) error {
	if errors.Is(cause, permissions.ErrPromptExpired) {
		return ErrPromptExpired
	}
	return ErrPromptCanceled
}

// rememberGone records that id is gone and why. Callers hold b.mu.
func (b *PromptBroker) rememberGone(id string, cause error) {
	b.gone = append(b.gone, gonePrompt{id: id, at: time.Now().UTC(), err: goneReason(cause)})
	if len(b.gone) > maxGoneRemembered {
		b.gone = append(b.gone[:0], b.gone[len(b.gone)-maxGoneRemembered:]...)
	}
}

// goneErr returns the sentinel for a prompt this broker stopped waiting
// on, or nil if it is not holding a tombstone for id. Callers hold b.mu.
func (b *PromptBroker) goneErr(id string) error {
	for _, e := range b.gone {
		if e.id == id {
			return e.err
		}
	}
	return nil
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
// call identified by id, without attributing it. Equivalent to
// RespondAs with an empty approver — for hosts where the answerer is
// implicit (one operator, one terminal).
//
// Returns ErrPromptNotFound if the id doesn't match a live request
// (already responded, already cancelled, or never existed).
func (b *PromptBroker) Respond(id string, decision permissions.Decision) error {
	return b.RespondAs(id, decision, "")
}

// RespondAs is Respond with the principal that made the decision, so
// the gate's approval log can record who approved rather than only
// what was approved (#830).
//
// by must be an identity the CALLER verified — the HTTP handler passes
// the caller-resolution middleware's verdict, never a name taken from
// the request body. Pass "" when there is nothing verified to record;
// see permissions.Approval.By.
func (b *PromptBroker) RespondAs(id string, decision permissions.Decision, by string) error {
	b.mu.Lock()
	pending, ok := b.pending[id]
	var gone error
	if !ok {
		gone = b.goneErr(id)
	}
	b.mu.Unlock()
	if gone != nil {
		return gone
	}
	if !ok {
		return ErrPromptNotFound
	}
	select {
	case pending.response <- promptResponse{decision: decision, by: by}:
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

// ErrPromptExpired is returned by Respond when the id names a prompt
// that this broker let time out under the gate's approval timeout.
//
// Separate from ErrPromptNotFound because the two answer different
// questions for the human who just clicked approve. "Not found" leaves
// them unable to tell whether the write went through under somebody
// else's answer; this one says plainly that it did not happen, and that
// the reason was the clock rather than a decision.
var ErrPromptExpired = errors.New("attach: approval arrived after the prompt expired; the action was not taken")

// ErrPromptCanceled is returned by Respond when the id names a prompt
// that was still open when its turn ended — an operator's stop, a
// guardrail cutting the turn, a daemon going down under it (#1088).
//
// Separate from ErrPromptExpired because the operator's next move is
// different. An expiry says the window was too short: re-run it and
// answer faster, or raise approval_timeout. A cancellation says the
// agent stopped for an unrelated reason and the request went with it,
// so answering faster would not have helped and the thing to look at is
// why the turn ended. Separate from ErrPromptNotFound because the
// prompt WAS here: "never issued" is the one reading that would send a
// late approver looking for a write that no part of the system ever
// attempted.
var ErrPromptCanceled = errors.New("attach: the prompt's turn ended before the approval arrived; the action was not taken")

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
	case permissions.PromptKindControlPlaneWrite:
		return "control_plane_write"
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
