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

// Package peer implements the `call_peer` built-in tool: named
// delegation from one core-agent daemon to another over attach-mode
// HTTP. It is Gap 2 of docs/kube-agents-platform-fit.md — the piece a
// fleet parent needs to route "what's going on in cluster prod-1?" to
// the operator agent that actually lives in prod-1.
//
// Three properties the package holds by construction, because the
// alternative is a tool whose docs claim more than the code enforces:
//
//   - No arbitrary-URL parameter. The model names a PEER; the endpoint
//     comes from the hub's own registry, which only accepts absolute
//     http(s) URLs (attach.validatePeerEndpoint) from an authenticated
//     registrant. An unknown name is refused, not dialed. Same
//     anti-SSRF shape as the alert tool.
//   - No credentials in the schema. The bearer token presented to the
//     peer is read from the operator-named env var at call time, so it
//     never appears in the tool's arguments, the audit log, or the
//     model's context.
//   - Bounded. Every call gets a wall-clock deadline and a response
//     byte cap; a peer that never finishes its turn fails the call
//     instead of pinning the parent's turn open.
//
// Each call runs in a FRESH session on the peer (POST /sessions), so
// two concurrent callers can't interleave prompts into one transcript
// and the turn boundary we wait on is unambiguously ours. That makes
// the peer's attach.multi_session a hard requirement — see New.
package peer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"google.golang.org/adk/session"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"

	"github.com/go-steer/core-agent/v2/internal/attachclient"
	"github.com/go-steer/core-agent/v2/pkg/attach"
	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/permissions"
)

const (
	// DefaultToolName is the tool's name unless the operator renames it
	// via tools.call_peer.name.
	DefaultToolName = "call_peer"

	// DefaultTimeout bounds one delegated call end to end: session
	// creation, inject, and the wait for the peer's turn to complete.
	// Generous because the callee is a full agent turn (tool calls
	// included), not an HTTP echo.
	DefaultTimeout = 120 * time.Second

	// MaxTimeout is the ceiling config may raise the deadline to. A
	// call_peer that can outlive a Kubernetes liveness probe is a
	// wedged parent, not a patient one.
	MaxTimeout = 15 * time.Minute

	// DefaultMaxResponseBytes caps how much of the peer's answer enters
	// the parent's context. Over the cap the answer is cut and the
	// result is flagged Truncated — the operator can read the rest in
	// the peer's own session, whose ID the result carries.
	DefaultMaxResponseBytes = 16 * 1024
)

// Peer is one callable destination: the subset of attach.Peer this
// package needs. Deliberately not attach.Peer — the tool has no
// business seeing registration IDs or hub-side ownership.
type Peer struct {
	Name     string
	Endpoint string
	Labels   map[string]string
}

// Directory is the live roster of callable peers. Implemented over the
// hub's *attach.PeerRegistry by the wiring layer (pkg/compose); the
// tool takes the interface so it never reaches into registry internals
// and so tests can hand it a fixed roster.
//
// Peers is consulted per call, not cached: registrations come and go
// on a lease, and a call to a peer that let its lease lapse should
// fail as unknown rather than dial a dead pod.
type Directory interface {
	Peers() []Peer
}

// DirectoryFunc adapts a plain function to Directory.
type DirectoryFunc func() []Peer

// Peers implements Directory.
func (f DirectoryFunc) Peers() []Peer {
	if f == nil {
		return nil
	}
	return f()
}

// Args is the tool's input. There is no endpoint, header, or timeout
// parameter: everything that decides WHERE the call goes and WHAT
// credentials it carries is operator configuration.
type Args struct {
	Peer   string `json:"peer" jsonschema:"required; the name of a peer agent registered with this hub (call with an empty name to have the error list the currently registered peers)"`
	Prompt string `json:"prompt" jsonschema:"required; the request to send the peer, written as a self-contained instruction — the peer answers in a fresh session and can see none of this conversation"`
}

// Result is the tool's output.
type Result struct {
	Peer string `json:"peer"`
	// SessionID is the session the prompt ran in ON THE PEER. Surfaced
	// so an operator can attach to it and read the full transcript when
	// the summary here isn't enough (and it's the only handle they get
	// when Truncated is true).
	SessionID  string `json:"session_id"`
	Response   string `json:"response"`
	DurationMs int64  `json:"duration_ms"`
	// Truncated reports that the peer's answer exceeded the response
	// cap and Response holds only its leading bytes.
	Truncated bool `json:"truncated,omitempty"`
}

