// Copyright 2026 The Cogo Authors.
// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/go-steer/core-agent/permissions"
	"github.com/go-steer/core-agent/usage"
)

// thinkingTickMsg fires on a timer while a turn is in flight so the
// in-chat "Thinking…" indicator can rotate to the next phrase. The
// scheduler reschedules itself only while StateStreaming, so no
// background CPU is spent when the TUI is idle.
type thinkingTickMsg struct{}

// thinkingTickCmd returns a tea.Cmd that emits a thinkingTickMsg after
// thinkingTickInterval milliseconds.
func thinkingTickCmd() tea.Cmd {
	return tea.Tick(time.Duration(thinkingTickInterval)*time.Millisecond, func(time.Time) tea.Msg {
		return thinkingTickMsg{}
	})
}

// Update is Cogo's central message dispatch.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		return m.handleResize(msg)
	case tea.MouseMsg:
		// Forward only wheel events; everything else (clicks, drags,
		// motion) is dropped so the input area and modals don't react
		// to stray clicks. Shift-drag bypasses our capture at the
		// terminal layer, so text selection still works.
		if tea.MouseEvent(msg).IsWheel() {
			var cmd tea.Cmd
			m.viewport, cmd = m.viewport.Update(msg)
			return m, cmd
		}
		return m, nil
	case tea.KeyMsg:
		// Elicit modal preempts everything (it can carry a free-text
		// input that would otherwise eat slash commands).
		if m.pendingElicit != nil {
			return m.handleElicitKey(msg)
		}
		// Permission modal preempts every other key handler when up.
		if m.pendingConfirm != nil {
			return m.handleConfirmKey(msg)
		}
		// Model picker likewise.
		if m.modelPicker != nil {
			return m.handleModelPickerKey(msg)
		}
		// Permissions review picker takes the same precedence as the
		// model picker — it replaces the input area while open.
		if m.permissionsPicker != nil {
			return m.handlePermissionsPickerKey(msg)
		}
		return m.handleKey(msg)
	case confirmReqMsg:
		// Show modal; remember the request so handleConfirmKey can
		// reply to the same channel. If a request is already in flight
		// we deny the new one immediately to avoid stacking.
		if m.pendingConfirm != nil {
			msg.Out <- permissions.DecisionDeny
			return m, nil
		}
		m.pendingConfirm = &msg
		return m, nil
	case elicitReqMsg:
		// Build render state. If the server's request is malformed
		// (nested schemas, unsupported types) we auto-decline rather
		// than render a possibly-unsafe form.
		if m.pendingElicit != nil {
			// Already a modal up; decline the new one to avoid
			// stacking. Server can retry.
			select {
			case msg.Out <- &mcpsdk.ElicitResult{Action: "decline"}:
			default:
			}
			return m, nil
		}
		st, err := newElicitState(msg.ServerName, msg.Req, msg.Out)
		if err != nil {
			select {
			case msg.Out <- &mcpsdk.ElicitResult{Action: "decline"}:
			default:
			}
			m.history.Append(Message{Role: RoleSystem, Text: fmt.Sprintf(
				"MCP server %q sent an unsupported elicitation request (%s); declined.",
				msg.ServerName, err.Error())})
			m.refreshViewport()
			return m, nil
		}
		m.pendingElicit = st
		return m, textinput.Blink
	case streamChunkMsg:
		return m.handleStreamChunk(msg)
	case toolCallMsg:
		// Tool calls split the assistant's response into segments. Close
		// out the in-progress assistant message (if any) so the next
		// streaming chunks land in a fresh assistant message *below*
		// this tool line. Without this the tool call appears under
		// the assistant text forever and chunks that arrive after the
		// tool look out of order.
		if m.currentAssistantIdx >= 0 {
			cur := m.history.Snapshot()[m.currentAssistantIdx]
			if cur.Text != "" {
				rendered := strings.TrimRight(m.md.Render(cur.Text), "\n")
				m.history.SetRendered(m.currentAssistantIdx, rendered)
			}
			m.currentAssistantIdx = -1
		}
		m.history.Append(Message{Role: RoleTool, Text: formatToolCall(msg.Name, msg.Args)})
		m.refreshViewport()
		return m, nil
	case usageMsg:
		if m.usage != nil {
			pricing := usage.PriceFor(m.cfg.Model.Name, m.cfg)
			m.usage.Append(m.cfg.Model.Name, msg.InputTokens, msg.OutputTokens, pricing)
		}
		return m, nil
	case turnDoneMsg:
		return m.handleTurnDone()
	case turnErrMsg:
		return m.handleTurnErr(msg)
	case turnCancelledMsg:
		return m.handleTurnCancelled()
	case spinner.TickMsg:
		// Only animate while streaming to avoid background CPU usage.
		if m.state != StateStreaming {
			return m, nil
		}
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	case thinkingTickMsg:
		// Drop the tick if the turn finished mid-flight. Otherwise
		// rotate to the next phrase, redraw, and reschedule.
		if m.state != StateStreaming {
			return m, nil
		}
		m.thinkingIdx++
		m.refreshViewport()
		return m, thinkingTickCmd()
	}
	// Unhandled — forward typing/etc. to the textarea.
	var cmd tea.Cmd
	m.textarea, cmd = m.textarea.Update(msg)
	return m, cmd
}

