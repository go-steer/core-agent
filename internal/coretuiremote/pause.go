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

// coretui.Pauser — the operator hold in attach mode.
//
// A paused session is one where no new turn starts until someone
// resumes, which is a different thing from "no turn is running": an
// idle agent picks up the next queued prompt on its own, a paused one
// does not. Both halves of the wire already existed before this file —
// pkg/attach serves POST /pause + /resume and folds the gate into GET
// /status (protocol v1.5.0), and attachclient speaks both — so this is
// the projection layer between them and core-tui, in the same spirit as
// typed_events.go. The vocabularies were built to match, so every
// mapping here is a field-for-field copy rather than a translation:
// core-tui's "steer" / "continue" / "abandon" and "paused" / "resumed"
// are the same strings pkg/attach put on the wire.

package coretuiremote

import (
	"context"
	"sync"

	coretui "github.com/go-steer/core-tui/tui"

	"github.com/go-steer/core-agent/v2/pkg/attach"
)

// pauseCache is the adapter's last-known projection of the session's
// pause gate, served to core-tui's once-a-second PauseState poll.
//
// It exists because PauseState must not block. core-tui refreshes it
// from the same background tick as StatusReporter.Status, and the
// interface contract is explicit that an implementation returns
// cached state rather than doing I/O inline — the wedge #630 fixed was
// exactly this shape of read reaching the network on a render path.
//
// Its own mutex rather than the adapter's mu: this is read on core-tui's
// snapshot tick and written from the SSE read loop, neither of which has
// any business contending with the lastSeq/lastTurn bookkeeping mu
// guards.
type pauseCache struct {
	mu     sync.Mutex
	info   coretui.PauseInfo
	seeded bool
}

// Pause satisfies coretui.Pauser. Backs /pause — park the loop without
// touching an in-flight turn ("stop after this one").
//
// core-tui calls this off the Update-loop path and a blocking round-trip
// is expected, so the caller's ctx (which carries its deadline) is passed
// straight through rather than replaced with a background one.
func (a *Adapter) Pause(ctx context.Context, reason string) error {
	resp, err := a.client.Pause(ctx, a.sessionPath, reason)
	if err != nil {
		return err
	}
	// Fold the ack in rather than waiting for the pause frame to come
	// back around the stream. Both will land; this one is already here,
	// and PauseState answering "not paused" for the tick between the ack
	// and the frame would flicker the banner off under the operator.
	//
	// Interrupted stays false: /pause is the disposition that explicitly
	// does not cancel anything. An interrupt-with-hold arrives through
	// RemoteInterrupter and reports itself on the frame.
	a.applyPauseInfo(coretui.PauseInfo{
		Paused: resp.Paused,
		Since:  resp.Since,
		Reason: resp.Reason,
	})
	return nil
}

// Resume satisfies coretui.Pauser. Backs /continue, /abandon, and the
// Enter-with-text steer.
//
// The mode strings are core-tui's and pkg/attach's alike, so the request
// copies across unchanged. An empty Mode is legal on both sides and
// means the same thing on both: the server defaults it to "steer" when
// Steer is non-empty and "continue" otherwise.
func (a *Adapter) Resume(ctx context.Context, req coretui.ResumeRequest) error {
	if _, err := a.client.Resume(ctx, a.sessionPath, attach.ResumeRequest{
		Mode:  req.Mode,
		Steer: req.Steer,
	}); err != nil {
		return err
	}
	// Resume is idempotent server-side: resp.Resumed is false, with a
	// 200, when the gate was already open. Clearing unconditionally is
	// still right — either way the gate is open now, and that is what
	// this cache reports.
	//
	// Doing it here rather than waiting for the resumed frame closes the
	// window core-tui's own pauseSettleWindow exists to paper over: a
	// poll sampled before the transition landed reads back as "still
	// paused". Clearing at the ack means our poll never carries that
	// stale answer in the first place.
	a.applyPauseInfo(coretui.PauseInfo{})
	return nil
}