// New builds the call_peer tool. The caller (cmd/core-agent) invokes
// this only when tools.call_peer.enabled is set AND the daemon is
// running as a peer hub, so a registered call_peer always has a
// registry behind it.
//
// dir supplies the live roster and must be non-nil. Callers that build
// the directory over a registry constructed later in boot should pass a
// DirectoryFunc closure rather than a nil Directory.
func New(gate *permissions.Gate, cfg *config.Config, dir Directory) (tool.Tool, error) {
	return newTool(gate, cfg, dir, nil)
}

// newTool is New with the env lookup injectable for tests
// (getenv nil → the process environment).
func newTool(gate *permissions.Gate, cfg *config.Config, dir Directory, getenv func(string) string) (tool.Tool, error) {
	h, err := newHandler(gate, cfg, dir, getenv)
	if err != nil {
		return nil, err
	}
	return functiontool.New(
		functiontool.Config{Name: h.toolName, Description: h.description},
		h.run,
	)
}

// handler is the per-tool state closed over by the ADK function.
type handler struct {
	gate        *permissions.Gate
	dir         Directory
	toolName    string
	description string
	tokenEnv    string
	timeout     time.Duration
	maxBytes    int
	getenv      func(string) string
}

func newHandler(gate *permissions.Gate, cfg *config.Config, dir Directory, getenv func(string) string) (*handler, error) {
	if gate == nil {
		return nil, errors.New("call_peer: gate is required")
	}
	if cfg == nil {
		return nil, errors.New("call_peer: cfg is required")
	}
	if dir == nil {
		return nil, errors.New("call_peer: peer directory is required")
	}
	if getenv == nil {
		getenv = os.Getenv
	}
	cp := cfg.Tools.CallPeer
	h := &handler{
		gate:     gate,
		dir:      dir,
		toolName: DefaultToolName,
		tokenEnv: cp.TokenEnv,
		timeout:  DefaultTimeout,
		maxBytes: DefaultMaxResponseBytes,
		getenv:   getenv,
	}
	if cp.Name != "" {
		h.toolName = cp.Name
	}
	if cp.TimeoutSeconds > 0 {
		h.timeout = time.Duration(cp.TimeoutSeconds) * time.Second
	}
	if cp.MaxResponseBytes > 0 {
		h.maxBytes = cp.MaxResponseBytes
	}
	h.description = cp.Description
	if h.description == "" {
		h.description = defaultDescription(h.toolName)
	}
	// call_peer runs through CheckGeneric under its configured name, so
	// plan-first gating denies it before a plan exists. Say so, or
	// record_plan can't name it among what the plan unblocked (#747) —
	// and the name is operator-configurable, so nothing else can.
	gate.RegisterPlanGatedTools(h.toolName)
	return h, nil
}

// defaultDescription is the model-facing text. It cannot enumerate the
// peers the way the alert tool enumerates targets — the roster is
// dynamic, and the description is fixed at registration time, before
// any peer has registered. So it tells the model how to discover them
// instead.
func defaultDescription(name string) string {
	return "Delegate a request to a peer agent registered with this hub and return its answer.\n\n" +
		"Use this when the work belongs to another agent in the fleet — one that is attached to a " +
		"cluster, namespace, or system this agent cannot see. The peer runs the prompt in a fresh " +
		"session of its own: it has none of this conversation's context, so write the prompt as a " +
		"complete, self-contained request.\n\n" +
		"The set of peers is dynamic (agents register and their leases expire), so it is not listed " +
		"here. Call " + name + " with an empty peer name and the error will name every peer " +
		"currently registered. The call returns the peer's final answer plus the session ID it ran " +
		"in, so an operator can read the full transcript on the peer itself."
}

func (h *handler) run(ctx tool.Context, in Args) (Result, error) {
	target, err := h.resolve(in.Peer)
	if err != nil {
		return Result{}, err
	}
	if strings.TrimSpace(in.Prompt) == "" {
		return Result{}, fmt.Errorf("%s: prompt is required", h.toolName)
	}

	// Gate on the peer name — operators scope per destination with
	// permissions.allow: ["call_peer:operator-prod-1"] (or
	// "call_peer:*"). Gated before any network work, and before the
	// token is read out of the environment.
	if err := h.gate.CheckGeneric(ctx, h.toolName, target.Name); err != nil {
		return Result{}, err
	}

	token, err := h.token()
	if err != nil {
		return Result{}, err
	}

	// Parent on the inbound tool ctx (not Background) so /interrupt and
	// daemon shutdown abort an in-flight delegation. tool.Context is an
	// interface; some tests pass nil.
	parent := context.Context(ctx)
	if parent == nil {
		parent = context.Background()
	}
	cctx, cancel := context.WithTimeout(parent, h.timeout)
	defer cancel()

	return h.call(cctx, target, token, in.Prompt)
}

