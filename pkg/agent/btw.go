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
	"strings"

	"google.golang.org/genai"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/adk/session"

	"github.com/go-steer/core-agent/v2/pkg/attach"
	"github.com/go-steer/core-agent/v2/pkg/models"
)

// AskSideQuestion runs one tool-less LLM call that sees the agent's
// current conversation history plus the supplied question, and
// returns the model's answer as a single string. Intended for the
// TUI's /btw side-question flow (docs/operator-input-design.md
// layer C): the operator asks a quick context-grounded question
// ("what was that file again?") without polluting the main
// conversation. The call bypasses Agent.Run entirely — no inbox
// drain, no permission gating, no event-log writeback, no tools.
//
// The question is appended to the existing history as a transient
// user turn that exists only for this one call; nothing about the
// agent's persisted session state changes. The model can therefore
// reference prior tool output, prior assistant turns, prior user
// messages — but cannot call any tool to do new work.
//
// Errors:
//   - context cancellation: returns ctx.Err().
//   - no session.Service wired: returns a clear error (defensive;
//     agent.New always installs one, but hand-constructed Agents
//     used in tests don't).
//   - GenerateContent failures bubble up unchanged so callers can
//     distinguish transport vs API vs model errors via errors.Is.
//   - the model answering with no text: *SideQuestionEmptyError,
//     which wraps ErrSideQuestionEmpty. Callers are expected to
//     render it as "no answer" rather than as a failed call — see
//     that type's doc comment.
func (a *Agent) AskSideQuestion(ctx context.Context, question string) (string, error) {
	if a == nil {
		return "", errors.New("agent: AskSideQuestion: nil receiver")
	}
	if a.model == nil {
		// Defensive: a hand-constructed Agent struct may have skipped
		// the model wiring. agent.New always sets it.
		return "", errors.New("agent: AskSideQuestion: no model wired (construct via agent.New)")
	}
	question = strings.TrimSpace(question)
	if question == "" {
		return "", errors.New("agent: AskSideQuestion: empty question")
	}

	history, err := a.sessionHistory(ctx)
	if err != nil {
		return "", fmt.Errorf("agent: AskSideQuestion: load history: %w", err)
	}
	// The status preamble rides in the SAME user content as the
	// question rather than as a content of its own. Two reasons: it
	// reads as "here is the state, now answer this" instead of as a
	// stray turn the model has to guess the purpose of, and it keeps
	// the number of appended contents at exactly one — the shape this
	// call had before the preamble existed. Appending a second
	// consecutive user content would be a new thing to be wrong about
	// on providers that care about role alternation.
	prompt := question
	if preamble := a.sideQuestionStatus(); preamble != "" {
		prompt = preamble + "\n\n" + question
	}
	history = append(history, genai.NewContentFromText(prompt, genai.RoleUser))

	req := &adkmodel.LLMRequest{
		Contents: history,
		// Explicitly non-nil with an empty tool set. A nil Config is not
		// the same as "no tools": the Gemini wrapper creates the Config
		// itself and appends its server-side built-ins into it, so a nil
		// Config is how /btw ended up with google_search despite being
		// documented as tool-less.
		Config: &genai.GenerateContentConfig{Tools: nil},
	}

	// A side question is a one-shot: tool-less, no system instruction,
	// and the appended question makes the prefix unique to this call.
	// Writing a prompt-cache entry for it would cost the 1.25x write
	// premium for a read that never comes.
	ctx = models.WithoutPromptCache(ctx)
	// And genuinely tool-less: no provider-injected built-ins, and no
	// context-cache reference stamped onto a request that already
	// carries the whole history.
	ctx = models.WithoutBuiltins(ctx)

	// Capture usage and commit once after the loop — see
	// recordInternalLLMUsage's docstring for the shape. /btw was the
	// second internal-LLM caller bypassing the tracker before #61's
	// fix.
	var lastIn, lastOut int
	var lastMeta *genai.GenerateContentResponseUsageMetadata
	var lastCustom map[string]any
	var b strings.Builder
	var detail string
	for resp, err := range a.model.GenerateContent(ctx, req, false) {
		if err != nil {
			// "The model produced nothing" is an answer here, not a
			// failure. The Gemini adapter raises it as an error on
			// purpose — inside the agentic loop a contentless turn is a
			// hang, so it retries once and then escalates (#220) — but
			// for a one-shot question the escalation IS the bug the
			// operator reported: a paragraph of provider prose where an
			// answer should be. Keep the retry, drop the escalation.
			if errors.Is(err, models.ErrEmptyResponse) {
				// A refusal still costs tokens (twice over, since the
				// adapter retried) — record what we saw before giving up
				// so the cost the operator is shown stays honest.
				a.recordInternalLLMUsage(lastIn, lastOut, lastMeta, lastCustom)
				return "", &SideQuestionEmptyError{Detail: detail}
			}
			return "", fmt.Errorf("agent: AskSideQuestion: generate: %w", err)
		}
		if d := emptyAnswerDetail(resp); d != "" {
			detail = d
		}
		if resp != nil && resp.UsageMetadata != nil {
			lastIn = int(resp.UsageMetadata.PromptTokenCount)
			lastOut = int(resp.UsageMetadata.CandidatesTokenCount)
			lastMeta = resp.UsageMetadata
			lastCustom = resp.CustomMetadata
		}
		if resp == nil || resp.Content == nil {
			continue
		}
		// Only accumulate final (non-partial) text. Partials carry
		// streaming chunks that the runner re-emits; for a one-shot
		// side question we want the committed turn's full text once.
		// Some providers omit Partial and ship one final response —
		// that case is also covered.
		if resp.Partial {
			continue
		}
		for _, p := range resp.Content.Parts {
			if p != nil && p.Text != "" {
				b.WriteString(p.Text)
			}
		}
	}
	a.recordInternalLLMUsage(lastIn, lastOut, lastMeta, lastCustom)
	out := strings.TrimSpace(b.String())
	if out == "" {
		return "", &SideQuestionEmptyError{Detail: detail}
	}
	return out, nil
}

