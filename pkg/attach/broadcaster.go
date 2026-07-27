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
	"errors"
	"log"
	"sync"

	"google.golang.org/adk/session"

	"github.com/go-steer/core-agent/v2/pkg/eventlog"
)

// Frame is one item the SSE stream emits. Two shapes carried in
// one struct, distinguished by whether Type is set:
//
//   - Legacy form (Type == ""): carries an eventlog seq + ADK
//     session.Event. The writer emits this as `event: agent` with
//     the JSON Frame as the data block (back-compat for poll-mode
//     clients and any consumer of the legacy stream).
//
//   - Typed form (Type != ""): carries a protocol event type and
//     a payload that gets marshaled directly. The writer emits this
//     as `event: <Type>` with JSON(TypedData) as the data block.
//     Used for every event defined in the SSE event-stream
//     protocol spec (capabilities, status-update, usage-update,
//     inbox, turn-complete, turn-error).
//
// Type and TypedData carry the `json:"-"` tag so they never appear
// in the legacy frame's serialized form — the writer handles them
// out-of-band based on the Type discriminator.
type Frame struct {
	Seq   int64          `json:"seq"`
	Event *session.Event `json:"event"`

	Type      string `json:"-"`
	TypedData any    `json:"-"`
}

// subscriberBufferSize is the per-subscriber channel capacity. Slow
// subscribers that fall behind get dropped (their channel is closed)
// rather than stalling the publisher — the design's "we don't share
// one buffered channel across subscribers" decision in action.
//
// 256 frames is generous: at 10 events/sec sustained, that's ~25s of
// catch-up room before a subscriber is declared slow.
const subscriberBufferSize = 256

// maxReplayEvents caps how many historical frames one Subscribe call
// replays. Without it, any authenticated caller could open
// /events?since=0 against a long-lived session and trigger a
// full-table replay per connection — a cheap resource-exhaustion
// lever (#385). When the catch-up range exceeds the cap, the OLDEST
// events are dropped and the subscriber gets the most recent
// maxReplayEvents (the tail); incremental resumes whose gap is below
// the cap are unaffected.
//
// Package-level var (not const) purely as a test seam; production
// code never mutates it.
var maxReplayEvents int64 = 5000

// latestSeqStream is the optional eventlog.Stream extension the
// replay cap needs (implemented by the production gormStream).
// Streams without it — test fakes, exotic embeddings — skip the cap
// and keep the uncapped pre-#385 behavior.
type latestSeqStream interface {
	LatestSeq(ctx context.Context, opts ...eventlog.QueryOption) (int64, error)
}

// clampReplaySince bounds the catch-up range to the newest
// maxReplayEvents frames: if the log's head is more than the cap
// ahead of since, the cursor is advanced so only the tail replays.
// The clamped value feeds BOTH delivery sources (replayThenTail and
// the pump's startedAt), so no goroutine re-introduces the dropped
// head. The SSE protocol has no "replay truncated" frame, so
// truncation is surfaced via the server log only; clients that need
// the full history can page it from the eventlog directly.
func (b *Broadcaster) clampReplaySince(ctx context.Context, since int64) int64 {
	ls, ok := b.stream.(latestSeqStream)
	if !ok {
		return since
	}
	latest, err := ls.LatestSeq(ctx, b.query...)
	if err != nil {
		// Best-effort: a failed max-seq probe must not kill the
		// subscribe; the replay itself will surface a real error.
		debugf("broadcaster %s/%s LatestSeq failed (replay uncapped): %v", b.entry.AppName, b.entry.SessionID, err)
		return since
	}
	if floor := latest - maxReplayEvents; floor > since {
		log.Printf("attach: broadcaster %s/%s replay truncated: since=%d is %d events behind head %d; replaying newest %d only", //nolint:gosec // AppName/SessionID are server-managed identifiers from the SessionRegistry
			b.entry.AppName, b.entry.SessionID, since, latest-since, latest, maxReplayEvents)
		return floor
	}
	return since
}

