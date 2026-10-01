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

// Permission prompts follow the session on screen across /switch, /new
// and /attach <url> <sid> (#1183).
//
// core-tui exports no headless harness, so these tests play its part
// from the SwitchTarget contract (tui/slash.go, applySwitchTarget in
// tui/update.go at the pinned version): attaching to a session is
// ranging over its Events, applying a target is cancelling the
// outgoing Events context and then ranging over the incoming Agent's,
// and the operator is a prompter the test answers for.

package coretuiremote

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	coretui "github.com/go-steer/core-tui/tui"

	"github.com/go-steer/core-agent/v2/internal/attachclient"
	"github.com/go-steer/core-agent/v2/pkg/attach"
)

const promptTestWait = 5 * time.Second

// promptDaemon is a daemon whose sessions raise permission prompts on
// demand: push sends a frame to every open /perms/stream of a session,
// and every /perms/respond is recorded with the session it named.
type promptDaemon struct {
	*httptest.Server
	protocol string // advertised in the capabilities frame on /events

	mu       sync.Mutex
	sessions []attachclient.SessionDescriptor
	listFail bool
	created  attachclient.NewSessionResponse
	streams  map[string][]chan attach.PromptFrame
	replies  chan promptReply
}

type promptReply struct {
	sid string
	attach.PromptResponse
}

func startPromptDaemon(t *testing.T, protocol string, sids ...string) *promptDaemon {
	t.Helper()
	d := &promptDaemon{
		protocol: protocol,
		streams:  map[string][]chan attach.PromptFrame{},
		replies:  make(chan promptReply, 64),
	}
	for _, sid := range sids {
		d.sessions = append(d.sessions, attachclient.SessionDescriptor{SessionID: sid})
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /sessions", func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.listFail {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"sessions": d.sessions})
	})
	mux.HandleFunc("POST /sessions", func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(d.created)
	})
	mux.HandleFunc("GET /sessions/{sid}/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, "event: capabilities\ndata: {\"protocol_version\":%q,\"event_types\":[\"agent\"]}\n\n", d.protocol)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	})
	mux.HandleFunc("GET /sessions/{sid}/perms/stream", func(w http.ResponseWriter, r *http.Request) {
		sid := r.PathValue("sid")
		ch := make(chan attach.PromptFrame, 8)
		d.mu.Lock()
		d.streams[sid] = append(d.streams[sid], ch)
		d.mu.Unlock()
		defer func() {
			d.mu.Lock()
			defer d.mu.Unlock()
			subs := d.streams[sid]
			for i, c := range subs {
				if c == ch {
					d.streams[sid] = append(subs[:i:i], subs[i+1:]...)
					break
				}
			}
		}()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f, _ := w.(http.Flusher)
		if f != nil {
			f.Flush()
		}
		for {
			select {
			case <-r.Context().Done():
				return
			case frame := <-ch:
				payload, _ := json.Marshal(frame)
				_, _ = io.WriteString(w, "data: "+string(payload)+"\n\n")
				if f != nil {
					f.Flush()
				}
			}
		}
	})
	mux.HandleFunc("POST /sessions/{sid}/perms/respond", func(w http.ResponseWriter, r *http.Request) {
		var body attach.PromptResponse
		_ = json.NewDecoder(r.Body).Decode(&body)
		d.replies <- promptReply{sid: r.PathValue("sid"), PromptResponse: body}
		_ = json.NewEncoder(w).Encode(attach.PromptRespondResponse{Acknowledged: true})
	})
	d.Server = httptest.NewServer(mux)
	t.Cleanup(d.Close)
	return d
}

func (d *promptDaemon) client(t *testing.T) *attachclient.Client {
	t.Helper()
	parsed, err := attachclient.ParseURL(d.URL)
	if err != nil {
		t.Fatal(err)
	}
	return attachclient.New(parsed, "", 0)
}

func (d *promptDaemon) subscribers(sid string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.streams[sid])
}

