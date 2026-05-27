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
	"context"
	"errors"
	"fmt"
	"iter"
	"strings"
	"time"

	"google.golang.org/genai"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/adk/session"
)

// CompactionEventTag is the value stored under
// session.Event.CustomMetadata["compaction"] to mark an event as a
// compaction summary. The history-slicing path in Run scans for the
// most recent event carrying this marker and drops everything
// before it from the LLM request. See
// docs/context-management-design.md (Mechanism A) for the rationale.
const CompactionEventTag = "summary"

// CompactionMetadataKey is the key under which CompactionEventTag is
// stored on session.Event.CustomMetadata. Exported so consumers
// querying the audit log can find summaries deterministically.
const CompactionMetadataKey = "compaction"

// CompactionFocusKey carries the operator-supplied focus hint (from
// `/compact <focus>` or Agent.Compact(ctx, focus)) on the summary
// event's CustomMetadata, so it survives in the audit log alongside
// the summary text.
const CompactionFocusKey = "compaction_focus"

// Compactor decides when context-window compaction should fire and
// produces the summary prompt the agent sends to its model. Consumers
// plug a custom implementation via agent.WithCompactor; the package
// default (NewDefaultCompactor) covers the common case.
type Compactor interface {
	// ShouldCompact returns true when the agent should compact before
	// the next turn. Called from Agent.Run's post-turn hook with the
	// agent's usage tracker so the implementation can read context-
	// window state (Tracker.ContextWindowUsed / ContextWindowSize).
	// Returning false is a no-op — the next turn proceeds normally.
	ShouldCompact(ctx context.Context, a *Agent) bool

	// SummarizerInstruction returns the system instruction the
	// compactor LLM call uses. focus carries the operator's optional
	// focus hint (empty when none). The instruction tells the model
	// what kind of summary to produce; the conversation history is
	// supplied as the LLMRequest.Contents.
	SummarizerInstruction(focus string) string
}

// DefaultCompactor is the package-default Compactor. Triggers on
// context-window utilization ≥ Threshold (default 0.85) and produces
// a five-section "teammate handover" summary (current state, files
// & changes, technical context, strategy & approach, exact next
// steps) per docs/context-management-design.md §Mechanism A.
//
// Consumers needing a different prompt or trigger logic implement
// Compactor themselves; this type is a sensible default, not a
// required base class.
type DefaultCompactor struct {
	// Threshold is the context-window utilization at which
	// compaction fires. 0.85 means "compact once we've used 85% of
	// the model's context window." Zero is treated as "use the
	// package default."
	Threshold float64
}

// DefaultCompactionThreshold is the default for
// DefaultCompactor.Threshold. 0.85 leaves headroom for one more
// full turn before hitting the actual context wall, and is high
// enough that we don't compact eagerly on lightly-used sessions.
const DefaultCompactionThreshold = 0.85

// NewDefaultCompactor returns a DefaultCompactor at the package-
// default threshold. Pass &DefaultCompactor{Threshold: x} for a
// custom value (must be 0 < x < 1).
func NewDefaultCompactor() Compactor { return &DefaultCompactor{Threshold: DefaultCompactionThreshold} }

// ShouldCompact returns true when the agent's usage tracker reports
// context-window utilization at or above Threshold. Returns false
// when the tracker doesn't yet know the window size (no turn has
// landed, or the model isn't in usage.ContextWindowSizeFor's table)
// so a session with an unknown model never triggers premature
// compaction.
func (c *DefaultCompactor) ShouldCompact(_ context.Context, a *Agent) bool {
	if a == nil || a.tracker == nil {
		return false
	}
	size := a.tracker.ContextWindowSize()
	if size == 0 {
		return false
	}
	used := a.tracker.ContextWindowUsed()
	threshold := c.Threshold
	if threshold <= 0 {
		threshold = DefaultCompactionThreshold
	}
	return float64(used)/float64(size) >= threshold
}

// SummarizerInstruction returns the five-section handover prompt.
// focus, when non-empty, appended as a "Compact focus: <text>"
// directive so the summarizer prioritizes that thread.
func (c *DefaultCompactor) SummarizerInstruction(focus string) string {
	var b strings.Builder
	b.WriteString(defaultSummarizerHeader)
	if strings.TrimSpace(focus) != "" {
		b.WriteString("\n\nCompact focus: ")
		b.WriteString(strings.TrimSpace(focus))
	}
	return b.String()
}

