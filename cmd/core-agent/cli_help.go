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
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/go-steer/core-agent/v3/pkg/config"
)

const (
	// planFirstDeprecationNotice is emitted when --plan-first is passed on argv (#1380).
	planFirstDeprecationNotice = "--plan-first is deprecated: use --plan-mode=required or --plan-mode=off (or --plan-mode=advisory to record plans without blocking tools)."

	// acceptEditsHyphenDeprecationNotice is emitted when --permission-mode=accept-edits is passed (#1380).
	acceptEditsHyphenDeprecationNotice = "--permission-mode=accept-edits is deprecated: use the canonical spelling --permission-mode=acceptEdits."
)

// resolvePermissionModeFlag validates and applies --permission-mode and
// --yolo to cfgMode and yolo (#1380).
//
// Canonical permission modes: ask, auto, acceptEdits, plan, allow, yolo.
// The hyphenated spelling "accept-edits" is accepted as a deprecated
// compatibility alias and normalized to "acceptEdits".
// Passing both --permission-mode and --yolo with different modes is a
// startup error.
func resolvePermissionModeFlag(cfgMode *string, yolo *bool, flagMode string, send func(string)) error {
	mode := strings.TrimSpace(flagMode)
	if mode == "accept-edits" {
		if send != nil {
			send(acceptEditsHyphenDeprecationNotice)
		}
		mode = config.PermissionModeAcceptEdits
	}
	if mode != "" {
		switch mode {
		case config.PermissionModeAsk,
			config.PermissionModeAuto,
			config.PermissionModeAcceptEdits,
			config.PermissionModePlan,
			config.PermissionModeAllow,
			config.PermissionModeYolo:
		default:
			return fmt.Errorf("invalid --permission-mode %q (want ask|auto|acceptEdits|plan|allow|yolo)", flagMode)
		}
		if yolo != nil && *yolo && mode != config.PermissionModeYolo {
			return fmt.Errorf("--permission-mode=%s contradicts --yolo (which means --permission-mode=yolo); pass one or the other", mode)
		}
		if cfgMode != nil {
			*cfgMode = mode
		}
		if yolo != nil && mode == config.PermissionModeYolo {
			*yolo = true
		}
		return nil
	}
	if yolo != nil && *yolo && cfgMode != nil {
		*cfgMode = config.PermissionModeYolo
	}
	return nil
}

// resolveSandboxFlags validates and applies --sandbox and --no-sandbox to
// sandboxCfg (#651).
func resolveSandboxFlags(sandboxCfg *config.SandboxConfig, sandboxFlag string, noSandbox bool) error {
	val := strings.ToLower(strings.TrimSpace(sandboxFlag))
	if noSandbox && val != "" && val != config.SandboxModeOff {
		return fmt.Errorf("--no-sandbox contradicts --sandbox=%s; pass one or the other", val)
	}
	if noSandbox {
		if sandboxCfg != nil {
			sandboxCfg.Mode = config.SandboxModeOff
		}
		return nil
	}
	if val == "" {
		return nil
	}
	switch val {
	case config.SandboxModeAuto:
		if sandboxCfg != nil {
			sandboxCfg.Mode = config.SandboxModeAuto
			sandboxCfg.Driver = config.SandboxDriverAuto
		}
	case config.SandboxModeEnforce:
		if sandboxCfg != nil {
			sandboxCfg.Mode = config.SandboxModeEnforce
			if sandboxCfg.Driver == "" || sandboxCfg.Driver == config.SandboxDriverPolicy {
				sandboxCfg.Driver = config.SandboxDriverAuto
			}
		}
	case config.SandboxModeOff:
		if sandboxCfg != nil {
			sandboxCfg.Mode = config.SandboxModeOff
		}
	case config.SandboxDriverPolicy:
		if sandboxCfg != nil {
			sandboxCfg.Mode = config.SandboxModeAuto
			sandboxCfg.Driver = config.SandboxDriverPolicy
		}
	case config.SandboxDriverBwrap, config.SandboxDriverSeatbelt, config.SandboxDriverContainer:
		if sandboxCfg != nil {
			if val == config.SandboxDriverContainer && strings.TrimSpace(sandboxCfg.ContainerImage) == "" {
				return fmt.Errorf("--sandbox=container requires safety.sandbox.container_image in config")
			}
			sandboxCfg.Mode = config.SandboxModeEnforce
			sandboxCfg.Driver = val
		}
	default:
		return fmt.Errorf("invalid --sandbox %q (want auto|enforce|off|policy|bwrap|seatbelt|container)", sandboxFlag)
	}
	return nil
}