// Broadcaster owns one goroutine per session that pumps events from
// eventlog.Stream.Watch into N subscriber channels. Subscribers can
// join any time; replay-then-tail is handled via the since parameter.
//
// One Broadcaster per Entry. Lazily created on first Subscribe and
// torn down when the last subscriber leaves (refcount).
type Broadcaster struct {
	entry  *Entry
	stream eventlog.Stream
	query  []eventlog.QueryOption // ForSession(...) for this entry

	mu        sync.Mutex
	subs      map[*subscriber]struct{}
	cancel    context.CancelFunc // cancels the pump goroutine
	startedAt int64              // last seq the pump has yielded

	// wg tracks every goroutine this broadcaster spawns (the pump and
	// each replayThenTail). Close() waits on it so that, once Close
	// returns, no goroutine is still reading the eventlog — the caller
	// (Server.Close → pool.Close) can then close the eventlog handle
	// without a lingering Stream.Watch/Since read racing the teardown
	// (which manifested as a flaky "TempDir: directory not empty" on
	// the SQLite -wal/-shm sidecar files).
	wg sync.WaitGroup

	// closing is closed exactly once by Close to wake replayThenTail
	// goroutines parked on the post-replay live-tail wait, so shutdown
	// doesn't have to depend on each subscriber's request context
	// being cancelled first.
	closing   chan struct{}
	closeOnce sync.Once

	// capsBuilder, when non-nil, extends the default boot Capabilities
	// frame with runtime-derived fields (features, slash_commands,
	// agent identity, caller_id) per SSE spec v1.4.0. Set by
	// BroadcasterPool.For from the server-level builder closure so
	// tests that construct a Broadcaster directly still get the
	// pre-1.4.0 minimal shape without ceremony. Called per-subscribe
	// so caller_id reflects the current request's Caller.
	capsBuilder func(ctx context.Context, entry *Entry) Capabilities
}

type subscriber struct {
	ch     chan Frame
	closed bool

	// since is the operator's catch-up cursor — frames with seq <=
	// since must not reach this subscriber. Both replayThenTail (per
	// subscriber, knows its own since) and pump (shared across
	// subscribers, doesn't know per-sub since) call send(); the
	// since filter keeps pump from leaking events the operator
	// already had.
	since int64

	// lastSent dedupes the dual-source delivery: pump and
	// replayThenTail both race to push the same (sub.since,
	// currentMax] range — without per-subscriber dedup, every
	// catch-up event is broadcast twice and downstream consumers
	// (coretui's chat view) silently drop or double-render. Both
	// sources emit events monotonically by seq starting from
	// sub.since, so a single high-water mark is enough — whichever
	// goroutine wins the race for seq=N delivers it; the other's
	// attempt for seq=N is a no-op skip.
	dedupMu  sync.Mutex
	lastSent int64
}

// NewBroadcaster constructs a broadcaster for one registered session.
// The pump goroutine is NOT started until the first Subscribe — we
// don't want background goroutines for sessions nobody's watching.
func NewBroadcaster(entry *Entry) (*Broadcaster, error) {
	if entry == nil {
		return nil, errors.New("attach: NewBroadcaster: nil entry")
	}
	if entry.Agent == nil {
		return nil, errors.New("attach: NewBroadcaster: entry has nil Agent")
	}
	h := entry.Agent.EventLog()
	if h == nil {
		return nil, errors.New("attach: NewBroadcaster: agent has no eventlog (attach-mode requires --session-db)")
	}
	return &Broadcaster{
		entry:  entry,
		stream: h.Stream,
		query: []eventlog.QueryOption{
			eventlog.ForSession(entry.AppName, entry.UserID, entry.SessionID),
		},
		subs:    make(map[*subscriber]struct{}),
		closing: make(chan struct{}),
	}, nil
}