// PauseState satisfies coretui.Pauser. Pure in-memory read — see
// pauseCache for why it must stay that way.
//
// Zero value (not paused) until the gate is seeded, which is the honest
// answer before anything has said otherwise, and the same thing a host
// with no hold at all would report.
func (a *Adapter) PauseState() coretui.PauseInfo {
	a.pause.mu.Lock()
	defer a.pause.mu.Unlock()
	return a.pause.info
}

// applyPauseInfo overwrites the cached gate state and marks it seeded.
func (a *Adapter) applyPauseInfo(info coretui.PauseInfo) {
	a.pause.mu.Lock()
	defer a.pause.mu.Unlock()
	a.pause.info = info
	a.pause.seeded = true
}

// applyPauseEvent folds a pause frame off the SSE stream into the cache.
//
// The stream owns this state once it is flowing. This adapter is a
// LiveAgent host — a standing Events subscription, not a per-turn one —
// so every transition arrives here, and the protocol's lossless replay
// means a reconnect re-delivers any frame missed while disconnected.
// That is what lets seedPauseFromStatus fire once instead of having
// /status continuously fight the stream for ownership.
//
// Unknown states are tolerated per spec §2.8 and change nothing, so a
// future third state can't silently read as "resumed".
func (a *Adapter) applyPauseEvent(p *attach.PauseEvent) {
	if p == nil {
		return
	}
	switch p.State {
	case attach.PauseStatePaused:
		a.applyPauseInfo(coretui.PauseInfo{
			Paused:      true,
			Since:       p.At,
			Reason:      p.Reason,
			Interrupted: p.Interrupted,
		})
	case attach.PauseStateResumed:
		a.applyPauseInfo(coretui.PauseInfo{})
	}
}

// seedPauseFromStatus folds the gate projected onto GET /status into the
// cache, once, on the first status read that lands.
//
// This is what makes a TUI attaching to an ALREADY-paused session render
// the banner. The stream cannot supply that on its own: the transition
// happened before this client connected, and typed frames are live
// fan-out only — broadcaster.Emit writes to the subscribers registered
// at that instant and never to the eventlog, so a pause frame is not
// among the frames a since=0 subscribe replays. (Note this is a
// different mechanism from the isReplay filter, which only guards the
// legacy agent-event path; typed frames bypass it entirely.)
//
// Once only, deliberately. After the first read the stream is the single
// writer, and a 1 Hz status refresh folding in on every tick would race
// it — a refresh already in flight when a resume lands returns the
// pre-resume answer and would flip the banner back on. Seeding once and
// then getting out of the way removes the race rather than adding a
// settle window to arbitrate it.
//
// The caller passes the whole StatusInfo because the gate is spread over
// four fields of it: State names it, and the other three describe it.
//
// Lock order: both call sites hold a.status.mu, so this takes a.pause.mu
// underneath it. Nothing in this file ever reaches for status.mu, which
// is what keeps that one-way and deadlock-free.
func (a *Adapter) seedPauseFromStatus(info attach.StatusInfo) {
	a.pause.mu.Lock()
	if a.pause.seeded {
		a.pause.mu.Unlock()
		return
	}
	a.pause.seeded = true
	if info.State == attach.AgentStatePaused {
		a.pause.info = coretui.PauseInfo{
			Paused:      true,
			Since:       info.PausedSince,
			Reason:      info.PauseReason,
			Interrupted: info.Interrupted,
		}
	}
	a.pause.mu.Unlock()
}

// pauseEventToCoreTui projects an attach pause frame into the core-tui
// event core-tui renders the transition from. Same field-for-field copy
// as the rest of typed_events.go.
func pauseEventToCoreTui(p *attach.PauseEvent) *coretui.PauseEvent {
	return &coretui.PauseEvent{
		State:       p.State,
		Reason:      p.Reason,
		Interrupted: p.Interrupted,
		Mode:        p.Mode,
		At:          p.At,
	}
}
