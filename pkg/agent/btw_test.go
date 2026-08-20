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
	"sync"
	"testing"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v2/pkg/models"
	"github.com/go-steer/core-agent/v2/pkg/usage"
)

// captureLLM is a tiny adkmodel.LLM that records every request it's
// asked to generate, so tests can assert what conversation history
// reached the model. Optionally emits UsageMetadata (inputTokens /
// outputTokens) so cost-rollup tests can verify the parent's
// tracker picks up subtask usage.
type captureLLM struct {
	mu           sync.Mutex
	reqs         []*adkmodel.LLMRequest
	response     string
	err          error
	inputTokens  int32 // optional: include in UsageMetadata on the response
	outputTokens int32 // optional: include in UsageMetadata on the response
	// cachedInputTokens, when > 0, is the cache-READ bucket, reported
	// where every adapter reports it: genai's CachedContentTokenCount.
	// It is a SUBSET of inputTokens.
	cachedInputTokens int32
	// cacheWriteTokens, when > 0, is reported the way the Anthropic
	// adapter reports it: on CustomMetadata, since genai's usage struct
	// has no third input bucket (#263). It is a SUBSET of inputTokens.
	cacheWriteTokens int64
	// cacheWrite1hTokens, when > 0, is the share of cacheWriteTokens
	// that went to a 1-hour breakpoint (#770). It is a SUBSET of
	// cacheWriteTokens, which is itself a subset of inputTokens.
	cacheWrite1hTokens int64
	// noPromptCache records, per request, whether the caller marked its
	// context as a one-shot (models.WithoutPromptCache). Parallel to
	// reqs.
	noPromptCache []bool
	// noBuiltins records, per request, whether the caller marked its
	// context as tool-less (models.WithoutBuiltins). Parallel to reqs.
	noBuiltins []bool
	// finishReason, when set, is stamped on the response — the shape a
	// provider uses to explain a text-less answer.
	finishReason genai.FinishReason
}

func (l *captureLLM) Name() string { return "capture" }

func (l *captureLLM) GenerateContent(ctx context.Context, req *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	l.mu.Lock()
	l.reqs = append(l.reqs, req)
	l.noPromptCache = append(l.noPromptCache, models.PromptCacheSuppressed(ctx))
	l.noBuiltins = append(l.noBuiltins, models.BuiltinsSuppressed(ctx))
	resp := l.response
	err := l.err
	in := l.inputTokens
	out := l.outputTokens
	cached := l.cachedInputTokens
	writes := l.cacheWriteTokens
	writes1h := l.cacheWrite1hTokens
	finish := l.finishReason
	l.mu.Unlock()
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		if err != nil {
			yield(nil, err)
			return
		}
		r := &adkmodel.LLMResponse{
			Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: resp}}},
			TurnComplete: true,
			FinishReason: finish,
		}
		if in > 0 || out > 0 {
			r.UsageMetadata = &genai.GenerateContentResponseUsageMetadata{
				PromptTokenCount:        in,
				CachedContentTokenCount: cached,
				CandidatesTokenCount:    out,
				TotalTokenCount:         in + out,
			}
		}
		if writes > 0 {
			r.CustomMetadata = map[string]any{usage.CacheCreationTokensMetadataKey: writes}
			if writes1h > 0 {
				r.CustomMetadata[usage.CacheCreation1hTokensMetadataKey] = writes1h
			}
		}
		yield(r, nil)
	}
}

func (l *captureLLM) lastRequest() *adkmodel.LLMRequest {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.reqs) == 0 {
		return nil
	}
	return l.reqs[len(l.reqs)-1]
}

func TestAskSideQuestion_ReturnsModelTextAndHasNoTools(t *testing.T) {
	t.Parallel()
	llm := &captureLLM{response: "It was main.go."}
	a, err := New(llm)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ans, err := a.AskSideQuestion(context.Background(), "what was that file again?")
	if err != nil {
		t.Fatalf("AskSideQuestion: %v", err)
	}
	if ans != "It was main.go." {
		t.Errorf("answer = %q, want %q", ans, "It was main.go.")
	}
	req := llm.lastRequest()
	if req == nil {
		t.Fatalf("no request recorded on the model")
	}
	if len(req.Tools) != 0 {
		t.Errorf("Tools = %d, want 0 (side queries are tool-less)", len(req.Tools))
	}
	if len(req.Contents) == 0 {
		t.Fatalf("Contents empty, want at least the question")
	}
	last := req.Contents[len(req.Contents)-1]
	if last.Role != genai.RoleUser {
		t.Errorf("last content role = %q, want user", last.Role)
	}
	gotQ := ""
	for _, p := range last.Parts {
		if p != nil && p.Text != "" {
			gotQ += p.Text
		}
	}
	if !strings.Contains(gotQ, "what was that file again?") {
		t.Errorf("last content text = %q, want it to include the question", gotQ)
	}
}

