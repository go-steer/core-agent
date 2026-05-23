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

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"

	"google.golang.org/adk/session"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/attach"
	"github.com/go-steer/core-agent/internal/attachclient"
)

// chatModel is the active-session view. Top: scrollback (viewport
// over a styled chat log). Bottom: textarea input. Around them: a
// status bar (top) + footer (bottom).
type chatModel struct {
	client *attachclient.Client
	entry  pickerEntry // identifies which session this is

	theme string
	alias string

	width, height int

	viewport viewport.Model
	input    textarea.Model
	renderer *glamour.TermRenderer

	// renderedEvents holds the pretty-printed event log. We retain
	// the raw frames in `events` so they can be re-rendered on theme
	// switch / resize without losing content.
	events        []chatEvent
	connectionMsg string
	statusInfo    attach.StatusInfo
	usage         usagePanel
	lastSeq       int64
	reconnecting  bool

	wantsPicker bool // set true by Esc; root reads + clears
}

// chatEvent is one rendered entry in the scrollback. Each frame from
// the SSE stream becomes (at most) one chatEvent — model partials
// merge into the existing in-flight asst entry.
type chatEvent struct {
	kind     string // user | asst | tool | system | error
	body     string // raw markdown body for kind=asst, plain text otherwise
	rendered string // glamour-rendered body for kind=asst
	meta     string // small inline metadata (e.g. tool name + status + bytes)
	partial  bool   // true while still streaming
}

func newChatModel(client *attachclient.Client, entry pickerEntry, theme, alias string) chatModel {
	ta := textarea.New()
	ta.Placeholder = "type a message + Enter to inject  ·  / for commands  ·  Esc to leave"
	ta.SetWidth(80)
	ta.SetHeight(3)
	ta.MaxHeight = 10
	ta.ShowLineNumbers = false
	ta.Focus()

	vp := viewport.New(80, 20)
	vp.SetContent("connecting…")

	renderer, _ := glamour.NewTermRenderer(
		themeFor(theme),
		glamour.WithWordWrap(80),
	)

	return chatModel{
		client:   client,
		entry:    entry,
		theme:    theme,
		alias:    alias,
		viewport: vp,
		input:    ta,
		renderer: renderer,
		usage:    usagePanel{},
	}
}

// themeFor maps the --theme flag to a glamour option.
func themeFor(theme string) glamour.TermRendererOption {
	switch strings.ToLower(theme) {
	case "dark":
		return glamour.WithStandardStyle("dark")
	case "light":
		return glamour.WithStandardStyle("light")
	case "notty":
		return glamour.WithStandardStyle("notty")
	default:
		return glamour.WithAutoStyle()
	}
}

func (m *chatModel) SetSize(w, h int) {
	m.width = w
	m.height = h
	if m.client == nil {
		return
	}
	// Vertical layout:
	//   status bar       1
	//   viewport         h - status - input - footer
	//   input            input.height (≥3, ≤10) + 1 border
	//   footer           1
	inputH := m.input.Height() + 1
	vpH := h - 1 - inputH - 1
	if vpH < 3 {
		vpH = 3
	}
	m.viewport.Width = w
	m.viewport.Height = vpH
	m.input.SetWidth(w)
	if rr, err := glamour.NewTermRenderer(
		themeFor(m.theme),
		glamour.WithWordWrap(w-4),
	); err == nil {
		m.renderer = rr
		// re-render every model message so the wrap matches the new width
		for i := range m.events {
			if m.events[i].kind == "asst" && !m.events[i].partial {
				m.events[i].rendered = m.renderMarkdown(m.events[i].body)
			}
		}
		m.refreshViewport()
	}
}

func (m chatModel) Init() tea.Cmd {
	return tea.Batch(
		m.subscribeCmd(0),
		m.fetchStatusCmd(),
	)
}

// --- Commands (tea.Cmd factories) ---