// handleConfirmKey resolves the pending permission request based on the
// user's keypress. Anything other than the four configured keys is
// ignored so accidental typing doesn't auto-deny.
func (m *Model) handleConfirmKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.pendingConfirm == nil {
		return m, nil
	}
	var d permissions.Decision
	switch {
	case key.Matches(msg, m.keys.ConfirmAllowOnce):
		d = permissions.DecisionAllowOnce
	case key.Matches(msg, m.keys.ConfirmAllowSession):
		d = permissions.DecisionAllowSession
	case key.Matches(msg, m.keys.ConfirmAllowSessionVerb):
		// Only honor "v" when the gate populated a verb; otherwise
		// the modal didn't show this option and the keystroke is a
		// no-op (prevents an accidental tap from broadening
		// permissions to nothing useful).
		if m.pendingConfirm.Req.Verb == "" {
			return m, nil
		}
		d = permissions.DecisionAllowSessionVerb
	case key.Matches(msg, m.keys.ConfirmAllowSessionTool):
		d = permissions.DecisionAllowSessionTool
	case key.Matches(msg, m.keys.ConfirmAllowAlways):
		d = permissions.DecisionAllowAlways
	case key.Matches(msg, m.keys.ConfirmDeny):
		d = permissions.DecisionDeny
	default:
		return m, nil
	}
	req := m.pendingConfirm.Req
	m.pendingConfirm.Out <- d
	m.pendingConfirm = nil

	// Echo the user's choice into the chat so there's a paper trail.
	m.history.Append(Message{Role: RoleSystem, Text: confirmEcho(req, d)})

	// "Always allow" persists via the host-supplied callback.
	if d == permissions.DecisionAllowAlways && m.AlwaysAllow != nil {
		if err := m.AlwaysAllow(req); err != nil {
			m.history.Append(Message{Role: RoleError, Text: "Couldn't persist allowlist entry: " + err.Error()})
		}
	}
	m.refreshViewport()
	return m, nil
}

func confirmEcho(req permissions.PromptRequest, d permissions.Decision) string {
	return "Permission " + d.String() + ": " + req.ToolName + " — " + req.Detail
}

// handleElicitKey processes a keystroke while the MCP elicitation modal
// is up. The state machine:
//
//   - URL mode:  o = open in browser, a = accept (server treats this as
//     "I completed the flow"), n = decline, esc = cancel.
//   - Form mode: tab/down = next field, shift+tab/up = previous, enter
//     submits (validates first), esc = cancel, n = decline, and any
//     other key is forwarded to the active textinput or used to cycle
//     enum/boolean choices (left/right + space).
//
// The reply happens via st.reply(), which writes onto the buffered Out
// channel the elicitor goroutine is blocked on. After replying we clear
// pendingElicit so normal key handling resumes.
func (m *Model) handleElicitKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	st := m.pendingElicit
	if st == nil {
		return m, nil
	}
	switch st.Mode {
	case elicitURL:
		return m.handleElicitURLKey(st, msg)
	default:
		return m.handleElicitFormKey(st, msg)
	}
}

func (m *Model) handleElicitURLKey(st *elicitState, msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "o":
		// Best-effort browser launch; failure is silent (the URL is
		// still on screen for the user to copy).
		openURL(context.Background(), st.URL)
		return m, nil
	case "a", "enter":
		st.reply("accept", nil)
		m.history.Append(Message{Role: RoleSystem, Text: fmt.Sprintf(
			"MCP %q elicitation: accepted (URL flow).", st.ServerName)})
		m.pendingElicit = nil
		m.refreshViewport()
		return m, nil
	case "n":
		st.reply("decline", nil)
		m.history.Append(Message{Role: RoleSystem, Text: fmt.Sprintf(
			"MCP %q elicitation: declined.", st.ServerName)})
		m.pendingElicit = nil
		m.refreshViewport()
		return m, nil
	case "esc":
		st.reply("cancel", nil)
		m.history.Append(Message{Role: RoleSystem, Text: fmt.Sprintf(
			"MCP %q elicitation: cancelled.", st.ServerName)})
		m.pendingElicit = nil
		m.refreshViewport()
		return m, nil
	}
	return m, nil
}

func (m *Model) handleElicitFormKey(st *elicitState, msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		st.reply("cancel", nil)
		m.history.Append(Message{Role: RoleSystem, Text: fmt.Sprintf(
			"MCP %q elicitation: cancelled.", st.ServerName)})
		m.pendingElicit = nil
		m.refreshViewport()
		return m, nil
	case "tab", "down":
		st.nextField()
		return m, nil
	case "shift+tab", "up":
		st.prevField()
		return m, nil
	case "enter":
		content, errMsg := st.validate()
		if errMsg != "" {
			st.Err = errMsg
			return m, nil
		}
		st.reply("accept", content)
		m.history.Append(Message{Role: RoleSystem, Text: fmt.Sprintf(
			"MCP %q elicitation: accepted.", st.ServerName)})
		m.pendingElicit = nil
		m.refreshViewport()
		return m, nil
	case "left":
		// On enum/boolean fields the arrow keys cycle the choice. On
		// text fields they fall through to the textinput so the cursor
		// can move.
		if !st.activeUsesInput() {
			st.cycleChoice(-1)
			return m, nil
		}
	case "right":
		if !st.activeUsesInput() {
			st.cycleChoice(+1)
			return m, nil
		}
	case " ", "space":
		// Spacebar toggles enum/boolean choice forward — handy on
		// boolean (true ↔ false) and lets enum users avoid hunting for
		// the arrow keys.
		if !st.activeUsesInput() {
			st.cycleChoice(+1)
			return m, nil
		}
	}
	// Default: forward the key to the active textinput (no-op for
	// enum/boolean fields). Any side-effect cmd (cursor blink) bubbles
	// up so it animates while the modal is open.
	cmd := st.updateActiveInput(msg)
	return m, cmd
}