func TestAskSideQuestion_EmptyQuestionErrors(t *testing.T) {
	t.Parallel()
	llm := &captureLLM{response: "irrelevant"}
	a, err := New(llm)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := a.AskSideQuestion(context.Background(), "   "); err == nil {
		t.Errorf("expected error for empty question, got nil")
	}
}

func TestAskSideQuestion_PropagatesModelError(t *testing.T) {
	t.Parallel()
	llm := &captureLLM{err: errMockBoom}
	a, err := New(llm)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := a.AskSideQuestion(context.Background(), "ping"); err == nil {
		t.Errorf("expected wrapped model error, got nil")
	} else if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error = %q, want it to wrap boom", err.Error())
	}
}

func TestAskSideQuestion_BypassesAgentRun(t *testing.T) {
	t.Parallel()
	// Use the agent's inbox + a queued message to prove the side
	// question does NOT trigger the pre-turn inbox drain that
	// Agent.Run performs. If AskSideQuestion went through Run, the
	// queued message would land in the model's request — and the
	// inbox would be empty after the call.
	llm := &captureLLM{response: "ack"}
	a, err := New(llm)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := a.Inject("a queued operator note"); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if _, err := a.AskSideQuestion(context.Background(), "side q"); err != nil {
		t.Fatalf("AskSideQuestion: %v", err)
	}
	if got := a.PendingInboxCount(); got != 1 {
		t.Errorf("inbox count after /btw = %d, want 1 (side query must not drain)", got)
	}
	req := llm.lastRequest()
	for _, c := range req.Contents {
		for _, p := range c.Parts {
			if p != nil && strings.Contains(p.Text, "queued operator note") {
				t.Errorf("queued inbox note leaked into side-query request: %#v", p)
			}
		}
	}
}

// A text-less answer is an OUTCOME, not a failure. Before this it came
// back as a bare errors.New, so every surface rendered a
// safety-blocked or thought-only response identically to a dead
// endpoint — the "blank / infra error" symptom /btw was reported for.
func TestAskSideQuestion_EmptyAnswerIsTypedWithDetail(t *testing.T) {
	t.Parallel()
	llm := &captureLLM{response: "", finishReason: genai.FinishReasonSafety}
	a, err := New(llm)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = a.AskSideQuestion(context.Background(), "why did that fail?")
	if err == nil {
		t.Fatal("AskSideQuestion returned nil error for an empty answer")
	}
	if !errors.Is(err, ErrSideQuestionEmpty) {
		t.Fatalf("err = %v, want it to wrap ErrSideQuestionEmpty so callers can "+
			"tell 'nothing to say' apart from a transport failure", err)
	}
	var empty *SideQuestionEmptyError
	if !errors.As(err, &empty) {
		t.Fatalf("err = %v (%T), want *SideQuestionEmptyError", err, err)
	}
	if empty.Detail != "finish_reason=SAFETY" {
		t.Errorf("Detail = %q, want %q — without the reason the operator can't tell "+
			"'rephrase it' from 'retry it'", empty.Detail, "finish_reason=SAFETY")
	}
}

// The Gemini adapter turns a contentless turn into an ERROR on
// purpose: inside the agentic loop it's a hang, so it retries once and
// then escalates (#220). A side question has to undo that escalation —
// otherwise the single most common blank (`FinishReason=STOP`, no
// parts) reaches the operator as a paragraph about silent safety
// filters and transient Vertex faults, which is exactly the
// infra-error-instead-of-an-answer symptom, arriving through the one
// path the typed empty error doesn't cover.
func TestAskSideQuestion_ProviderEmptyResponseErrorIsAnEmptyAnswer(t *testing.T) {
	t.Parallel()
	llm := &captureLLM{
		// The shape gemini.ErrEmptyResponse has: adapter prose wrapping
		// the shared sentinel.
		err: fmt.Errorf("gemini: model returned no usable content (%w)", models.ErrEmptyResponse),
	}
	a, err := New(llm)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = a.AskSideQuestion(context.Background(), "what happened?")
	if !errors.Is(err, ErrSideQuestionEmpty) {
		t.Fatalf("err = %v, want the provider's empty-response error to become an empty ANSWER", err)
	}
	// And it must not still look like a provider failure to a caller
	// deciding between "render inline" and "show an error".
	if errors.Is(err, models.ErrEmptyResponse) {
		t.Error("the adapter's error leaked through; surfaces would still render it as a failure")
	}
}

