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

package coretuiremote

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	coretui "github.com/go-steer/core-tui/tui"
	"golang.org/x/mod/semver"

	"github.com/go-steer/core-agent/v2/internal/attachclient"
	"github.com/go-steer/core-agent/v2/pkg/attach"
)

// denyReasonProtocol is the first attach protocol whose
// /perms/respond takes a "reason" with a deny (#1165). An older daemon
// accepts the field and drops it, so the 200 it answers proves
// nothing; only the advertised protocol_version says whether the
// operator's words will reach the model.
const denyReasonProtocol = "v1.15.0"

// protocolWait bounds how long the bridge holds the first prompt while
// the daemon's protocol version is still unknown. The capabilities
// frame is the first frame of a stream core-tui opens at startup, so it
// normally lands within a round trip of the prompt; this only has to
// cover that race, not a slow daemon.
const protocolWait = 3 * time.Second

// awaitDaemonProtocol returns once host knows the daemon's protocol
// version, after wait, or when ctx ends, whichever is first. No host,
// or a version already known, returns at once.
func awaitDaemonProtocol(ctx context.Context, host PromptBridgeHost, wait time.Duration) {
	if host == nil || host.DaemonProtocolVersion() != "" {
		return
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-host.DaemonProtocolKnown():
	case <-t.C:
	case <-ctx.Done():
	}
}

// PromptBridgeHost is what the prompt bridge needs from the attached
// session beyond the client: what protocol the daemon speaks, and a
// way to tell the operator something in the chat. *Adapter implements
// it. A nil host means neither is known, and the bridge behaves as it
// did before deny reasons existed.
type PromptBridgeHost interface {
	// DaemonProtocolVersion is the daemon's advertised protocol
	// version, or "" while it is not yet known.
	DaemonProtocolVersion() string
	// DaemonProtocolKnown is closed once DaemonProtocolVersion has been
	// read from the daemon. It may never close (a daemon too old to
	// advertise one); the bridge bounds its wait.
	DaemonProtocolKnown() <-chan struct{}
	// NotifyOperator surfaces err as a row in the chat. Must not block.
	NotifyOperator(err error)
}

// detachGrace is how long a prompt the bridge has already handed to
// the operator outlives the bridge being stopped. core-tui stops the
// bridge (by cancelling the outgoing session's Events context) in the
// same Update call that answers any prompt on screen with a deny,
// "superseded", so the answer is normally in hand microseconds after
// the stop. The grace lets that deny reach the daemon instead of
// leaving the tool call blocked until the daemon's approval timeout.
// The same holds for the deny core-tui (v0.28.1 and later) gives a
// prompt the bridge handed over in the instant of the switch: it
// arrives after the stop, on the outgoing prompter. Otherwise the grace
// only bounds a prompt nobody will answer any more.
const detachGrace = 5 * time.Second

// promptBinding is the attach client's permission-prompt wiring. It is
// shared by every Adapter one operator session hops through (handOff
// passes it on, as it does the view), so each session the operator
// switches to gets a prompter of its own built the same way.
type promptBinding struct {
	// root is the program's lifetime. No bridge outlives it, whatever
	// core-tui does or does not cancel on the way out.
	root context.Context
	// errOut receives the bridges' one-line network diagnostics.
	errOut io.Writer
	// newPrompter builds the prompter for one session. coretui's in
	// production; a test supplies one that answers for the operator.
	newPrompter func() remotePrompter
}