func (m *Model) handleResize(msg tea.WindowSizeMsg) (tea.Model, tea.Cmd) {
	m.width, m.height = msg.Width, msg.Height
	headerH := 1
	inputH := m.textarea.Height() + 2 // border lines
	footerH := 1
	vpH := m.height - headerH - inputH - footerH - bottomPad
	if vpH < 3 {
		vpH = 3
	}
	m.viewport.Width = m.width
	m.viewport.Height = vpH
	m.textarea.SetWidth(m.width - 4) // border + padding
	// Re-init markdown renderer at the new wrap width. Using the
	// pre-resolved style name avoids re-querying the terminal.
	if md, err := NewMarkdownRenderer(m.width-2, m.mdStyle); err == nil {
		m.md = md
	}
	m.refreshViewport()
	return m, nil
}

func (m *Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Palette intercepts up/down/enter/esc/tab when open.
	if m.palette != nil {
		switch msg.String() {
		case "up":
			if m.palette.cursor > 0 {
				m.palette.cursor--
			}
			return m, nil
		case "down":
			if m.palette.cursor < len(m.palette.items)-1 {
				m.palette.cursor++
			}
			return m, nil
		case "esc":
			m.palette = nil
			return m, nil
		case "tab":
			// Tab fills the highlighted item without submitting (slash
			// commands stay un-submitted so the user can add args).
			return m.applyPaletteCompletion()
		case "enter":
			return m.applyPaletteSelection()
		}
		// Other keys fall through to the textarea (typing filters the palette).
	}

	switch {
	case key.Matches(msg, m.keys.Cancel):
		return m.handleCtrlC()
	case key.Matches(msg, m.keys.ClearView):
		m.viewport.GotoTop()
		return m, nil
	case key.Matches(msg, m.keys.ClearInput):
		m.textarea.Reset()
		m.historyCursor = -1
		m.refreshPalette()
		return m, nil
	case key.Matches(msg, m.keys.ScrollUp), key.Matches(msg, m.keys.ScrollDown):
		// PgUp/PgDn always scroll the viewport.
		m.pendingExit = false
		var cmd tea.Cmd
		m.viewport, cmd = m.viewport.Update(msg)
		return m, cmd
	case key.Matches(msg, m.keys.LineUp):
		// Up on empty input: recall previous prompts (shell-style
		// history). When already navigating, step further back.
		// Otherwise (input has text) the keypress falls through to the
		// textarea for cursor movement.
		if m.textarea.Value() == "" || m.historyCursor >= 0 {
			m.recallPrompt(-1)
			return m, nil
		}
	case key.Matches(msg, m.keys.LineDown):
		// Down: step forward through history when navigating; otherwise
		// fall through to textarea cursor movement (most common while
		// composing).
		if m.historyCursor >= 0 {
			m.recallPrompt(+1)
			return m, nil
		}
	case key.Matches(msg, m.keys.Submit):
		// Submit Enter only fires a turn when idle. While streaming we
		// swallow it so users don't accidentally enqueue a half-composed
		// prompt; typed text continues to land in the textarea below so
		// they can compose their next message in the background.
		if m.state == StateStreaming {
			return m, nil
		}
		return m.handleSubmit()
	}
	// Reset pendingExit on any other key so a stray Ctrl+C doesn't linger.
	m.pendingExit = false

	// Always forward character/navigation keys to the textarea — even
	// during streaming — so the user's input doesn't disappear into a
	// state-machine race when the turn ends mid-typing.
	var cmd tea.Cmd
	m.textarea, cmd = m.textarea.Update(msg)

	// After the textarea consumes the key, re-evaluate whether a
	// palette should be open or closed.
	m.refreshPalette()
	return m, cmd
}

// refreshPalette syncs m.palette with the textarea state. Called after
// any keystroke that may have changed the input.
func (m *Model) refreshPalette() {
	value := m.textarea.Value()
	cursor := len(value) // bubbles textarea uses byte offsets; cursor approximated as end
	kind, triggerPos, filter, ok := detectPaletteTrigger(value, cursor)
	if !ok {
		m.palette = nil
		return
	}
	var items []paletteItem
	switch kind {
	case paletteSlash:
		items = filterPaletteItems(allSlashItems(), filter)
	case paletteFile:
		items = listProjectFiles(m.projectRoot, filter)
	}
	if len(items) == 0 {
		m.palette = nil
		return
	}
	cur := 0
	if m.palette != nil && m.palette.kind == kind {
		// Preserve cursor if still in range; otherwise clamp.
		cur = m.palette.cursor
		if cur >= len(items) {
			cur = len(items) - 1
		}
		if cur < 0 {
			cur = 0
		}
	}
	m.palette = &paletteState{
		kind:       kind,
		items:      items,
		cursor:     cur,
		triggerPos: triggerPos,
		filter:     filter,
	}
	if kind == paletteSlash {
		m.palette.trigger = '/'
	} else {
		m.palette.trigger = '@'
	}
}