// Guard the boundary: any other provider error stays an error.
func TestAskSideQuestion_OtherProviderErrorsStayErrors(t *testing.T) {
	t.Parallel()
	llm := &captureLLM{err: errors.New("dial tcp: connection refused")}
	a, err := New(llm)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = a.AskSideQuestion(context.Background(), "what happened?")
	if err == nil {
		t.Fatal("AskSideQuestion returned nil for a transport failure")
	}
	if errors.Is(err, ErrSideQuestionEmpty) {
		t.Errorf("a transport failure was reported as an empty answer: %v", err)
	}
}

func TestAskSideQuestion_EmptyAnswerWithNoProviderReason(t *testing.T) {
	t.Parallel()
	llm := &captureLLM{response: ""}
	a, err := New(llm)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = a.AskSideQuestion(context.Background(), "anything?")
	var empty *SideQuestionEmptyError
	if !errors.As(err, &empty) {
		t.Fatalf("err = %v (%T), want *SideQuestionEmptyError", err, err)
	}
	if empty.Detail != "" {
		t.Errorf("Detail = %q, want empty (the provider explained nothing)", empty.Detail)
	}
}

// A nil Config is not "no tools": the Gemini wrapper builds the Config
// itself and appends its server-side built-ins into it. /btw has to say
// tool-less out loud, both on the request and on the context.
func TestAskSideQuestion_IsExplicitlyToolLess(t *testing.T) {
	t.Parallel()
	llm := &captureLLM{response: "sure"}
	a, err := New(llm)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := a.AskSideQuestion(context.Background(), "ping"); err != nil {
		t.Fatalf("AskSideQuestion: %v", err)
	}

	req := llm.lastRequest()
	if req.Config == nil {
		t.Fatal("req.Config is nil — the provider wrapper would create it and inject " +
			"google_search/url_context into a request documented as tool-less")
	}
	if len(req.Config.Tools) != 0 {
		t.Errorf("req.Config.Tools = %d, want 0", len(req.Config.Tools))
	}
	llm.mu.Lock()
	defer llm.mu.Unlock()
	if len(llm.noBuiltins) != 1 || !llm.noBuiltins[0] {
		t.Errorf("noBuiltins = %v, want [true] — the context marker is what stops the "+
			"provider appending built-ins and stamping a cache reference", llm.noBuiltins)
	}
}

// The motivating operator questions ("what are you doing?", "how much
// has this cost?") aren't answerable from the transcript. The preamble
// is what puts the answer in the window.
func TestAskSideQuestion_PrependsSessionStatus(t *testing.T) {
	t.Parallel()
	llm := &captureLLM{response: "running a bash tool"}
	a, err := New(llm, WithUsageTracker(usage.NewTracker()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := a.Inject("look at the logs"); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if _, err := a.AskSideQuestion(context.Background(), "what are you doing right now?"); err != nil {
		t.Fatalf("AskSideQuestion: %v", err)
	}

	req := llm.lastRequest()
	if len(req.Contents) == 0 {
		t.Fatal("Contents empty, want the question")
	}
	// The preamble rides in the question's own content — exactly one
	// content is appended, the same shape this call had before the
	// preamble existed.
	last := contentText(req.Contents[len(req.Contents)-1])
	for _, want := range []string{
		"[Session status at the time of this question]",
		"state: idle",
		"turns: 0",
		"cost so far: $0.00",
		"pending inbox: 1",
		"what are you doing right now?",
	} {
		if !strings.Contains(last, want) {
			t.Errorf("prompt missing %q:\n%s", want, last)
		}
	}
	// The status must precede the question: the model should read it as
	// context for what's being asked, not as a trailing afterthought.
	if strings.Index(last, "[Session status") > strings.Index(last, "what are you doing") {
		t.Errorf("status block came after the question:\n%s", last)
	}
}

func TestSideQuestionStatus_ReportsPaused(t *testing.T) {
	t.Parallel()
	llm := &captureLLM{response: "parked"}
	a, err := New(llm)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, paused := a.InterruptAndHold(""); !paused {
		t.Fatal("InterruptAndHold did not park the agent")
	}
	got := a.sideQuestionStatus()
	if !strings.Contains(got, "state: paused ("+PauseReasonOperatorInterrupt+")") {
		t.Errorf("status = %q, want it to report the park and its reason", got)
	}
}

// errMockBoom is a sentinel error used by TestAskSideQuestion_PropagatesModelError.
var errMockBoom = mockErr("boom")

type mockErr string

func (e mockErr) Error() string { return string(e) }
