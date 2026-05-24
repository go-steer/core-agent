// Copyright 2026 The Cogo Authors.
// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/muesli/reflow/wordwrap"
	"github.com/muesli/reflow/wrap"

	"github.com/go-steer/core-agent/agent"
	"github.com/go-steer/core-agent/config"
	"github.com/go-steer/core-agent/instruction"
	"github.com/go-steer/core-agent/mcp"
	"github.com/go-steer/core-agent/permissions"
	"github.com/go-steer/core-agent/skills"
	"github.com/go-steer/core-agent/usage"
)

// State tracks the agent's current activity for input gating and View rendering.
type State int

const (
	StateIdle      State = iota // accepting input, no turn in flight
	StateStreaming              // a turn is running, input disabled
)

// Model is Cogo's Bubble Tea model. Mutated through *Model receivers so
// goroutine-driven Sends can update the same instance.
type Model struct {
	// Set by program.go after tea.NewProgram constructs the program.
	program programSender

	cfg   *config.Config
	agent *agent.Agent

	// UI components.
	history  History
	textarea textarea.Model
	viewport viewport.Model
	spinner  spinner.Model
	keys     KeyMap
	styles   Styles
	md       *MarkdownRenderer

	// Style name passed to Glamour. Resolved once at construction so
	// repeat renderer builds (on every resize) don't re-query the
	// terminal background.
	mdStyle string

	width  int
	height int

	// Turn state.
	state               State
	cancelTurn          context.CancelFunc
	currentAssistantIdx int // index in history of the in-progress assistant msg, -1 if none

	// Slash-command state.
	confirmingClear bool

	// Open palette overlay (slash-command discovery or @-file picker).
	// Non-nil while the overlay is visible; key handling intercepts
	// up/down/enter/esc in that case.
	palette *paletteState

	// projectRoot is the resolved directory used as the source for the
	// @-file picker; defaults to the cwd at NewModel time.
	projectRoot string

	// scope is consulted only to warn about @-file references that
	// point outside the in-scope roots. The user's keystroke is
	// authoritative consent (we still inline the file), but a system
	// message keeps them aware so they don't paste private files into
	// the model context by accident. Nil-safe.
	scope *permissions.PathScope

	// memory holds the AGENTS.md/CLAUDE.md/GEMINI.md contents loaded
	// at startup; surfaced via /instruction.
	memory instruction.Loaded

	// usage records per-turn token + cost accounting; surfaced via
	// /stats and the per-message footer + header running total.
	usage *usage.Tracker

	// rebuildAgent rebuilds the agent + runner with a new model ID,
	// preserving memory + tools. Set by program.go so /model can
	// switch mid-session without the TUI knowing about provider /
	// gate / tools wiring.
	rebuildAgent func(modelID string) (*agent.Agent, error)

	// reloadFromDisk re-reads .agents/ (mcp.json, skills/, AGENTS.md,
	// config.json) and rebuilds the agent in place. The new agent +
	// state get installed on the model. Set by program.go; nil-safe.
	reloadFromDisk func() (reloadResult, error)

	// persistModelChoice saves the new model choice to
	// .agents/config.json when invoked. May be nil if no project
	// config exists; in that case the switch is in-session only.
	persistModelChoice func(modelID string) error

	// modelPicker is the open Model picker overlay, if any.
	modelPicker *modelPickerState

	// permissionsPicker is the open /permissions overlay, if any. See
	// permissions_picker.go for layout + key handling.
	permissionsPicker *permissionsPicker

	// mcpServers + skills carry the discovered extensibility for
	// /mcp + /skills rendering. Both are nil-safe.
	mcpServers []*mcp.Server
	skills     skills.Skills

	// Pending permission request from the gate. Non-nil while the
	// permission modal is up; the user's keypress writes back to
	// pendingConfirm.Out and clears this field.
	pendingConfirm *confirmReqMsg

	// Pending MCP elicitation request. Non-nil while the elicit modal
	// is up; key handling intercepts Tab / Enter / Esc / printable
	// keys until the user replies.
	pendingElicit *elicitState

	// Prompt history: the user's submitted prompts in submission
	// order. cursor is the active recall position when navigating
	// (-1 = not navigating, len(promptHistory) = past-end / empty input).
	promptHistory []string
	historyCursor int

	// True when the user just hit Ctrl+C while idle once. Second press exits.
	pendingExit bool

	// mouseEnabled mirrors whether the program is currently capturing
	// mouse events. Toggled by /mouse, which dispatches the matching
	// tea.EnableMouseCellMotion / tea.DisableMouse command.
	mouseEnabled bool

	// thinkingIdx selects which entry of thinkingPhrases is currently
	// flashing in the chat while we wait for the model. A thinkingTick
	// scheduler in update.go bumps the index on each tick; renderMessage
	// reads it when the in-progress assistant message has no chunks yet.
	// Ignored entirely outside StateStreaming, so the rotator costs
	// nothing when the TUI is idle.
	thinkingIdx int

	// AlwaysAllow is invoked when the user picks "always allow" in the
	// permission modal. The host (TUI launcher) plugs in a function
	// that persists the pattern to .agents/config.json. May be nil in
	// tests.
	AlwaysAllow func(req permissions.PromptRequest) error

	// SessionApprovals returns the gate's chronological approval log
	// for the current session. Used by the /permissions slash command
	// to drive the recommendation picker. May be nil in tests; in
	// that case /permissions reports nothing-to-review.
	SessionApprovals func() []permissions.ApprovalLog

	// AddAllowPatterns appends one or more allowlist patterns to
	// .agents/config.json's permissions.allow block AND patches the
	// live gate so the additions take effect for the rest of this
	// session (no /reload needed). Called by the /permissions picker
	// and the /allow slash command. May be nil when running without a
	// project root; callers should report the lack of persistence to
	// the user as a system message.
	AddAllowPatterns func(patterns []string) error

	// AddDenyPatterns is the symmetric extension for permissions.deny,
	// driven by /deny. Same nil semantics as AddAllowPatterns.
	AddDenyPatterns func(patterns []string) error

	// AddBuiltinAllowExtra appends a bundle name to
	// permissions.builtin_allow_extras AND injects that bundle's
	// patterns into the live gate. Used by /allow bundle:<name>.
	// May be nil when running without a project root.
	AddBuiltinAllowExtra func(name string) error
}