// recallPrompt steps the history cursor by delta and updates the
// textarea. delta is -1 for "older" and +1 for "newer". The cursor's
// final position is clamped to [-1, len(promptHistory)]; reaching
// past-end clears the input and exits navigation mode.
func (m *Model) recallPrompt(delta int) {
	if len(m.promptHistory) == 0 {
		return
	}
	switch {
	case m.historyCursor < 0:
		// Begin navigation from the most recent.
		m.historyCursor = len(m.promptHistory) - 1
	default:
		m.historyCursor += delta
	}
	switch {
	case m.historyCursor < 0:
		m.historyCursor = 0
	case m.historyCursor >= len(m.promptHistory):
		// Past end → clear input and exit navigation.
		m.historyCursor = -1
		m.textarea.SetValue("")
		m.refreshPalette()
		return
	}
	m.textarea.SetValue(m.promptHistory[m.historyCursor])
	m.refreshPalette()
}

// applyPaletteSelection acts on Enter while the palette is open. Slash
// items: replace the input with the selected command and submit
// immediately. File items: insert the @-path at the trigger position;
// directories drill in (palette stays open with the new filter), files
// finalize and close the palette.
func (m *Model) applyPaletteSelection() (tea.Model, tea.Cmd) {
	if m.palette == nil || len(m.palette.items) == 0 {
		return m, nil
	}
	sel := m.palette.items[m.palette.cursor]
	switch m.palette.kind {
	case paletteSlash:
		m.palette = nil
		m.textarea.SetValue(sel.Value)
		return m.handleSubmit()
	case paletteFile:
		current := m.textarea.Value()
		// Drilling into a dir: replace the partial @-token with the
		// directory's value (which ends in "/") and let refreshPalette
		// re-list files filtered by the new path.
		if sel.IsDir {
			newVal := current[:m.palette.triggerPos] + sel.Value
			m.textarea.SetValue(newVal)
			m.refreshPalette()
			return m, nil
		}
		// File: insert + space + close palette.
		newVal := current[:m.palette.triggerPos] + sel.Value + " "
		m.textarea.SetValue(newVal)
		m.palette = nil
		return m, nil
	}
	return m, nil
}

// applyPaletteCompletion is the Tab variant: like Enter for files, but
// for slash commands it inserts "<command> " (with trailing space) and
// closes the palette without submitting, so the user can add args.
func (m *Model) applyPaletteCompletion() (tea.Model, tea.Cmd) {
	if m.palette == nil || len(m.palette.items) == 0 {
		return m, nil
	}
	sel := m.palette.items[m.palette.cursor]
	switch m.palette.kind {
	case paletteSlash:
		m.textarea.SetValue(sel.Value + " ")
		m.palette = nil
		return m, nil
	case paletteFile:
		// Same as Enter for files (drill-in for dirs; insert+close for files).
		return m.applyPaletteSelection()
	}
	return m, nil
}

func (m *Model) handleCtrlC() (tea.Model, tea.Cmd) {
	if m.state == StateStreaming {
		// Cancel current turn.
		if m.cancelTurn != nil {
			m.cancelTurn()
		}
		return m, nil
	}
	// Idle: first press warns, second exits.
	if !m.pendingExit {
		m.pendingExit = true
		m.history.Append(Message{Role: RoleSystem, Text: "Press Ctrl+C again to exit, or any key to cancel."})
		m.refreshViewport()
		return m, nil
	}
	return m, tea.Quit
}

func (m *Model) handleSubmit() (tea.Model, tea.Cmd) {
	input := m.textarea.Value()
	if strings.TrimSpace(input) == "" {
		return m, nil
	}

	// Confirmation flow for /clear.
	if m.confirmingClear {
		m.confirmingClear = false
		m.textarea.Reset()
		if isYes(input) {
			m.history.Reset()
			m.history.Append(Message{Role: RoleSystem, Text: "History cleared."})
		} else {
			m.history.Append(Message{Role: RoleSystem, Text: "Cancelled."})
		}
		m.refreshViewport()
		return m, nil
	}

	// Slash command?
	if action, cmd, args, isSlash := ParseSlash(input); isSlash {
		m.textarea.Reset()
		return m.handleSlash(action, cmd, args)
	}

	// Regular prompt → start a turn. Expand any @<path> file references
	// before sending to the model; show the user-facing prompt as-typed
	// in history (preserving the @ tokens) but pass the expanded form
	// to the agent so it has the file contents inline.
	m.history.Append(Message{Role: RoleUser, Text: input})
	// Recall history: append the submitted prompt and reset the cursor.
	m.promptHistory = append(m.promptHistory, input)
	m.historyCursor = -1
	expanded, refs, diags := expandAtRefs(input, readFileSafe(64*1024))
	for _, d := range diags {
		m.history.Append(Message{Role: RoleSystem, Text: d})
	}
	if len(refs) > 0 {
		// Surface a warning for any @-ref that lands outside the
		// configured path scope. We still inlined the file (the user
		// typed the @-token explicitly) but they should be aware.
		var outOfScope []string
		if m.scope != nil {
			for _, r := range refs {
				if in, _ := m.scope.Contains(r); !in {
					outOfScope = append(outOfScope, r)
				}
			}
		}
		if len(outOfScope) > 0 {
			m.history.Append(Message{
				Role: RoleSystem,
				Text: "⚠ Inlined out-of-scope file(s): " + strings.Join(outOfScope, ", ") +
					" — these were sent to the model. Add them to .agents/config.json path_scope.allow if you want this without the warning.",
			})
		}
		m.history.Append(Message{Role: RoleSystem, Text: "Inlined file references: " + strings.Join(refs, ", ")})
	}
	m.textarea.Reset()
	m.palette = nil
	// Don't pre-create an assistant placeholder. The first text chunk
	// (handleStreamChunk) lazily creates one; tool calls that arrive
	// before any text aren't pinned beneath an empty placeholder.
	// Every tool call closes the current assistant segment so the
	// NEXT chunk starts a fresh assistant message below the tool
	// line — that's how the user sees the model's response after the
	// tool calls / permission prompts, not above them.
	m.currentAssistantIdx = -1
	m.state = StateStreaming
	// Reset the thinking-phrase rotator so every turn starts on the
	// anchor phrase ("Thinking…") and the cycle is predictable.
	m.thinkingIdx = 0

	ctx, cancel := context.WithCancel(context.Background())
	m.cancelTurn = cancel
	startAgentTurn(ctx, m.program, m.agent, expanded)

	m.refreshViewport()
	return m, tea.Batch(m.spinner.Tick, thinkingTickCmd())
}

