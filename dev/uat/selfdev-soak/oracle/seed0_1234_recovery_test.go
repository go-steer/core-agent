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

//go:build a7oracle

// The task oracle for seed 0 of the self-development soak: the recovery
// half of #1234. grade_a7.py copies this file into pkg/agent of an
// exported tree at the run's base commit and at its PR tip, and runs it
// with -tags a7oracle. It must FAIL at the base and PASS at the tip.
//
// It never runs in place. The build tag keeps it out of `go test ./...`
// here, where it fails by design until #1234's recovery half lands, and
// it is kept out of the worker's tree because it is the answer key
// (docs/selfdev-soak-design.md, decision 20).
//
// It is an external test package and uses only exported API: New,
// WithCompactor, WithoutSessionTitle, Run, SessionService and the
// boundary metadata keys. A fix is free to restructure the compaction
// internals, so the oracle must not depend on them. It drives
// compaction the way a session does, through turns, with a Compactor
// whose threshold is always crossed.
//
// The contract, from #1234 ("Two separable fixes", fix 2):
//
//   - The summarizer answers an empty STOP on both attempts of one
//     compaction (summarizeWithRetry retries an unexplained empty STOP
//     once inside the call). An empty summary is a deterministic failure
//     class, so the mechanical boundary (MechanicalCompactionKey) must be
//     written after that single failed compaction, before the summarizer
//     is tried again, not after MechanicalCompactionAfterFailures of
//     them. Pre-fix code fails it with "the mechanical fallback came only
//     after 2 failed compactions".
//   - A 429 is different: the summarizer may come back, so backing off
//     is right and a single 429 must NOT write a boundary. This half
//     passes before and after the fix by design; it fails a fix that
//     falls back mechanically on every failure.
//
// Failure lines carry one of two markers so the grader can tell why the
// oracle failed: "A7 ORACLE UNFIXED:" means the contract above does not
// hold, which is the expected result at the base. "A7 ORACLE RIG:" means
// the oracle could not set up its own scenario; that is never evidence
// about the fix, at the base or the tip.

package agent_test

import (
	"context"
	"errors"
	"iter"
	"strings"
	"sync"
	"testing"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/adk/session"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v2/pkg/agent"
	"github.com/go-steer/core-agent/v2/pkg/models"
)

// a7SummarizerSentinel is in the system instruction a7Compactor hands
// the summarizer, so the scripted model can tell a summarizer request
// from a turn's request without depending on how the agent tags it.
const a7SummarizerSentinel = "A7-ORACLE-SUMMARIZER-REQUEST"

// a7MaxTurnsToFirstCompaction bounds the turns the oracle drives while
// waiting for the first compaction attempt.
const a7MaxTurnsToFirstCompaction = 4

// a7ExtraTurns is how many turns after the first failed compaction the
// oracle waits for the fallback. Pre-fix code backs off for two turns
// (compactionBackoffTurns(1)) and fails a second compaction in the
// third, so three turns is long enough for pre-fix code to show its
// two-failure fallback, and for a fix that falls back when one backoff
// expires.
const a7ExtraTurns = 3

// a7Compactor is over its threshold on every check.
type a7Compactor struct{}

func (a7Compactor) ShouldCompact(context.Context, *agent.Agent) bool { return true }

func (a7Compactor) SummarizerInstruction(string) string {
	return "Summarize the conversation. " + a7SummarizerSentinel
}

// a7Model answers a turn with text and a summarizer request with
// whatever summarize returns. It records the turn each summarizer call
// was made in.
type a7Model struct {
	summarize func() (*adkmodel.LLMResponse, error)

	mu          sync.Mutex
	turn        int
	summarizeAt []int
}

func (m *a7Model) Name() string { return "a7-oracle" }

func (m *a7Model) setTurn(n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.turn = n
}

// summarizerTurns returns the turn of every summarizer call so far.
func (m *a7Model) summarizerTurns() []int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]int(nil), m.summarizeAt...)
}

func isSummarizerRequest(ctx context.Context, req *adkmodel.LLMRequest) bool {
	if models.SideCallName(ctx) == "summarizer" {
		return true
	}
	if req == nil || req.Config == nil || req.Config.SystemInstruction == nil {
		return false
	}
	for _, p := range req.Config.SystemInstruction.Parts {
		if p != nil && strings.Contains(p.Text, a7SummarizerSentinel) {
			return true
		}
	}
	return false
}

func (m *a7Model) GenerateContent(ctx context.Context, req *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	if !isSummarizerRequest(ctx, req) {
		return func(yield func(*adkmodel.LLMResponse, error) bool) {
			yield(&adkmodel.LLMResponse{
				Content:      genai.NewContentFromText("working on it", genai.RoleModel),
				FinishReason: genai.FinishReasonStop,
				TurnComplete: true,
			}, nil)
		}
	}
	m.mu.Lock()
	m.summarizeAt = append(m.summarizeAt, m.turn)
	m.mu.Unlock()
	resp, err := m.summarize()
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		yield(resp, err)
	}
}

// emptyStop is the #1234 shape: the summarizer finished with STOP and
// produced no text.
func emptyStop() (*adkmodel.LLMResponse, error) {
	return &adkmodel.LLMResponse{
		Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: ""}}},
		FinishReason: genai.FinishReasonStop,
		TurnComplete: true,
	}, nil
}

func rateLimited() (*adkmodel.LLMResponse, error) {
	return nil, errors.New("Error 429, Status: RESOURCE_EXHAUSTED, Message: Resource exhausted. Please try again later.")
}