// NewModel constructs a fresh chat session bound to a configured agent.
// The program reference is set later via SetProgram.
//
// mdStyle picks the Glamour style ("dark" or "light") for assistant
// markdown rendering. Detect this once before tea.NewProgram (see
// program.Run); resolving it during the program's lifetime causes
// Glamour's background-color query response to leak into the textarea.
func NewModel(cfg *config.Config, a *agent.Agent, mdStyle string) *Model {
	keys := DefaultKeyMap()

	ta := textarea.New()
	ta.Placeholder = "Message · / for commands · @ for files…"
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	ta.SetHeight(3)
	ta.KeyMap.InsertNewline = keys.Newline
	ta.Focus()

	vp := viewport.New(0, 0)

	styles := DefaultStyles()
	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = styles.Spinner

	md, _ := NewMarkdownRenderer(80, mdStyle) // tightened on first WindowSizeMsg

	cwd, _ := os.Getwd()

	m := &Model{
		cfg:                 cfg,
		agent:               a,
		textarea:            ta,
		viewport:            vp,
		spinner:             sp,
		keys:                keys,
		styles:              styles,
		md:                  md,
		mdStyle:             mdStyle,
		state:               StateIdle,
		currentAssistantIdx: -1,
		projectRoot:         cwd,
		historyCursor:       -1,
		usage:               usage.NewTracker(),
	}
	return m
}

// SetProgram wires the running tea.Program in so background goroutines
// (the agent runner) can Send messages back to the loop.
func (m *Model) SetProgram(p programSender) { m.program = p }

// Init returns the initial commands. The spinner is started so its Tick
// loop can animate when transitioning into the streaming state; the
// textarea blink keeps the cursor visible.
func (m *Model) Init() tea.Cmd {
	return tea.Batch(textarea.Blink, m.spinner.Tick)
}

