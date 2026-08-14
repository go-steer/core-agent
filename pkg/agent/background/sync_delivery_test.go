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

package background

import (
	"context"
	"iter"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v2/pkg/agent/autonomous"
)

// findingsThenSummaryLLM reproduces the kube-platform-agent UAT shape that
// surfaced #641: the subagent does its work and states the substantive
// answer as assistant text, then signals completion with a terse status
// line. Before the fix the parent received only the status line and redid
// the analysis it had just delegated.
type findingsThenSummaryLLM struct {
	findings string
	detail   string
	turn     atomic.Int32
}

func (*findingsThenSummaryLLM) Name() string { return "findings-then-summary" }

func (l *findingsThenSummaryLLM) GenerateContent(_ context.Context, _ *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		text := func(s string) {
			content := &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: s}}}
			yield(&adkmodel.LLMResponse{Content: content, FinishReason: genai.FinishReasonStop, TurnComplete: true}, nil)
		}
		switch l.turn.Add(1) {
		case 1:
			text(l.findings)
		case 2:
			fc := &genai.FunctionCall{Name: "report_done", Args: map[string]any{"state": "done", "detail": l.detail}}
			content := &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: fc}}}
			yield(&adkmodel.LLMResponse{Content: content, FinishReason: genai.FinishReasonStop, TurnComplete: true}, nil)
		default:
			text("ok")
		}
	}
}

// declCapturingLLM records the tool declarations and system instruction
// it is offered, so a test can assert on the prompt surface a spawned
// subagent actually sees. It completes on the first call so the run
// terminates promptly.
type declCapturingLLM struct {
	mu    sync.Mutex
	decls map[string]string
	sys   string
}

func (*declCapturingLLM) Name() string { return "decl-capturing" }

func (l *declCapturingLLM) GenerateContent(_ context.Context, req *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	l.mu.Lock()
	if l.decls == nil {
		l.decls = map[string]string{}
	}
	if req != nil && req.Config != nil {
		for _, t := range req.Config.Tools {
			if t == nil {
				continue
			}
			for _, fd := range t.FunctionDeclarations {
				if fd != nil {
					l.decls[fd.Name] = fd.Description
				}
			}
		}
		if si := req.Config.SystemInstruction; si != nil {
			var b strings.Builder
			for _, p := range si.Parts {
				if p != nil {
					b.WriteString(p.Text)
				}
			}
			l.sys = b.String()
		}
	}
	l.mu.Unlock()
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		content := &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "done looking"}}}
		yield(&adkmodel.LLMResponse{Content: content, FinishReason: genai.FinishReasonStop, TurnComplete: true}, nil)
	}
}

func (l *declCapturingLLM) declaration(name string) (string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	d, ok := l.decls[name]
	return d, ok
}

func (l *declCapturingLLM) systemInstruction() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.sys
}

// TestAwaitResult_CarriesFinalTextAlongsideReport is one half of #641: a
// subagent whose last assistant text differs from its completion report
// must surface both, so a persona that answers in prose and then reports
// a one-line status doesn't leave the parent holding only the status.
//
// Fails on pre-fix code twice over: spawnAgentResult had no second
// channel at all, and the driver's turn-text collection only accumulated
// streaming partials, so a non-streaming subagent's FinalText was always
// empty and there was nothing to carry.
func TestAwaitResult_CarriesFinalTextAlongsideReport(t *testing.T) {
	t.Parallel()
	const findings = "RCA: emailservice is in ImagePullBackOff; image tag does-not-exist:v0-demo-break is invalid; revert to emailservice:v0.10.5"
	const summary = "diagnosed the issue and proposed a patch"

	prov := &recordingProvider{llm: &findingsThenSummaryLLM{findings: findings, detail: summary}}
	mgr := newTemplateManager(t, prov, []SubagentTemplate{{
		Name:         "cluster",
		Instruction:  "triage",
		ModelFactory: tmplFactory(prov, "cluster-model"),
		ModelID:      "cluster-model",
		// Standing: this scenario terminates through report_done,
		// which only a standing worker is offered since #730.
		Mode: ModeStanding,
	}}, WithDefaultBudgets(Budgets{MaxTurns: 4}), WithSyncWaitTimeout(10*time.Second))
	attachEchoParent(t, mgr)
	defer mgr.Close()

	h, err := mgr.SpawnTemplate(context.Background(), "", "cluster", RefOverrides{Goal: "triage emailservice"}, "")
	if err != nil {
		t.Fatalf("SpawnTemplate: %v", err)
	}
	res := mgr.awaitResult(context.Background(), h)

	if res.Status != "completed" {
		t.Fatalf("status = %q, want completed", res.Status)
	}
	if res.Output != summary {
		t.Errorf("output = %q, want the completion report %q", res.Output, summary)
	}
	if res.FinalText == "" {
		t.Error("final_text is empty; the subagent's last assistant text must reach the parent alongside the report")
	}
	if res.FinalText == res.Output {
		t.Errorf("final_text duplicates output (%q); it is only carried when it adds something", res.FinalText)
	}
}