// Subscribe adds a new client and returns its frame channel. Replays
// every frame with seq > since before switching to live-tail (which is
// invisible to the caller; same channel). The catch-up range is
// bounded by maxReplayEvents — a cursor further behind the log head
// than the cap replays only the newest cap-many frames (#385).
//
// The returned channel is closed when:
//   - ctx is cancelled (typical: HTTP request ends), OR
//   - the subscriber falls behind subscriberBufferSize frames (the
//     drop-the-subscriber decision; better than stalling everyone).
//
// Caller MUST drain the channel until close to release goroutine
// resources.
func (b *Broadcaster) Subscribe(ctx context.Context, since int64) <-chan Frame {
	since = b.clampReplaySince(ctx, since)
	sub := &subscriber{
		ch:       make(chan Frame, subscriberBufferSize),
		since:    since,
		lastSent: since, // dedup baseline — skip anything at or below the operator's cursor
	}

	// Boot frames per the SSE event-stream protocol spec: capabilities
	// is required as the FIRST frame on every newly-opened stream,
	// followed by snapshot status-update and (when usage data exists)
	// a cumulative usage-update. Delivered BEFORE the subscriber is
	// published into b.subs: once registered, the pump / Emit fan-out
	// can push a live frame into sub.ch, and doing that ahead of the
	// boot frames would break the "capabilities first" protocol
	// invariant (#385). Here the channel is still private to this
	// goroutine, so the fresh 256-slot buffer takes the three small
	// frames with no possible interleaving.
	b.deliverBootFrames(ctx, sub)

	b.mu.Lock()
	firstSub := b.cancel == nil
	b.subs[sub] = struct{}{}
	// Lazy pump start on first subscriber.
	if firstSub {
		pumpCtx, cancel := context.WithCancel(context.Background())
		b.cancel = cancel
		// startedAt is set to the lowest "since" we've ever seen so
		// the pump pulls from far enough back to satisfy this
		// subscriber. Subsequent subscribers either find their
		// since >= startedAt (already in flight) or get a fresh
		// scan via the replay loop below.
		b.startedAt = since
		// Add/Done are paired here at the spawn site (not inside pump)
		// so Close's wg.Wait tracks the goroutine's full lifetime.
		b.wg.Add(1)
		go func() {
			defer b.wg.Done()
			b.pump(pumpCtx)
		}()
	}
	b.mu.Unlock()

	// First-subscriber wiring: hand the broadcaster's Emit method to
	// the agent so it can push typed events to the SSE stream while
	// somebody is listening. Cleared in detachLocked when the last
	// subscriber leaves so we don't retain an emit channel that
	// drops everything to the floor (and so a re-subscribed
	// broadcaster re-wires cleanly).
	if firstSub {
		if et, ok := b.entry.Agent.(EmitTarget); ok {
			et.SetAttachEmitter(b.Emit)
		}
	}

	// Replay loop runs in its own goroutine so Subscribe returns
	// immediately. The same channel carries both replayed and live
	// frames — the client doesn't distinguish.
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		b.replayThenTail(ctx, sub, since)
	}()

	return sub.ch
}

// Emit pushes a typed event to every current subscriber. Non-blocking
// per-subscriber: a subscriber whose buffer is full gets dropped (its
// channel is closed), same drop-the-subscriber policy as the legacy
// frame path.
//
// Emit is the entry point for every event type defined in the SSE
// event-stream protocol spec — agent lifecycle hooks, perm-mode
// mutations, inbox queue/dequeue, and the usage tracker all call
// here when something happens that needs to reach the operator.
//
// Safe to call concurrently from any goroutine.
func (b *Broadcaster) Emit(eventType string, payload any) {
	if eventType == "" {
		return // Defensive: callers should always pass a non-empty type.
	}
	frame := Frame{Type: eventType, TypedData: payload}
	b.mu.Lock()
	defer b.mu.Unlock()
	for sub := range b.subs {
		b.sendTyped(sub, frame)
	}
}