// BindPrompts wires the remote daemon's permission prompts into the
// TUI and returns the prompter to pass as coretui.Options.Prompter.
//
// Prompts follow the session on screen (#1183). Each Adapter owns a
// prompter of its own, and its bridge to <sessionPath>/perms/stream
// runs exactly while core-tui is attached to it: it starts when
// core-tui calls Events on the Adapter, and stops when core-tui
// cancels that call's context or ctx ends. Every SwitchTarget the
// Adapter returns (/switch, /new, /attach <url> <sid>) carries the
// incoming Adapter's prompter, and that Adapter is its bridge's
// PromptBridgeHost, so the protocol version that decides whether the
// prompt offers "r" comes from the daemon the operator is now looking
// at.
//
// Tying the bridge to Events rather than to building the target is
// what makes the lifecycle safe. core-tui calls Events on an incoming
// Agent only once it has applied the switch, and cancels the outgoing
// Agent's Events context in that same step (or, when the switch lands
// before that stream's start was recorded, as soon as it is). A target core-tui never
// applies (the operator pressed esc while SessionInput.Submit was
// still dialling) never starts a bridge, so it has nothing to leak; a
// switch that fails never cancels the live session's context, so its
// bridge keeps running. The outgoing bridge stops reading new prompts
// at once, and a prompt it already showed the operator gets
// detachGrace to deliver the deny core-tui answers it with.
//
// The bridge stops only once the outgoing Events context is cancelled,
// so it can still hand the outgoing prompter a prompt in the instant
// of the switch. Since core-tui v0.28.1 (core-tui#353) that prompt
// never reaches the new session. Replacing a prompter releases its
// listener, and core-tui answers a prompt the released listener had
// already read, or one still queued on the channel, with a deny on
// the prompter it came from. That deny goes through this bridge's
// ask after the bridge's context has ended, which is why the ask keeps
// detachGrace. A prompt handed over after core-tui stopped reading is
// never read: its ask gives up after detachGrace without answering,
// and the daemon re-offers the prompt when the operator returns.
// Before v0.28.1 such a prompt could be drawn over the new session,
// with the answer going to the new prompter, and every switch left a
// listener parked on the old prompter for the life of the program.
//
// errOut receives one-line diagnostics about the bridges' network
// trouble (transient stream errors, 404 on response). Pass nil for
// stderr.
//
// Call it once, before coretui.Run, on the Adapter passed as
// Options.Agent.
func (a *Adapter) BindPrompts(ctx context.Context, errOut io.Writer) coretui.PermissionPrompter {
	return a.bindPrompts(ctx, errOut, func() remotePrompter { return coretui.NewPrompter() })
}

func (a *Adapter) bindPrompts(ctx context.Context, errOut io.Writer, newPrompter func() remotePrompter) coretui.PermissionPrompter {
	a.prompts = &promptBinding{root: ctx, errOut: errOut, newPrompter: newPrompter}
	a.prompter = newPrompter()
	return a.prompter
}

// targetPrompter is the value for SwitchTarget.Prompter when core-tui
// switches to a: a's own prompter, or a nil interface when prompts are
// not bound. Never a typed nil, which core-tui would install and then
// dereference.
func (a *Adapter) targetPrompter() coretui.PermissionPrompter {
	if a.prompter == nil {
		return nil
	}
	return a.prompter
}

// startPromptBridge starts a's bridge for the Events call whose
// context is viewCtx, and returns the func that stops it. Nil when
// prompts are not bound. The bridge's context ends with viewCtx or
// with the program, whichever is first.
func (a *Adapter) startPromptBridge(viewCtx context.Context) (stop func()) {
	b, p := a.prompts, a.prompter
	if b == nil || p == nil {
		return nil
	}
	ctx, cancel := context.WithCancel(viewCtx)
	unhook := context.AfterFunc(b.root, cancel)
	go runRemotePromptBridge(ctx, a.client, a.sessionPath, p, b.errOut, a)
	return func() {
		unhook()
		cancel()
	}
}

// graceAfter returns a context that outlives ctx by grace: it is not
// cancelled when ctx is, only grace later (or by the returned cancel).
func graceAfter(ctx context.Context, grace time.Duration) (context.Context, context.CancelFunc) {
	out, cancel := context.WithCancel(context.WithoutCancel(ctx))
	var timer *time.Timer
	var mu sync.Mutex
	stop := context.AfterFunc(ctx, func() {
		mu.Lock()
		defer mu.Unlock()
		timer = time.AfterFunc(grace, cancel)
	})
	return out, func() {
		stop()
		mu.Lock()
		if timer != nil {
			timer.Stop()
		}
		mu.Unlock()
		cancel()
	}
}