// TestAwaitResult_NoDuplicateFinalText keeps the second channel quiet
// when it would only repeat the report — the point is to add the missing
// substance, not to double every result.
func TestAwaitResult_NoDuplicateFinalText(t *testing.T) {
	t.Parallel()
	prov := &recordingProvider{llm: &completingLLM{detail: "cluster is healthy"}}
	mgr := newTemplateManager(t, prov, []SubagentTemplate{{
		Name:         "cluster",
		Instruction:  "triage",
		ModelFactory: tmplFactory(prov, "cluster-model"),
		ModelID:      "cluster-model",
		// Standing: this scenario terminates through report_done,
		// which only a standing worker is offered since #730.
		Mode: ModeStanding,
	}}, WithDefaultBudgets(Budgets{MaxTurns: 1}), WithSyncWaitTimeout(10*time.Second))
	attachEchoParent(t, mgr)
	defer mgr.Close()

	h, err := mgr.SpawnTemplate(context.Background(), "", "cluster", RefOverrides{Goal: "g"}, "")
	if err != nil {
		t.Fatalf("SpawnTemplate: %v", err)
	}
	res := mgr.awaitResult(context.Background(), h)
	if res.Output != "cluster is healthy" {
		t.Errorf("output = %q, want the completion report", res.Output)
	}
}

// TestSpawnedSubagent_DoneToolAsksForTheDeliverable is the durable half
// of #641: the fix that makes the completion report carry findings by
// construction rather than by persona luck. The driver's stock prose asks
// for "a one-sentence detail", which is what produced the content-free
// reports the parent had to re-derive.
//
// Fails on pre-fix code: spawn.go passed no WithDoneToolDescription, so
// spawned subagents saw the generic single-sentence instruction.
func TestSpawnedSubagent_DoneToolAsksForTheDeliverable(t *testing.T) {
	t.Parallel()
	capture := &declCapturingLLM{}
	prov := &recordingProvider{llm: capture}
	mgr := newTemplateManager(t, prov, []SubagentTemplate{{
		Name:         "cluster",
		Instruction:  "triage",
		ModelFactory: tmplFactory(prov, "cluster-model"),
		ModelID:      "cluster-model",
		// Standing: this scenario terminates through report_done,
		// which only a standing worker is offered since #730.
		Mode: ModeStanding,
	}}, WithDefaultBudgets(Budgets{MaxTurns: 1}))
	attachEchoParent(t, mgr)
	defer mgr.Close()

	h, err := mgr.SpawnTemplate(context.Background(), "", "cluster", RefOverrides{Goal: "g"}, "")
	if err != nil {
		t.Fatalf("SpawnTemplate: %v", err)
	}
	waitDone(t, h)

	desc, ok := capture.declaration("report_done")
	if !ok {
		t.Fatal("spawned subagent was never offered a report_done tool")
	}
	for _, want := range []string{"FINDINGS", "delegated"} {
		if !strings.Contains(desc, want) {
			t.Errorf("report_done description = %q, want it to mention %q — the report is the deliverable handed to the parent", desc, want)
		}
	}
}

// drainAlerts collects every alert currently queued on the manager's
// channel, giving the completion goroutine a short grace period to push
// one. Absence is what the #646 test asserts, so the wait has to be long
// enough that a still-in-flight alert would have landed.
func drainAlerts(m *Manager, grace time.Duration) []Alert {
	var out []Alert
	deadline := time.After(grace)
	for {
		select {
		case a := <-m.Alerts():
			out = append(out, a)
		case <-deadline:
			return out
		}
	}
}

// TestAwaitResult_SuppressesRedundantBackgroundReport is the #646
// acceptance test: a wait:true completion is delivered exactly once, as
// the tool result. Pre-fix the same outcome also arrived as a
// "[Background reports]" line on the parent's next turn.
func TestAwaitResult_SuppressesRedundantBackgroundReport(t *testing.T) {
	t.Parallel()
	prov := &recordingProvider{llm: &completingLLM{detail: "cluster is healthy"}}
	mgr := newTemplateManager(t, prov, []SubagentTemplate{{
		Name:         "cluster",
		Instruction:  "triage",
		ModelFactory: tmplFactory(prov, "cluster-model"),
		ModelID:      "cluster-model",
		// Standing: this scenario terminates through report_done,
		// which only a standing worker is offered since #730.
		Mode: ModeStanding,
	}}, WithDefaultBudgets(Budgets{MaxTurns: 2}), WithSyncWaitTimeout(10*time.Second))
	attachEchoParent(t, mgr)
	defer mgr.Close()

	h, err := mgr.SpawnTemplate(context.Background(), "", "cluster", RefOverrides{Goal: "g"}, "")
	if err != nil {
		t.Fatalf("SpawnTemplate: %v", err)
	}
	res := mgr.awaitResult(context.Background(), h)
	if res.Status != "completed" {
		t.Fatalf("status = %q, want completed", res.Status)
	}
	for _, a := range drainAlerts(mgr, 200*time.Millisecond) {
		if a.From == h.Name && a.Kind == "completed" {
			t.Errorf("wait:true completion also pushed a background report: %+v", a)
		}
	}
}

