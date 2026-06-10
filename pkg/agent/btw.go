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
	history = append(history, genai.NewContentFromText(question, genai.RoleUser))

	req := &adkmodel.LLMRequest{
		Contents: history,
		// Tools intentionally nil — the model answers from in-context
		// info only. Caller's responsibility to surface "I don't know"
		// when the answer isn't in history.
	}

	// Capture usage and commit once after the loop — see
	// recordInternalLLMUsage's docstring for the shape. /btw was the
	// second internal-LLM caller bypassing the tracker before #61's
	// fix.
	var lastIn, lastOut int
	var b strings.Builder
	for resp, err := range a.model.GenerateContent(ctx, req, false) {
		if err != nil {
			return "", fmt.Errorf("agent: AskSideQuestion: generate: %w", err)
		}
		if resp != nil && resp.UsageMetadata != nil {
			lastIn = int(resp.UsageMetadata.PromptTokenCount)
			lastOut = int(resp.UsageMetadata.CandidatesTokenCount)
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
	a.recordInternalLLMUsage(lastIn, lastOut)
	out := strings.TrimSpace(b.String())
	if out == "" {
		return "", errors.New("agent: AskSideQuestion: model returned no text")
	}
	return out, nil
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
	return out, nil
}