// renderHistory builds the viewport contents from the current history.
// One block per message, separated by blank lines. With no messages
// yet, an empty-state hint stands in so the viewport isn't blank on
// first launch — the brand wordmark already lives in the persistent
// header above, so no banner is needed here.
//
// During a streaming turn, the rotating "Thinking…" indicator is
// appended at the very end whenever the agent is between segments —
// either before any chunk has arrived, or after a tool call closed
// the previous segment and the next chunk hasn't landed yet. The
// indicator therefore always stays at the bottom of the chat as new
// tool calls and assistant segments scroll past it.
func (m *Model) renderHistory() string {
	if m.history.Len() == 0 {
		return emptyStateHint()
	}
	msgs := m.history.Snapshot()
	parts := make([]string, 0, len(msgs)+1)
	for _, msg := range msgs {
		parts = append(parts, m.renderMessage(msg))
	}
	if m.state == StateStreaming && m.currentAssistantIdx < 0 {
		parts = append(parts, m.renderThinkingLine())
	}
	return strings.Join(parts, "\n\n")
}

func (m *Model) renderMessage(msg Message) string {
	switch msg.Role {
	case RoleUser:
		prefix := m.styles.UserPrefix.Render("❯")
		// Wrap the prompt at the viewport width so long messages
		// don't run off the right edge. Continuation lines are
		// indented to align with the text after the "❯ " prefix.
		wrapped := wrapForChat(msg.Display(), m.viewport.Width-2, "  ")
		return prefix + " " + m.styles.UserText.Render(wrapped)
	case RoleAssistant:
		// Display() prefers the Glamour-rendered form when available;
		// during streaming it falls back to raw text. The thinking
		// indicator no longer renders here — renderHistory appends it
		// at the bottom of the chat when the agent is between segments.
		text := msg.Display()
		if msg.Rendered == "" {
			return strings.TrimRight(m.md.Render(text), "\n")
		}
		// Append a per-prompt usage footer when available.
		if footer := m.lastTurnUsageFooter(); footer != "" {
			return text + "\n" + footer
		}
		return text
	case RoleSystem:
		return m.styles.System.Render(wrapForChat("ℹ  "+msg.Display(), m.viewport.Width, "   "))
	case RoleError:
		return m.styles.Error.Render(wrapForChat("⚠  "+msg.Display(), m.viewport.Width, "   "))
	case RoleTool:
		// Tool lines render the icon + name in the bold-accent style
		// (matches the model name in the header — proven stable on
		// every host we test). If an arg summary is present (separated
		// by " · "), it is styled with the System style to recede visually.
		// Both spans are wrapped at viewport width with continuation indent
		// past the "⚙ " prefix.
		text := msg.Display()
		parts := strings.SplitN(text, " · ", 2)
		var line string
		if len(parts) == 2 {
			line = m.styles.HeaderAccent.Render("⚙  "+parts[0]+" · ") + m.styles.System.Render(parts[1])
		} else {
			line = m.styles.HeaderAccent.Render("⚙  " + text)
		}
		return wrapForChat(line, m.viewport.Width, "   ")
	default:
		return msg.Display()
	}
}

// formatToolCall renders a one-line summary of a tool invocation for
// the chat: just the tool name when no useful arg is available, or
// `name · <hint>` when we can pull a recognizable hint out of args.
// We deliberately keep it single-line and short — the chat is for the
// human to glance at the agent's actions, not for full audit trails.
func formatToolCall(name string, args map[string]any) string {
	hint := toolArgHint(name, args)
	if hint == "" {
		return name
	}
	// Cap the hint so a giant inline file or 4 KB bash command can't
	// blow out a single chat row. Wrap will still shorten it further
	// if the viewport is narrow; this is an absolute upper bound.
	const maxHint = 200
	if len(hint) > maxHint {
		hint = hint[:maxHint] + "…"
	}
	return name + " · " + hint
}