const defaultSummarizerHeader = `You are compacting a long agent conversation so it fits the context window with the most important state intact. Produce a teammate-style handover with these FIVE sections in order, using these exact headings:

# Current state
The exact user request (verbatim if you can). What's been completed. What's actively in progress. What's specifically remaining.

# Files & changes
Files modified (one-line per file describing the change). Files read or analyzed but not modified. Files that still need changes but weren't touched yet. Critical code locations with line numbers when known.

# Technical context
Architectural decisions made and why. Patterns adopted. Commands that worked. Commands that failed and why. Environment quirks discovered.

# Strategy & approach
The strategy chosen. Alternatives considered and why they were rejected. Gotchas. Assumptions in play. Blockers.

# Exact next steps
A concrete numbered list of the next developer-style actions. Be specific — file paths, function names, line numbers, expected behavior.

Be dense and concrete. This summary REPLACES the conversation history for future turns — anything you omit is effectively gone. Skip social niceties; capture facts.`

// CompactionResult reports what happened on a Compact call.
type CompactionResult struct {
	// SummaryEventID is the ID of the event the compactor wrote to
	// the session. Empty when no compaction ran (compactor returned
	// no-op, or the call errored before writing).
	SummaryEventID string

	// SummaryText is the full text the model produced. Useful for
	// callers that want to surface the summary in the UI before the
	// next turn runs.
	SummaryText string

	// Duration is wall-clock time spent in the summarizer LLM call.
	Duration time.Duration

	// Skipped is true when the compactor decided not to compact
	// (e.g., ShouldCompact returned false from a programmatic
	// Agent.CompactIfNeeded call). When Skipped is true, the rest of
	// the fields are zero-valued.
	Skipped bool
}

// ErrNoCompactor is returned by Agent.Compact when the agent was
// constructed without WithCompactor. Callers should check for this
// sentinel before treating it as a hard failure.
var ErrNoCompactor = errors.New("agent: no compactor wired (pass WithCompactor at agent.New)")

// Compact runs an out-of-band summarizer LLM call against the
// current session's history and writes the result as a marker
// event the history-slicing path in Run picks up on the next turn.
// Used both programmatically and by the TUI's /compact slash.
//
// focus is an optional hint for the summarizer ("focus on the
// auth-rewrite thread"). Empty is fine — the default prompt
// instructs the model to produce a balanced handover.
//
// Errors:
//   - ErrNoCompactor when no compactor was wired.
//   - Context cancellation: ctx.Err().
//   - Model errors propagate wrapped so callers can errors.Is on
//     transport vs API failures.
func (a *Agent) Compact(ctx context.Context, focus string) (CompactionResult, error) {
	if a == nil {
		return CompactionResult{}, errors.New("agent: Compact: nil receiver")
	}
	if a.compactor == nil {
		return CompactionResult{}, ErrNoCompactor
	}
	if a.model == nil {
		return CompactionResult{}, errors.New("agent: Compact: no model wired (construct via agent.New)")
	}
	if a.sessionService == nil {
		return CompactionResult{}, errors.New("agent: Compact: no session.Service wired")
	}

	// Load the full session history — unsliced. Compact is the one
	// place that wants to see EVERYTHING (so the summary can capture
	// the early-conversation context that's about to be dropped from
	// future turns).
	history, err := a.sessionHistory(ctx)
	if err != nil {
		return CompactionResult{}, fmt.Errorf("agent: Compact: load history: %w", err)
	}
	if len(history) == 0 {
		// Nothing to summarize. Don't write an empty marker — that
		// would cause the next turn to start with a useless "[no
		// history]" prefix.
		return CompactionResult{Skipped: true}, nil
	}

	systemInstruction := a.compactor.SummarizerInstruction(focus)
	req := &adkmodel.LLMRequest{
		Contents: history,
		Config: &genai.GenerateContentConfig{
			SystemInstruction: genai.NewContentFromText(systemInstruction, genai.RoleUser),
		},
		// Tools intentionally nil — compaction is a tool-less
		// summarization, like AskSideQuestion.
	}

	start := time.Now()
	var b strings.Builder
	for resp, err := range a.model.GenerateContent(ctx, req, false) {
		if err != nil {
			return CompactionResult{}, fmt.Errorf("agent: Compact: generate: %w", err)
		}
		if resp == nil || resp.Content == nil {
			continue
		}
		if resp.Partial {
			continue
		}
		for _, p := range resp.Content.Parts {
			if p != nil && p.Text != "" {
				b.WriteString(p.Text)
			}
		}
	}
	elapsed := time.Since(start)
	summary := strings.TrimSpace(b.String())
	if summary == "" {
		return CompactionResult{}, errors.New("agent: Compact: model returned no summary text")
	}

	// Persist the summary as a session event with the compaction
	// marker. The history-slicing path in Run scans for this marker
	// on the next turn and uses it as the new history baseline.
	id, err := a.appendCompactionEvent(ctx, summary, focus)
	if err != nil {
		return CompactionResult{}, fmt.Errorf("agent: Compact: persist: %w", err)
	}

	if a.compactor != nil {
		// Clear any pending flag — we just compacted, no need to
		// re-fire on the next turn.
		a.mu.Lock()
		a.compactionPending = false
		a.mu.Unlock()
	}

	return CompactionResult{
		SummaryEventID: id,
		SummaryText:    summary,
		Duration:       elapsed,
	}, nil
}