// waitSubscribers waits until sid has exactly n open /perms/stream
// subscriptions.
func (d *promptDaemon) waitSubscribers(t *testing.T, sid string, n int) {
	t.Helper()
	deadline := time.Now().Add(promptTestWait)
	for d.subscribers(sid) != n {
		if time.Now().After(deadline) {
			t.Fatalf("session %s has %d prompt subscribers, want %d", sid, d.subscribers(sid), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// push raises a prompt on sid, to every bridge subscribed to it.
func (d *promptDaemon) push(t *testing.T, sid, id string) {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.streams[sid]) == 0 {
		t.Fatalf("push %s on %s: no bridge is subscribed", id, sid)
	}
	for _, ch := range d.streams[sid] {
		ch <- attach.PromptFrame{ID: id, Kind: "bash", ToolName: "bash", Detail: sid + ": " + id}
	}
}

func (d *promptDaemon) nextReply(t *testing.T) promptReply {
	t.Helper()
	select {
	case r := <-d.replies:
		return r
	case <-time.After(promptTestWait):
		t.Fatal("the daemon never got a /perms/respond")
		return promptReply{}
	}
}

// operator is a prompter the test answers for. Each ask blocks until
// the test replies, the way core-tui's modal does.
type operator struct {
	asks chan operatorAsk
}

type operatorAsk struct {
	req      coretui.PermissionRequest
	detailed bool
	reply    chan coretui.PermissionOutcome
}

func newOperator() remotePrompter { return &operator{asks: make(chan operatorAsk, 8)} }

func (o *operator) ask(ctx context.Context, req coretui.PermissionRequest, detailed bool) (coretui.PermissionOutcome, error) {
	a := operatorAsk{req: req, detailed: detailed, reply: make(chan coretui.PermissionOutcome, 1)}
	o.asks <- a
	select {
	case out := <-a.reply:
		return out, nil
	case <-ctx.Done():
		return coretui.PermissionOutcome{Decision: coretui.DecisionDeny}, ctx.Err()
	}
}

func (o *operator) AskApproval(ctx context.Context, req coretui.PermissionRequest) (coretui.PermissionDecision, error) {
	out, err := o.ask(ctx, req, false)
	return out.Decision, err
}

func (o *operator) AskApprovalDetailed(ctx context.Context, req coretui.PermissionRequest) (coretui.PermissionOutcome, error) {
	return o.ask(ctx, req, true)
}

// shown waits for the next prompt put in front of this operator.
func (o *operator) shown(t *testing.T) operatorAsk {
	t.Helper()
	select {
	case a := <-o.asks:
		return a
	case <-time.After(promptTestWait):
		t.Fatal("no prompt reached the operator")
		return operatorAsk{}
	}
}

func asOperator(t *testing.T, p coretui.PermissionPrompter) *operator {
	t.Helper()
	o, ok := p.(*operator)
	if !ok || o == nil {
		t.Fatalf("prompter = %T (%v), want the session's own *operator", p, p)
	}
	return o
}

// tuiView is core-tui attached to one Agent: its Events drain.
type tuiView struct {
	cancel context.CancelFunc
	done   chan struct{}
}

func attachView(agent coretui.Agent) *tuiView {
	ctx, cancel := context.WithCancel(context.Background())
	v := &tuiView{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(v.done)
		for range agent.(coretui.LiveAgent).Events(ctx) {
		}
	}()
	return v
}

func (v *tuiView) detach(t *testing.T) {
	t.Helper()
	v.cancel()
	select {
	case <-v.done:
	case <-time.After(promptTestWait):
		t.Fatal("Events did not return after its context was cancelled")
	}
}

// apply is applySwitchTarget's part in the prompt lifecycle: step 1
// cancels the outgoing Events context, step 8 drains the incoming
// Agent's Events.
func apply(t *testing.T, v *tuiView, tgt coretui.SwitchTarget) *tuiView {
	t.Helper()
	if tgt.Agent == nil {
		t.Fatal("SwitchTarget has no Agent")
	}
	v.detach(t)
	return attachView(tgt.Agent)
}

// boundAdapter is the startup session as cmd/core-agent-tui wires it:
// prompts bound, core-tui attached.
func boundAdapter(t *testing.T, a *Adapter) (*operator, *tuiView) {
	t.Helper()
	root, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	op := asOperator(t, a.bindPrompts(root, io.Discard, newOperator))
	v := attachView(a)
	t.Cleanup(v.cancel)
	return op, v
}

// answer allows the prompt in front of op and checks it was the one
// sid raised as id, and that the allow reached sid on the daemon.
func answer(t *testing.T, d *promptDaemon, op *operator, sid, id string) {
	t.Helper()
	d.push(t, sid, id)
	a := op.shown(t)
	if want := sid + ": " + id; a.req.Detail != want {
		t.Fatalf("prompt shown = %q, want %q", a.req.Detail, want)
	}
	a.reply <- coretui.PermissionOutcome{Decision: coretui.DecisionAllowOnce}
	if r := d.nextReply(t); r.sid != sid || r.ID != id || r.Decision != "allow-once" {
		t.Fatalf("respond = %+v, want allow-once for %s on %s", r, id, sid)
	}
}

func TestPrompts_FollowSwitch(t *testing.T) {
	t.Parallel()
	d := startPromptDaemon(t, "1.14.0", "s1", "s2")
	a := New(d.client(t), "/sessions/s1")
	op1, v := boundAdapter(t, a)
	d.waitSubscribers(t, "s1", 1)
	answer(t, d, op1, "s1", "before")

	tgt, err := a.SwitchToSession("s2")
	if err != nil {
		t.Fatalf("SwitchToSession: %v", err)
	}
	op2 := asOperator(t, tgt.Prompter)
	if op2 == op1 {
		t.Fatal("the target carries the outgoing session's prompter")
	}
	v = apply(t, v, tgt)
	t.Cleanup(v.cancel)

	d.waitSubscribers(t, "s2", 1)
	d.waitSubscribers(t, "s1", 0) // the outgoing bridge stopped
	answer(t, d, op2, "s2", "after")
}

func TestPrompts_FollowNew(t *testing.T) {
	t.Parallel()
	d := startPromptDaemon(t, "1.14.0", "s1")
	d.created = attachclient.NewSessionResponse{SessionID: "fresh", URL: d.URL + "/sessions/fresh"}
	a := New(d.client(t), "/sessions/s1")
	_, v := boundAdapter(t, a)
	d.waitSubscribers(t, "s1", 1)

	res, err := a.invokeAsyncSlash(context.Background(), "new", "")
	if err != nil || res.SwitchTo == nil {
		t.Fatalf("/new = %+v, %v; want a SwitchTo", res, err)
	}
	op2 := asOperator(t, res.SwitchTo.Prompter)
	v = apply(t, v, *res.SwitchTo)
	t.Cleanup(v.cancel)

	d.waitSubscribers(t, "fresh", 1)
	d.waitSubscribers(t, "s1", 0)
	answer(t, d, op2, "fresh", "p")
}

// /attach to a daemon on a newer protocol: the incoming session's
// prompts reach the operator, and they offer "r" because the bridge's
// host is now the incoming adapter, whose capabilities frame says
// 1.15.0, not the startup daemon's 1.14.0.
func TestPrompts_FollowAttach_AndItsProtocol(t *testing.T) {
	t.Parallel()
	old := startPromptDaemon(t, "1.14.0", "s1")
	peer := startPromptDaemon(t, "1.15.0", "s9")
	factory := func(endpoint string) (*attachclient.Client, error) {
		p, err := attachclient.ParseURL(endpoint)
		if err != nil {
			return nil, err
		}
		return attachclient.New(p, "", 0), nil
	}
	a := NewWithClientFactory(old.client(t), "/sessions/s1", factory)
	op1, v := boundAdapter(t, a)
	old.waitSubscribers(t, "s1", 1)
	old.push(t, "s1", "plain")
	if ask := op1.shown(t); ask.detailed {
		t.Fatal("a 1.14.0 daemon's prompt offered the deny reason")
	} else {
		ask.reply <- coretui.PermissionOutcome{Decision: coretui.DecisionDeny}
	}
	if r := old.nextReply(t); r.ID != "plain" || r.Decision != "deny" {
		t.Fatalf("respond = %+v, want a plain deny for plain", r)
	}

	res, err := a.invokeAsyncSlash(context.Background(), "attach", peer.URL+" s9")
	if err != nil || res.SwitchTo == nil {
		t.Fatalf("/attach = %+v, %v; want a SwitchTo", res, err)
	}
	op2 := asOperator(t, res.SwitchTo.Prompter)
	v = apply(t, v, *res.SwitchTo)
	t.Cleanup(v.cancel)

	peer.waitSubscribers(t, "s9", 1)
	old.waitSubscribers(t, "s1", 0)
	peer.push(t, "s9", "why")
	ask := op2.shown(t)
	if !ask.detailed {
		t.Fatal("the 1.15.0 daemon's prompt did not offer the deny reason after /attach")
	}
	ask.reply <- coretui.PermissionOutcome{Decision: coretui.DecisionDeny, Reason: "wrong cluster"}
	if r := peer.nextReply(t); r.sid != "s9" || r.Decision != "deny" || r.Reason != "wrong cluster" {
		t.Fatalf("respond = %+v, want the deny with its reason on s9", r)
	}
}

// A target core-tui never applies (esc while SessionInput.Submit was
// dialling) starts no bridge, and the session on screen keeps its own.
func TestPrompts_DiscardedTargetLeavesTheLiveBridge(t *testing.T) {
	t.Parallel()
	d := startPromptDaemon(t, "1.14.0", "s1", "s2")
	a := New(d.client(t), "/sessions/s1")
	op1, _ := boundAdapter(t, a)
	d.waitSubscribers(t, "s1", 1)

	tgt, err := a.SwitchToSession("s2")
	if err != nil {
		t.Fatalf("SwitchToSession: %v", err)
	}
	asOperator(t, tgt.Prompter) // built, but discarded below
	time.Sleep(150 * time.Millisecond)
	if n := d.subscribers("s2"); n != 0 {
		t.Fatalf("an unapplied target opened %d prompt streams on s2", n)
	}
	d.waitSubscribers(t, "s1", 1)
	answer(t, d, op1, "s1", "still-here")
}

// A switch that fails leaves the old session attached, and so its
// bridge. Every failure SwitchToSession can return today happens before
// handOff builds anything for the incoming session, so this pins that
// ordering: should a failure ever come after it, the live bridge must
// still be the one running.
func TestPrompts_FailedSwitchLeavesTheLiveBridge(t *testing.T) {
	t.Parallel()
	d := startPromptDaemon(t, "1.14.0", "s1", "s2")
	a := New(d.client(t), "/sessions/s1")
	op1, _ := boundAdapter(t, a)
	d.waitSubscribers(t, "s1", 1)

	d.mu.Lock()
	d.listFail = true
	d.mu.Unlock()
	if _, err := a.SwitchToSession("s2"); err == nil {
		t.Fatal("SwitchToSession succeeded against a failing GET /sessions")
	}
	d.waitSubscribers(t, "s1", 1)
	answer(t, d, op1, "s1", "still-here")
}

// A prompt on screen at switch time is answered by core-tui with a
// deny ("superseded") just after it cancels the outgoing Events
// context. Stopping the bridge must not swallow that deny: the old
// session's tool call would otherwise wait out the approval timeout.
func TestPrompts_SupersededPromptDenyReachesTheOldSession(t *testing.T) {
	t.Parallel()
	d := startPromptDaemon(t, "1.14.0", "s1", "s2")
	a := New(d.client(t), "/sessions/s1")
	op1, v := boundAdapter(t, a)
	d.waitSubscribers(t, "s1", 1)
	d.push(t, "s1", "on-screen")
	held := op1.shown(t)

	tgt, err := a.SwitchToSession("s2")
	if err != nil {
		t.Fatalf("SwitchToSession: %v", err)
	}
	asOperator(t, tgt.Prompter)
	v = apply(t, v, tgt) // step 1: the outgoing Events context ends
	t.Cleanup(v.cancel)
	held.reply <- coretui.PermissionOutcome{Decision: coretui.DecisionDeny} // step 2: superseded

	if r := d.nextReply(t); r.sid != "s1" || r.ID != "on-screen" || r.Decision != "deny" {
		t.Fatalf("respond = %+v, want the superseded deny for on-screen on s1", r)
	}
	d.waitSubscribers(t, "s1", 0)
}

// The bridge ends with the program even though core-tui never cancels
// the last session's Events context on the way out.
func TestPrompts_BridgeStopsWithTheProgram(t *testing.T) {
	t.Parallel()
	d := startPromptDaemon(t, "1.14.0", "s1")
	a := New(d.client(t), "/sessions/s1")
	root, cancel := context.WithCancel(context.Background())
	a.bindPrompts(root, io.Discard, newOperator)
	v := attachView(a)
	t.Cleanup(v.cancel)
	d.waitSubscribers(t, "s1", 1)
	cancel()
	d.waitSubscribers(t, "s1", 0)
}

// Without BindPrompts nothing changes: no bridge, and the target's
// Prompter is a nil interface rather than a typed nil, which core-tui
// would install as the prompter and then dereference.
func TestPrompts_UnboundTargetsCarryNoPrompter(t *testing.T) {
	t.Parallel()
	d := startPromptDaemon(t, "1.14.0", "s1", "s2")
	a := New(d.client(t), "/sessions/s1")
	v := attachView(a)
	t.Cleanup(v.cancel)
	tgt, err := a.SwitchToSession("s2")
	if err != nil {
		t.Fatalf("SwitchToSession: %v", err)
	}
	if tgt.Prompter != nil {
		t.Fatalf("Prompter = %#v, want nil when prompts are not bound", tgt.Prompter)
	}
	time.Sleep(100 * time.Millisecond)
	if n := d.subscribers("s1"); n != 0 {
		t.Fatalf("an unbound adapter opened %d prompt streams", n)
	}
}

// The grace a shown prompt gets past the bridge's stop starts when the
// bridge stops, not before, and ends on its own.
func TestGraceAfter(t *testing.T) {
	t.Parallel()
	parent, stop := context.WithCancel(context.Background())
	ctx, cancel := graceAfter(parent, 50*time.Millisecond)
	defer cancel()
	stop()
	select {
	case <-ctx.Done():
		t.Fatal("the graced context ended with its parent")
	case <-time.After(20 * time.Millisecond):
	}
	select {
	case <-ctx.Done():
	case <-time.After(promptTestWait):
		t.Fatal("the graced context outlived its grace")
	}

	ctx2, cancel2 := graceAfter(context.Background(), time.Hour)
	cancel2()
	if ctx2.Err() == nil {
		t.Fatal("cancel did not end the graced context")
	}
}
