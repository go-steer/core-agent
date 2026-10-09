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
	"encoding/json"
	"iter"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v2/pkg/agent/autonomous"
	"github.com/go-steer/core-agent/v2/pkg/attach"
)

// stepLLM answers its Nth model call with steps[N]; calls past the end
// repeat the last step. A step may block, which is how a test holds a
// subagent mid-turn while it reads the roster.
type stepLLM struct {
	mu    sync.Mutex
	n     int
	steps []func(context.Context) *adkmodel.LLMResponse
}

func (*stepLLM) Name() string { return "step" }

func (l *stepLLM) GenerateContent(ctx context.Context, _ *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	l.mu.Lock()
	i := min(l.n, len(l.steps)-1)
	l.n++
	step := l.steps[i]
	l.mu.Unlock()
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		resp := step(ctx)
		if resp == nil {
			yield(nil, ctx.Err())
			return
		}
		yield(resp, nil)
	}
}

func callStep(name string, args map[string]any) func(context.Context) *adkmodel.LLMResponse {
	return func(context.Context) *adkmodel.LLMResponse {
		fc := &genai.FunctionCall{Name: name, Args: args}
		return &adkmodel.LLMResponse{
			Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: fc}}},
			FinishReason: genai.FinishReasonStop, TurnComplete: true,
		}
	}
}

func textStep(text string) func(context.Context) *adkmodel.LLMResponse {
	return func(context.Context) *adkmodel.LLMResponse {
		return &adkmodel.LLMResponse{
			Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: text}}},
			FinishReason: genai.FinishReasonStop, TurnComplete: true,
		}
	}
}

// gated signals started, then waits for release before answering with
// next. A cancelled context answers nil, which stepLLM turns into an
// error so the run can unwind.
func gated(started, release chan struct{}, next func(context.Context) *adkmodel.LLMResponse) func(context.Context) *adkmodel.LLMResponse {
	var once sync.Once
	return func(ctx context.Context) *adkmodel.LLMResponse {
		once.Do(func() { close(started) })
		select {
		case <-release:
			return next(ctx)
		case <-ctx.Done():
			return nil
		}
	}
}

func rosterRow(t *testing.T, mgr *Manager, name string) attach.AgentInfo {
	t.Helper()
	for _, ai := range mgr.ListSubagents() {
		if ai.Name == name {
			return ai
		}
	}
	t.Fatalf("subagent %q not on the roster", name)
	return attach.AgentInfo{}
}

func spawnStepped(t *testing.T, llm *stepLLM, scheduler string) (*Manager, *Handle) {
	t.Helper()
	prov := &recordingProvider{llm: llm}
	mgr := newTemplateManager(t, prov, []SubagentTemplate{{
		Name:         "monitor",
		Instruction:  "watch the cluster",
		ModelFactory: tmplFactory(prov, "monitor-model"),
		ModelID:      "monitor-model",
		Scheduler:    scheduler,
		Mode:         ModeStanding,
	}}, WithDefaultBudgets(Budgets{MaxTurns: 6}))
	attachEchoParent(t, mgr)
	t.Cleanup(func() { _ = mgr.Close() })
	h, err := mgr.SpawnTemplate(context.Background(), "", "monitor", RefOverrides{Goal: "watch cluster-A"}, "")
	if err != nil {
		t.Fatalf("SpawnTemplate: %v", err)
	}
	return mgr, h
}

