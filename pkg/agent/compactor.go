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

	"github.com/go-steer/core-agent/v2/pkg/modeltier"
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
// context-window utilization ≥ a per-model threshold (default 0.85
// for frontier, 0.65 for mid, 0.35 for small; see
// pkg/modeltier.DefaultCompactionThresholds) and produces a
// five-section "teammate handover" summary (current state, files
// & changes, technical context, strategy & approach, exact next
// steps) per docs/context-management-design.md §Mechanism A.
//
// Consumers needing a different prompt or trigger logic implement
// Compactor themselves; this type is a sensible default, not a
// required base class.
type DefaultCompactor struct {
	// Threshold is the fallback context-window utilization at which
	// compaction fires when no per-tier override applies (unknown
	// model, or the tier isn't in ThresholdByTier). 0.85 means
	// "compact once we've used 85% of the model's context window."
	// Zero is treated as "use the package default."
	Threshold float64

	// ThresholdByTier overrides Threshold per modeltier classification.
	// Keys are pkg/modeltier tier labels ("frontier", "mid", "small").
	// When the current model classifies to a tier present here, that
	// tier's threshold wins. Empty map (or zero value at a key) falls
	// back to Threshold. Use this to keep frontier sessions at 0.85
	// while compacting small-tier sessions much earlier.
	ThresholdByTier map[string]float64

	// TierClassifier is the function used to look up the current
	// model's tier. Defaults to modeltier.Classify. Override in tests
	// to inject deterministic tier resolutions without depending on
	// the modeltier table's current state.
	TierClassifier func(modelID string) string
}

// DefaultCompactionThreshold is the fallback for
// DefaultCompactor.Threshold when no per-tier entry matches. 0.85
// leaves headroom for one more full turn before hitting the actual
// context wall, and matches the historical universal default so
// frontier sessions see no behavior change.
const DefaultCompactionThreshold = 0.85

// NewDefaultCompactor returns a DefaultCompactor with the package-
// default fallback threshold AND the default per-tier overrides
// from modeltier.DefaultCompactionThresholds — so small/mid/frontier
// each get a tier-appropriate trigger out of the box. Pass a
// &DefaultCompactor{...} literal directly to customize either knob.
func NewDefaultCompactor() Compactor {
	return &DefaultCompactor{
		Threshold:       DefaultCompactionThreshold,
		ThresholdByTier: modeltier.DefaultCompactionThresholds(),
	}
}

// ShouldCompact returns true when the agent's usage tracker reports
// context-window utilization at or above the resolved threshold for
// the current model's tier. Returns false when the tracker doesn't
// yet know the window size (no turn has landed, or the model isn't
// in usage.ContextWindowSizeFor's table) so a session with an
// unknown model never triggers premature compaction.
func (c *DefaultCompactor) ShouldCompact(_ context.Context, a *Agent) bool {
	if a == nil || a.tracker == nil {
		return false
	}
	size := a.tracker.ContextWindowSize()
	if size == 0 {
		return false
	}
	used := a.tracker.ContextWindowUsed()
	threshold := c.resolveThreshold(a)
	return float64(used)/float64(size) >= threshold
}

// resolveThreshold picks the compaction trigger for the current
// session's model. Per-tier overrides win; fall through to
// c.Threshold; final fallback is the package default. Exposed for
// observability via the wrapping interfaces in the rest of the
// package — kept lowercase because the resolution policy is an
// implementation detail of this Compactor.
func (c *DefaultCompactor) resolveThreshold(a *Agent) float64 {
	if len(c.ThresholdByTier) > 0 {
		classifier := c.TierClassifier
		if classifier == nil {
			classifier = modeltier.Classify
		}
		if last, ok := a.tracker.Last(); ok {
			if tier := classifier(last.Model); tier != "" {
				if v, present := c.ThresholdByTier[tier]; present && v > 0 {
					return v
				}
			}
		}
	}
	if c.Threshold > 0 {
		return c.Threshold
	}
	return DefaultCompactionThreshold
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
// current session's history and writes the result as a "summary"
// marker event the history-slicing path in Run picks up on the
// next turn. Used both programmatically and by the TUI's /compact
// slash. See Agent.Checkpoint for the task-boundary variant that
// shares this machinery.
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
	out, err := a.runSummarizer(ctx, summarizerSpec{
		operation:         "Compact",
		systemInstruction: a.compactor.SummarizerInstruction(focus),
		tag:               CompactionEventTag,
		noteKey:           CompactionFocusKey,
		note:              focus,
	})
	if err != nil {
		return CompactionResult{}, err
	}
	if out.Skipped {
		return CompactionResult{Skipped: true}, nil
	}
	a.mu.Lock()
	a.compactionPending = false
	a.mu.Unlock()
	return CompactionResult{
		SummaryEventID: out.SummaryEventID,
		SummaryText:    out.SummaryText,
		Duration:       out.Duration,
	}, nil
}