func (m *Model) handleSlash(action SlashAction, cmd, args string) (tea.Model, tea.Cmd) {
	switch action {
	case SlashHelp:
		m.history.Append(Message{Role: RoleSystem, Text: HelpText()})
		m.refreshViewport()
		return m, nil
	case SlashClear:
		m.confirmingClear = true
		m.history.Append(Message{Role: RoleSystem, Text: "Clear chat history? Type 'y' or 'yes' to confirm; anything else cancels."})
		m.refreshViewport()
		return m, nil
	case SlashMemory:
		m.history.Append(Message{Role: RoleSystem, Text: m.renderMemoryInfo()})
		m.refreshViewport()
		return m, nil
	case SlashStats:
		m.history.Append(Message{Role: RoleSystem, Text: m.renderStatsInfo()})
		m.refreshViewport()
		return m, nil
	case SlashMCP:
		m.history.Append(Message{Role: RoleSystem, Text: m.renderMCPInfo()})
		m.refreshViewport()
		return m, nil
	case SlashSkills:
		m.history.Append(Message{Role: RoleSystem, Text: m.renderSkillsInfo()})
		m.refreshViewport()
		return m, nil
	case SlashTools:
		m.history.Append(Message{Role: RoleSystem, Text: m.renderToolsInfo()})
		m.refreshViewport()
		return m, nil
	case SlashReload:
		return m.handleReload()
	case SlashMouse:
		return m.handleMouseCommand(args)
	case SlashPermissions:
		return m.handlePermissionsCommand(args)
	case SlashAllow:
		return m.handleAllowCommand(args)
	case SlashDeny:
		return m.handleDenyCommand(args)
	case SlashModel:
		return m.handleModelCommand(args)
	case SlashQuit:
		return m, tea.Quit
	default:
		m.history.Append(Message{Role: RoleSystem, Text: fmt.Sprintf("Unknown command: /%s. Type /help for the list.", cmd)})
		m.refreshViewport()
		return m, nil
	}
}

// handleReload re-reads .agents/ from disk and swaps the agent in
// place. Existing chat history and usage totals are preserved so the
// session feels continuous.
func (m *Model) handleReload() (tea.Model, tea.Cmd) {
	if m.reloadFromDisk == nil {
		m.history.Append(Message{Role: RoleError, Text: "Reload not available (no project root or builder not configured)."})
		m.refreshViewport()
		return m, nil
	}
	res, err := m.reloadFromDisk()
	if err != nil {
		m.history.Append(Message{Role: RoleError, Text: "Reload failed: " + err.Error()})
		m.refreshViewport()
		return m, nil
	}
	m.agent = res.Agent
	m.memory = res.Memory
	m.mcpServers = res.MCPServers
	m.skills = res.Skills
	m.history.Append(Message{Role: RoleSystem, Text: "Reloaded .agents/ from disk. Memory + MCP servers + skills refreshed; chat history and usage totals preserved."})
	m.refreshViewport()
	return m, nil
}