// CompactIfNeeded fires Compact when the wired Compactor's
// ShouldCompact returns true, otherwise reports Skipped=true. Useful
// for hosts that want to drive automatic compaction on a turn-end
// hook without re-implementing the threshold check.
func (a *Agent) CompactIfNeeded(ctx context.Context, focus string) (CompactionResult, error) {
	if a == nil || a.compactor == nil {
		return CompactionResult{Skipped: true}, nil
	}
	if !a.compactor.ShouldCompact(ctx, a) {
		return CompactionResult{Skipped: true}, nil
	}
	return a.Compact(ctx, focus)
}

// maybeMarkCompactionPending is the post-turn hook. Called from
// wrapWithCleanup after a Run iterator drains. Checks the
// compactor's threshold and sets compactionPending so the next Run
// call fires Compact before its actual work.
func (a *Agent) maybeMarkCompactionPending() {
	if a == nil || a.compactor == nil {
		return
	}
	// No ctx available in the cleanup callback; use background.
	// ShouldCompact implementations should be cheap and never block
	// (default is a tracker lookup + arithmetic).
	if !a.compactor.ShouldCompact(context.Background(), a) {
		return
	}
	a.mu.Lock()
	a.compactionPending = true
	a.mu.Unlock()
}

// runPendingCompaction fires Compact when the post-turn hook from
// the prior turn flagged the session as over-threshold. Called from
// Run before constructing the model request — analogous to how
// inbox messages drain pre-turn. No-op when no flag is set or when
// no compactor is wired.
//
// Errors from the compactor are intentionally logged-and-swallowed:
// a failed compaction shouldn't block the operator's turn. The flag
// is cleared in either case so we don't retry-loop on a persistent
// model failure.
func (a *Agent) runPendingCompaction(ctx context.Context) {
	if a == nil || a.compactor == nil {
		return
	}
	a.mu.Lock()
	pending := a.compactionPending
	a.compactionPending = false
	a.mu.Unlock()
	if !pending {
		return
	}
	if _, err := a.Compact(ctx, ""); err != nil {
		// Don't fail the turn. The next post-turn hook may re-flag
		// pending if we're still over threshold and the operator
		// can /compact manually.
		_ = err
	}
}

// appendCompactionEvent writes a marker event carrying the summary
// text to the session. The event's CustomMetadata carries
// CompactionMetadataKey: CompactionEventTag so the history-slicing
// scanner finds it cheaply.
func (a *Agent) appendCompactionEvent(ctx context.Context, summary, focus string) (string, error) {
	resp, err := a.sessionService.Get(ctx, &session.GetRequest{
		AppName:   a.appName,
		UserID:    a.userID,
		SessionID: a.sessionID,
	})
	if err != nil {
		return "", err
	}
	if resp == nil || resp.Session == nil {
		return "", errors.New("session not found")
	}
	ev := &session.Event{
		ID:        newCompactionEventID(),
		Author:    a.agentName,
		Timestamp: time.Now(),
		LLMResponse: adkmodel.LLMResponse{
			Content: genai.NewContentFromText(summary, genai.RoleModel),
			CustomMetadata: map[string]any{
				CompactionMetadataKey: CompactionEventTag,
				CompactionFocusKey:    focus,
			},
		},
	}
	if err := a.sessionService.AppendEvent(ctx, resp.Session, ev); err != nil {
		return "", err
	}
	return ev.ID, nil
}

// newCompactionEventID returns a unique-enough event ID. Format
// mirrors what ADK and our checkpoint events do — a stable prefix
// for log filtering plus a time-ordered nanos suffix.
func newCompactionEventID() string {
	return fmt.Sprintf("compaction-%d", time.Now().UnixNano())
}

// findLatestCompactionSummary scans events newest-first for one
// carrying the CompactionMetadataKey marker. Returns the matching
// index and event, or (-1, nil) when none found.
//
// Exported (lowercase) for internal use by the history-slicing path
// in Run and for unit tests in the package.
func findLatestCompactionSummary(events []*session.Event) (int, *session.Event) {
	for i := len(events) - 1; i >= 0; i-- {
		ev := events[i]
		if ev == nil || ev.CustomMetadata == nil {
			continue
		}
		if v, ok := ev.CustomMetadata[CompactionMetadataKey]; ok {
			if s, ok := v.(string); ok && s == CompactionEventTag {
				return i, ev
			}
		}
	}
	return -1, nil
}

