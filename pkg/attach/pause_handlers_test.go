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
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-steer/core-agent/v2/pkg/auth"
)

// pausingRegistrant is a stubRegistrant that carries the full v1.5.0
// hold surface: PauseController + AgentStopper + AgentsProvider +
// InterruptProvider + InterruptSelfAuditor. Models attachadapter.Adapter
// closely enough to pin the handler's dispatch decisions.
type pausingRegistrant struct {
	stubRegistrant

	mu       sync.Mutex
	paused   bool
	reason   string
	since    time.Time
	killed   bool // an in-flight turn was cancelled on the way into the pause
	live     []AgentInfo
	stops    []string
	resumes  []ResumeRequest
	callers  []auth.Caller
	resumeAt string // mode the fake resolved
	// raceToFinish names a subagent whose goroutine exits just before
	// the stop takes the handle's lock — the "finished between the
	// list and the stop" window /interrupt used to claim credit for.
	raceToFinish string

	canInterrupt  atomic.Bool // "there is a turn in flight"
	plainCancels  atomic.Int32
	holdCancels   atomic.Int32
	markedPending atomic.Int32
}

func (p *pausingRegistrant) AttachInterrupt() bool {
	p.plainCancels.Add(1)
	return p.canInterrupt.Swap(false)
}

func (p *pausingRegistrant) MarkInterruptPending() { p.markedPending.Add(1) }

func (p *pausingRegistrant) AttachPause(reason string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.paused {
		return false
	}
	p.paused = true
	if reason == "" {
		reason = "operator-pause"
	}
	p.reason = reason
	p.since = time.Now().UTC()
	return true
}

func (p *pausingRegistrant) AttachInterruptHold(reason string) (bool, bool) {
	p.holdCancels.Add(1)
	interrupted := p.canInterrupt.Swap(false)
	if interrupted {
		// Real adapters arm the audit row here, before the cancel.
		p.markedPending.Add(1)
	}
	p.mu.Lock()
	if !p.paused {
		p.paused = true
		p.reason = reason
		if p.reason == "" {
			p.reason = "operator-interrupt"
		}
		p.since = time.Now().UTC()
		p.killed = interrupted
	}
	p.mu.Unlock()
	return interrupted, true
}

func (p *pausingRegistrant) AttachResume(req ResumeRequest, caller auth.Caller) (ResumeResponse, error) {
	p.mu.Lock()
	p.resumes = append(p.resumes, req)
	p.callers = append(p.callers, caller)
	wasPaused := p.paused
	p.paused = false
	p.reason = ""
	p.since = time.Time{}
	p.killed = false
	p.mu.Unlock()
	mode := req.Mode
	if mode == "" {
		mode = ResumeModeContinue
		if req.Steer != "" {
			mode = ResumeModeSteer
		}
	}
	p.resumeAt = mode
	return ResumeResponse{Resumed: wasPaused, Mode: mode, State: AgentStateIdle}, nil
}

func (p *pausingRegistrant) AttachPauseState() PauseInfo {
	p.mu.Lock()
	defer p.mu.Unlock()
	return PauseInfo{Paused: p.paused, Since: p.since, Reason: p.reason, Interrupted: p.killed}
}

func (p *pausingRegistrant) AttachAgents() []AgentInfo {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]AgentInfo, len(p.live))
	copy(out, p.live)
	return out
}

// AttachStopAgentOutcome models the real manager: a handle stays
// registered after it terminates, so a name in `live` is Found
// whatever its status, and only a running one is Stopped by the call.
func (p *pausingRegistrant) AttachStopAgentOutcome(name string) (StopAgentOutcome, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, a := range p.live {
		if a.Name != name {
			continue
		}
		if a.Status == AgentStatusRunning && name == p.raceToFinish {
			p.live[i].Status = AgentStatusCompleted
			return StopAgentOutcome{Found: true, Status: AgentStatusCompleted}, nil
		}
		if a.Status != AgentStatusRunning {
			return StopAgentOutcome{Found: true, Status: a.Status}, nil
		}
		p.live[i].Status = AgentStatusStopped
		p.stops = append(p.stops, name)
		return StopAgentOutcome{Found: true, Stopped: true, Status: AgentStatusStopped}, nil
	}
	return StopAgentOutcome{}, nil
}