// handleMouseCommand toggles mouse capture, or sets it explicitly when
// the user passes "on"/"off". The change applies to the current session
// only; persistence lives in `ui.mouse` in .agents/config.json.
func (m *Model) handleMouseCommand(args string) (tea.Model, tea.Cmd) {
	want := !m.mouseEnabled
	switch strings.ToLower(strings.TrimSpace(args)) {
	case "", "toggle":
		// fall through with the flipped value
	case "on", "true", "yes", "enable", "enabled":
		want = true
	case "off", "false", "no", "disable", "disabled":
		want = false
	default:
		m.history.Append(Message{Role: RoleSystem, Text: fmt.Sprintf(
			"Usage: /mouse [on|off]. Currently %s.", mouseStateLabel(m.mouseEnabled))})
		m.refreshViewport()
		return m, nil
	}
	if want == m.mouseEnabled {
		m.history.Append(Message{Role: RoleSystem, Text: fmt.Sprintf(
			"Mouse capture already %s.", mouseStateLabel(m.mouseEnabled))})
		m.refreshViewport()
		return m, nil
	}
	m.mouseEnabled = want
	var teaCmd tea.Cmd
	if want {
		teaCmd = tea.EnableMouseCellMotion
		m.history.Append(Message{Role: RoleSystem, Text: "Mouse capture on — wheel scrolls the chat. Hold Shift while dragging to select text."})
	} else {
		teaCmd = tea.DisableMouse
		m.history.Append(Message{Role: RoleSystem, Text: "Mouse capture off — plain drag selects text. Use PgUp/PgDn to scroll."})
	}
	m.refreshViewport()
	return m, teaCmd
}

func mouseStateLabel(on bool) string {
	if on {
		return "on"
	}
	return "off"
}

// handleModelCommand handles `/model` (no args → open picker) and
// `/model <id>` (direct switch). Switching mid-session resets the
// agent and clears the chat history (the viewport content stays for
// reference).
func (m *Model) handleModelCommand(args string) (tea.Model, tea.Cmd) {
	args = strings.TrimSpace(args)
	if args == "" {
		// Open picker.
		items := availableModels()
		cur := indexOfModel(m.cfg.Model.Name)
		if cur < 0 {
			cur = 0
		}
		m.modelPicker = &modelPickerState{items: items, cursor: cur}
		return m, nil
	}
	return m.switchModel(args)
}

// switchModel rebuilds the agent with the given model ID and persists
// the choice to .agents/config.json when an agentsDir is available.
// Returns a system message describing the result.
func (m *Model) switchModel(modelID string) (tea.Model, tea.Cmd) {
	if m.rebuildAgent == nil {
		m.history.Append(Message{Role: RoleError, Text: "Cannot switch model: agent rebuilder not configured."})
		m.refreshViewport()
		return m, nil
	}
	if modelID == m.cfg.Model.Name {
		m.history.Append(Message{Role: RoleSystem, Text: "Already using " + modelID + "."})
		m.refreshViewport()
		return m, nil
	}
	// Reject unknown model IDs up front — the provider builds the model
	// lazily, so without this check `/model bogus` looks like it
	// succeeded and only fails on the next prompt with an opaque
	// API 400. Listing the candidates makes the failure actionable.
	if indexOfModel(modelID) < 0 {
		m.history.Append(Message{Role: RoleError, Text: fmt.Sprintf(
			"Unknown model %q. Try one of: %s",
			modelID, strings.Join(availableModels(), ", "))})
		m.refreshViewport()
		return m, nil
	}
	newAgent, err := m.rebuildAgent(modelID)
	if err != nil {
		m.history.Append(Message{Role: RoleError, Text: "Switch failed: " + err.Error()})
		m.refreshViewport()
		return m, nil
	}
	m.agent = newAgent
	m.cfg.Model.Name = modelID
	if m.persistModelChoice != nil {
		if err := m.persistModelChoice(modelID); err != nil {
			m.history.Append(Message{Role: RoleSystem, Text: "Switched in-session, but couldn't persist to config: " + err.Error()})
		}
	}
	m.history.Append(Message{Role: RoleSystem, Text: "Switched to " + modelID + ". Conversation context resets for the new model."})
	m.refreshViewport()
	return m, nil
}

func (m *Model) handleStreamChunk(msg streamChunkMsg) (tea.Model, tea.Cmd) {
	// Outside a streaming turn the chunk is stale — drop it.
	if m.state != StateStreaming {
		return m, nil
	}
	// Lazily create the assistant message on the first chunk of each
	// segment. handleSubmit clears currentAssistantIdx so the very
	// first chunk of a turn lands here, and toolCallMsg clears it
	// again so chunks after a tool call start a new message below
	// the tool line.
	if m.currentAssistantIdx < 0 {
		m.currentAssistantIdx = m.history.Append(Message{Role: RoleAssistant})
	}
	m.history.AppendText(m.currentAssistantIdx, msg.Text)
	m.refreshViewport()
	return m, nil
}

// handlePermissionsCommand dispatches on the optional sub-action.
// Bare /permissions opens the review picker; /permissions list prints
// a read-only snapshot of the current allow/deny/bundle config. Any
// other argument falls through to the picker so we don't surprise
// users who type /permissions <typo>.
func (m *Model) handlePermissionsCommand(args string) (tea.Model, tea.Cmd) {
	switch strings.TrimSpace(args) {
	case "list", "show", "ls":
		m.history.Append(Message{Role: RoleSystem, Text: m.renderPermissionsListInfo()})
		m.refreshViewport()
		return m, nil
	}
	if m.SessionApprovals == nil {
		m.history.Append(Message{Role: RoleSystem, Text: "Permissions review unavailable: this build has no session approval log wired up."})
		m.refreshViewport()
		return m, nil
	}
	approvals := m.SessionApprovals()
	picker := newPermissionsPicker(approvals)
	if picker == nil {
		m.history.Append(Message{Role: RoleSystem, Text: "No interactive approvals this session yet — there's nothing to review. Use /allow <pattern> to pre-approve patterns up front, or /permissions list to see what's already configured."})
		m.refreshViewport()
		return m, nil
	}
	m.permissionsPicker = picker
	return m, nil
}