// applyPermissionAndSandboxFlags resolves both --permission-mode / --yolo and
// --sandbox / --no-sandbox onto cfg (#1380, #651).
func applyPermissionAndSandboxFlags(cfg *config.Config, yolo *bool, guardrails guardrailOpts, send func(string)) error {
	if err := resolvePermissionModeFlag(&cfg.Permissions.Mode, yolo, guardrails.permissionMode, send); err != nil {
		return err
	}
	if strings.TrimSpace(guardrails.sandbox) != "" || guardrails.noSandbox {
		if err := resolveSandboxFlags(cfg.Safety.EnsureSandbox(), guardrails.sandbox, guardrails.noSandbox); err != nil {
			return err
		}
	}
	if cfg.Safety.SandboxConfig().Mode == config.SandboxModeOff && send != nil {
		send("warning: sandbox disabled (--no-sandbox / safety.sandbox.mode=off)")
	}
	return nil
}

// cliFlagDoc describes one row in the grouped --help output (#1380).
type cliFlagDoc struct {
	// Names lists the flag names on flag.CommandLine covered by this row
	// (e.g. []string{"c", "config"}).
	Names []string
	// Spec is the formatted left column (e.g. "-c, --config <path>").
	Spec string
	// Summary is the single-line description printed in --help.
	Summary string
	// Optional marks flags that may only be registered in certain builds/branches.
	Optional bool
}

// cliFlagGroup groups related flags under a section heading in --help.
type cliFlagGroup struct {
	Title string
	Flags []cliFlagDoc
}