// subscribeCmd opens the SSE stream and emits chatFrameMsg per
// arrival, plus a final chatStreamEndedMsg.
func (m chatModel) subscribeCmd(since int64) tea.Cmd {
	client := m.client
	sessionPath := m.entry.sessionPath()
	return func() tea.Msg {
		// Stream runs as a goroutine inside the client; it closes
		// when the upstream HTTP body closes (or the process exits
		// via tea.Quit). Pass context.Background() so we don't leak
		// a cancel that nothing in v1 invokes. Reconnect / per-session
		// cancellation are tracked as v1.1 polish.
		ch, err := client.Stream(context.Background(), sessionPath, since)
		if err != nil {
			return chatStreamEndedMsg{err: err}
		}
		// Drain frames in a goroutine, emit them as a stream of
		// messages. We exploit the fact that tea.Cmd runs in its own
		// goroutine; sending messages back happens via the program's
		// Send channel — but in v1 we use a polling approach: each
		// Cmd returns the *next* msg. Use a simple worker that holds
		// the channel and emits one frame per Cmd invocation via a
		// closure trick: re-issue the Cmd from Update for each frame.
		// For simplicity here we relay the first frame and rely on
		// the Update loop to re-subscribe on each chatFrameMsg.
		frame, ok := <-ch
		if !ok {
			return chatStreamEndedMsg{err: nil}
		}
		// Stash the channel on a global registry keyed by session
		// path so the next pump call resumes the same stream. This
		// is the simplest pattern that doesn't require a custom
		// tea.Program with our own Send loop.
		streamRegistry.set(sessionPath, ch)
		return chatFrameMsg{frame: frame}
	}
}

// nextFrameCmd consumes one more frame from the in-flight stream.
// Returned in response to a chatFrameMsg so the chain continues
// until the stream closes.
func (m chatModel) nextFrameCmd() tea.Cmd {
	sessionPath := m.entry.sessionPath()
	return func() tea.Msg {
		ch := streamRegistry.get(sessionPath)
		if ch == nil {
			return chatStreamEndedMsg{err: nil}
		}
		frame, ok := <-ch
		if !ok {
			streamRegistry.clear(sessionPath)
			return chatStreamEndedMsg{err: nil}
		}
		return chatFrameMsg{frame: frame}
	}
}

func (m chatModel) fetchStatusCmd() tea.Cmd {
	client := m.client
	sessionPath := m.entry.sessionPath()
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		st, err := client.Status(ctx, sessionPath)
		return chatStatusLoadedMsg{status: st, err: err}
	}
}

func (m chatModel) injectCmd(message string) tea.Cmd {
	client := m.client
	sessionPath := m.entry.sessionPath()
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err := client.Inject(ctx, sessionPath, message)
		return chatInjectAckMsg{message: message, err: err}
	}
}

func (m chatModel) wakeCmd() tea.Cmd {
	client := m.client
	sessionPath := m.entry.sessionPath()
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err := client.Wake(ctx, sessionPath)
		return chatWakeAckMsg{err: err}
	}
}

func (m chatModel) fetchToolsCmd() tea.Cmd {
	client := m.client
	sessionPath := m.entry.sessionPath()
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		tools, err := client.Tools(ctx, sessionPath)
		return chatToolsLoadedMsg{tools: tools, err: err}
	}
}

func (m chatModel) fetchAgentsCmd() tea.Cmd {
	client := m.client
	sessionPath := m.entry.sessionPath()
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		agents, err := client.Agents(ctx, sessionPath)
		return chatAgentsLoadedMsg{agents: agents, err: err}
	}
}

func (m chatModel) fetchPeersCmd() tea.Cmd {
	client := m.client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		peers, err := client.ListPeers(ctx)
		if err != nil {
			return chatPeersLoadedMsg{err: err}
		}
		out := make([]peerEntry, 0, len(peers))
		for _, p := range peers {
			out = append(out, peerEntry{
				Name: p.Name, Endpoint: p.Endpoint,
				Labels: p.Labels, RegID: p.RegistrationID,
			})
		}
		return chatPeersLoadedMsg{peers: out}
	}
}

// --- Update ---