// toolArgHint extracts the most useful one-line hint from a tool's
// argument map. Recognizes the common cogo built-in tools by name; for
// anything else it returns "" so the line stays as just the tool name.
// Adding new tools here is cheap; not adding them is harmless.
func toolArgHint(name string, args map[string]any) string {
	if args == nil {
		return ""
	}
	pick := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := args[k]; ok {
				if s, ok := v.(string); ok && s != "" {
					return s
				}
			}
		}
		return ""
	}
	switch name {
	case "bash":
		if cmd := pick("command", "cmd"); cmd != "" {
			return "$ " + strings.ReplaceAll(strings.ReplaceAll(cmd, "\n", " "), "\t", " ")
		}
	case "read_file", "write_file", "edit_file":
		return pick("path", "file", "filename")
	case "read_many_files":
		if pattern := pick("pattern"); pattern != "" {
			return pattern
		}
		if paths, ok := args["paths"].([]any); ok && len(paths) > 0 {
			if s, ok := paths[0].(string); ok {
				if len(paths) > 1 {
					return fmt.Sprintf("%s (+%d)", s, len(paths)-1)
				}
				return s
			}
		}
	case "grep", "glob":
		pattern := pick("pattern", "query")
		path := pick("path", "dir")
		switch {
		case pattern != "" && path != "":
			return strconv.Quote(pattern) + " in " + path
		case pattern != "":
			return strconv.Quote(pattern)
		case path != "":
			return path
		}
	case "list_files", "ls", "list_dir":
		return pick("path", "dir")
	case "go_build", "go_test", "go_vet":
		if p := pick("pattern"); p != "" {
			return p
		}
		return "./..."
	case "go_doc":
		return pick("target")
	case "go_symbol_find":
		return pick("name")
	case "go_implements":
		return pick("interface")
	case "todo":
		action := pick("action")
		if action == "add" {
			if text := pick("text"); text != "" {
				return "add: " + text
			}
		}
		return action
	}
	return ""
}

// wrapForChat word-wraps text to fit inside a chat line of the given
// visible width. Continuation lines are prefixed with `indent` so they
// align past the role's leading glyph (e.g. "❯ " -> indent "  "). When
// width <= 0 (pre-resize) the original text is returned untouched so
// callers don't have to special-case the boot path.
//
// muesli/reflow's wordwrap respects word boundaries and never breaks a
// single mega-word — a long URL or token would still overflow. Chain
// it with reflow's wrap (force-break at column) so the over-long
// fragments left by wordwrap also get split. wordwrap first to
// preserve word boundaries where possible; wrap second to mop up.
func wrapForChat(text string, width int, indent string) string {
	if width <= 0 {
		return text
	}
	wrapped := wrap.String(wordwrap.String(text, width), width)
	if indent == "" {
		return wrapped
	}
	lines := strings.Split(wrapped, "\n")
	for i := 1; i < len(lines); i++ {
		lines[i] = indent + lines[i]
	}
	return strings.Join(lines, "\n")
}

// lastTurnUsageFooter renders the most recent turn's usage as a small
// muted footer line. Empty when no tracker is wired or no turns yet.
func (m *Model) lastTurnUsageFooter() string {
	if m.usage == nil {
		return ""
	}
	last, ok := m.usage.Last()
	if !ok {
		return ""
	}
	line := fmt.Sprintf("↑%d in · ↓%d out · $%s", last.InputTokens, last.OutputTokens, formatCost(last.CostUSD))
	return m.styles.Footer.Render(line)
}

// refreshViewport re-renders the history into the viewport. If the user
// was already pinned to the bottom (the common "tail" position), scroll
// stays at the bottom as new content arrives. If they had scrolled up
// to read history, leave them where they were.
func (m *Model) refreshViewport() {
	atBottom := m.viewport.AtBottom()
	m.viewport.SetContent(m.renderHistory())
	if atBottom {
		m.viewport.GotoBottom()
	}
}

// reloadResult is what reloadFromDisk hands back to the model so the
// fresh state can be installed in one atomic update.
type reloadResult struct {
	Agent      *agent.Agent
	Memory     instruction.Loaded
	MCPServers []*mcp.Server
	Skills     skills.Skills
}