// runRemotePromptBridge ranges over /perms/stream; each frame is
// handed to the prompter (which blocks until the operator picks a
// decision in the modal), then the decision is POSTed back via
// /perms/respond. If the remote daemon wasn't constructed with
// attachadapter.WithPromptBroker the initial GET returns 501; the
// bridge logs once and returns (the prompter sits idle and the
// daemon's gate then surfaces its usual "no prompter configured"
// error, which is the correct headless-mode behavior).
//
// Deny with a reason (#1165): the bridge asks with
// AskApprovalDetailed, which is what puts core-tui's "r" key on the
// prompt, only when host reports a daemon protocol of 1.15.0 or later
// at the moment the prompt arrives. Otherwise it asks with plain
// AskApproval and the operator is never offered a reason the daemon
// would silently drop. The version comes from the `capabilities` frame
// on the adapter's event stream, which core-tui opens in the same step
// that starts this bridge, so a prompt already pending (exactly the
// prompt an operator attached to answer) usually replays before the
// version is known. Before the first frame the bridge therefore waits
// up to protocolWait for the version, once per bridge: a daemon that
// never advertises one costs one short delay, not one per prompt, and
// a prompt that outlasts the wait simply doesn't offer "r". On the
// session's own stream, a drop clears the version until the reconnect
// re-announces it, so a daemon restarted at an older protocol is not
// offered a reason it would ignore.
func runRemotePromptBridge(ctx context.Context, client *attachclient.Client, sessionPath string, prompter remotePrompter, errOut io.Writer, host PromptBridgeHost) {
	const (
		initialBackoff = 5 * time.Second
		maxBackoff     = 30 * time.Second
	)
	backoff := initialBackoff
	waited := false
	for ctx.Err() == nil {
		debugf("prompt bridge: connecting to %s/perms/stream", sessionPath)
		frames, err := client.PromptStream(ctx, sessionPath)
		if err != nil {
			// 501 = daemon has no broker → don't reconnect-loop.
			if strings.Contains(err.Error(), "501") || strings.Contains(err.Error(), "not registered") {
				debugf("prompt bridge: capability not registered on daemon (%v); exiting", err)
				logBridge(errOut, "remote prompt bridge: capability not registered on daemon (%v)", err)
				return
			}
			debugf("prompt bridge: connect failed: %v; sleeping %s", err, backoff)
			logBridge(errOut, "remote prompt stream: %v; retrying in %v", err, backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff < maxBackoff {
				backoff *= 2
			}
			continue
		}
		debugf("prompt bridge: connected; reading frames")
		backoff = initialBackoff
		for frame := range frames {
			if ctx.Err() != nil {
				return
			}
			debugf("prompt bridge: frame id=%s kind=%s tool=%s", frame.ID, frame.Kind, frame.ToolName)
			if !waited {
				awaitDaemonProtocol(ctx, host, protocolWait)
				waited = true
				if ctx.Err() != nil {
					// Detached during the wait: the prompt belongs to
					// a session the operator has left, and asking now
					// would put it over the one they switched to.
					return
				}
			}
			// The prompt may outlive the bridge by detachGrace, so the
			// deny a switch answers it with still reaches the daemon.
			askCtx, cancelAsk := graceAfter(ctx, detachGrace)
			handleRemotePromptFrame(askCtx, client, sessionPath, prompter, frame, errOut, host)
			cancelAsk()
		}
		debugf("prompt bridge: stream closed; will reconnect after %s", backoff)
	}
}

// remotePrompter is the slice of *coretui.Prompter the bridge asks
// through: both entry points, because which one it calls is what
// decides whether the prompt offers "r". An interface so a test can
// answer for the operator.
type remotePrompter interface {
	AskApproval(ctx context.Context, req coretui.PermissionRequest) (coretui.PermissionDecision, error)
	AskApprovalDetailed(ctx context.Context, req coretui.PermissionRequest) (coretui.PermissionOutcome, error)
}

var (
	_ remotePrompter   = (*coretui.Prompter)(nil)
	_ PromptBridgeHost = (*Adapter)(nil)
)

func handleRemotePromptFrame(ctx context.Context, client *attachclient.Client, sessionPath string, prompter remotePrompter, frame attach.PromptFrame, errOut io.Writer, host PromptBridgeHost) {
	req := coretui.PermissionRequest{
		Kind:        permissionKindFromWire(frame.Kind),
		ToolName:    frame.ToolName,
		Detail:      frame.Detail,
		DetailKind:  detailKindFor(frame),
		Verb:        frame.Verb,
		Source:      frame.Source,
		PersistTool: frame.PersistTool,
		PersistKey:  frame.PersistKey,
	}
	var out coretui.PermissionOutcome
	var err error
	if daemonTakesDenyReason(host) {
		out, err = prompter.AskApprovalDetailed(ctx, req)
	} else {
		out.Decision, err = prompter.AskApproval(ctx, req)
	}
	if err != nil {
		// ctx cancelled or prompter torn down mid-decision; nothing to send.
		return
	}
	if out.Decision == coretui.DecisionDeny && out.Reason != "" {
		sendDenyWithReason(ctx, client, sessionPath, frame.ID, out.Reason, errOut, host)
		return
	}
	wire := decisionToWire(out.Decision)
	if rerr := client.RespondToPrompt(ctx, sessionPath, frame.ID, wire); rerr != nil {
		logBridge(errOut, "remote prompt respond (id=%s decision=%s): %v", frame.ID, wire, rerr)
	}
}

// sendDenyWithReason POSTs a deny carrying the operator's reason. The
// 400 branch is defensive: core-tui caps a reason at the same 500 bytes
// the daemon does and only ever sets one on a deny, so a 1.15.0+ daemon
// has no 400 to give this body today. Should one appear anyway, a
// 400 leaves the prompt pending on the daemon (it refuses the reason,
// not the deny), so the bridge re-sends the deny without it: losing the
// operator's words is recoverable, losing their refusal is not — the
// call would sit waiting until the approval timeout, or forever. The
// operator is told the reason did not make it, so they can steer
// instead. Other failures (404/410, transport) would fail the same way
// on a retry and are only logged, as for any other decision.
func sendDenyWithReason(ctx context.Context, client *attachclient.Client, sessionPath, id, reason string, errOut io.Writer, host PromptBridgeHost) {
	err := client.DenyPrompt(ctx, sessionPath, id, reason)
	if err == nil {
		return
	}
	if attachclient.HTTPStatus(err) != http.StatusBadRequest {
		logBridge(errOut, "remote prompt respond (id=%s decision=deny with reason): %v", id, err)
		return
	}
	if rerr := client.RespondToPrompt(ctx, sessionPath, id, "deny"); rerr != nil {
		logBridge(errOut, "remote prompt respond (id=%s decision=deny, reason dropped after %v): %v", id, err, rerr)
		return
	}
	if host != nil {
		host.NotifyOperator(fmt.Errorf("the daemon refused your deny reason for prompt %s on %s, so the call was denied without it (%w); send a steer if the model needs to know why", id, sessionPath, err))
	}
}

// daemonTakesDenyReason reports whether host says the daemon speaks a
// protocol whose /perms/respond accepts a deny's reason. Unknown —
// no host, no capabilities frame yet, an unparseable version — is no.
// So is a different major: the protocol only promises additive minors,
// and a major this client predates may have reshaped /perms/respond.
// Saying no there costs the operator the "r" key, never a deny.
func daemonTakesDenyReason(host PromptBridgeHost) bool {
	if host == nil {
		return false
	}
	v := strings.TrimSpace(host.DaemonProtocolVersion())
	if v == "" {
		return false
	}
	if !strings.HasPrefix(v, "v") {
		v = "v" + v
	}
	return semver.IsValid(v) &&
		semver.Major(v) == semver.Major(denyReasonProtocol) &&
		semver.Compare(v, denyReasonProtocol) >= 0
}

// DaemonProtocolVersion implements PromptBridgeHost: the
// protocol_version of the last `capabilities` frame this adapter's
// event stream received, or "" before the first one.
func (a *Adapter) DaemonProtocolVersion() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.daemonProtocol
}