// deliverBootFrames pushes the spec-required opening frames into a
// freshly-subscribed channel. Called from Subscribe BEFORE the
// subscriber is registered in b.subs (and before the replay/tail
// goroutine starts), so no pump/Emit fan-out can interleave a live
// frame ahead of the capabilities frame (#385).
//
// All three sends are non-blocking (sub.ch is buffered, freshly
// created). If for any reason a send would block, the subscriber is
// detached — the operator gets no stream, but the broadcaster is
// not stalled.
//
// ctx is the subscriber's request context — used by the caps builder
// to resolve the request's Caller for the caller_id field. When
// capsBuilder is nil (test callers of NewBroadcaster that skip the
// pool wiring) the minimal pre-1.4.0 shape is emitted.
func (b *Broadcaster) deliverBootFrames(ctx context.Context, sub *subscriber) {
	// Build the frame payloads BEFORE taking b.mu — capsBuilder,
	// statusSnapshot and usageSnapshot all call into agent code and we
	// don't want that running under the broadcaster mutex.

	// 1. Capabilities — required first frame per spec section 2.1.
	caps := Capabilities{
		ProtocolVersion: ProtocolVersion,
		EventTypes:      SupportedEventTypes,
		Server:          serverBanner(),
	}
	if b.capsBuilder != nil {
		extended := b.capsBuilder(ctx, b.entry)
		// Preserve the required scaffolding — the builder is only
		// responsible for the additive v1.4.0 fields; the wire-format
		// invariants (protocol_version, event_types) stay owned here.
		caps.Features = extended.Features
		caps.SlashCommands = extended.SlashCommands
		caps.Agent = extended.Agent
		caps.CallerID = extended.CallerID
		if extended.Server != "" {
			caps.Server = extended.Server
		}
	}

	// 2. Status snapshot — full state so a fresh client sees the
	// agent's current model / turn state without waiting for the
	// next state change. Agents that don't implement StatusProvider
	// get a minimal idle snapshot.
	status := b.statusSnapshot()

	// 3. Usage snapshot — cumulative tracker state so the consumer
	// can render cost without polling /usage. Optional per spec;
	// skip silently if the agent has no usage data wired.
	usage, hasUsage := b.usageSnapshot()

	// The sends still run under b.mu even though Subscribe now calls
	// this before registering sub in b.subs (so no other goroutine
	// can be sending to THIS subscriber yet): sendTyped's full-buffer
	// path calls detachLocked(), which mutates the shared b.subs map
	// and inspects b.cancel — state that concurrent pump/Emit/detach
	// activity for OTHER subscribers touches under the same mutex
	// (#377). Sends are non-blocking.
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.sendTyped(sub, Frame{Type: EventCapabilities, TypedData: caps}) {
		return
	}
	if !b.sendTyped(sub, Frame{Type: EventStatusUpdate, TypedData: status}) {
		return
	}
	if hasUsage {
		if !b.sendTyped(sub, Frame{Type: EventUsageUpdate, TypedData: usage}) {
			return
		}
	}
}

// statusSnapshot constructs the initial StatusUpdate from the entry's
// agent. Falls back to a minimal idle status if the agent doesn't
// implement StatusProvider. The mapping from the existing StatusInfo
// (state/model_name) to the spec's StatusUpdate (turn_state/model)
// keeps the two surfaces aligned without forcing agents to implement
// a second snapshot method just for SSE.
func (b *Broadcaster) statusSnapshot() StatusUpdate {
	out := StatusUpdate{TurnState: TurnStateIdle}
	p, ok := b.entry.Agent.(StatusProvider)
	if !ok {
		return out
	}
	info := p.AttachStatus()
	out.Model = info.ModelName
	switch info.State {
	case AgentStateRunning:
		out.TurnState = TurnStateStreaming
	default:
		// deferred / paused / idle / unknown all map to idle from
		// the consumer's perspective — the agent isn't currently
		// producing tokens. Future PRs can distinguish deferred /
		// paused with their own turn-state values.
		out.TurnState = TurnStateIdle
	}
	return out
}

// usageSnapshot builds the cumulative UsageUpdate from the entry's
// agent. Returns ok=false when the agent doesn't implement
// UsageProvider so the caller can omit the frame entirely (spec
// allows usage-update to be skipped on stream open when no data
// exists yet).
func (b *Broadcaster) usageSnapshot() (UsageUpdate, bool) {
	p, ok := b.entry.Agent.(UsageProvider)
	if !ok {
		return UsageUpdate{}, false
	}
	info := p.AttachUsage()
	out := UsageUpdate{
		TokensInTotal:  int(info.Overall.InputTokens),
		TokensOutTotal: int(info.Overall.OutputTokens),
		CostUSDTotal:   info.Overall.CostUSD,
		TurnsTotal:     info.Overall.Turns,
	}
	if len(info.PerModel) > 0 {
		out.ByModel = make(map[string]UsageByModel, len(info.PerModel))
		for model, totals := range info.PerModel {
			out.ByModel[model] = UsageByModel{
				TokensIn:  int(totals.InputTokens),
				TokensOut: int(totals.OutputTokens),
				CostUSD:   totals.CostUSD,
				Turns:     totals.Turns,
			}
		}
	}
	return out, true
}