// summarizerSpec carries the per-call configuration runSummarizer
// needs to write either a "summary" (compaction) or "checkpoint"
// (task-boundary) marker event. Internal contract — not exported.
type summarizerSpec struct {
	operation         string         // "Compact" | "Checkpoint" — for error messages
	systemInstruction string         // model-facing prompt
	tag               string         // CompactionEventTag | CheckpointEventTag
	noteKey           string         // CompactionFocusKey | CheckpointNoteKey
	note              string         // operator hint or task note (empty OK)
	extraMetadata     map[string]any // optional additional CustomMetadata to merge
}

// summarizerOutcome bundles the per-call result fields the public
// Compact / Checkpoint methods map into their respective result
// types. Internal contract — not exported.
type summarizerOutcome struct {
	SummaryEventID string
	SummaryText    string
	Duration       time.Duration
	Skipped        bool
}

// runSummarizer is the shared body for Compact and Checkpoint. It
// loads the unsliced session history, runs a tool-less LLM call
// with the supplied system instruction, and persists the result
// as a marker event with the spec's tag + metadata. Returns
// Skipped=true (no error) when there's no history to summarize.
//
// Callers (Compact / Checkpoint) are responsible for the wired-
// implementation precondition checks (ErrNoCompactor / ErrNoCheckpointer)
// before calling this — runSummarizer assumes the agent has a
// model and session.Service.
func (a *Agent) runSummarizer(ctx context.Context, spec summarizerSpec) (summarizerOutcome, error) {
	if a.model == nil {
		return summarizerOutcome{}, fmt.Errorf("agent: %s: no model wired (construct via agent.New)", spec.operation)
	}
	if a.sessionService == nil {
		return summarizerOutcome{}, fmt.Errorf("agent: %s: no session.Service wired", spec.operation)
	}

	// Load the full session history — unsliced. The summarizer is
	// the one place that wants to see EVERYTHING (so the summary
	// can capture the early-conversation context that's about to
	// be dropped from future turns).
	history, err := a.sessionHistory(ctx)
	if err != nil {
		return summarizerOutcome{}, fmt.Errorf("agent: %s: load history: %w", spec.operation, err)
	}
	if len(history) == 0 {
		// Nothing to summarize. Don't write an empty marker — that
		// would cause the next turn to start with a useless "[no
		// history]" prefix.
		return summarizerOutcome{Skipped: true}, nil
	}

	req := &adkmodel.LLMRequest{
		Contents: history,
		Config: &genai.GenerateContentConfig{
			SystemInstruction: genai.NewContentFromText(spec.systemInstruction, genai.RoleUser),
		},
		// Tools intentionally nil — summarization is tool-less.
	}

	start := time.Now()
	var b strings.Builder
	// Capture usage from the streamed response. The summarizer is a
	// single LLM call (one TurnComplete), so we just overwrite as
	// events arrive and commit once after the loop — same shape as
	// the subtask tracker.Append in subtask.go's Run loop. Without
	// this, summarizer turns escape /stats accounting (issue #61) and
	// therefore also escape the --max-turn-cost-usd / --max-session-
	// cost-usd ceilings from #145.
	var lastIn, lastOut int
	var lastMeta *genai.GenerateContentResponseUsageMetadata
	for resp, err := range a.model.GenerateContent(ctx, req, false) {
		if err != nil {
			return summarizerOutcome{}, fmt.Errorf("agent: %s: generate: %w", spec.operation, err)
		}
		if resp != nil && resp.UsageMetadata != nil {
			lastIn = int(resp.UsageMetadata.PromptTokenCount)
			lastOut = int(resp.UsageMetadata.CandidatesTokenCount)
			lastMeta = resp.UsageMetadata
		}
		if resp == nil || resp.Content == nil || resp.Partial {
			continue
		}
		for _, p := range resp.Content.Parts {
			if p != nil && p.Text != "" {
				b.WriteString(p.Text)
			}
		}
	}
	elapsed := time.Since(start)
	a.recordInternalLLMUsage(lastIn, lastOut, lastMeta)
	summary := strings.TrimSpace(b.String())
	if summary == "" {
		return summarizerOutcome{}, fmt.Errorf("agent: %s: model returned no summary text", spec.operation)
	}

	id, err := a.appendBoundaryEvent(ctx, summary, spec)
	if err != nil {
		return summarizerOutcome{}, fmt.Errorf("agent: %s: persist: %w", spec.operation, err)
	}
	return summarizerOutcome{
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

// appendBoundaryEvent writes a marker event (either summary or
// checkpoint, per spec.tag) carrying the summary text to the
// session. The event's CustomMetadata carries
// CompactionMetadataKey: spec.tag so the history-slicing scanner
// in sliceFromBoundary finds it cheaply, plus the spec.noteKey
// holding the operator/model note, plus any extra metadata the
// caller wants to attach (e.g. CheckpointStartedKey).
func (a *Agent) appendBoundaryEvent(ctx context.Context, summary string, spec summarizerSpec) (string, error) {
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
	md := map[string]any{
		CompactionMetadataKey: spec.tag,
	}
	if spec.noteKey != "" {
		md[spec.noteKey] = spec.note
	}
	for k, v := range spec.extraMetadata {
		md[k] = v
	}
	ev := &session.Event{
		ID:        newBoundaryEventID(spec.tag),
		Author:    a.agentName,
		Timestamp: time.Now(),
		LLMResponse: adkmodel.LLMResponse{
			Content:        genai.NewContentFromText(summary, genai.RoleModel),
			CustomMetadata: md,
		},
	}
	if err := a.sessionService.AppendEvent(ctx, resp.Session, ev); err != nil {
		return "", err
	}
	return ev.ID, nil
}

// newBoundaryEventID returns a unique-enough event ID, prefixed
// with the boundary tag so audit-log greps can filter cheaply.
// Format: "<tag>-<unix-nanos>".
func newBoundaryEventID(tag string) string {
	return fmt.Sprintf("%s-%d", tag, time.Now().UnixNano())
}

// findLatestBoundary scans events newest-first for one carrying
// the CompactionMetadataKey marker with EITHER recognized value
// (CompactionEventTag for compaction summaries, CheckpointEventTag
// for task-boundary checkpoints). Both kinds act as history-
// slicing boundaries — the latest one wins.
//
// Returns the matching index, event, and the tag value found
// (caller may want to distinguish for telemetry). Returns
// (-1, nil, "") when no boundary marker is present.
func findLatestBoundary(events []*session.Event) (int, *session.Event, string) {
	for i := len(events) - 1; i >= 0; i-- {
		ev := events[i]
		if ev == nil || ev.CustomMetadata == nil {
			continue
		}
		v, ok := ev.CustomMetadata[CompactionMetadataKey]
		if !ok {
			continue
		}
		s, ok := v.(string)
		if !ok {
			continue
		}
		if s == CompactionEventTag || s == CheckpointEventTag {
			return i, ev, s
		}
	}
	return -1, nil, ""
}

// compactionPrefix wraps the summary text on the user-side so the
// resuming model treats it as prior-conversation context rather
// than as a fresh task description. The wording is deliberately
// imperative about what to do when the user asks about prior
// discussion — without that, models read the structured handover
// as opaque "latent state" they need to verify by running tools.
//
// Iteration history:
//   - v1 (PR I): "Treat it as conversational context (NOT as a
//     fresh task description)." Fixed slicing-engaged-but-empty
//     symptom (model ran list_dir 7 times after /compact). But
//     Gemini Flash still interpreted "recap" requests as
//     "acknowledge you have context" rather than "list what was
//     discussed" — it understood the summary was THERE but didn't
//     understand it was QUOTABLE.
//   - v2 (PR III): names the recap-class failure mode directly +
//     forbids tool-based rediscovery. Paired with the new
//     DefaultInstruction sentence below for belt-and-suspenders
//     coverage on instruction-following-weak models.
const compactionPrefix = "[Conversation compacted. Below is the handover summary of everything we discussed. When the user asks what we discussed, recapped, covered, or worked on, read FROM the summary and answer with its contents — do not run tools to rediscover what's already written here.]\n\n"

// checkpointPrefix is the kind-specific framing for task-boundary
// checkpoint events. Compaction's prefix above works for the
// "we hit the context wall mid-task" case but failed in the
// 2026-05-27 smoke for the checkpoint case: the checkpoint
// summary starts with a "# Task" section that flash-tier models
// read as "everything's done, fresh start" — and then they treat
// the next prompt as a NEW task and re-run tools the summary
// already records. This prefix names the conversation-continues
// invariant explicitly + addresses the recap case for prior-task
// questions specifically, since "what we discussed" framing
// doesn't quite cover "what files did we touch last task." Same
// suffix as compaction.
const checkpointPrefix = "[The prior task is complete and its conversation history has been sliced from your context. A completion record of that task is below — read it as authoritative shared knowledge of what was done. The conversation CONTINUES from here; the next user message is part of the SAME ongoing session, not a fresh start. When the user asks anything about the prior task — what was done, what files were touched, what was learned, recap, summary, status — read FROM the record below and answer with its contents. Do not re-run tools to rediscover what's already recorded here.]\n\n"

const compactionSuffix = "\n\n[End of summary. The actual user message follows below.]"

// prefixForTag picks the right user-side framing for a boundary
// event based on its CompactionMetadataKey value. Defaults to the
// compaction prefix for unknown tags so a forward-compat scenario
// (newer agent writes a new boundary kind, older code reads it)
// degrades to the safer wording.
func prefixForTag(tag string) string {
	switch tag {
	case CheckpointEventTag:
		return checkpointPrefix
	case CompactionEventTag:
		return compactionPrefix
	default:
		return compactionPrefix
	}
}

// sliceFromBoundary returns events from the latest boundary
// (summary OR checkpoint) forward, with the boundary event itself
// rewritten to RoleUser and wrapped with framing so the resuming
// model treats it as "the user told me this is where we are"
// rather than as an opaque structured document.
//
// The original events slice is not mutated; a new slice is
// returned containing a shallow copy of the boundary event with
// the rewritten role + prefix/suffix framing parts.
//
// When no boundary is present, returns the original slice
// unchanged.
func sliceFromBoundary(events []*session.Event) []*session.Event {
	idx, boundary, tag := findLatestBoundary(events)
	if idx < 0 || boundary == nil {
		return events
	}
	// Shallow-copy the boundary event so we can rewrite its role
	// and wrap its parts without mutating the audit log.
	rewritten := *boundary
	if boundary.Content != nil {
		c := *boundary.Content
		c.Role = genai.RoleUser
		// Frame the summary text so the model understands it's
		// prior-conversation context. Prefix tells it what's
		// coming; suffix marks the boundary back to the operator's
		// actual question. Prefix is kind-aware (compaction vs
		// checkpoint) because the model interprets the two
		// summary shapes differently — see prefixForTag for
		// the why.
		framed := make([]*genai.Part, 0, len(c.Parts)+2)
		framed = append(framed, &genai.Part{Text: prefixForTag(tag)})
		framed = append(framed, c.Parts...)
		framed = append(framed, &genai.Part{Text: compactionSuffix})
		c.Parts = framed
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
	// Unwrap if this is one of our sliced views — the inner
	// session.Service type-asserts the session to its own
	// concrete type (e.g. ADK's InMemoryService rejects
	// *slicedSession with "unexpected session type"). Reads went
	// through the wrapper to get a sliced Events() view; writes
	// must go straight back to the real backing storage.
	if sliced, ok := sess.(*slicedSession); ok {
		sess = sliced.inner
	}
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
	sliced := sliceFromBoundary(all)
	if len(sliced) == len(all) {
		// No boundary marker; pass through unchanged.
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