// renderMemoryInfo formats the loaded-memory provenance for /instruction.
func (m *Model) renderMemoryInfo() string {
	if m.memory.Empty() {
		return "No memory loaded.\n\nDrop AGENTS.md, CLAUDE.md, or GEMINI.md at the project root, or ~/.cogo/AGENTS.md for personal preferences."
	}
	var b strings.Builder
	b.WriteString("Memory loaded:\n")
	for _, s := range m.memory.Sources {
		marker := ""
		if s.Truncated {
			marker = " (truncated)"
		}
		b.WriteString("  ")
		b.WriteString(s.Scope)
		b.WriteString(": ")
		b.WriteString(s.Path)
		b.WriteString(" — ")
		b.WriteString(formatBytes(s.Bytes))
		b.WriteString(marker)
		b.WriteByte('\n')
	}
	return b.String()
}

// renderMCPInfo formats the configured MCP servers for /mcp.
// Each server is followed by an indented list of the tools it
// exposes (the namespaced names the agent actually sees).
func (m *Model) renderMCPInfo() string {
	if len(m.mcpServers) == 0 {
		return "No MCP servers configured. Drop a .agents/mcp.json describing servers (stdio or HTTP transport) to expose external tools to the agent."
	}
	var b strings.Builder
	b.WriteString("MCP servers:\n")
	for _, s := range m.mcpServers {
		b.WriteString("  ")
		b.WriteString(s.Name)
		b.WriteString(" — ")
		b.WriteString(s.Status)
		if s.Err != nil {
			b.WriteString(" (")
			b.WriteString(s.Err.Error())
			b.WriteString(")")
		}
		b.WriteByte('\n')
		switch {
		case s.Status != "ok":
			// Skip tool list for failed servers.
		case len(s.Tools) == 0:
			b.WriteString("      (server exposes no tools, or enumeration failed)\n")
		default:
			for _, t := range s.Tools {
				b.WriteString("      • ")
				b.WriteString(t)
				b.WriteByte('\n')
			}
		}
	}
	return b.String()
}

// renderSkillsInfo formats the discovered skills for /skills.
func (m *Model) renderSkillsInfo() string {
	if m.skills.Empty() {
		return "No skills discovered. Drop SKILL.md bundles under .agents/skills/<name>/ to expose them to the agent."
	}
	var b strings.Builder
	b.WriteString("Skills:\n")
	for _, info := range m.skills.Infos {
		b.WriteString("  ")
		b.WriteString(info.Name)
		if info.Description != "" {
			b.WriteString(" — ")
			b.WriteString(info.Description)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// renderStatsInfo formats the per-turn + total usage for /stats.
func (m *Model) renderStatsInfo() string {
	if m.usage == nil {
		return "Usage tracking not available."
	}
	tot := m.usage.Totals()
	if tot.Turns == 0 {
		return "No turns yet — try sending a prompt first."
	}
	var b strings.Builder
	b.WriteString("Session stats:\n")
	b.WriteString("  Turns:    ")
	b.WriteString(strconv.Itoa(tot.Turns))
	b.WriteByte('\n')
	b.WriteString("  Input:    ")
	b.WriteString(strconv.Itoa(tot.InputTokens))
	b.WriteString(" tokens\n")
	b.WriteString("  Output:   ")
	b.WriteString(strconv.Itoa(tot.OutputTokens))
	b.WriteString(" tokens\n")
	b.WriteString("  Cost:     $")
	b.WriteString(formatCost(tot.CostUSD))
	b.WriteByte('\n')
	b.WriteString("  Duration: ")
	b.WriteString(m.usage.Duration().Round(0).String())
	b.WriteByte('\n')
	b.WriteString("  Model:    ")
	b.WriteString(m.cfg.Model.Name)
	return b.String()
}

func formatBytes(n int) string {
	if n >= 1024 {
		return fmt.Sprintf("%d KiB", n/1024)
	}
	return fmt.Sprintf("%d B", n)
}

// formatCost renders c with 4 decimals, trimming trailing zeros so
// "$0.0019" stays compact and "$0.1500" becomes "$0.15".
func formatCost(c float64) string {
	s := fmt.Sprintf("%.4f", c)
	s = strings.TrimRight(s, "0")
	return strings.TrimRight(s, ".")
}