// serverBanner returns the optional `server` field of the
// Capabilities event. Best-effort identification of this process for
// operator diagnostics; not used for any protocol logic.
func serverBanner() string {
	// TODO: populate from build-stamped version once that's wired
	// (see cmd/core-agent's --version flag). Empty banner is valid
	// per spec — the field is diagnostic, not required.
	return "core-agent"
}

// replayThenTail does an eventlog.Stream.Since pull for the catch-up
// range, sending each frame to the subscriber, then leaves the
// subscriber attached for the live-tail (pump goroutine handles
// live broadcasts). Honors ctx.Done so disconnects are clean.
func (b *Broadcaster) replayThenTail(ctx context.Context, sub *subscriber, since int64) {
	for entry, err := range b.stream.Since(ctx, since, b.query...) {
		if err != nil {
			// Replay failures close the subscriber; the client sees
			// EOF on its SSE stream and can reconnect.
			b.detach(sub)
			return
		}
		// send() must run under b.mu: on a full buffer it calls
		// detachLocked() (map delete + close(sub.ch)), which races the
		// pump goroutine's own locked send/iterate over b.subs. Without
		// the lock a slow consumer during replay triggers "send on
		// closed channel" or "concurrent map writes" — a daemon-wide
		// crash (#377). The send is non-blocking, so holding the mutex
		// here can't stall the pump.
		b.mu.Lock()
		ok := b.send(sub, Frame{Seq: entry.Seq, Event: entry.Event})
		b.mu.Unlock()
		if !ok {
			return // dropped or ctx cancelled
		}
	}
	// Wait for the subscriber to go away (live frames are delivered by
	// the pump goroutine into our channel directly). Two triggers: the
	// request context ending (normal client disconnect) or Close
	// signalling a server-wide shutdown — the latter so teardown
	// doesn't hang waiting on a request context that may outlive it.
	select {
	case <-ctx.Done():
	case <-b.closing:
	}
	b.detach(sub)
}

// pump is the single publisher goroutine per Broadcaster. Drains
// eventlog.Stream.Watch and fans out to every subscriber that's
// attached at the time of the broadcast. Exits when no subscribers
// remain (set by detach).
func (b *Broadcaster) pump(ctx context.Context) {
	debugf("broadcaster pump START %s/%s startedAt=%d", b.entry.AppName, b.entry.SessionID, b.startedAt)
	defer debugf("broadcaster pump END %s/%s", b.entry.AppName, b.entry.SessionID)
	for entry, err := range b.stream.Watch(ctx, b.startedAt, b.query...) {
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				log.Printf("attach: broadcaster %s/%s pump error: %v", //nolint:gosec // AppName/SessionID are server-managed identifiers from the SessionRegistry, not request-scoped user input
					b.entry.AppName, b.entry.SessionID, err)
				debugf("broadcaster pump %s/%s error: %v", b.entry.AppName, b.entry.SessionID, err)
			}
			return
		}
		author := ""
		if entry.Event != nil {
			author = entry.Event.Author
		}
		// Every event pumped counts as session activity — a
		// busy autonomous agent (long tool call, background
		// compaction, etc.) is not idle. Prevents the eviction
		// sweep from killing an actively-working session.
		b.entry.touch()
		frame := Frame{Seq: entry.Seq, Event: entry.Event}
		b.mu.Lock()
		nSubs := len(b.subs)
		for sub := range b.subs {
			b.send(sub, frame)
		}
		debugf("broadcaster pump %s/%s seq=%d author=%q → %d subs", b.entry.AppName, b.entry.SessionID, entry.Seq, author, nSubs)
		// If no subscribers left, shut down the pump.
		if len(b.subs) == 0 {
			b.cancel = nil
			b.mu.Unlock()
			return
		}
		b.mu.Unlock()
	}
}