// ErrSideQuestionEmpty is the sentinel every empty side-question answer
// wraps. Match on it (errors.Is) to tell "the model had nothing to say"
// apart from a transport, auth, or API failure — the two want opposite
// treatments at every surface: the first is an answer of sorts and
// renders inline, the second is an error and renders as one.
//
// Before this existed, /btw returned a bare errors.New for the empty
// case, so a thought-only response and a dead endpoint looked identical
// to the TUI. That is the "blank/infra error" symptom the operator
// reported: the response WAS empty, and the surface had no way to say
// so except by failing.
var ErrSideQuestionEmpty = errors.New("agent: AskSideQuestion: model returned no text")

// SideQuestionEmptyError carries whatever the provider said about WHY
// the answer was empty. Detail is free-form and may be empty (some
// providers just return nothing); when set it looks like
// "finish_reason=SAFETY" or "error=RESOURCE_EXHAUSTED: ...", which is
// the difference between "retry" and "rephrase" for the operator.
type SideQuestionEmptyError struct {
	Detail string
}

func (e *SideQuestionEmptyError) Error() string {
	if e == nil || e.Detail == "" {
		return ErrSideQuestionEmpty.Error()
	}
	return ErrSideQuestionEmpty.Error() + " (" + e.Detail + ")"
}

func (e *SideQuestionEmptyError) Unwrap() error { return ErrSideQuestionEmpty }

// emptyAnswerDetail extracts the provider's explanation for a
// text-less response. Returns "" when the response carries no
// explanation, which is itself the common case on a plain
// FinishReason=STOP with empty parts.
//
// STOP is deliberately reported: on a side question it is not the
// benign "done" it is mid-loop — it means the model chose to end the
// turn without saying anything, and an operator staring at a blank
// overlay deserves to know that's what happened.
func emptyAnswerDetail(resp *adkmodel.LLMResponse) string {
	if resp == nil {
		return ""
	}
	switch {
	case resp.ErrorCode != "" && resp.ErrorMessage != "":
		return "error=" + resp.ErrorCode + ": " + resp.ErrorMessage
	case resp.ErrorCode != "":
		return "error=" + resp.ErrorCode
	case resp.ErrorMessage != "":
		return "error=" + resp.ErrorMessage
	case resp.FinishReason != "":
		return "finish_reason=" + string(resp.FinishReason)
	}
	return ""
}