// DaemonProtocolKnown implements PromptBridgeHost: closed once the
// first `capabilities` frame has been read.
func (a *Adapter) DaemonProtocolKnown() <-chan struct{} {
	return a.protocolKnown
}

// NotifyOperator implements PromptBridgeHost by queueing err on the
// channel inject failures use, which the Events loop renders as an
// error row in the chat. It queues on the adapter for the session the
// operator is viewing, because after a /switch this adapter's Events
// loop has stopped and nobody would drain its queue. Non-blocking: a
// full queue drops the note rather than stall the prompt bridge.
func (a *Adapter) NotifyOperator(err error) {
	target := a
	if a.view != nil {
		if cur := a.view.get(); cur != nil {
			target = cur
		}
	}
	select {
	case target.injectErrs <- err:
	default:
	}
}

func permissionKindFromWire(s string) coretui.PermissionKind {
	switch s {
	case "bash":
		return coretui.PermissionKindBash
	case "file_write":
		return coretui.PermissionKindEdit
	default:
		// path_scope + generic both render as "other" — coretui's
		// modal handles the detail rendering, the kind only steers
		// the title/icon.
		return coretui.PermissionKindOther
	}
}

// detailKindFor picks the modal's payload renderer based on the
// frame's tool. Bash gets the shell renderer; file_write gets the
// diff renderer if the detail looks like one; everything else falls
// to the generic args renderer. The remote frame doesn't carry the
// payload type explicitly today, so we infer from heuristics.
func detailKindFor(frame attach.PromptFrame) coretui.DetailKind {
	switch frame.Kind {
	case "bash":
		return coretui.DetailShell
	case "file_write":
		if strings.Contains(frame.Detail, "\n@@") || strings.HasPrefix(frame.Detail, "---") {
			return coretui.DetailDiff
		}
		return coretui.DetailArgs
	}
	return coretui.DetailArgs
}

func decisionToWire(d coretui.PermissionDecision) string {
	switch d {
	case coretui.DecisionAllowOnce:
		return "allow-once"
	case coretui.DecisionAllowSession:
		return "allow-session"
	case coretui.DecisionAllowSessionVerb:
		return "allow-session-verb"
	case coretui.DecisionAllowSessionTool:
		return "allow-session-tool"
	case coretui.DecisionAllowAlways:
		return "allow-always"
	}
	return "deny"
}

func logBridge(w io.Writer, format string, args ...any) {
	if w == nil {
		w = os.Stderr
	}
	fmt.Fprintf(w, "core-agent-tui: "+format+"\n", args...)
}