// send is non-blocking — if the subscriber's buffer is full, the
// subscriber is closed (treated as slow / dead). Returns false when
// the subscriber should be considered detached.
//
// Typed frames (Type != "") bypass the seq-based dedup and are
// routed through sendTyped — they have no monotonic eventlog seq
// and the dedup logic would silently drop every one of them.
func (b *Broadcaster) send(sub *subscriber, f Frame) bool {
	if f.Type != "" {
		return b.sendTyped(sub, f)
	}
	if sub.closed {
		debugf("broadcaster send %s/%s seq=%d → sub already closed", b.entry.AppName, b.entry.SessionID, f.Seq)
		return false
	}
	// Dedup the pump/replay race: both goroutines push the same
	// catch-up range to every subscriber. lastSent is the
	// high-water mark; both goroutines deliver in monotonic order
	// from sub.since, so the first send for any seq wins and the
	// other's attempt is a no-op skip. Without this, every
	// catch-up event reached downstream consumers twice.
	sub.dedupMu.Lock()
	if f.Seq <= sub.lastSent {
		sub.dedupMu.Unlock()
		return true // already delivered (or below operator's since); skip silently
	}
	sub.lastSent = f.Seq
	sub.dedupMu.Unlock()

	select {
	case sub.ch <- f:
		return true
	default:
		// Buffer full → drop the subscriber.
		log.Printf("attach: broadcaster %s/%s dropping slow subscriber (buffer=%d full)", //nolint:gosec // AppName/SessionID are server-managed identifiers from the SessionRegistry
			b.entry.AppName, b.entry.SessionID, subscriberBufferSize)
		debugf("broadcaster send %s/%s seq=%d → buffer FULL, dropping subscriber", b.entry.AppName, b.entry.SessionID, f.Seq)
		b.detachLocked(sub)
		return false
	}
}

// sendTyped delivers a typed event frame to one subscriber.
// Bypasses send's seq-based dedup (typed events have no eventlog
// seq) and shares the same drop-the-slow-subscriber policy.
func (b *Broadcaster) sendTyped(sub *subscriber, f Frame) bool {
	if sub.closed {
		debugf("broadcaster sendTyped %s/%s type=%s → sub already closed", b.entry.AppName, b.entry.SessionID, f.Type)
		return false
	}
	select {
	case sub.ch <- f:
		return true
	default:
		log.Printf("attach: broadcaster %s/%s dropping slow subscriber (typed=%s, buffer=%d full)", //nolint:gosec // AppName/SessionID/Type are server-managed; Type is one of the typed-event protocol constants
			b.entry.AppName, b.entry.SessionID, f.Type, subscriberBufferSize)
		debugf("broadcaster sendTyped %s/%s type=%s → buffer FULL, dropping subscriber", b.entry.AppName, b.entry.SessionID, f.Type)
		b.detachLocked(sub)
		return false
	}
}

// detach removes the subscriber under the broadcaster's mutex. If
// this was the last subscriber, the pump goroutine is cancelled at
// its next iteration.
func (b *Broadcaster) detach(sub *subscriber) {
	b.mu.Lock()
	b.detachLocked(sub)
	b.mu.Unlock()
}

func (b *Broadcaster) detachLocked(sub *subscriber) {
	if sub.closed {
		return
	}
	sub.closed = true
	close(sub.ch)
	delete(b.subs, sub)
	if len(b.subs) == 0 && b.cancel != nil {
		b.cancel()
		b.cancel = nil
		// Last subscriber left — clear the agent's typed-event
		// callback so it stops doing emit work for an audience that
		// doesn't exist. The next Subscribe call re-wires.
		if et, ok := b.entry.Agent.(EmitTarget); ok {
			et.SetAttachEmitter(nil)
		}
	}
}