// sideQuestionStatus renders the compact run-state block prepended to
// every side question. Returns "" when there's nothing worth saying
// (no model, no tracker, nothing running) so a bare agent doesn't pay
// for a block of empty fields.
//
// This is what makes the operator's actual questions answerable. "How
// much has this cost?" and "what are you doing right now?" are the
// motivating asks for /btw, and neither is in the transcript: cost
// lives in the usage tracker, run-state lives on the agent, and the
// queue lives in the inbox. The model saw none of it before this. A
// few dozen tokens is a cheap price for turning "I don't have access
// to that" into an answer.
//
// Sourced from the same projections AttachStatus / AttachUsage build,
// deliberately as prose rather than JSON — it's read by a model, not
// parsed by a client, and prose survives a field being unavailable
// without leaving a null behind.
func (a *Agent) sideQuestionStatus() string {
	if a == nil {
		return ""
	}
	var lines []string

	state := "idle (waiting for the next turn)"
	if a.turnInFlight() {
		state = "running (a turn is in flight right now)"
	}
	if st := a.PauseState(); st.Paused {
		state = "paused (" + st.Reason + ")"
		if st.Interrupted {
			state += " — a turn was cancelled on the way in"
		}
	}
	lines = append(lines, "state: "+state)

	second := ""
	if name := a.ModelName(); name != "" {
		second = "model: " + name
	}
	if t := a.Tracker(); t != nil {
		totals := t.Totals()
		if second != "" {
			second += " · "
		}
		second += fmt.Sprintf("turns: %d · cost so far: $%.2f", totals.Turns, totals.CostUSD)
	}
	if second != "" {
		lines = append(lines, second)
	}

	third := fmt.Sprintf("pending inbox: %d", a.PendingInboxCount())
	if running := a.runningSubagentNames(); len(running) > 0 {
		third += fmt.Sprintf(" · background subagents: %d running (%s)",
			len(running), strings.Join(running, ", "))
	}
	lines = append(lines, third)

	return "[Session status at the time of this question]\n" + strings.Join(lines, "\n")
}

// runningSubagentNames lists the live background subagents by name.
// Nil manager (the common embedded case) yields nothing.
func (a *Agent) runningSubagentNames() []string {
	mgr := a.BackgroundManager()
	if mgr == nil {
		return nil
	}
	var out []string
	for _, info := range mgr.ListSubagents() {
		if info.Status == attach.AgentStatusRunning {
			out = append(out, info.Name)
		}
	}
	return out
}

// sessionHistory pulls the current session's events from the
// configured session.Service and renders them as a []*genai.Content
// slice ready to feed into LLMRequest.Contents. Background-subagent
// events (Branch != "") are filtered out so the side question sees
// only the operator-visible conversation. Partial events are
// skipped — they're streaming chunks of an in-flight turn, not
// committed history.
func (a *Agent) sessionHistory(ctx context.Context) ([]*genai.Content, error) {
	if a.sessionService == nil {
		return nil, errors.New("no session.Service wired")
	}
	resp, err := a.sessionService.Get(ctx, &session.GetRequest{
		AppName:   a.appName,
		UserID:    a.userID,
		SessionID: a.sessionID,
	})
	if err != nil {
		// A side question fired before any Run() call has no prior
		// turns and the in-memory service returns "session not
		// found". That's not a real error from the operator's
		// perspective — treat it as empty history so /btw still
		// works on a fresh agent.
		if strings.Contains(err.Error(), "not found") {
			return nil, nil
		}
		return nil, err
	}
	if resp == nil || resp.Session == nil {
		return nil, nil
	}
	var out []*genai.Content
	for ev := range resp.Session.Events().All() {
		if ev == nil {
			continue
		}
		if ev.Branch != "" {
			continue // background subagent event; skip
		}
		if ev.Partial {
			continue
		}
		if ev.Content == nil || len(ev.Content.Parts) == 0 {
			continue
		}
		out = append(out, ev.Content)
	}
	// Same call/response normalization the summarizer path applies —
	// side-question history is raw event contents too (#541).
	return normalizeToolPairs(out), nil
}