var cliFlagGroups = []cliFlagGroup{
	{
		Title: "Session & Prompt",
		Flags: []cliFlagDoc{
			{Names: []string{"p"}, Spec: "-p <prompt>", Summary: "Run a single headless turn with <prompt> and exit"},
			{Names: []string{"i", "interactive-prompt"}, Spec: "-i, --interactive-prompt <prompt>", Summary: "Seed the first turn with <prompt>, then stay in the interactive REPL/TUI"},
			{Names: []string{"c", "config"}, Spec: "-c, --config <path>", Summary: "Config file path (default: discover .agents/config.json)"},
			{Names: []string{"agents-dir"}, Spec: "--agents-dir <dir>", Summary: "Directory MCP servers, skills, plans, and content roots resolve from"},
			{Names: []string{"task"}, Spec: "--task <class>", Summary: "Task profile preset: debug|implement|chat|research|review"},
			{Names: []string{"ask"}, Spec: "--ask <mode>", Summary: "Register the ask_user tool: off|stdin|auto (default: off)"},
			{Names: []string{"output-format"}, Spec: "--output-format <fmt>", Summary: "Headless (-p) stdout format: text|json|stream-json (default: text)"},
			{Names: []string{"q", "quiet"}, Spec: "-q, --quiet", Summary: "Suppress startup and turn-end summaries on stderr"},
			{Names: []string{"color"}, Spec: "--color <mode>", Summary: "ANSI color in streamed output: auto|always|never (default: auto)"},
			{Names: []string{"no-tui"}, Spec: "--no-tui", Summary: "Use the line-mode REPL instead of the full-screen Bubble Tea TUI"},
			{Names: []string{"no-repl"}, Spec: "--no-repl", Summary: "Run without a stdin REPL until SIGTERM/SIGINT (requires --attach-listen or --attach-unix-socket)"},
			{Names: []string{"session-db"}, Spec: "--session-db", Summary: "Persist sessions and audit log to SQLite"},
			{Names: []string{"session-db-path"}, Spec: "--session-db-path <path>", Summary: "Database file path when --session-db or attach mode is enabled"},
			{Names: []string{"continue"}, Spec: "--continue", Summary: "Continue the most recent session for this project (implies --session-db)"},
			{Names: []string{"resume"}, Spec: "--resume [id]", Summary: "Resume a specific session by ID, or pick interactively from project sessions"},
			{Names: []string{"append-system-prompt"}, Spec: "--append-system-prompt <text|@file>", Summary: "Append operator instructions to the assembled system prompt"},
			{Names: []string{"system-prompt-file"}, Spec: "--system-prompt-file <path>", Summary: "Replace the assembled system prompt wholesale with file contents"},
			{Names: nil, Spec: "--version", Summary: "Print build version and exit"},
		},
	},
	{
		Title: "Model & Caching",
		Flags: []cliFlagDoc{
			{Names: []string{"m", "model"}, Spec: "-m, --model <id>", Summary: "Override model.name from config"},
			{Names: []string{"provider"}, Spec: "--provider <name>", Summary: "Override model.provider (gemini|vertex|anthropic|anthropic-vertex|openai|...)"},
			{Names: []string{"no-context-cache"}, Spec: "--no-context-cache", Summary: "Disable Vertex explicit CachedContent prefix caching"},
			{Names: []string{"no-prompt-cache"}, Spec: "--no-prompt-cache", Summary: "Disable Anthropic prompt-cache breakpoints"},
			{Names: []string{"prompt-cache-ttl"}, Spec: "--prompt-cache-ttl <ttl>", Summary: "Anthropic prompt-cache TTL: 5m (default) or 1h"},
			{Names: []string{"no-pricing-refresh"}, Spec: "--no-pricing-refresh", Summary: "Skip the startup pricing-catalog refresh (for air-gapped or CI runs)"},
			{Names: []string{"script"}, Spec: "--script <path>", Summary: "JSONL script path for --provider=scripted"},
			{Names: []string{"script-strict"}, Spec: "--script-strict", Summary: "Assert incoming requests match recorded ones under --provider=scripted"},
			{Names: []string{"record-to"}, Spec: "--record-to <path>", Summary: "Record all LLM turns to a JSONL file for replay"},
		},
	},
	{
		Title: "Permissions & Safety",
		Flags: []cliFlagDoc{
			{Names: []string{"permission-mode"}, Spec: "--permission-mode <mode>", Summary: "Permission gate mode: ask|auto|acceptEdits|plan|allow|yolo"},
			{Names: []string{"yolo"}, Spec: "--yolo", Summary: "Shorthand for --permission-mode=yolo (bypass permission prompts)"},
			{Names: []string{"sandbox"}, Spec: "--sandbox <mode|driver>", Summary: "Execution sandbox mode or driver: auto|enforce|off|bwrap|seatbelt|container"},
			{Names: []string{"no-sandbox"}, Spec: "--no-sandbox", Summary: "Disable execution sandboxing and workspace write containment"},
			{Names: []string{"trust"}, Spec: "--trust", Summary: "Trust the workspace .agents/ directory and persist its hash"},
			{Names: []string{"plan-mode"}, Spec: "--plan-mode <mode>", Summary: "Plan-first gate mode: off|advisory|required"},
			{Names: []string{"approval-timeout"}, Spec: "--approval-timeout <dur>", Summary: "How long a permission prompt waits before denying (e.g. 15m, 0s)"},
			{Names: []string{"allow-path"}, Spec: "--allow-path <path>:<r|w|rw>", Summary: "Grant file access outside workspace/home roots (repeatable)"},
			{Names: []string{"allow-url-host"}, Spec: "--allow-url-host <hosts>", Summary: "Comma-separated host patterns appended to url_scope.allow"},
			{Names: []string{"watchdog"}, Spec: "--watchdog <mode>", Summary: "Behavioral loop watchdog mode: off|warn|feedback|enforce"},
			{Names: []string{"max-turn-cost-usd"}, Spec: "--max-turn-cost-usd <usd>", Summary: "Per-turn spend ceiling in USD (0 = disabled)"},
			{Names: []string{"max-session-cost-usd"}, Spec: "--max-session-cost-usd <usd>", Summary: "Session-wide spend ceiling in USD (default $10 unattended, 0 = disabled)"},
			{Names: []string{"small-tier-parent"}, Spec: "--small-tier-parent <mode>", Summary: "Action when interactive parent uses a small-tier model: warn|refuse|allow"},
			{Names: []string{"bash-search-gate"}, Spec: "--bash-search-gate <mode>", Summary: "Gate grep/find shell commands when native tools exist: enforce|warn|allow"},
		},
	},
	{
		Title: "Tools, Subagents & Context",
		Flags: []cliFlagDoc{
			{Names: []string{"no-builtin-tools"}, Spec: "--no-builtin-tools", Summary: "Disable the built-in tool suite"},
			{Names: []string{"disable-tools"}, Spec: "--disable-tools <list>", Summary: "Comma-separated built-in tools to disable (e.g. bash,write_file)"},
			{Names: []string{"enable-tools"}, Spec: "--enable-tools <list>", Summary: "Comma-separated built-in tools to re-enable after a --task profile"},
			{Names: []string{"agentic-tools"}, Spec: "--agentic-tools[=false]", Summary: "Register subtask-backed agentic_* tool wrappers (default: true)"},
			{Names: []string{"agentic-small-model"}, Spec: "--agentic-small-model <id>", Summary: "Small/cheap model ID used by agentic_* subtask wrappers"},
			{Names: []string{"no-background-agents"}, Spec: "--no-background-agents", Summary: "Disable spawn_agent and stop_agent background subagent tools"},
			{Names: []string{"subagent-sync-wait"}, Spec: "--subagent-sync-wait <dur>", Summary: "Max duration spawn_agent {wait: true} blocks parent turn (default: 5m)"},
			{Names: []string{"agents-content-dir"}, Spec: "--agents-content-dir <dir>", Summary: "Additional external instruction/skill root directory (repeatable)"},
			{Names: []string{"no-mcp-digest"}, Spec: "--no-mcp-digest", Summary: "Disable structural digest pruning of large MCP and survey tool responses"},
			{Names: []string{"mcp-agentic-wrap-llm"}, Spec: "--mcp-agentic-wrap-llm", Summary: "Enable LLM subagent summarization fallback for large MCP responses"},
			{Names: []string{"mcp-agentic-wrap-model"}, Spec: "--mcp-agentic-wrap-model <id>", Summary: "Model override for --mcp-agentic-wrap-llm"},
			{Names: []string{"no-compact"}, Spec: "--no-compact", Summary: "Disable automatic post-turn context-window compaction"},
			{Names: []string{"compaction-threshold"}, Spec: "--compaction-threshold <0..1>", Summary: "Context utilization (0,1) triggering automatic compaction"},
			{Names: []string{"checkpoint"}, Spec: "--checkpoint <mode>", Summary: "Task-boundary checkpoint trigger mode: model|operator|off"},
			{Names: []string{"memory-ref"}, Spec: "--memory-ref <uri>", Summary: "Long-term memory backend URI (memorybank://, sqlite://, inmemory://)"},
			{Names: []string{"agent-engine"}, Spec: "--agent-engine <resource>", Summary: "Vertex AI ReasoningEngine resource shared by session store and memory"},
			{Names: []string{"memory-namespace"}, Spec: "--memory-namespace <ns>", Summary: "Logical memory namespace partitioning scopes across environments"},
			{Names: []string{"memory-agent"}, Spec: "--memory-agent <id[:rw|ro]|off>", Summary: "Agent private memory mount ID and access mode"},
			{Names: []string{"memory-user"}, Spec: "--memory-user <user>", Summary: "Explicit user identity for user-partitioned memory scopes"},
			{Names: []string{"memory-shared"}, Spec: "--memory-shared <pool[:ro|rw]>", Summary: "Mount a named shared memory pool (default ro; repeatable)"},
			{Names: []string{"memory-preload"}, Spec: "--memory-preload <mode>", Summary: "Pre-turn memory recall mode: off|session_start|turn_start"},
			{Names: []string{"memory-consolidate"}, Spec: "--memory-consolidate <mode>", Summary: "Conversation memory extraction mode: off|turn_end|session_end"},
		},
	},
	{
		Title: "Remote Attach & Fleet",
		Flags: []cliFlagDoc{
			{Names: []string{"attach-listen"}, Spec: "--attach-listen <addr>", Summary: "Serve HTTP/SSE attach listener on <addr> (e.g. 127.0.0.1:7777)"},
			{Names: []string{"attach-unix-socket"}, Spec: "--attach-unix-socket <path>", Summary: "Serve attach listener on a Unix domain socket"},
			{Names: []string{"attach-token"}, Spec: "--attach-token <env-var>", Summary: "Env var name holding the bearer token required by attach clients"},
			{Names: []string{"attach-token-file"}, Spec: "--attach-token-file <path>", Summary: "Read attach bearer token from a 0600 file or pipe at startup"},
			{Names: []string{"attach-tls-cert"}, Spec: "--attach-tls-cert <path>", Summary: "TLS server certificate (PEM) for --attach-listen"},
			{Names: []string{"attach-tls-key"}, Spec: "--attach-tls-key <path>", Summary: "TLS server private key (PEM) for --attach-listen"},
			{Names: []string{"attach-client-ca"}, Spec: "--attach-client-ca <path>", Summary: "Client CA bundle (PEM) for mTLS verification"},
			{Names: []string{"attach-readonly"}, Spec: "--attach-readonly", Summary: "Disable mutating attach endpoints (POST /inject, /wake)"},
			{Names: []string{"attach-allow-unauthenticated-local"}, Spec: "--attach-allow-unauthenticated-local", Summary: "Allow unauthenticated loopback/Unix attach even with bash enabled"},
			{Names: []string{"attach-peer-hub"}, Spec: "--attach-peer-hub", Summary: "Enable fleet peer-discovery hub endpoints (/peers)"},
			{Names: []string{"attach-peer-state-file"}, Spec: "--attach-peer-state-file <path>", Summary: "Persist peer hub registry across restarts in a JSONL file"},
			{Names: []string{"attach-register-to"}, Spec: "--attach-register-to <url>", Summary: "Register this daemon with a remote attach peer hub"},
			{Names: []string{"attach-register-name"}, Spec: "--attach-register-name <name>", Summary: "Peer name published to the hub (default: hostname)"},
			{Names: []string{"attach-register-endpoint"}, Spec: "--attach-register-endpoint <url>", Summary: "Reachable endpoint URL published to the peer hub"},
			{Names: []string{"ui"}, Spec: "--ui", Summary: "Serve the embedded mast-web operator UI at /ui/* on the attach listener"},
			{Names: []string{"ui-dir"}, Spec: "--ui-dir <dir>", Summary: "Serve mast-web assets from a local directory (implies --ui)"},
		},
	},
	{
		Title: "Observability & Agent Card",
		Flags: []cliFlagDoc{
			{Names: []string{"log-file"}, Spec: "--log-file <path>", Summary: "Mirror stderr diagnostics to <path> (e.g. /tmp/core-agent.log)"},
			{Names: []string{"metrics-addr"}, Spec: "--metrics-addr <addr>", Summary: "Prometheus metrics scrape bind address (default: :9464 when enabled)"},
			{Names: []string{"agent-card-config"}, Spec: "--agent-card-config <path>", Summary: "Path to agent-card.json (or '-' to disable file lookup)"},
			{Names: []string{"agent-card-name"}, Spec: "--agent-card-name <name>", Summary: "Override name in /.well-known/agent-card.json"},
			{Names: []string{"agent-card-description"}, Spec: "--agent-card-description <text>", Summary: "Override description in /.well-known/agent-card.json"},
			{Names: []string{"agent-card-external-url"}, Spec: "--agent-card-external-url <url>", Summary: "Override canonical url in /.well-known/agent-card.json"},
			{Names: []string{"agent-card-version"}, Spec: "--agent-card-version <ver>", Summary: "Override version in /.well-known/agent-card.json"},
			{Names: []string{"agent-card-provider-org"}, Spec: "--agent-card-provider-org <org>", Summary: "Override provider.organization in /.well-known/agent-card.json"},
			{Names: []string{"agent-card-provider-url"}, Spec: "--agent-card-provider-url <url>", Summary: "Override provider.url in /.well-known/agent-card.json"},
			{Names: []string{"agent-card-docs-url"}, Spec: "--agent-card-docs-url <url>", Summary: "Override documentationUrl in /.well-known/agent-card.json"},
		},
	},
	{
		Title: "Deprecated",
		Flags: []cliFlagDoc{
			{Names: []string{"plan-first"}, Spec: "--plan-first", Summary: "Deprecated: use --plan-mode=required or --plan-mode=off"},
			{Names: []string{"no-checkpoint"}, Spec: "--no-checkpoint", Summary: "Deprecated: use --checkpoint=off or --checkpoint=operator"},
		},
	},
}

