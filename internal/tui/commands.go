// Copyright 2026 The Cogo Authors.
// SPDX-License-Identifier: Apache-2.0

package tui

import "strings"

// SlashAction identifies the slash command typed by the user.
type SlashAction string

const (
	SlashHelp        SlashAction = "help"
	SlashClear       SlashAction = "clear"
	SlashQuit        SlashAction = "quit"
	SlashMemory      SlashAction = "memory"
	SlashStats       SlashAction = "stats"
	SlashModel       SlashAction = "model"
	SlashMCP         SlashAction = "mcp"
	SlashSkills      SlashAction = "skills"
	SlashTools       SlashAction = "tools"
	SlashInterrupt   SlashAction = "interrupt"
	SlashPricing     SlashAction = "pricing"
	SlashReload      SlashAction = "reload"
	SlashMouse       SlashAction = "mouse"
	SlashPermissions SlashAction = "permissions"
	SlashAllow       SlashAction = "allow"
	SlashDeny        SlashAction = "deny"
	SlashBTW         SlashAction = "btw"
	SlashSubagent    SlashAction = "subagent"
	SlashCompact     SlashAction = "compact"
	SlashDone        SlashAction = "done"
	SlashUnknown     SlashAction = "unknown"
)

var slashAliases = map[string]SlashAction{
	"help":        SlashHelp,
	"?":           SlashHelp,
	"clear":       SlashClear,
	"quit":        SlashQuit,
	"exit":        SlashQuit,
	"q":           SlashQuit,
	"memory":      SlashMemory,
	"stats":       SlashStats,
	"model":       SlashModel,
	"models":      SlashModel,
	"mcp":         SlashMCP,
	"skills":      SlashSkills,
	"tools":       SlashTools,
	"interrupt":   SlashInterrupt,
	"int":         SlashInterrupt,
	"pricing":     SlashPricing,
	"reload":      SlashReload,
	"mouse":       SlashMouse,
	"permissions": SlashPermissions,
	"perms":       SlashPermissions,
	"allow":       SlashAllow,
	"deny":        SlashDeny,
	"btw":         SlashBTW,
	"by-the-way":  SlashBTW,
	"subagent":    SlashSubagent,
	"sub":         SlashSubagent,
	"compact":     SlashCompact,
	"summarize":   SlashCompact,
	"done":        SlashDone,
	"checkpoint":  SlashDone,
}

// ParseSlash inspects input. If it looks like a slash command (leading
// `/` after trimming whitespace), returns the recognized action, the
// raw command name (without leading `/`, as the user typed it for error
// messages), the args (everything after the command token, trimmed),
// and isSlash=true. Otherwise returns ("", "", "", false).
//
// Unrecognized slash commands return SlashUnknown so callers can show a
// friendly "unknown command" message in the chat without leaking input
// to the model.
func ParseSlash(input string) (action SlashAction, command, args string, isSlash bool) {
	trimmed := strings.TrimSpace(input)
	if !strings.HasPrefix(trimmed, "/") {
		return "", "", "", false
	}
	body := strings.TrimSpace(trimmed[1:])
	if body == "" {
		// Bare "/" — treat as unknown.
		return SlashUnknown, "", "", true
	}
	fields := strings.Fields(body)
	cmd := strings.ToLower(fields[0])
	args = ""
	if len(fields) > 1 {
		args = strings.TrimSpace(strings.TrimPrefix(body, fields[0]))
	}
	if a, ok := slashAliases[cmd]; ok {
		return a, cmd, args, true
	}
	return SlashUnknown, cmd, args, true
}