// TestListSubagents_ReportsPendingWake is #1283's first ask: a subagent
// sleeping on schedule_next_turn shows when it will wake and why, stays
// "running" while it sleeps, and drops both fields the moment its next
// turn starts.
func TestListSubagents_ReportsPendingWake(t *testing.T) {
	t.Parallel()
	turn2, release := make(chan struct{}), make(chan struct{})
	llm := &stepLLM{steps: []func(context.Context) *adkmodel.LLMResponse{
		callStep("schedule_next_turn", map[string]any{"wake_in_sec": 3, "detail": "polling cluster-A"}),
		textStep("node pool healthy, checking again shortly"),
		gated(turn2, release, callStep("return_result", map[string]any{"result": "cluster-A stayed healthy"})),
		textStep("ok"),
	}}
	mgr, h := spawnStepped(t, llm, "sleep")
	defer close(release)

	var asleep attach.AgentInfo
	deadline := time.Now().Add(5 * time.Second)
	for {
		asleep = rosterRow(t, mgr, h.Name)
		if !asleep.NextWakeAt.IsZero() {
			break
		}
		select {
		case <-turn2:
			t.Fatalf("the subagent slept and woke without the roster ever showing a wake")
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("no next_wake_at on the roster while the subagent sleeps; row = %+v", asleep)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if asleep.Status != attach.AgentStatusRunning {
		t.Errorf("status while asleep = %q, want %q — a new word reads as finished to shipped TUIs", asleep.Status, attach.AgentStatusRunning)
	}
	if asleep.WakeDetail != "polling cluster-A" {
		t.Errorf("wake_detail = %q, want the model's reason %q", asleep.WakeDetail, "polling cluster-A")
	}
	if d := time.Until(asleep.NextWakeAt); d > 4*time.Second {
		t.Errorf("next_wake_at is %v away, want about 3s", d)
	}
	if asleep.LastReport != "node pool healthy, checking again shortly" {
		t.Errorf("last_report while asleep = %q, want the latest model message", asleep.LastReport)
	}

	select {
	case <-turn2:
	case <-time.After(5 * time.Second):
		t.Fatal("the subagent never woke for its second turn")
	}
	awake := rosterRow(t, mgr, h.Name)
	if !awake.NextWakeAt.IsZero() || awake.WakeDetail != "" {
		t.Errorf("mid-turn row = %+v, want no wake fields — the subagent is working, not sleeping", awake)
	}
	if awake.Status != attach.AgentStatusRunning {
		t.Errorf("mid-turn status = %q, want running", awake.Status)
	}

	release <- struct{}{}
	waitDone(t, h)
	done := rosterRow(t, mgr, h.Name)
	if !done.NextWakeAt.IsZero() {
		t.Errorf("finished row still carries next_wake_at %v", done.NextWakeAt)
	}
	if done.LastReport != "cluster-A stayed healthy" {
		t.Errorf("finished last_report = %q, want the returned result", done.LastReport)
	}
}

// TestListSubagents_LiveLastReportFollowsAlerts is #1283's second ask: a
// running subagent's row carries its latest report_alert, ahead of any
// later narration, and the returned result replaces it at the end.
func TestListSubagents_LiveLastReportFollowsAlerts(t *testing.T) {
	t.Parallel()
	turn2, release := make(chan struct{}), make(chan struct{})
	const alert = "pod web-1 is OOMKilled: limit 8Mi"
	llm := &stepLLM{steps: []func(context.Context) *adkmodel.LLMResponse{
		callStep("report_alert", map[string]any{"text": alert}),
		textStep("looking at the events next"),
		gated(turn2, release, callStep("return_result", map[string]any{"result": "RCA: memory limit squeezed to 8Mi"})),
		textStep("ok"),
	}}
	mgr, h := spawnStepped(t, llm, "none")
	defer close(release)

	select {
	case <-turn2:
	case <-time.After(5 * time.Second):
		t.Fatal("the subagent never reached its second turn")
	}
	row := rosterRow(t, mgr, h.Name)
	if row.LastReport != alert {
		t.Errorf("running last_report = %q, want the latest alert %q", row.LastReport, alert)
	}
	if !row.NextWakeAt.IsZero() {
		t.Errorf("a subagent with no scheduler reports next_wake_at %v", row.NextWakeAt)
	}

	release <- struct{}{}
	waitDone(t, h)
	if got := rosterRow(t, mgr, h.Name).LastReport; got != "RCA: memory limit squeezed to 8Mi" {
		t.Errorf("finished last_report = %q, want the returned result to win", got)
	}
}

// TestAgentInfo_WakeFieldsAbsentWhenUnset pins the wire: a row with no
// pending wake carries neither key, so a client can treat presence as
// "asleep". omitempty would not do this for a time.Time.
func TestAgentInfo_WakeFieldsAbsentWhenUnset(t *testing.T) {
	t.Parallel()
	b, err := json.Marshal(attach.AgentInfo{ID: "m", Name: "m", Status: attach.AgentStatusRunning})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"next_wake_at", "wake_detail"} {
		if strings.Contains(string(b), key) {
			t.Errorf("%s = %s, want no %q key when no wake is pending", "AgentInfo JSON", b, key)
		}
	}
}

// TestLastReport_TerminalDropsLiveText: a run that finished with no
// returned result and no final text reports "", not the narration it
// left behind mid-run, which next to a terminal status would read as
// the outcome.
func TestLastReport_TerminalDropsLiveText(t *testing.T) {
	t.Parallel()
	h := &Handle{lastAlert: "halfway through node 3", lastText: "checking node 4"}
	if got := h.LastReport(); got != "halfway through node 3" {
		t.Fatalf("running LastReport = %q, want the alert", got)
	}
	h.result = &autonomous.RunResult{Reason: autonomous.StopReasonMaxTurns}
	if got := h.LastReport(); got != "" {
		t.Errorf("finished LastReport = %q, want \"\" — the run ended with nothing to report", got)
	}
}

// TestNoteAlert_ClipsLongText: live text rides on every polled roster
// row, so it is capped, and never mid-rune.
func TestNoteAlert_ClipsLongText(t *testing.T) {
	t.Parallel()
	h := &Handle{}
	h.noteAlert("x" + strings.Repeat("é", maxLiveReportBytes)) // odd offset: the cap lands mid-rune
	got := h.LastReport()
	if len(got) > maxLiveReportBytes+len("…") {
		t.Errorf("len(LastReport) = %d, want at most %d", len(got), maxLiveReportBytes+len("…"))
	}
	if !utf8.ValidString(got) || !strings.HasSuffix(got, "…") {
		t.Errorf("clipped LastReport is not valid UTF-8 ending in an ellipsis: %q", got[len(got)-8:])
	}
}