// UpdateInner dispatches messages to the input / viewport / chat
// state. Returns the (possibly mutated) chatModel and a tea.Cmd to
// chain.
func (m chatModel) UpdateInner(msg tea.Msg) (chatModel, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		// Esc returns to the picker (only when input isn't capturing).
		if msg.Type == tea.KeyEsc {
			m.wantsPicker = true
			return m, nil
		}
		if msg.Type == tea.KeyEnter && !msg.Alt {
			text := strings.TrimSpace(m.input.Value())
			if text == "" {
				return m, nil
			}
			m.input.Reset()
			return m.handleSubmit(text)
		}
	case chatFrameMsg:
		m.applyFrame(msg.frame)
		return m, m.nextFrameCmd()
	case chatStreamEndedMsg:
		m.connectionMsg = fmt.Sprintf("stream ended: %v — reconnect with /reconnect", msg.err)
		m.appendSystem(m.connectionMsg)
		m.refreshViewport()
		return m, nil
	case chatStatusLoadedMsg:
		if msg.err == nil {
			m.statusInfo = msg.status
			m.usage.modelName = msg.status.ModelName
		}
		return m, nil
	case chatInjectAckMsg:
		if msg.err != nil {
			m.appendErr(fmt.Sprintf("inject failed: %v", msg.err))
		} else {
			m.appendUser(msg.message)
		}
		m.refreshViewport()
		return m, nil
	case chatWakeAckMsg:
		if msg.err != nil {
			m.appendErr(fmt.Sprintf("wake failed: %v", msg.err))
		} else {
			m.appendSystem("wake sent")
		}
		m.refreshViewport()
		return m, nil
	case chatToolsLoadedMsg:
		if msg.err != nil {
			m.appendErr(fmt.Sprintf("/tools: %v", msg.err))
		} else {
			m.appendSystem(renderToolsList(msg.tools))
		}
		m.refreshViewport()
		return m, nil
	case chatAgentsLoadedMsg:
		if msg.err != nil {
			m.appendErr(fmt.Sprintf("/subagents: %v", msg.err))
		} else {
			m.appendSystem(renderAgentsList(msg.agents))
		}
		m.refreshViewport()
		return m, nil
	case chatPeersLoadedMsg:
		if msg.err != nil {
			m.appendErr(fmt.Sprintf("/peers: %v", msg.err))
		} else if len(msg.peers) == 0 {
			m.appendSystem("/peers: no peers registered (or listener isn't a peer-registration hub)")
		} else {
			m.appendSystem(renderPeersList(msg.peers))
		}
		m.refreshViewport()
		return m, nil
	}

	// Forward to input + viewport.
	var icmd, vcmd tea.Cmd
	m.input, icmd = m.input.Update(msg)
	m.viewport, vcmd = m.viewport.Update(msg)
	return m, tea.Batch(icmd, vcmd)
}

// --- Rendering ---

func (m chatModel) View() string {
	if m.client == nil {
		return "" // pre-init
	}
	statusBar := styleStatusBar.Width(m.width).Render(
		fmt.Sprintf("core-agent-tui  ●  %s  ·  %s",
			m.entry.displayLabel(m.alias), m.entry.Endpoint),
	)
	footer := styleFooter.Width(m.width).Render(m.usage.render(m.width, m.reconnecting, m.connectionMsg))
	return lipgloss.JoinVertical(lipgloss.Top,
		statusBar,
		m.viewport.View(),
		m.input.View(),
		footer,
	)
}

// applyFrame integrates one SSE frame into the chat state. Two
// concerns: (a) merge streaming model partials into the in-flight
// asst entry; (b) update usage counters from the event metadata.
func (m *chatModel) applyFrame(f attach.Frame) {
	if f.Seq > m.lastSeq {
		m.lastSeq = f.Seq
	}
	if f.Event == nil {
		return
	}
	ev := f.Event
	// Usage panel update — every model event carries usage in CustomMetadata.
	m.usage.ingest(ev.CustomMetadata)

	if ev.Content == nil {
		return
	}
	switch ev.Author {
	case "user":
		text := assembleText(ev)
		if text != "" {
			m.appendUser(text)
		}
	default:
		// Treat anything else as asst output (model name varies by provider).
		text, hasFnCall, hasFnResp := assembleAll(ev)
		if hasFnCall {
			m.appendTool(assembleFnCall(ev))
		}
		if hasFnResp {
			m.appendTool(assembleFnResp(ev))
		}
		if text != "" {
			if ev.Partial {
				m.appendOrExtendPartial(text)
			} else {
				m.appendOrFinalizePartial(text)
			}
		}
	}
}

func (m *chatModel) appendUser(text string) {
	m.events = append(m.events, chatEvent{kind: "user", body: text})
}

func (m *chatModel) appendTool(label string) {
	m.events = append(m.events, chatEvent{kind: "tool", meta: label})
}

func (m *chatModel) appendSystem(text string) {
	m.events = append(m.events, chatEvent{kind: "system", body: text})
}

func (m *chatModel) appendErr(text string) {
	m.events = append(m.events, chatEvent{kind: "error", body: text})
}

func (m *chatModel) appendOrExtendPartial(text string) {
	// If the last entry is an in-flight asst partial, extend it.
	if n := len(m.events); n > 0 && m.events[n-1].kind == "asst" && m.events[n-1].partial {
		m.events[n-1].body += text
		return
	}
	m.events = append(m.events, chatEvent{kind: "asst", body: text, partial: true})
}

func (m *chatModel) appendOrFinalizePartial(text string) {
	// On final delta: extend body, mark non-partial, render glamour.
	if n := len(m.events); n > 0 && m.events[n-1].kind == "asst" && m.events[n-1].partial {
		m.events[n-1].body += text
		m.events[n-1].partial = false
		m.events[n-1].rendered = m.renderMarkdown(m.events[n-1].body)
		return
	}
	m.events = append(m.events, chatEvent{
		kind:     "asst",
		body:     text,
		rendered: m.renderMarkdown(text),
	})
}