// writeCLIUsage writes the grouped CLI help output, including
// subcommands and --version, to w (#1380).
func writeCLIUsage(w io.Writer, fs *flag.FlagSet) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  core-agent [flags]")
	fmt.Fprintln(w, "  core-agent <subcommand> [args]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Subcommands:")
	fmt.Fprintln(w, "  attach <url>               Attach an interactive TUI/REPL client to a running daemon")
	fmt.Fprintln(w, "  ls <url>                   List active sessions on a running attach daemon")
	fmt.Fprintln(w, "  auth hash-token            Hash a bearer token from stdin for multi-session users_file")
	fmt.Fprintln(w, "  config init|show|validate  Scaffold, inspect, or validate .agents/config.json")
	fmt.Fprintln(w, "  trust [list|revoke <dir>]  Trust, list, or revoke repo-supplied .agents/ directories")

	covered := make(map[string]bool)
	const specColWidth = 36
	for _, group := range cliFlagGroups {
		var rows []cliFlagDoc
		for _, f := range group.Flags {
			for _, name := range f.Names {
				covered[name] = true
			}
			if f.Optional && (fs == nil || (len(f.Names) > 0 && fs.Lookup(f.Names[0]) == nil)) {
				continue
			}
			rows = append(rows, f)
		}
		if len(rows) == 0 {
			continue
		}
		fmt.Fprintf(w, "\n%s:\n", group.Title)
		for _, f := range rows {
			if len(f.Spec) >= specColWidth {
				fmt.Fprintf(w, "  %s\n  %s%s\n", f.Spec, strings.Repeat(" ", specColWidth+2), f.Summary)
			} else {
				fmt.Fprintf(w, "  %-*s  %s\n", specColWidth, f.Spec, f.Summary)
			}
		}
	}

	if fs != nil {
		var ungrouped []*flag.Flag
		fs.VisitAll(func(fl *flag.Flag) {
			if !covered[fl.Name] {
				ungrouped = append(ungrouped, fl)
			}
		})
		if len(ungrouped) > 0 {
			fmt.Fprintln(w, "\nOther Flags:")
			for _, fl := range ungrouped {
				spec := "--" + fl.Name
				if len(fl.Name) == 1 {
					spec = "-" + fl.Name
				}
				line := strings.SplitN(strings.TrimSpace(fl.Usage), "\n", 2)[0]
				fmt.Fprintf(w, "  %-*s  %s\n", specColWidth, spec, line)
			}
		}
	}
	fmt.Fprintln(w, "\nRun 'core-agent <subcommand> --help' for subcommand flags.")
}