// HelpText returns the multi-line help message printed by /help.
func HelpText() string {
	return strings.Join([]string{
		"core-agent — interactive mode",
		"",
		"Type a message and press Enter to send.",
		"Shift+Enter inserts a newline (multi-line input).",
		"",
		"Type / at the start of an empty prompt to open the slash-command palette.",
		"Type @ to open the file picker — selecting a file inserts @path/to/file,",
		"and core-agent inlines the file's contents when you send the message.",
		"",
		"Slash commands:",
		"  /help       show this help",
		"  /clear      clear chat history (asks for confirmation)",
		"  /quit       exit Cogo (alias: /exit)",
		"  /memory     show which AGENTS.md/CLAUDE.md/GEMINI.md files were loaded",
		"  /stats      show per-turn token use and session totals",
		"  /model      open the model picker (alias: /models)",
		"  /model <id> switch to <id> directly without the picker",
		"  /mcp        show configured MCP servers and their status",
		"  /skills     show discovered skills",
		"  /tools      list every tool the agent can call (built-in + MCP + skills)",
		"  /interrupt  cancel the in-flight model turn (alias: /int; also bound to Esc on empty input)",
		"  /btw <question>  ask a quick side question — sees full history, runs in parallel,",
		"              no tools, dismissible overlay (alias: /by-the-way; never enters history)",
		"  /subagent <goal>  spawn a background subagent directly without going through the main agent",
		"              (alias: /sub). Optional flags: --name=<id>, --prompt=<system_prompt>,",
		"              --tools=<csv>, --extras=<csv>, --max-turns=<n>, --max-cost=<usd>,",
		"              --max-wallclock=<duration>, --scheduler=<default|sleep|exit_on_defer|none>",
		"  /compact [focus]  summarize the conversation so far into a single dense handover and",
		"              slice prior events from future turns (alias: /summarize). Optional focus",
		"              hint biases the summarizer; auto-fires at 85% context-window utilization.",
		"  /done [note]  write a task-boundary checkpoint and slice prior events from future",
		"              turns (alias: /checkpoint). Use at natural task boundaries — finished",
		"              a feature, completed a review, etc. The model can also self-trigger via",
		"              its mark_task_done tool when it judges a task complete.",
		"  /pricing refresh        force-refresh the pricing catalog from LiteLLM",
		"  /pricing set <model> <input/M> <output/M>   set rates for one model (writes to ~/.core-agent/pricing.json manual section)",
		"  /reload     re-read .agents/ from disk (mcp.json, skills/, AGENTS.md, config.json)",
		"  /mouse      toggle mouse-wheel scrolling (or /mouse on|off)",
		"  /permissions  review session approvals + add recommended allowlist entries (alias: /perms)",
		"  /permissions list  show current allow/deny patterns and enabled built-in bundles",
		"  /allow <pattern>   append a pattern to permissions.allow and apply now (e.g. /allow bash:git *)",
		"  /allow bundle:<name>  enable a built-in bundle (dev_tools, core_agent_tools)",
		"  /deny  <pattern>   append a pattern to permissions.deny and apply now",
		"",
		"Keys:",
		"  PgUp/PgDn   scroll chat history",
		"  ↑/↓         recall previous prompts when input is empty",
		"              (cursor movement in the textarea otherwise)",
		"  Tab         in the slash/file palette: complete the highlighted",
		"              item without submitting (slash) or insert it (file)",
		"  Esc         dismiss the palette / permission prompt",
		"  Ctrl+C      cancel current turn (or exit when idle)",
		"  Ctrl+L      reset viewport scroll (history preserved)",
		"  Ctrl+U      clear the input box",
		"",
		"Mouse: wheel scrolls the chat when capture is on (the default).",
		"To select text while capture is on, hold your terminal's bypass",
		"modifier while dragging:",
		"  Shift   most Linux/Windows terminals, WezTerm, Alacritty, Kitty",
		"  Option  iTerm2, Ghostty, VS Code's integrated terminal",
		"  Fn      Apple Terminal.app",
		"VS Code on macOS also needs `terminal.integrated.macOptionClickForcesSelection: true`",
		"in settings.json before Option-drag will bypass capture.",
		"Or run /mouse off to give plain drag back to the terminal.",
	}, "\n")
}