// resolve maps a model-supplied name onto a registered peer. Every
// failure path names the peers that ARE registered: an agent that
// guessed the name wrong can correct itself in one turn, and an
// operator reading the transcript sees the roster at the moment of
// the call.
func (h *handler) resolve(name string) (Peer, error) {
	roster := h.dir.Peers()
	if name != "" {
		for _, p := range roster {
			if p.Name == name {
				if p.Endpoint == "" {
					return Peer{}, fmt.Errorf("%s: peer %q has no endpoint registered", h.toolName, name)
				}
				return p, nil
			}
		}
	}
	if len(roster) == 0 {
		return Peer{}, fmt.Errorf("%s: no peers are registered with this hub right now", h.toolName)
	}
	names := make([]string, 0, len(roster))
	for _, p := range roster {
		names = append(names, p.Name)
	}
	sort.Strings(names)
	if name == "" {
		return Peer{}, fmt.Errorf("%s: peer is required (registered: %s)", h.toolName, strings.Join(names, ", "))
	}
	return Peer{}, fmt.Errorf("%s: unknown peer %q (registered: %s)", h.toolName, name, strings.Join(names, ", "))
}

// token resolves the outbound bearer token. An operator who named an
// env var and left it empty gets an error rather than an anonymous
// request that the peer answers with 401 — the same
// configured-but-unset treatment the alert tool gives url_env.
func (h *handler) token() (string, error) {
	if h.tokenEnv == "" {
		return "", nil
	}
	v := strings.TrimSpace(h.getenv(h.tokenEnv))
	if v == "" {
		return "", fmt.Errorf("%s: tools.call_peer.token_env %q is unset or empty", h.toolName, h.tokenEnv)
	}
	return v, nil
}

// call performs the delegation: open a session on the peer, subscribe
// to its event stream, inject the prompt, and read until the turn ends.
//
// Subscribe BEFORE inject. The peer starts the turn the moment the
// message lands, and a stream opened afterwards can miss the frames it
// was opened to read.
func (h *handler) call(ctx context.Context, target Peer, token, prompt string) (Result, error) {
	parsed, err := attachclient.ParseURL(target.Endpoint)
	if err != nil {
		return Result{}, fmt.Errorf("%s: peer %q endpoint %q: %w", h.toolName, target.Name, target.Endpoint, err)
	}
	switch parsed.Scheme {
	case "http", "https":
	default:
		// Belt and braces: the registry already refuses anything else
		// at registration and on state-file load. If that ever
		// regresses, the tool must not be the thing that dials it.
		return Result{}, fmt.Errorf("%s: peer %q endpoint scheme %q is not callable (want http or https)", h.toolName, target.Name, parsed.Scheme)
	}
	client := attachclient.New(parsed, token, h.timeout)

	start := time.Now()
	sess, err := client.NewSession(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("%s: peer %q: open session: %w%s", h.toolName, target.Name, err, sessionHint(err))
	}
	path := sessionPath(sess)

	frames, err := client.Stream(ctx, path, 0)
	if err != nil {
		return Result{}, fmt.Errorf("%s: peer %q: stream %s: %w", h.toolName, target.Name, path, err)
	}
	if err := client.Inject(ctx, path, prompt); err != nil {
		return Result{}, fmt.Errorf("%s: peer %q: inject: %w", h.toolName, target.Name, err)
	}

	text, truncated, err := h.collect(ctx, frames)
	if err != nil {
		return Result{}, fmt.Errorf("%s: peer %q (session %s): %w", h.toolName, target.Name, sess.SessionID, err)
	}
	return Result{
		Peer:       target.Name,
		SessionID:  sess.SessionID,
		Response:   text,
		DurationMs: time.Since(start).Milliseconds(),
		Truncated:  truncated,
	}, nil
}