// Close cancels the pump goroutine, closes every subscriber channel,
// and blocks until all of this broadcaster's goroutines (pump and any
// replayThenTail) have returned. Once Close returns, nothing this
// broadcaster spawned is still reading the eventlog, so the caller may
// safely close the underlying handle. Idempotent. Called from
// Server.Close.
func (b *Broadcaster) Close() {
	b.mu.Lock()
	// Wake replayThenTail goroutines parked on the live-tail wait,
	// independent of their request contexts. Once — the channel is a
	// broadcast latch, and Close may be called more than once. The nil
	// check tolerates broadcasters built directly in white-box tests
	// (production always goes through NewBroadcaster, which sets it).
	b.closeOnce.Do(func() {
		if b.closing != nil {
			close(b.closing)
		}
	})
	for sub := range b.subs {
		if !sub.closed {
			sub.closed = true
			close(sub.ch)
		}
	}
	b.subs = nil
	if b.cancel != nil {
		b.cancel()
		b.cancel = nil
	}
	b.mu.Unlock()

	// Wait OUTSIDE the mutex: the pump takes b.mu on every iteration,
	// so holding it here would deadlock against the goroutines we're
	// waiting to drain.
	b.wg.Wait()
}

// BroadcasterPool lazily constructs and tracks one Broadcaster per
// Entry. Server uses this so multiple SSE clients for the same session
// share one pump goroutine.
type BroadcasterPool struct {
	mu sync.Mutex
	// Keyed by tripleKey so the (app, user, sid) identity matches
	// the registry's.
	bcasts map[tripleKey]*Broadcaster

	// capsBuilder, when non-nil, is stamped onto every Broadcaster
	// this pool constructs so the SSE v1.4.0 capabilities frame gets
	// its features/slash_commands/agent/caller_id fields populated
	// from server-level state (see BroadcasterPool.SetCapabilitiesBuilder).
	// Set once at server startup; reads are lock-free per the
	// "capsBuilder set exactly once before first For" contract.
	capsBuilder func(ctx context.Context, entry *Entry) Capabilities
}

// NewBroadcasterPool returns an empty pool.
func NewBroadcasterPool() *BroadcasterPool {
	return &BroadcasterPool{bcasts: make(map[tripleKey]*Broadcaster)}
}

// SetCapabilitiesBuilder wires the closure that produces the
// runtime-derived Capabilities extensions (features/slash_commands/
// agent/caller_id) for every new Broadcaster this pool constructs.
// Called once during Server construction; ignored (and a no-op)
// on subsequent calls to keep the "one builder per pool" contract
// simple. Passing nil clears the builder — Broadcasters constructed
// after clear fall back to the minimal pre-1.4.0 shape.
//
// Called BEFORE the first For() so pool-created broadcasters see
// the builder. Existing broadcasters (from an earlier For hit)
// keep whatever builder they were constructed with — the pool
// doesn't back-fill.
func (p *BroadcasterPool) SetCapabilitiesBuilder(fn func(ctx context.Context, entry *Entry) Capabilities) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.capsBuilder = fn
}

// For returns a Broadcaster for entry, constructing it on first use.
// Returns an error when the entry's agent has no eventlog (attach
// requires it).
func (p *BroadcasterPool) For(entry *Entry) (*Broadcaster, error) {
	key := tripleKey{App: entry.AppName, User: entry.UserID, SID: entry.SessionID}
	p.mu.Lock()
	defer p.mu.Unlock()
	if b, ok := p.bcasts[key]; ok {
		return b, nil
	}
	b, err := NewBroadcaster(entry)
	if err != nil {
		return nil, err
	}
	b.capsBuilder = p.capsBuilder
	p.bcasts[key] = b
	return b, nil
}

// Remove pulls the broadcaster for entry out of the pool and
// returns it, without lazily constructing one on miss. Returns nil
// when no broadcaster exists (e.g. a session with no subscribers
// this session). Callers should Close() the returned broadcaster
// to disconnect active subscribers — used by DELETE /sessions to
// force-hang up SSE clients streaming the deleted session.
func (p *BroadcasterPool) Remove(entry *Entry) *Broadcaster {
	key := tripleKey{App: entry.AppName, User: entry.UserID, SID: entry.SessionID}
	p.mu.Lock()
	defer p.mu.Unlock()
	b, ok := p.bcasts[key]
	if !ok {
		return nil
	}
	delete(p.bcasts, key)
	return b
}

// Close stops every broadcaster in the pool. Used by Server.Close.
func (p *BroadcasterPool) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, b := range p.bcasts {
		b.Close()
	}
	p.bcasts = nil
}