func (m chatModel) renderMarkdown(body string) string {
	if m.renderer == nil {
		return body
	}
	out, err := m.renderer.Render(body)
	if err != nil {
		return body
	}
	return strings.TrimRight(out, "\n")
}

// refreshViewport regenerates the viewport content from the events
// slice. Called whenever events change or theme/width changes.
func (m *chatModel) refreshViewport() {
	var sb strings.Builder
	for _, e := range m.events {
		switch e.kind {
		case "user":
			sb.WriteString("\n")
			sb.WriteString(styleBubbleUser.Render("user │ "))
			sb.WriteString(e.body)
			sb.WriteString("\n")
		case "asst":
			sb.WriteString("\n")
			sb.WriteString(styleBubbleAsst.Render("asst │ "))
			body := e.rendered
			if e.partial || body == "" {
				body = e.body
			}
			sb.WriteString(indent(body, "       "))
			sb.WriteString("\n")
		case "tool":
			sb.WriteString(styleBubbleTool.Render("  ⚙ "))
			sb.WriteString(styleBubbleTool.Render(e.meta))
			sb.WriteString("\n")
		case "system":
			sb.WriteString("\n")
			sb.WriteString(styleHint.Render(e.body))
			sb.WriteString("\n")
		case "error":
			sb.WriteString("\n")
			sb.WriteString(styleBubbleErr.Render("✗ " + e.body))
			sb.WriteString("\n")
		}
	}
	m.viewport.SetContent(sb.String())
	m.viewport.GotoBottom()
}

// indent prefixes every line of s with prefix (except the first).
func indent(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i := 1; i < len(lines); i++ {
		lines[i] = prefix + lines[i]
	}
	return strings.Join(lines, "\n")
}

// --- Helpers for event introspection ---

func assembleText(ev *session.Event) string {
	if ev == nil || ev.Content == nil {
		return ""
	}
	var b strings.Builder
	for _, p := range ev.Content.Parts {
		if p != nil && p.Text != "" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

func assembleAll(ev *session.Event) (text string, hasFnCall, hasFnResp bool) {
	if ev == nil || ev.Content == nil {
		return "", false, false
	}
	var b strings.Builder
	for _, p := range ev.Content.Parts {
		if p == nil {
			continue
		}
		if p.FunctionCall != nil {
			hasFnCall = true
		}
		if p.FunctionResponse != nil {
			hasFnResp = true
		}
		if p.Text != "" {
			b.WriteString(p.Text)
		}
	}
	return b.String(), hasFnCall, hasFnResp
}

func assembleFnCall(ev *session.Event) string {
	if ev == nil || ev.Content == nil {
		return ""
	}
	var parts []string
	for _, p := range ev.Content.Parts {
		if p == nil || p.FunctionCall == nil {
			continue
		}
		args := "..."
		if len(p.FunctionCall.Args) > 0 {
			buf, _ := json.Marshal(p.FunctionCall.Args)
			args = string(buf)
			if len(args) > 80 {
				args = args[:80] + "…"
			}
		}
		parts = append(parts, fmt.Sprintf("%s(%s)", p.FunctionCall.Name, args))
	}
	return strings.Join(parts, " · ")
}

func assembleFnResp(ev *session.Event) string {
	if ev == nil || ev.Content == nil {
		return ""
	}
	var parts []string
	for _, p := range ev.Content.Parts {
		if p == nil || p.FunctionResponse == nil {
			continue
		}
		name := p.FunctionResponse.Name
		summary := "ok"
		if p.FunctionResponse.Response != nil {
			buf, _ := json.Marshal(p.FunctionResponse.Response)
			summary = fmt.Sprintf("%d bytes", len(buf))
		}
		parts = append(parts, fmt.Sprintf("← %s (%s)", name, summary))
	}
	return strings.Join(parts, " · ")
}

// Force the genai import to be used — Part is a genai type used
// indirectly via session.Event.Content.Parts above.
var _ = (*genai.Part)(nil)

func (e pickerEntry) sessionPath() string {
	if e.App == "" {
		return "/sessions/" + e.SessionID
	}
	return "/sessions/" + e.App + "/" + e.SessionID
}

func (e pickerEntry) displayLabel(alias string) string {
	if alias != "" {
		return alias
	}
	if e.App != "" {
		return e.App + "/" + e.SessionID
	}
	return e.SessionID
}