// TestSpawn_AsyncStillReportsCompletion is the other half of #646: only
// the synchronously-consumed path is deduped. A fire-and-continue spawn
// must still push its completion to the parent's inbox, otherwise the
// result is lost entirely.
func TestSpawn_AsyncStillReportsCompletion(t *testing.T) {
	t.Parallel()
	prov := &recordingProvider{llm: &completingLLM{detail: "cluster is healthy"}}
	mgr := newTemplateManager(t, prov, []SubagentTemplate{{
		Name:         "cluster",
		Instruction:  "triage",
		ModelFactory: tmplFactory(prov, "cluster-model"),
		ModelID:      "cluster-model",
		// Standing: this scenario terminates through report_done,
		// which only a standing worker is offered since #730.
		Mode: ModeStanding,
	}}, WithDefaultBudgets(Budgets{MaxTurns: 2}))
	attachEchoParent(t, mgr)
	defer mgr.Close()

	h, err := mgr.SpawnTemplate(context.Background(), "", "cluster", RefOverrides{Goal: "g"}, "")
	if err != nil {
		t.Fatalf("SpawnTemplate: %v", err)
	}
	waitDone(t, h)

	var found bool
	for _, a := range drainAlerts(mgr, time.Second) {
		if a.From == h.Name && a.Kind == "completed" {
			found = true
		}
	}
	if !found {
		t.Error("fire-and-continue spawn pushed no completion alert; async delivery must be unchanged")
	}
}

// TestReleaseSync_LosesRaceToCompletion pins the race resolution the
// timeout path depends on: when the completion goroutine consumes the
// claim first, a waiter that then gives up must learn the alert was
// suppressed so it delivers the result inline rather than dropping it.
func TestReleaseSync_LosesRaceToCompletion(t *testing.T) {
	t.Parallel()
	h := &Handle{Name: "cluster-1", done: make(chan struct{})}

	if !h.claimSync() {
		t.Fatal("claimSync on a live handle = false, want true")
	}
	if !h.takeSyncClaim() {
		t.Fatal("takeSyncClaim with an outstanding claim = false, want true (alert suppressed)")
	}
	if h.releaseSync() {
		t.Error("releaseSync after the goroutine consumed the claim = true; the waiter would report 'still running' and the suppressed result would be lost")
	}
}

// TestReleaseSync_WinsRaceAgainstCompletion is the mirror: a waiter that
// gives up before the subagent finishes must hand the terminal alert back
// to the normal async path.
func TestReleaseSync_WinsRaceAgainstCompletion(t *testing.T) {
	t.Parallel()
	h := &Handle{Name: "cluster-1", done: make(chan struct{})}

	if !h.claimSync() {
		t.Fatal("claimSync on a live handle = false, want true")
	}
	if !h.releaseSync() {
		t.Fatal("releaseSync on an outstanding claim = false, want true")
	}
	if h.takeSyncClaim() {
		t.Error("takeSyncClaim after release = true; the completion alert would be suppressed with nobody to deliver it")
	}
}

// TestAbandonWait_PrefersAnAlreadyLandedResult covers the tie in the
// waiter's select: when the subagent finishes in the same instant the
// wait gives up, Go picks a ready case arbitrarily, so the timeout /
// cancellation branch must still notice the result is already in hand.
// Reporting "still running, result will be pushed" here would be a
// promise nothing keeps for the completions that never alert (a stopped
// subagent), losing the outcome entirely.
func TestAbandonWait_PrefersAnAlreadyLandedResult(t *testing.T) {
	t.Parallel()
	done := make(chan struct{})
	h := &Handle{Name: "cluster-1", done: done}
	if !h.claimSync() {
		t.Fatal("claimSync on a live handle = false, want true")
	}
	h.mu.Lock()
	h.status = StatusCompleted
	h.result = &autonomous.RunResult{DoneDetail: "found the bad image tag"}
	h.mu.Unlock()
	close(done)

	mgr := &Manager{}
	res, ok := mgr.abandonWait(h, true)
	if ok {
		t.Fatal("abandonWait reported 'still running' for a handle that already finished")
	}
	if res.Output != "found the bad image tag" {
		t.Errorf("output = %q, want the completion report", res.Output)
	}
}

// TestClaimSync_TooLateOnTerminalHandle documents the one window the
// dedup can't close: a subagent that finishes before the waiter claims
// has already passed the suppression check, so its alert is in flight and
// the claim must not pretend otherwise.
func TestClaimSync_TooLateOnTerminalHandle(t *testing.T) {
	t.Parallel()
	done := make(chan struct{})
	close(done)
	h := &Handle{Name: "cluster-1", done: done}

	if h.claimSync() {
		t.Error("claimSync on an already-terminal handle = true, want false")
	}
}