// collect drains the peer's event stream until its turn ends,
// accumulating model-authored text: every non-partial event the peer
// authored, blank-line separated. That's narration as well as the
// final answer — a turn with tool calls says useful things before it
// concludes — but never the echoed prompt, never streaming chunks
// (which would double every sentence), and never tool arguments or
// results.
//
// Turn end is detected the same two ways the remote TUI detects it
// (internal/coretuiremote.isTurnEnd): the typed turn-complete SSE
// event, or — because core-agent's eventlog projection does not
// currently persist the ADK TurnComplete flag — a final, non-partial,
// model-authored event that carries text and no tool round-trip.
func (h *handler) collect(ctx context.Context, frames <-chan attach.Frame) (string, bool, error) {
	var sb strings.Builder
	truncated := false
	for {
		select {
		case <-ctx.Done():
			// Deadline or interrupt. Whatever text arrived is not
			// returned: a partial answer that looks complete is worse
			// for the caller than a clean failure.
			return "", false, fmt.Errorf("no turn completed within %s: %w", h.timeout, ctx.Err())
		case frame, ok := <-frames:
			if !ok {
				// Stream closed without a turn end — peer restarted,
				// dropped us as a slow subscriber, or the session went
				// away underneath us.
				return "", false, errors.New("event stream ended before the turn completed")
			}
			switch frame.Type {
			case attach.EventTurnError:
				return "", false, turnError(frame)
			case attach.EventTurnComplete:
				return strings.TrimSpace(sb.String()), truncated, nil
			case "", attach.EventAgent:
			default:
				// status-update / usage-update / inbox / capabilities:
				// not this tool's business.
				continue
			}
			ev := frame.Event
			if ev == nil || ev.Partial || ev.Author == "" || ev.Author == "user" {
				continue
			}
			text, hasCall := eventText(ev)
			if text != "" && !truncated {
				// Blank line between events: a turn with tool calls
				// interleaves narration with the answer, and running
				// them together reads as one garbled paragraph.
				if sb.Len() > 0 {
					text = "\n\n" + text
				}
				if remaining := h.maxBytes - sb.Len(); remaining <= len(text) {
					sb.WriteString(text[:max(remaining, 0)])
					truncated = true
				} else {
					sb.WriteString(text)
				}
			}
			if truncated {
				// The cap is the point: stop reading rather than let a
				// runaway peer stream unbounded output at the parent.
				// The peer's turn continues; its session ID is in the
				// result for anyone who wants the rest.
				return strings.TrimSpace(sb.String()), true, nil
			}
			if ev.TurnComplete || (text != "" && !hasCall) {
				return strings.TrimSpace(sb.String()), false, nil
			}
		}
	}
}

// eventText pulls the text out of one event's parts and reports
// whether the event also carried a tool call or tool result — the
// signal that the peer is mid-turn rather than answering.
func eventText(ev *session.Event) (string, bool) {
	if ev.Content == nil {
		return "", false
	}
	var sb strings.Builder
	hasCall := false
	for _, p := range ev.Content.Parts {
		if p == nil {
			continue
		}
		if p.Text != "" {
			sb.WriteString(p.Text)
		}
		if p.FunctionCall != nil || p.FunctionResponse != nil {
			hasCall = true
		}
	}
	return sb.String(), hasCall
}

// turnError renders a turn-error frame as a Go error, keeping the
// peer's own kind + message rather than flattening it to "failed".
func turnError(frame attach.Frame) error {
	te, ok := frame.TypedData.(*attach.TurnError)
	if !ok || te == nil {
		return errors.New("turn failed on the peer (no detail supplied)")
	}
	kind := te.Kind
	if kind == "" {
		kind = attach.TurnErrorUnknown
	}
	if te.Message == "" {
		return fmt.Errorf("turn failed on the peer (%s)", kind)
	}
	return fmt.Errorf("turn failed on the peer (%s): %s", kind, te.Message)
}

// sessionPath builds the attach path prefix for a freshly created
// session, matching the convention in cmd/core-agent-tui: the app name
// is part of the path when the peer reports one.
func sessionPath(sess attachclient.NewSessionResponse) string {
	if sess.AppName == "" {
		return "/sessions/" + sess.SessionID
	}
	return "/sessions/" + sess.AppName + "/" + sess.SessionID
}

// sessionHint appends the fix for the one failure operators will
// actually hit: POST /sessions is only wired when the peer runs with
// multi-session enabled, and its 501 says nothing about how to turn
// that on.
func sessionHint(err error) string {
	if err == nil || !strings.Contains(err.Error(), "501") {
		return ""
	}
	return " (the peer does not allow session creation; start it with attach.multi_session.enabled so callers can open their own session)"
}