// newPauseHarness registers reg and starts a server, returning the base URL.
func newPauseHarness(t *testing.T, ag Registrant) string {
	t.Helper()
	reg := NewSessionRegistry()
	if _, err := reg.Register(ag); err != nil {
		t.Fatal(err)
	}
	base, cleanup := startTestServer(t, reg)
	t.Cleanup(cleanup)
	return base
}

func postJSON(t *testing.T, url, body string) *http.Response {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	resp, err := http.Post(url, "application/json", r)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func decodeInterrupt(t *testing.T, resp *http.Response) InterruptResponse {
	t.Helper()
	var out InterruptResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

func decodeStop(t *testing.T, resp *http.Response, out *StopAgentResponse) {
	t.Helper()
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatalf("decode: %v", err)
	}
}

// The v1.5.0 default: a bodyless POST /interrupt holds. This is the
// reported bug in one assertion — before the hold, the operator's stop
// was undone by the next wake / scheduler tick / auto-continue, so
// "/interrupt doesn't actually interrupt the loop".
func TestInterrupt_DefaultsToHold(t *testing.T) {
	t.Parallel()
	ag := &pausingRegistrant{stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s1"}}
	ag.canInterrupt.Store(true)
	base := newPauseHarness(t, ag)

	resp := postJSON(t, base+"/sessions/core-agent/s1/interrupt", "")
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	got := decodeInterrupt(t, resp)
	if !got.Interrupted || !got.Paused {
		t.Errorf("response = %+v, want interrupted+paused", got)
	}
	if ag.holdCancels.Load() != 1 {
		t.Errorf("AttachInterruptHold called %d times, want 1", ag.holdCancels.Load())
	}
	if ag.plainCancels.Load() != 0 {
		t.Errorf("AttachInterrupt called %d times, want 0 (hold path must not double-cancel)",
			ag.plainCancels.Load())
	}
	// The hold path arms the audit row itself, BEFORE the cancel (#565
	// ordering). A second arm from the handler would queue a duplicate.
	if got := ag.markedPending.Load(); got != 1 {
		t.Errorf("interrupt audit armed %d times, want exactly 1", got)
	}
	if hdr := resp.Header.Get("X-Hold"); hdr != "" {
		t.Errorf("X-Hold = %q, want empty (hold was supported)", hdr)
	}
}

// hold=false is the pre-v1.5.0 escape hatch: cancel, don't park.
func TestInterrupt_HoldFalseCancelsWithoutParking(t *testing.T) {
	t.Parallel()
	ag := &pausingRegistrant{stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s1"}}
	ag.canInterrupt.Store(true)
	base := newPauseHarness(t, ag)

	got := decodeInterrupt(t, postJSON(t, base+"/sessions/core-agent/s1/interrupt", `{"hold":false}`))
	if !got.Interrupted {
		t.Error("Interrupted = false, want true")
	}
	if got.Paused {
		t.Error("Paused = true with hold=false, want false")
	}
	if ag.holdCancels.Load() != 0 {
		t.Errorf("AttachInterruptHold called %d times, want 0", ag.holdCancels.Load())
	}
	if ag.plainCancels.Load() != 1 {
		t.Errorf("AttachInterrupt called %d times, want 1", ag.plainCancels.Load())
	}
	if st := ag.AttachPauseState(); st.Paused {
		t.Error("registrant is paused after hold=false")
	}
}

// hold=false declines to ADD a hold; it does not lift one. An agent
// already parked by an earlier /pause is still parked when the cancel
// returns, and the response has to say so — mast-web and the TUI both
// render their banner from this body, so reporting the branch's intent
// instead of the gate's real state would clear a banner for a loop that
// is still stopped.
func TestInterrupt_HoldFalseStillReportsAnExistingPause(t *testing.T) {
	t.Parallel()
	ag := &pausingRegistrant{stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s1"}}
	base := newPauseHarness(t, ag)

	postJSON(t, base+"/sessions/core-agent/s1/pause", `{"reason":"reviewing the plan"}`)
	ag.canInterrupt.Store(true)

	got := decodeInterrupt(t, postJSON(t, base+"/sessions/core-agent/s1/interrupt", `{"hold":false}`))
	if !got.Interrupted {
		t.Error("Interrupted = false, want true")
	}
	if !got.Paused {
		t.Error("Paused = false, but the agent was parked before the cancel and still is")
	}
	if st := ag.AttachPauseState(); !st.Paused {
		t.Fatal("test bug: registrant should still be paused")
	}
}

// A registrant that predates PauseController still gets its turn
// cancelled — a stop that half-lands beats no stop — but the response
// says so, so a client doesn't render a park that didn't happen.
func TestInterrupt_HoldUnsupportedDegradesWithHeader(t *testing.T) {
	t.Parallel()
	h, cleanupLog := openTestEventLog(t)
	defer cleanupLog()
	ag := &interruptibleRegistrant{
		eventfulRegistrant: eventfulRegistrant{
			stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s1"},
			handle:         h,
		},
	}
	ag.canInterrupt.Store(true)
	base := newPauseHarness(t, ag)

	resp := postJSON(t, base+"/sessions/core-agent/s1/interrupt", "")
	got := decodeInterrupt(t, resp)
	if !got.Interrupted {
		t.Error("Interrupted = false, want true (cancel must still land)")
	}
	if got.Paused {
		t.Error("Paused = true, want false (registrant can't hold)")
	}
	if hdr := resp.Header.Get("X-Hold"); hdr != "unsupported" {
		t.Errorf("X-Hold = %q, want %q", hdr, "unsupported")
	}
}

// Interrupting the parent leaves background subagents running. An
// operator who believes they stopped everything has to be told
// otherwise — that's the #623-shaped runaway that survived every
// /interrupt before this.
func TestInterrupt_ReportsAndOptionallyStopsSubagents(t *testing.T) {
	t.Parallel()
	newAgent := func() *pausingRegistrant {
		return &pausingRegistrant{
			stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s1"},
			live: []AgentInfo{
				{Name: "cluster", ID: "cluster", Status: AgentStatusRunning},
				{Name: "scribe", ID: "scribe", Status: AgentStatusCompleted},
			},
		}
	}

	t.Run("reports by default", func(t *testing.T) {
		t.Parallel()
		ag := newAgent()
		base := newPauseHarness(t, ag)
		got := decodeInterrupt(t, postJSON(t, base+"/sessions/core-agent/s1/interrupt", ""))
		if len(got.RunningSubagents) != 1 || got.RunningSubagents[0].Name != "cluster" {
			t.Errorf("RunningSubagents = %+v, want [cluster]", got.RunningSubagents)
		}
		if len(got.StoppedSubagents) != 0 {
			t.Errorf("StoppedSubagents = %+v, want none without the flag", got.StoppedSubagents)
		}
		if len(ag.stops) != 0 {
			t.Errorf("stopped %v without stop_subagents", ag.stops)
		}
	})

	t.Run("stops on request", func(t *testing.T) {
		t.Parallel()
		ag := newAgent()
		base := newPauseHarness(t, ag)
		got := decodeInterrupt(t, postJSON(t,
			base+"/sessions/core-agent/s1/interrupt", `{"stop_subagents":true}`))
		if len(got.StoppedSubagents) != 1 || got.StoppedSubagents[0].Name != "cluster" {
			t.Errorf("StoppedSubagents = %+v, want [cluster]", got.StoppedSubagents)
		}
		if len(got.RunningSubagents) != 0 {
			t.Errorf("RunningSubagents = %+v, want none after the stop", got.RunningSubagents)
		}
	})

	// stopped_subagents is a claim about what the interrupt did. A
	// subagent whose goroutine exits in the window between the listing
	// and the stop was not stopped by the operator, and saying it was
	// is the same false credit #897 found on the single-agent route —
	// the handler's own comment already promised it was skipped.
	t.Run("does not claim one that finished between the list and the stop", func(t *testing.T) {
		t.Parallel()
		ag := newAgent()
		ag.raceToFinish = "cluster"
		base := newPauseHarness(t, ag)
		got := decodeInterrupt(t, postJSON(t,
			base+"/sessions/core-agent/s1/interrupt", `{"stop_subagents":true}`))
		if len(got.StoppedSubagents) != 0 {
			t.Errorf("StoppedSubagents = %+v, want none — it finished on its own", got.StoppedSubagents)
		}
		if len(ag.stops) != 0 {
			t.Errorf("stops = %v, want none", ag.stops)
		}
	})
}

func TestPause_RoundTripAndIdempotence(t *testing.T) {
	t.Parallel()
	ag := &pausingRegistrant{stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s1"}}
	base := newPauseHarness(t, ag)

	var first PauseResponse
	resp := postJSON(t, base+"/sessions/core-agent/s1/pause", `{"reason":"deploying"}`)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if err := json.NewDecoder(resp.Body).Decode(&first); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !first.Paused || !first.Transitioned {
		t.Errorf("first pause = %+v, want paused+transitioned", first)
	}
	if first.State != AgentStatePaused {
		t.Errorf("State = %q, want %q", first.State, AgentStatePaused)
	}
	if first.Reason != "deploying" {
		t.Errorf("Reason = %q, want %q", first.Reason, "deploying")
	}

	var second PauseResponse
	if err := json.NewDecoder(postJSON(t, base+"/sessions/core-agent/s1/pause", "").Body).Decode(&second); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !second.Paused || second.Transitioned {
		t.Errorf("second pause = %+v, want paused=true transitioned=false", second)
	}
}

// A parked loop must not report itself idle: that's the status lie
// AgentStatePaused existed for but nothing ever set.
func TestStatus_ReportsPausedForNonStatusProvider(t *testing.T) {
	t.Parallel()
	ag := &pausingRegistrant{stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s1"}}
	base := newPauseHarness(t, ag)
	postJSON(t, base+"/sessions/core-agent/s1/pause", `{"reason":"deploying"}`)

	resp, err := http.Get(base + "/sessions/core-agent/s1/status")
	if err != nil {
		t.Fatalf("GET status: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var got StatusInfo
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.State != AgentStatePaused {
		t.Errorf("State = %q, want %q", got.State, AgentStatePaused)
	}
	if got.PauseReason != "deploying" {
		t.Errorf("PauseReason = %q, want %q", got.PauseReason, "deploying")
	}
	if got.PausedSince.IsZero() {
		t.Error("PausedSince is zero, want the park timestamp")
	}
}

func TestResume_DelegatesWithCallerAndDefaultsMode(t *testing.T) {
	t.Parallel()
	ag := &pausingRegistrant{stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s1"}}
	base := newPauseHarness(t, ag)
	postJSON(t, base+"/sessions/core-agent/s1/pause", "")

	var got ResumeResponse
	resp := postJSON(t, base+"/sessions/core-agent/s1/resume", `{"steer":"check the nodes"}`)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Resumed {
		t.Error("Resumed = false, want true")
	}
	if got.Mode != ResumeModeSteer {
		t.Errorf("Mode = %q, want %q", got.Mode, ResumeModeSteer)
	}
	if got.Session != "s1" {
		t.Errorf("Session = %q, want s1", got.Session)
	}
	if len(ag.resumes) != 1 || ag.resumes[0].Steer != "check the nodes" {
		t.Fatalf("registrant saw resumes %+v, want one carrying the steer text", ag.resumes)
	}
	// The resumed turn is the operator's, so it must be attributed to
	// them and not to whoever last drove the session.
	if len(ag.callers) != 1 || ag.callers[0].Identity == "" {
		t.Errorf("resume caller = %+v, want the resolved operator identity", ag.callers)
	}
}

func TestResume_RejectsBadInput(t *testing.T) {
	t.Parallel()
	ag := &pausingRegistrant{stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s1"}}
	base := newPauseHarness(t, ag)

	for _, tc := range []struct{ name, body string }{
		{"unknown mode", `{"mode":"sideways"}`},
		{"steer mode with no text", `{"mode":"steer"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := postJSON(t, base+"/sessions/core-agent/s1/resume", tc.body)
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("status %d, want 400", resp.StatusCode)
			}
		})
	}
}

// Mutation endpoints 501 rather than silently no-op'ing when the
// registrant can't honour them — an operator POSTing for an effect has
// to know when there wasn't one.
func TestPauseResume_NotImplementedWithoutController(t *testing.T) {
	t.Parallel()
	ag := &stubRegistrant{app: "core-agent", user: "u", sid: "s1"}
	base := newPauseHarness(t, ag)

	for _, path := range []string{"/pause", "/resume", "/agents/cluster/stop"} {
		resp := postJSON(t, base+"/sessions/core-agent/s1"+path, "")
		if resp.StatusCode != http.StatusNotImplemented {
			t.Errorf("POST %s: status %d, want 501", path, resp.StatusCode)
		}
	}
}

// A client can't offer "what do you want me to do instead?" unless it
// knows the producer can actually hold, so the capability frame has to
// carry the answer.
func TestBuildFeatures_PauseFromController(t *testing.T) {
	t.Parallel()
	entry := &Entry{
		AppName:   "core-agent",
		SessionID: "s1",
		Agent:     &pausingRegistrant{stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s1"}},
	}
	if got := buildFeatures(entry, nil); !got[featurePause] {
		t.Errorf("feature %q = false for a registrant implementing PauseController", featurePause)
	}

	plain := &Entry{
		AppName:   "core-agent",
		SessionID: "s1",
		Agent:     &stubRegistrant{app: "core-agent", user: "u", sid: "s1"},
	}
	if got := buildFeatures(plain, nil); got[featurePause] {
		t.Errorf("feature %q = true for a registrant that can't hold", featurePause)
	}
}

func TestStopAgent_StopsOneRunawaySubagent(t *testing.T) {
	t.Parallel()
	ag := &pausingRegistrant{
		stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s1"},
		live:           []AgentInfo{{Name: "cluster", ID: "cluster", Status: AgentStatusRunning}},
	}
	base := newPauseHarness(t, ag)

	resp := postJSON(t, base+"/sessions/core-agent/s1/agents/cluster/stop", "")
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if len(ag.stops) != 1 || ag.stops[0] != "cluster" {
		t.Errorf("stops = %v, want [cluster]", ag.stops)
	}
	var got StopAgentResponse
	decodeStop(t, resp, &got)
	if !got.Stopped {
		t.Errorf("stopped = false for a subagent this call halted")
	}
	if got.Status != AgentStatusStopped {
		t.Errorf("status = %q, want %q", got.Status, AgentStatusStopped)
	}

	// Second stop: the handle is still registered, now terminal. 200
	// with stopped:false — the operator aimed correctly and there is
	// nothing to retry, but they did not do it (#897).
	again := postJSON(t, base+"/sessions/core-agent/s1/agents/cluster/stop", "")
	if again.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(again.Body)
		t.Fatalf("repeat stop: status %d, want 200: %s", again.StatusCode, body)
	}
	var repeat StopAgentResponse
	decodeStop(t, again, &repeat)
	if repeat.Stopped {
		t.Errorf("repeat stop: stopped = true, want false — this call halted nothing")
	}
	if repeat.Status != AgentStatusStopped {
		t.Errorf("repeat stop: status = %q, want %q", repeat.Status, AgentStatusStopped)
	}
	if len(ag.stops) != 1 {
		t.Errorf("stops = %v after a repeat stop, want the one real stop", ag.stops)
	}
}

// TestStopAgent_AlreadyFinishedIsNotAStop is the #897 case proper: a
// subagent that completed on its own before the operator's request
// arrived. It is registered, so it is not a 404; it was not halted by
// this call, so it is not `stopped: true`. Through v1.11.0 it was
// both-wrong — 200 claiming a stop the operator never performed.
func TestStopAgent_AlreadyFinishedIsNotAStop(t *testing.T) {
	t.Parallel()
	ag := &pausingRegistrant{
		stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s1"},
		live:           []AgentInfo{{Name: "cluster", ID: "cluster", Status: AgentStatusCompleted}},
	}
	base := newPauseHarness(t, ag)

	resp := postJSON(t, base+"/sessions/core-agent/s1/agents/cluster/stop", "")
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d, want 200 (the name exists): %s", resp.StatusCode, body)
	}
	var got StopAgentResponse
	decodeStop(t, resp, &got)
	if got.Stopped {
		t.Errorf("stopped = true for a subagent that had already completed")
	}
	if got.Status != AgentStatusCompleted {
		t.Errorf("status = %q, want %q — the operator needs to see it finished on its own", got.Status, AgentStatusCompleted)
	}
	if len(ag.stops) != 0 {
		t.Errorf("stops = %v, want none", ag.stops)
	}
}

// TestStopAgent_UnknownNameIs404 pins what 404 is reserved for: a name
// the manager has never registered. That is the only case where the
// operator missed and a retry would miss too.
func TestStopAgent_UnknownNameIs404(t *testing.T) {
	t.Parallel()
	ag := &pausingRegistrant{
		stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s1"},
		live:           []AgentInfo{{Name: "cluster", ID: "cluster", Status: AgentStatusRunning}},
	}
	base := newPauseHarness(t, ag)

	resp := postJSON(t, base+"/sessions/core-agent/s1/agents/ghost/stop", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status %d, want 404 for a name that was never registered", resp.StatusCode)
	}
}

// legacyOnly presents a registrant that carries the pre-1.12.0
// AgentStopper and nothing newer — an embedder pinned to an older
// core-agent, as the handler sees it. It embeds the Registrant
// INTERFACE rather than the concrete type precisely so that
// AttachStopAgentOutcome is not promoted along with everything else.
type legacyOnly struct {
	Registrant
	inner *pausingRegistrant
}

func (l legacyOnly) AttachStopAgent(name string) (bool, error) {
	out, err := l.inner.AttachStopAgentOutcome(name)
	return out.Found, err
}

// TestStopAgent_LegacyStopperKeepsItsOldAnswer pins the fallback arm:
// a registrant that can only answer "is this name registered" gets the
// answer it always gave, rather than a status the handler invented.
func TestStopAgent_LegacyStopperKeepsItsOldAnswer(t *testing.T) {
	t.Parallel()
	inner := &pausingRegistrant{
		stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s1"},
		live:           []AgentInfo{{Name: "cluster", ID: "cluster", Status: AgentStatusCompleted}},
	}
	base := newPauseHarness(t, legacyOnly{Registrant: inner, inner: inner})

	resp := postJSON(t, base+"/sessions/core-agent/s1/agents/cluster/stop", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	var got StopAgentResponse
	decodeStop(t, resp, &got)
	if !got.Stopped {
		t.Errorf("stopped = false; a pre-1.12.0 registrant's bool can only mean found, and the handler must not pretend otherwise")
	}
	if got.Status != "" {
		t.Errorf("status = %q, want empty — the old interface cannot name one", got.Status)
	}
}