// sliceFromSummary returns events from the summary forward, with
// the summary itself rewritten to RoleUser so the resuming model
// treats it as "the user told me this is where we are" rather than
// as an opaque assistant statement.
//
// The original events slice is not mutated; a new slice is returned
// containing a shallow copy of the summary event with the rewritten
// role.
//
// When no summary is present (no matching index), returns the
// original slice unchanged.
func sliceFromSummary(events []*session.Event) []*session.Event {
	idx, summary := findLatestCompactionSummary(events)
	if idx < 0 || summary == nil {
		return events
	}
	// Shallow-copy the summary event so we can rewrite its role
	// without mutating the audit log.
	rewritten := *summary
	if summary.Content != nil {
		c := *summary.Content
		c.Role = genai.RoleUser
		rewritten.Content = &c
	}
	out := make([]*session.Event, 0, len(events)-idx)
	out = append(out, &rewritten)
	out = append(out, events[idx+1:]...)
	return out
}

// compactingService wraps a session.Service so the runner sees a
// post-summary view of the session on Get() — pre-summary events
// are sliced off, and the summary event's role is rewritten to
// RoleUser so the resuming model treats it as the user-supplied
// baseline.
//
// CRUD methods other than Get pass through unchanged. AppendEvent
// in particular MUST pass through unchanged: the compactor itself
// writes summary events via the underlying service (via
// appendCompactionEvent's direct sessionService field on Agent,
// not through this wrapper), and the runner writes its own per-
// turn events that need to land in the real session.
//
// Construction is conditional in agent.New — only wrapped when a
// Compactor is wired. With no compactor, the runner gets the bare
// session.Service and slicing is a no-op cost.
type compactingService struct {
	inner session.Service
}

func (s *compactingService) Create(ctx context.Context, req *session.CreateRequest) (*session.CreateResponse, error) {
	return s.inner.Create(ctx, req)
}

func (s *compactingService) List(ctx context.Context, req *session.ListRequest) (*session.ListResponse, error) {
	return s.inner.List(ctx, req)
}

func (s *compactingService) Delete(ctx context.Context, req *session.DeleteRequest) error {
	return s.inner.Delete(ctx, req)
}

func (s *compactingService) AppendEvent(ctx context.Context, sess session.Session, ev *session.Event) error {
	return s.inner.AppendEvent(ctx, sess, ev)
}

// Get returns the same session struct (so AppendEvent on it lands
// in the real storage), but with Events() replaced by a sliced
// view that drops pre-summary events and rewrites the summary's
// role to user-style. When no summary marker is present, the slice
// is the full event list — equivalent to the unwrapped service.
func (s *compactingService) Get(ctx context.Context, req *session.GetRequest) (*session.GetResponse, error) {
	resp, err := s.inner.Get(ctx, req)
	if err != nil || resp == nil || resp.Session == nil {
		return resp, err
	}
	// Materialize all events so we can scan + slice them.
	var all []*session.Event
	for ev := range resp.Session.Events().All() {
		all = append(all, ev)
	}
	sliced := sliceFromSummary(all)
	if len(sliced) == len(all) {
		// No summary; pass through unchanged.
		return resp, nil
	}
	resp.Session = &slicedSession{inner: resp.Session, events: sliced}
	return resp, nil
}

// slicedSession wraps a real session.Session so Events() yields a
// pre-computed sliced view. Every other method delegates to inner
// so AppendEvent + ID + metadata behave normally (writes land in
// the real underlying storage).
type slicedSession struct {
	inner  session.Session
	events []*session.Event
}

func (s *slicedSession) AppName() string           { return s.inner.AppName() }
func (s *slicedSession) UserID() string            { return s.inner.UserID() }
func (s *slicedSession) ID() string                { return s.inner.ID() }
func (s *slicedSession) State() session.State      { return s.inner.State() }
func (s *slicedSession) LastUpdateTime() time.Time { return s.inner.LastUpdateTime() }

func (s *slicedSession) Events() session.Events {
	return &slicedEvents{events: s.events}
}

// slicedEvents implements session.Events over a pre-computed
// in-memory slice. The runner's contents processor reads via All()
// (and At/Len for indexing); writes go through session.Service's
// AppendEvent, not through this view.
type slicedEvents struct {
	events []*session.Event
}

func (e *slicedEvents) All() iter.Seq[*session.Event] {
	return func(yield func(*session.Event) bool) {
		for _, ev := range e.events {
			if !yield(ev) {
				return
			}
		}
	}
}

func (e *slicedEvents) Len() int { return len(e.events) }

func (e *slicedEvents) At(i int) *session.Event {
	if i < 0 || i >= len(e.events) {
		return nil
	}
	return e.events[i]
}