// handleAllowCommand handles `/allow <pattern>` and `/allow bundle:<name>`.
// Both paths validate first so the user gets a clear error before
// anything touches cogo.json or the live gate.
func (m *Model) handleAllowCommand(args string) (tea.Model, tea.Cmd) {
	arg := strings.TrimSpace(args)
	if arg == "" {
		m.history.Append(Message{Role: RoleSystem, Text: "Usage: /allow <pattern>   e.g. /allow bash:git *   or   /allow bundle:dev_tools"})
		m.refreshViewport()
		return m, nil
	}
	if name, ok := strings.CutPrefix(arg, "bundle:"); ok {
		return m.applyAllowBundle(strings.TrimSpace(name))
	}
	return m.applyAllowPattern(arg)
}

// handleDenyCommand persists a deny pattern. Deny always wins in the
// policy so adding here is the right escape hatch when the LLM finds
// a way to chain destructively through a previously-allowed verb.
func (m *Model) handleDenyCommand(args string) (tea.Model, tea.Cmd) {
	arg := strings.TrimSpace(args)
	if arg == "" {
		m.history.Append(Message{Role: RoleSystem, Text: "Usage: /deny <pattern>   e.g. /deny bash:curl *"})
		m.refreshViewport()
		return m, nil
	}
	if m.AddDenyPatterns == nil {
		m.history.Append(Message{Role: RoleError, Text: "Can't persist deny patterns: no project root for .agents/config.json. Run cogo from a directory with an .agents/ folder."})
		m.refreshViewport()
		return m, nil
	}
	if err := m.AddDenyPatterns([]string{arg}); err != nil {
		m.history.Append(Message{Role: RoleError, Text: "Couldn't add deny pattern: " + err.Error()})
		m.refreshViewport()
		return m, nil
	}
	m.history.Append(Message{Role: RoleSystem, Text: "Added to .agents/config.json permissions.deny:\n  " + arg + "\n(applies now; deny wins over any allow rule.)"})
	m.refreshViewport()
	return m, nil
}

func (m *Model) applyAllowPattern(pattern string) (tea.Model, tea.Cmd) {
	if m.AddAllowPatterns == nil {
		m.history.Append(Message{Role: RoleError, Text: "Can't persist allow patterns: no project root for .agents/config.json. Run cogo from a directory with an .agents/ folder."})
		m.refreshViewport()
		return m, nil
	}
	if err := m.AddAllowPatterns([]string{pattern}); err != nil {
		m.history.Append(Message{Role: RoleError, Text: "Couldn't add allow pattern: " + err.Error()})
		m.refreshViewport()
		return m, nil
	}
	m.history.Append(Message{Role: RoleSystem, Text: "Added to .agents/config.json permissions.allow:\n  " + pattern + "\n(applies immediately — no /reload needed.)"})
	m.refreshViewport()
	return m, nil
}

func (m *Model) applyAllowBundle(name string) (tea.Model, tea.Cmd) {
	if name == "" {
		m.history.Append(Message{Role: RoleSystem, Text: "Usage: /allow bundle:<name>   known bundles: " + strings.Join(permissions.KnownBundles(), ", ")})
		m.refreshViewport()
		return m, nil
	}
	if m.AddBuiltinAllowExtra == nil {
		m.history.Append(Message{Role: RoleError, Text: "Can't enable bundle: no project root for .agents/config.json. Run core-agent from a directory with an .agents/ folder."})
		m.refreshViewport()
		return m, nil
	}
	if err := m.AddBuiltinAllowExtra(name); err != nil {
		m.history.Append(Message{Role: RoleError, Text: "Couldn't enable bundle: " + err.Error()})
		m.refreshViewport()
		return m, nil
	}
	m.history.Append(Message{Role: RoleSystem, Text: fmt.Sprintf("Enabled bundle %q in .agents/config.json (permissions.builtin_allow_extras).\n(applies immediately.)", name)})
	m.refreshViewport()
	return m, nil
}