// a7Session drives turns on one agent and reads its boundaries back.
type a7Session struct {
	t     *testing.T
	a     *agent.Agent
	model *a7Model
	turns int
}

func newA7Session(t *testing.T, summarize func() (*adkmodel.LLMResponse, error)) *a7Session {
	t.Helper()
	m := &a7Model{summarize: summarize}
	a, err := agent.New(m, agent.WithCompactor(a7Compactor{}), agent.WithoutSessionTitle())
	if err != nil {
		t.Fatalf("A7 ORACLE RIG: agent.New: %v", err)
	}
	return &a7Session{t: t, a: a, model: m}
}

// turn runs one turn to completion. A turn error is the rig's problem,
// not the fix's: a failed compaction must never fail the operator's turn.
func (s *a7Session) turn() {
	s.t.Helper()
	s.turns++
	s.model.setTurn(s.turns)
	for _, err := range s.a.Run(context.Background(), "keep going on the issue") {
		if err != nil {
			s.t.Fatalf("A7 ORACLE RIG: turn %d returned an error: %v", s.turns, err)
		}
	}
}

// untilFirstCompaction runs turns until the summarizer has been asked
// for a summary, and returns the turn that asked.
func (s *a7Session) untilFirstCompaction() int {
	s.t.Helper()
	for range a7MaxTurnsToFirstCompaction {
		s.turn()
		if calls := s.model.summarizerTurns(); len(calls) > 0 {
			return calls[0]
		}
	}
	s.t.Fatalf("A7 ORACLE RIG: no compaction was attempted in %d turns with the threshold always crossed", a7MaxTurnsToFirstCompaction)
	return 0
}

// boundaries returns every compaction or checkpoint boundary event in
// the session, oldest first.
func (s *a7Session) boundaries() []*session.Event {
	s.t.Helper()
	resp, err := s.a.SessionService().Get(context.Background(), &session.GetRequest{
		AppName:   s.a.AppName(),
		UserID:    s.a.UserID(),
		SessionID: s.a.SessionID(),
	})
	if err != nil || resp == nil || resp.Session == nil {
		s.t.Fatalf("A7 ORACLE RIG: read the session back: %v", err)
	}
	var out []*session.Event
	for ev := range resp.Session.Events().All() {
		if ev == nil || ev.CustomMetadata == nil {
			continue
		}
		if tag, _ := ev.CustomMetadata[agent.CompactionMetadataKey].(string); tag != "" {
			out = append(out, ev)
		}
	}
	return out
}

func mechanicalBoundary(evs []*session.Event) *session.Event {
	for _, ev := range evs {
		tag, _ := ev.CustomMetadata[agent.CompactionMetadataKey].(string)
		if tag == agent.CompactionEventTag && ev.CustomMetadata[agent.MechanicalCompactionKey] == true {
			return ev
		}
	}
	return nil
}

// distinctTurns counts the turns the summarizer was called in. Calls in
// one turn are one compaction: its attempt and the retry inside it.
func distinctTurns(turns []int) int {
	seen := map[int]bool{}
	for _, t := range turns {
		seen[t] = true
	}
	return len(seen)
}

func TestA7Oracle1234Recovery(t *testing.T) {
	t.Run("empty_stop_twice_writes_mechanical_boundary", func(t *testing.T) {
		s := newA7Session(t, emptyStop)
		first := s.untilFirstCompaction()
		b := mechanicalBoundary(s.boundaries())
		// The fallback may land in a later turn than the failing one: a
		// fix is free to keep the backoff and bound the context when it
		// expires. Allow that, for as long as the summarizer is not
		// tried again: a second try is a second failed compaction, and
		// falling back after two is what pre-fix code already does.
		for range a7ExtraTurns {
			if b != nil || distinctTurns(s.model.summarizerTurns()) > 1 {
				break
			}
			s.turn()
			b = mechanicalBoundary(s.boundaries())
		}
		calls := s.model.summarizerTurns()
		t.Logf("summarizer calls by turn: %v (first compaction attempted in turn %d)", calls, first)
		if n := distinctTurns(calls); n != 1 {
			t.Errorf("A7 ORACLE UNFIXED: the mechanical fallback came only after %d failed compactions (summarizer calls by turn: %v); an empty summary is deterministic and must fall back after the first", n, calls)
		} else if b == nil {
			t.Errorf("A7 ORACLE UNFIXED: no mechanical compaction boundary (%s=true) after one compaction whose summarizer returned an empty STOP on every attempt (%d calls); an empty summary is deterministic and must fall back on the first failure", agent.MechanicalCompactionKey, len(calls))
		}
	})

	t.Run("rate_limit_writes_no_boundary", func(t *testing.T) {
		s := newA7Session(t, rateLimited)
		s.untilFirstCompaction()
		// The same window the empty-STOP half allows: a boundary written
		// before the summarizer is tried again is a truncation after one
		// 429, whether it lands in the failing turn or when the backoff
		// expires. After a second failure, #974's fallback is allowed.
		for i := 0; ; i++ {
			if evs := s.boundaries(); len(evs) > 0 && distinctTurns(s.model.summarizerTurns()) == 1 {
				t.Errorf("A7 ORACLE UNFIXED: a single rate-limited compaction wrote a boundary (%v, summarizer calls by turn: %v); a 429 may clear, so it must back off, not truncate", evs[0].CustomMetadata, s.model.summarizerTurns())
				break
			}
			if i == a7ExtraTurns || distinctTurns(s.model.summarizerTurns()) > 1 {
				break
			}
			s.turn()
		}
	})
}