// renderPermissionsListInfo returns a read-only snapshot of the
// current permissions config as a multi-line string. Mirrors the
// /memory and /stats info-style commands so the output lands in the
// chat history rather than a modal.
func (m *Model) renderPermissionsListInfo() string {
	pc := m.cfg.Permissions
	useBuiltin := true
	if pc.UseBuiltinAllow != nil {
		useBuiltin = *pc.UseBuiltinAllow
	}
	mode := pc.Mode
	if mode == "" {
		mode = "ask"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Permission mode: %s\n", mode)
	fmt.Fprintf(&b, "Built-in allow: %s\n", boolOnOff(useBuiltin))
	if len(pc.BuiltinAllowExtras) > 0 {
		fmt.Fprintf(&b, "  extra bundles: %s\n", strings.Join(pc.BuiltinAllowExtras, ", "))
	}
	if useBuiltin {
		fmt.Fprintf(&b, "  (read_only baseline always active; known bundles: %s)\n", strings.Join(permissions.KnownBundles(), ", "))
	}
	b.WriteString("\n")
	writePatternList(&b, "permissions.allow", pc.Allow)
	b.WriteString("\n")
	writePatternList(&b, "permissions.deny", pc.Deny)
	return strings.TrimRight(b.String(), "\n")
}

// boolOnOff renders true→"enabled" / false→"disabled" for the
// permissions snapshot display.
func boolOnOff(b bool) string {
	if b {
		return "enabled"
	}
	return "disabled"
}

func writePatternList(b *strings.Builder, label string, patterns []string) {
	if len(patterns) == 0 {
		fmt.Fprintf(b, "%s: (empty)\n", label)
		return
	}
	fmt.Fprintf(b, "%s (%d):\n", label, len(patterns))
	for _, p := range patterns {
		fmt.Fprintf(b, "  %s\n", p)
	}
}

// handlePermissionsPickerKey runs while the /permissions overlay is
// open. Up/Down navigate; Space toggles the row; Enter persists the
// selected patterns; Esc dismisses.
func (m *Model) handlePermissionsPickerKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.permissionsPicker == nil {
		return m, nil
	}
	p := m.permissionsPicker
	switch msg.String() {
	case "up":
		if p.cursor > 0 {
			p.cursor--
		}
	case "down":
		if p.cursor < len(p.recs)-1 {
			p.cursor++
		}
	case " ":
		if p.cursor >= 0 && p.cursor < len(p.selected) {
			p.selected[p.cursor] = !p.selected[p.cursor]
		}
	case "esc":
		m.permissionsPicker = nil
	case "enter":
		patterns := p.chosenPatterns()
		m.permissionsPicker = nil
		if len(patterns) == 0 {
			m.history.Append(Message{Role: RoleSystem, Text: "Permissions review closed without persisting anything."})
			m.refreshViewport()
			return m, nil
		}
		if m.AddAllowPatterns == nil {
			m.history.Append(Message{Role: RoleError, Text: "Can't persist allowlist entries: no project root for .agents/config.json. Run cogo from a directory with an .agents/ folder."})
			m.refreshViewport()
			return m, nil
		}
		if err := m.AddAllowPatterns(patterns); err != nil {
			m.history.Append(Message{Role: RoleError, Text: "Persist failed: " + err.Error()})
			m.refreshViewport()
			return m, nil
		}
		m.history.Append(Message{Role: RoleSystem, Text: "Added to .agents/config.json permissions.allow:\n  " + strings.Join(patterns, "\n  ")})
		m.refreshViewport()
	}
	return m, nil
}

// handleModelPickerKey runs while the /model picker overlay is open.
// Up/Down navigate; Enter selects + closes; Esc dismisses.
func (m *Model) handleModelPickerKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.modelPicker == nil {
		return m, nil
	}
	switch msg.String() {
	case "up":
		if m.modelPicker.cursor > 0 {
			m.modelPicker.cursor--
		}
	case "down":
		if m.modelPicker.cursor < len(m.modelPicker.items)-1 {
			m.modelPicker.cursor++
		}
	case "esc":
		m.modelPicker = nil
	case "enter":
		choice := m.modelPicker.items[m.modelPicker.cursor]
		m.modelPicker = nil
		return m.switchModel(choice)
	}
	return m, nil
}

func (m *Model) handleTurnDone() (tea.Model, tea.Cmd) {
	if m.currentAssistantIdx >= 0 {
		// Re-render the completed assistant message through Glamour.
		raw := m.history.Snapshot()[m.currentAssistantIdx].Text
		m.history.SetRendered(m.currentAssistantIdx, strings.TrimRight(m.md.Render(raw), "\n"))
	}
	m.endTurn()
	m.refreshViewport()
	return m, nil
}

func (m *Model) handleTurnErr(msg turnErrMsg) (tea.Model, tea.Cmd) {
	if m.currentAssistantIdx >= 0 {
		// If we accumulated any partial output, leave it; just append an
		// error notice afterward.
		current := m.history.Snapshot()[m.currentAssistantIdx]
		if current.Text == "" {
			// Drop the empty assistant placeholder rather than rendering a blank slot.
			m.dropLastAssistant()
		}
	}
	m.history.Append(Message{Role: RoleError, Text: fmt.Sprintf("Error: %v", msg.Err)})
	m.endTurn()
	m.refreshViewport()
	return m, nil
}

func (m *Model) handleTurnCancelled() (tea.Model, tea.Cmd) {
	if m.currentAssistantIdx >= 0 {
		current := m.history.Snapshot()[m.currentAssistantIdx]
		if current.Text == "" {
			m.dropLastAssistant()
		}
	}
	m.history.Append(Message{Role: RoleSystem, Text: "(interrupted)"})
	m.endTurn()
	m.refreshViewport()
	return m, nil
}

func (m *Model) endTurn() {
	m.state = StateIdle
	m.currentAssistantIdx = -1
	if m.cancelTurn != nil {
		m.cancelTurn()
		m.cancelTurn = nil
	}
}

// dropLastAssistant rewinds the in-progress assistant message. Called
// when the turn ended before any text was produced.
func (m *Model) dropLastAssistant() {
	if m.currentAssistantIdx < 0 {
		return
	}
	snap := m.history.Snapshot()
	m.history.Reset()
	for i, msg := range snap {
		if i == m.currentAssistantIdx {
			continue
		}
		m.history.Append(msg)
	}
}

func isYes(s string) bool {
	t := strings.ToLower(strings.TrimSpace(s))
	return t == "y" || t == "yes"
}
