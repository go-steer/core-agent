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

// Command core-agent is a thin CLI wrapper around the core-agent
// library. With -p PROMPT it runs a single turn and exits; without
// -p it drops into a stdin REPL that preserves conversation history
// across turns.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/glebarez/sqlite"
	"go.opentelemetry.io/otel"
	"golang.org/x/term"
	adktool "google.golang.org/adk/tool"

	"github.com/go-steer/core-agent/v2/internal/version"
	"github.com/go-steer/core-agent/v2/internal/webui"
	"github.com/go-steer/core-agent/v2/pkg/agent"
	"github.com/go-steer/core-agent/v2/pkg/agent/background"
	"github.com/go-steer/core-agent/v2/pkg/agentenv"
	"github.com/go-steer/core-agent/v2/pkg/attach"
	"github.com/go-steer/core-agent/v2/pkg/attachadapter"
	"github.com/go-steer/core-agent/v2/pkg/compose"
	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/digest"
	"github.com/go-steer/core-agent/v2/pkg/eventlog"
	"github.com/go-steer/core-agent/v2/pkg/hooks"
	"github.com/go-steer/core-agent/v2/pkg/instruction"
	"github.com/go-steer/core-agent/v2/pkg/mcp"
	"github.com/go-steer/core-agent/v2/pkg/models"
	_ "github.com/go-steer/core-agent/v2/pkg/models/anthropic"
	"github.com/go-steer/core-agent/v2/pkg/models/gemini"
	_ "github.com/go-steer/core-agent/v2/pkg/models/mock"
	"github.com/go-steer/core-agent/v2/pkg/modeltier"
	"github.com/go-steer/core-agent/v2/pkg/permissions"
	"github.com/go-steer/core-agent/v2/pkg/pricing"
	"github.com/go-steer/core-agent/v2/pkg/recording"
	"github.com/go-steer/core-agent/v2/pkg/runner"
	"github.com/go-steer/core-agent/v2/pkg/skills"
	"github.com/go-steer/core-agent/v2/pkg/taskclass"
	"github.com/go-steer/core-agent/v2/pkg/telemetry"
	"github.com/go-steer/core-agent/v2/pkg/tools"
	"github.com/go-steer/core-agent/v2/pkg/transcript"
	"github.com/go-steer/core-agent/v2/pkg/usage"
	"github.com/go-steer/core-agent/v2/pkg/watchdog"
)

func main() {
	// Subcommand dispatch: `core-agent attach <url>` and
	// `core-agent ls <url>` are entirely separate from the agent-run
	// flow. Peel them off before flag.Parse so their own flag sets
	// don't collide with the main flag set's --p / --c / etc.
	if len(os.Args) >= 2 {
		switch os.Args[1] {
		case "attach":
			os.Exit(runAttachSubcommand(os.Args[2:]))
		case "ls":
			os.Exit(runLsSubcommand(os.Args[2:]))
		}
	}

	// --version short-circuits before flag.Parse so the operator
	// doesn't have to satisfy any other required flags to read it.
	// Matches the convention every standard CLI uses (gh, kubectl,
	// go itself).
	for _, a := range os.Args[1:] {
		if a == "--version" || a == "-version" {
			fmt.Println(version.String("core-agent"))
			return
		}
	}

	prompt := flag.String("p", "", "single prompt; runs one turn and exits (REPL otherwise)")

	// `-i` seeds the first turn of an INTERACTIVE session — issue #291.
	// Both short (`-i`) and long (`--interactive-prompt`) forms bind to
	// the same variable. Mutually exclusive with `-p` (single-shot
	// headless) and incompatible with `--no-repl` (attach-only daemon
	// has no operator to stay interactive for); both combinations are
	// rejected at run() entry with a config error.
	var initialPromptVal string
	flag.StringVar(&initialPromptVal, "i", "", "initial prompt; runs one turn then stays in the interactive REPL/TUI. Mutually exclusive with -p.")
	flag.StringVar(&initialPromptVal, "interactive-prompt", "", "long-form alias for -i — same behavior")
	initialPrompt := &initialPromptVal

	// `-c` (short) and `--config` (long) both bind to the same
	// variable so operators can write manifests using whichever form
	// matches their muscle memory. Every other flag on this CLI uses
	// long form, so the historical -c-only shape was a footgun (a
	// distroless-container Deployment with args: ["--config=..."]
	// exits at flag-parse with "flag provided but not defined: -config"
	// — hit live during the v2.6 GKE-troubleshoot demo drive, see
	// go-steer/core-agent#209). If both are given, the last on argv
	// wins (Go flag package semantics).
	var cfgPathVal string
	flag.StringVar(&cfgPathVal, "c", "", "config file path (default: discover .agents/config.json)")
	flag.StringVar(&cfgPathVal, "config", "", "long-form alias for -c — same behavior")
	cfgPath := &cfgPathVal
	// -m / --model bind to the same var (same alias pattern as -c/--config,
	// issue #209). Several operator-facing messages suggest `--model ...`
	// (e.g. the small-tier-parent warning), so the long form must exist or
	// copy-pasting them fails with "flag provided but not defined: -model".
	var modelOverrideVal string
	flag.StringVar(&modelOverrideVal, "m", "", "override model name from config")
	flag.StringVar(&modelOverrideVal, "model", "", "long-form alias for -m — same behavior")
	modelOverride := &modelOverrideVal
	providerOverride := flag.String("provider", "", "override model.provider (gemini|vertex|anthropic|anthropic-vertex|echo|scripted)")
	noBuiltinTools := flag.Bool("no-builtin-tools", false, "disable the built-in tool suite ("+strings.Join(tools.BuiltinToolNames(), ", ")+")")
	disableTools := flag.String("disable-tools", "", "comma-separated list of built-in tools to disable (e.g. bash,write_file). Composes with cfg.tools.disable; ignored when --no-builtin-tools is set.")
	enableTools := flag.String("enable-tools", "", "comma-separated list of built-in tools to add back after a --task profile dropped them (e.g. --task=debug --enable-tools=bash). Cancels the profile's opinion only: it cannot re-enable a tool you turned off in cfg.tools.disable or --disable-tools, and asking for that combination is an error rather than a silent win for either side. Naming a tool the profile never dropped is a no-op.")
	planFirst := flag.Bool("plan-first", false, "require the model to call record_plan before any mutating tool call (write_file / edit_file / delete_file / bash / fetch_url / spawn_agent). CLI mirror of permissions.require_plan_artifact, and the way to opt OUT of a task profile that turns the gate on: --task=debug --plan-first=false. Needs a .agents/ directory to persist plans into; without one record_plan cannot register and the gate would deny every mutating call with no way to clear it.")
	scriptPath := flag.String("script", "", "JSONL transcript for --provider=scripted (overrides cfg.mock.script)")
	scriptStrict := flag.Bool("script-strict", false, "scripted: assert each incoming request matches the recorded one (overrides cfg.mock.strict)")
	recordTo := flag.String("record-to", "", "write a JSONL recording of all LLM turns to this path (overrides cfg.mock.record)")
	color := flag.String("color", "auto", "ANSI color in streamed output: auto|always|never (auto = TTY-detect on stdout)")
	// Default "" (not "off") so run() can tell "operator didn't pass --ask"
	// from an explicit --ask=off. A task profile's ask mode only applies to
	// the former; "" is treated as "off" everywhere downstream.
	ask := flag.String("ask", "", "register an ask_user tool the model can call when its instructions tell it to ask: off|stdin|auto (auto = stdin if interactive, refuse otherwise). Empty (default) behaves as off.")
	sessionDB := flag.Bool("session-db", false, "persist sessions + audit log to a durable database (default off; in-memory)")
	sessionDBPath := flag.String("session-db-path", "", "override the database path used when --session-db is set (default: ~/.<binary>/sessions.db)")
	yolo := flag.Bool("yolo", false, "bypass the permissions gate entirely (every tool call runs without approval). Equivalent to permissions.mode=\"yolo\" in config.")
	noBackgroundAgents := flag.Bool("no-background-agents", false, "disable the spawn_agent / stop_agent tools (model can't spawn background subagents). Default: enabled.")
	allowURLHost := flag.String("allow-url-host", "", "comma-separated host patterns appended to url_scope.allow for the fetch_url tool (e.g. \"github.com,*.googleapis.com\"). HTTPS only unless the pattern carries an http:// prefix. Disable the tool entirely with --disable-tools=fetch_url.")
	var allowPathEntries []config.PathScopeAllowEntry
	var contentDirEntries []string
	flag.Func("allow-path", "grant file access to a path tree outside the project + user-home roots, e.g. --allow-path /home/me/sibling-repo:rw (repeatable). Explicit access is required: r, w, or rw (long forms read/write/readwrite accepted). Skip the permission prompt for matching paths; unmatched paths still prompt.", func(s string) error {
		e, err := parseAllowPathSpec(s)
		if err != nil {
			return err
		}
		allowPathEntries = append(allowPathEntries, e)
		return nil
	})
	flag.Func("agents-content-dir", "trust an external directory as an additional instruction/skill scope, so an unmodified external agent tree (e.g. a kube-agents checkout) can be consumed without vendoring it, e.g. --agents-content-dir ../kube-agents/agents/platform (repeatable). Relative paths resolve against the agents dir. Composes with config content_roots; precedence is project > content_roots (listed order) > home-agents > user. See docs/external-content-root-design.md.", func(s string) error {
		if strings.TrimSpace(s) == "" {
			return fmt.Errorf("--agents-content-dir requires a non-empty path")
		}
		contentDirEntries = append(contentDirEntries, s)
		return nil
	})
	attachListen := flag.String("attach-listen", "", "enable attach-mode HTTP listener on this address (e.g. 127.0.0.1:7777). Requires --session-db. Non-loopback binds (:7777, 0.0.0.0:7777, ...) refuse to start without authentication — set --attach-token (or mTLS / enforced multi-session auth).")
	attachUnixSocket := flag.String("attach-unix-socket", "", "enable attach-mode on a Unix socket at this path. Mutually exclusive with --attach-listen.")
	attachTLSCert := flag.String("attach-tls-cert", "", "TLS server certificate (PEM) for --attach-listen. Pair with --attach-tls-key.")
	attachTLSKey := flag.String("attach-tls-key", "", "TLS server key (PEM) for --attach-listen.")
	attachClientCA := flag.String("attach-client-ca", "", "CA PEM for client-cert verification (mTLS). When set, clients must present a cert signed by this CA.")
	attachTokenEnv := flag.String("attach-token", "", "env var name holding the bearer token clients must present in Authorization: Bearer <token>. Empty disables bearer-token auth.")
	attachReadonly := flag.Bool("attach-readonly", false, "attach-mode: disable POST /inject and /wake. Read endpoints (GET /sessions, GET /events) remain open.")
	attachPeerHub := flag.Bool("attach-peer-hub", false, "enable peer-registration endpoints (POST/GET /peers + heartbeat) on the attach listener — this agent becomes a discovery hub for other peers.")
	attachRegisterTo := flag.String("attach-register-to", "", "register this agent with a remote attach hub at this URL (e.g. https://hub.default.svc:7777). Heartbeats automatically. Requires --attach-listen so the hub records a reachable endpoint.")
	attachRegisterName := flag.String("attach-register-name", "", "name to register with the hub. Defaults to hostname.")
	attachRegisterEndpoint := flag.String("attach-register-endpoint", "", "endpoint to publish to the hub (e.g. https://${POD_IP}:7777). Required when --attach-register-to is set; this agent's own --attach-listen value is NOT used since it may bind 0.0.0.0 and the hub can't reach that.")
	attachUI := flag.Bool("ui", false, "serve the mast-web operator UI at /ui/* on the attach listener. Requires --attach-listen. Assets come from the pinned mast-web release embedded into this binary at build time (see .mast-web-version + dev/tools/fetch-mast-web); use --ui-dir to override with a local checkout for development.")
	attachUIDir := flag.String("ui-dir", "", "serve mast-web assets from this filesystem directory instead of the embedded bundle. For local-dev iteration against a checked-out mast-web repo. Implies --ui.")
	noREPL := flag.Bool("no-repl", false, "skip the stdin REPL — run until ctx cancellation (SIGTERM / SIGINT). Useful for attach-only daemons (e.g. spawned by core-agent-tui --local) where the operator drives the agent over attach-mode and stdin is /dev/null. Requires --attach-listen or --attach-unix-socket.")
	noTUI := flag.Bool("no-tui", false, "skip the in-process bubble-tea TUI even when stdin is a terminal — falls back to the line-mode REPL (or whatever else --no-repl / -p select). Use for scripts or shells where the TUI's raw-mode takeover is disruptive. Equivalent to forcing the pre-v2 default behavior.")
	noPricingRefresh := flag.Bool("no-pricing-refresh", false, "skip the daily pricing-catalog refresh from LiteLLM at startup. Use for air-gapped pods, CI runs, or any environment without outbound network. Overrides cfg.pricing.refresh.")
	appendSystemPrompt := flag.String("append-system-prompt", "", "text appended to the assembled system prompt as an operator layer (layer 5); pass @<path> to read the text from a file. The harness contract and mode overlay stay intact underneath — this is the encouraged customization path. Beats config agent.append_system_prompt. (#459)")
	systemPromptFile := flag.String("system-prompt-file", "", "path to a file whose contents REPLACE the assembled system prompt wholesale. You lose the harness contract (compaction summaries arrive unexplained; tool-use degradation is on you) — prefer --append-system-prompt. Beats config agent.system_prompt_file. (#459)")
	noCompact := flag.Bool("no-compact", false, "disable automatic context-window compaction. /compact slash still works for manual summarization, but the post-turn threshold trigger is off. Use when running headless against a model whose window is huge enough that compaction would never fire anyway, or when debugging an issue where you don't want history rewrites in play.")
	compactionThreshold := flag.Float64("compaction-threshold", 0, "context-window utilization (0,1) at which automatic compaction fires, overriding cfg.compaction.threshold and any task-profile value. 0 (default) = unset; the per-tier substrate defaults apply. Ignored when --no-compact is set.")
	noCheckpoint := flag.Bool("no-checkpoint", false, "disable task-boundary checkpoints. /done slash + the model-facing mark_task_done tool are both removed. Use when running headless where the model shouldn't self-signal task completion, or when debugging an issue where you don't want auto-slicing in play.")
	taskClass := flag.String("task", "", "operator-declared task class — picks a bundle of defaults (model tier, compaction threshold, agentic-tools posture, ask mode) tuned for the kind of work being done. One of: debug, implement, chat, research, review. Empty = no task class applied (substrate defaults). Explicit flags (--model, --ask, etc.) always win over the task profile. Per docs/model-selection-design.md / issue #123. Config-file equivalent: session.task_class.")
	maxTurnCostUSD := flag.Float64("max-turn-cost-usd", 0, "per-turn spend ceiling in USD. When a single conversation turn's cumulative cost (across all model calls + subtask costs) meets or exceeds this value, the agent emits a structured turn-error (kind=cost_ceiling) and refuses new turns until the operator resets it (/guardrail reset in the TUI, or POST /sessions/{id}/guardrails/reset over attach). 0 = disabled (default). Defense against runaway tool-loops within one turn (e.g. issue #144). Pairs with --max-session-cost-usd; either or both can be set. Overrides config.agent.max_turn_cost_usd when set.")
	maxSessionCostUSD := flag.Float64("max-session-cost-usd", 0, fmt.Sprintf("session-level spend ceiling in USD. Cumulative across every turn including subtasks; same trip + refuse behavior as --max-turn-cost-usd. Useful for long-running autonomous deploys where per-turn cost is reasonable but the session total adds up. Overrides config.agent.max_session_cost_usd when set, including an explicit 0. When neither is set, unattended runs (-p, --no-repl, or a non-TTY stdin) default to $%.2f and interactive runs default to disabled (#642); pass --max-session-cost-usd=0 to opt an unattended run back out.", DefaultUnattendedSessionCostUSD))
	smallTierParent := flag.String("small-tier-parent", "", "what to do when an interactive session starts on a small-tier parent model (Flash/Haiku-class). One of warn|refuse|allow. warn (default when unset) logs a one-line operator notice but proceeds; refuse exits with a config-error code; allow suppresses the check entirely. Skipped regardless when -p (one-shot), --yolo, or the model's tier doesn't classify. Per docs/model-selection-design.md / issue #121. Config-file equivalent: safety.small_tier_parent.")
	watchdogMode := flag.String("watchdog", "", "behavioral watchdog mode (#123 PR 2). A ladder — each mode includes the one before it. 'warn' = observe tool-call stream + log structured alerts to the operator when a runaway pattern is detected (e.g. 5 consecutive identical tool calls — the read_file loop from #144). 'feedback' = same, plus the observation is injected into the model's next-turn context as a '[watchdog]' block, so the party actually making the looping call finds out about it (#159); a correction, not a backstop — nothing halts a model that reads it and loops anyway. 'enforce' = all of that, but a runaway also trips a turn-error (kind=watchdog) and the agent refuses new turns until the operator resets it (/guardrail reset in the TUI, or POST /sessions/{id}/guardrails/reset over attach) (#623 — the hard backstop against tool loops an auto-continue re-drive would otherwise re-issue). 'off' = no observation. Empty (default) resolves per mode: 'enforce' for unattended runs (-p, --no-repl, or a non-TTY stdin — nobody is reading the warn-mode log there) and 'warn' for interactive REPL/TUI runs (#642). One signal ships today (repeated-tool-call); future modes (prompt, auto) and additional signals (tools-without-text, files-not-touched) are deferred per the design doc. Config-file equivalent: safety.watchdog.")
	bashSearchGate := flag.String("bash-search-gate", "", "what to do when the model runs a search-shaped shell command (grep/egrep/fgrep/rgrep/rg/ag/ack/fd/find) while the native grep/glob tools are registered (#158). 'enforce' (default) refuses the call with a structured error naming the native replacement — bash-as-grep is a training prior strong enough that the existing 'PREFERRED over bash grep' tool descriptions bounce off it (measured: one Gemini variant picked bash for search 15/27 times anyway), and a refusal is the only feedback the model gets at the moment it makes the wrong choice. 'warn' runs the command but attaches the same advice to the tool result. 'allow' disables the check. Piping into a search binary is never gated ('go test ./... | grep -v ok' filters a stream, which the native tool cannot do), and 'find' with an action predicate (-delete, -exec, ...) is a file operation rather than a lookup, so it passes. Tests, builds, git and every other bash use are untouched — this is the surgical version of --disable-tools=bash. Config-file equivalent: safety.bash_search_gate.")
	agenticTools := flag.Bool("agentic-tools", true, "register the agentic tool wrappers (agentic_read_file, agentic_fetch_url, agentic_grep, agentic_research) that route through a subtask so only the digest enters the parent's context (docs/context-management-design.md Mechanism B). On by default since v2.1; pass --agentic-tools=false to register only the bare tools.")
	agenticSmallModel := flag.String("agentic-small-model", "", "small/cheap model ID the agentic_* wrappers should route subtasks to (e.g. gemini-3.5-flash-lite, claude-haiku-4-5). When empty, the provider's cheap-tier default is used (gemini-3.5-flash-lite for Gemini/Vertex, claude-haiku-4-5 for Anthropic); providers without a cheap tier (echo, scripted) fall through to inheriting the parent's model. Requires --agentic-tools.")
	noMCPDigest := flag.Bool("no-mcp-digest", false, "disable the structural pkg/digest wrap around MCP tool responses (docs/digest-design.md). Default: enabled. When on, JSON-shaped MCP responses get a deterministic prune (identifier keys preserved, long strings truncated, arrays collapsed head+tail) before reaching the parent context; prose passthroughs are bounded. Also registers retrieve_raw as a built-in tool so the model can fetch back the un-digested payload when a digest looks suspicious. Kill switch for demos / debugging; leave on for production. Also gated per-project by cfg.MCP.AgenticWrap and per-server by mcp.json's agentic_never.")
	mcpAgenticWrapLLM := flag.Bool("mcp-agentic-wrap-llm", false, "enable the LLM subagent second-chance path for MCP responses the structural pruner can't reduce below threshold (docs/agentic-mcp-design.md #223). Default off — opt-in until the operator has confirmed the cost trade-off works for their MCP surface. Layered on top of --no-mcp-digest: structural runs first regardless, and the LLM subagent only fires when structural leaves the response above threshold. Config-file equivalent: mcp.json's agentic_wrap_llm.")
	mcpAgenticWrapModel := flag.String("mcp-agentic-wrap-model", "", "MCP-specific small-model override for the --mcp-agentic-wrap-llm subagent. When empty, falls through to --agentic-small-model, then to the provider's cheap-tier default. Motivation: MCP responses can be shaped differently enough from built-in-tool wrappers that one tier works well for one surface but not the other. Requires --mcp-agentic-wrap-llm. Config-file equivalent: mcp.json's agentic_wrap_model.")
	noContextCache := flag.Bool("no-context-cache", false, "disable Vertex explicit context caching for the stable request prefix (system instruction + tools). Default: enabled on Vertex. When on, the daemon creates a CachedContent resource after turn 1 and stamps it onto every subsequent GenerateContent call so the prefix bills at ~10%% of the input rate. Kill switch for demos / debugging Vertex issues; leave on for production. See docs/vertex-context-caching-design.md. Also gated per-project by cfg.Model.Vertex.ContextCache.enabled.")

	// Agent-card discovery (docs/agent-card-design.md). All optional —
	// either the .agents/agent-card.json file or the CLI flags must
	// supply description + external_url to enable the endpoint.
	agentCardConfigPath := flag.String("agent-card-config", "", "path to the agent-card JSON file (default: .agents/agent-card.json under the project root). Disables the file lookup entirely when set to '-'.")
	agentCardName := flag.String("agent-card-name", "", "override name field in /.well-known/agent-card.json")
	agentCardDescription := flag.String("agent-card-description", "", "override description field in /.well-known/agent-card.json. Required (file or flag) to enable the endpoint.")
	agentCardExternalURL := flag.String("agent-card-external-url", "", "override url field in /.well-known/agent-card.json with a canonical value. Optional — by default the card echoes back the URL the caller used (Host header + X-Forwarded-Proto/Host).")
	agentCardVersion := flag.String("agent-card-version", "", "override version field in /.well-known/agent-card.json (defaults to the build version)")
	agentCardProviderOrg := flag.String("agent-card-provider-org", "", "override provider.organization in /.well-known/agent-card.json")
	agentCardProviderURL := flag.String("agent-card-provider-url", "", "override provider.url in /.well-known/agent-card.json")
	agentCardDocsURL := flag.String("agent-card-docs-url", "", "override documentationUrl in /.well-known/agent-card.json")
	logFile := flag.String("log-file", "", `mirror daemon stderr (operator diagnostics) to this path in addition to the terminal. Empty (default) or "-" leaves stderr-only. Recommended: /tmp/core-agent.log so TUI screen-takeover doesn't swallow startup diagnostics.`)
	metricsAddr := flag.String("metrics-addr", "", "Prometheus scrape endpoint bind address (e.g. :9464). Overrides cfg.otel.metrics.prometheus_addr. Ignored unless cfg.otel.metrics.exporter (or OTEL_METRICS_EXPORTER) selects prometheus or both. Empty leaves the config value in effect; :9464 is the default when neither is set.")
	flag.Parse()

	// Install --log-file tee BEFORE run() so any config-load / mcp
	// init / model-resolution diagnostics land in the file too. Errors
	// opening the file are fatal — an operator who asked for a log
	// destination and got nothing has been left worse off than the
	// no-flag baseline.
	if err := installLogFileTee(*logFile); err != nil {
		fmt.Fprintf(os.Stderr, "core-agent: --log-file: %v\n", err)
		os.Exit(runner.ExitConfigError)
	}

	code := run(*prompt, *initialPrompt, *cfgPath, *modelOverride, *providerOverride, *taskClass, *noBuiltinTools, *disableTools, *scriptPath, *scriptStrict, *recordTo, *color, *ask, *sessionDB, *sessionDBPath, *yolo, *noBackgroundAgents, *allowURLHost, allowPathEntries, contentDirEntries, *noREPL, *noTUI, *noPricingRefresh, *noCompact, *noCheckpoint, *compactionThreshold,
		guardrailOpts{
			watchdogMode:      *watchdogMode,
			maxTurnCostUSD:    *maxTurnCostUSD,
			maxSessionCostUSD: *maxSessionCostUSD,
			// Explicit 0 means "no ceiling" and must beat the
			// unattended default, so record presence, not value.
			maxSessionCostSet: flagWasSet(flag.CommandLine, "max-session-cost-usd"),
			bashSearchGate:    *bashSearchGate,
		},
		toolProfileOpts{
			enableTools: *enableTools,
			planFirst:   *planFirst,
			// --plan-first=false is the documented way out of a task
			// profile that turns the gate on, so presence matters, not
			// value.
			planFirstSet: flagWasSet(flag.CommandLine, "plan-first"),
		},
		*smallTierParent, *agenticTools, *agenticSmallModel, *noMCPDigest, *mcpAgenticWrapLLM, *mcpAgenticWrapModel, *noContextCache, promptOpts{appendSystemPrompt: *appendSystemPrompt, systemPromptFile: *systemPromptFile},
		attachOpts{
			Listen:           *attachListen,
			UnixSocket:       *attachUnixSocket,
			TLSCert:          *attachTLSCert,
			TLSKey:           *attachTLSKey,
			ClientCA:         *attachClientCA,
			TokenEnv:         *attachTokenEnv,
			ReadOnly:         *attachReadonly,
			PeerHub:          *attachPeerHub,
			RegisterTo:       *attachRegisterTo,
			RegisterName:     *attachRegisterName,
			RegisterEndpoint: *attachRegisterEndpoint,
			UI:               *attachUI || *attachUIDir != "",
			UIDir:            *attachUIDir,
		},
		agentCardOpts{
			ConfigPath:       *agentCardConfigPath,
			Name:             *agentCardName,
			Description:      *agentCardDescription,
			ExternalURL:      *agentCardExternalURL,
			Version:          *agentCardVersion,
			ProviderOrg:      *agentCardProviderOrg,
			ProviderURL:      *agentCardProviderURL,
			DocumentationURL: *agentCardDocsURL,
		},
		metricsOpts{Addr: *metricsAddr})
	os.Exit(code)
}

// agentCardOpts bundles --agent-card-* CLI flags. Loaded into an
// attach.AgentCardConfig inside run() by overlaying onto whatever
// .agents/agent-card.json (or --agent-card-config=<path>) supplied.
type agentCardOpts struct {
	ConfigPath       string // empty → .agents/agent-card.json under the resolved agentsDir; "-" → skip file load
	Name             string
	Description      string
	ExternalURL      string
	Version          string
	ProviderOrg      string
	ProviderURL      string
	DocumentationURL string
}

// attachOpts bundles the attach-mode CLI flags so run()'s signature
// doesn't grow by 11 more positional args. The struct itself lives in
// pkg/compose since the extraction (#386 PR 6) — main only owns the
// flag binding and the CLI-beats-config precedence in
// mergeAttachOpts.
type attachOpts = compose.AttachOptions

// resolveAgentCardConfig builds the attach.AgentCardConfig from
// .agents/agent-card.json plus CLI flag overrides, with
// defaultDescription as a final fallback. Precedence per field:
// CLI flag (when set non-empty) > file > defaultDescription
// (description only) > zero. Returns the zero config (endpoint
// disabled) when no source supplies a description.
//
// defaultDescription comes from .agents/config.json's
// agent.description — same value fed to agent.WithDescription so
// ADK's system prompt and the card share one source of truth.
//
// agentsDir may be empty (no .agents/ discovered). cardCfg.ConfigPath
// of "-" suppresses the file load entirely; an explicit non-empty
// path is loaded from disk (missing file → startup error, since the
// operator asked for it specifically).
func resolveAgentCardConfig(agentsDir string, cardCfg agentCardOpts, defaultDescription string) (attach.AgentCardConfig, error) {
	var fileCfg attach.AgentCardConfig
	switch {
	case cardCfg.ConfigPath == "-":
		// explicit skip
	case cardCfg.ConfigPath != "":
		loaded, present, err := attach.LoadAgentCardFile(cardCfg.ConfigPath)
		if err != nil {
			return attach.AgentCardConfig{}, err
		}
		if !present {
			return attach.AgentCardConfig{}, fmt.Errorf("--agent-card-config=%s: file not found", cardCfg.ConfigPath)
		}
		fileCfg = loaded
	case agentsDir != "":
		path := filepath.Join(agentsDir, attach.AgentCardFileName)
		loaded, _, err := attach.LoadAgentCardFile(path)
		if err != nil {
			return attach.AgentCardConfig{}, err
		}
		fileCfg = loaded
	}

	// Fall back to the config.json-level agent.description before
	// applying CLI overrides. The file's `description` field wins
	// over config.json (file is more specific to the card surface),
	// CLI flag wins over both.
	if fileCfg.Description == "" {
		fileCfg.Description = defaultDescription
	}
	// CLI overrides — non-empty flag wins.
	if cardCfg.Name != "" {
		fileCfg.Name = cardCfg.Name
	}
	if cardCfg.Description != "" {
		fileCfg.Description = cardCfg.Description
	}
	if cardCfg.ExternalURL != "" {
		fileCfg.ExternalURL = cardCfg.ExternalURL
	}
	if cardCfg.Version != "" {
		fileCfg.Version = cardCfg.Version
	}
	if cardCfg.DocumentationURL != "" {
		fileCfg.DocumentationURL = cardCfg.DocumentationURL
	}
	if cardCfg.ProviderOrg != "" {
		fileCfg.Provider.Organization = cardCfg.ProviderOrg
	}
	if cardCfg.ProviderURL != "" {
		fileCfg.Provider.URL = cardCfg.ProviderURL
	}

	if err := fileCfg.Validate(); err != nil {
		return attach.AgentCardConfig{}, err
	}
	return fileCfg, nil
}

// mergeAttachOpts overlays cfg onto opts where the CLI flag wasn't
// explicitly set. CLI > config > zero-value. String fields then pass
// through os.ExpandEnv so per-pod values like "https://${POD_IP}:7777"
// can live in a shared ConfigMap.
//
// flagSet is the parsed FlagSet used to register the --attach-* flags;
// production passes flag.CommandLine, tests pass their own.
func mergeAttachOpts(opts attachOpts, cfg config.AttachConfig, flagSet *flag.FlagSet) attachOpts {
	setOnCLI := map[string]bool{}
	flagSet.Visit(func(f *flag.Flag) { setOnCLI[f.Name] = true })

	// Config half (value translation + ${ENV} expansion) lives in
	// compose; this function owns only the CLI-beats-config
	// precedence, which needs flag.Visit and therefore stays in main.
	fromCfg := compose.BuildAttachOptions(cfg)

	overlayStr := func(name string, dst *string, cfgVal string) {
		if !setOnCLI[name] && *dst == "" {
			// cfgVal arrives pre-expanded from BuildAttachOptions.
			*dst = cfgVal
			return
		}
		*dst = os.ExpandEnv(*dst)
	}
	overlayBool := func(name string, dst *bool, cfgVal bool) {
		if !setOnCLI[name] {
			*dst = cfgVal
		}
	}

	overlayStr("attach-listen", &opts.Listen, fromCfg.Listen)
	overlayStr("attach-unix-socket", &opts.UnixSocket, fromCfg.UnixSocket)
	overlayStr("attach-tls-cert", &opts.TLSCert, fromCfg.TLSCert)
	overlayStr("attach-tls-key", &opts.TLSKey, fromCfg.TLSKey)
	overlayStr("attach-client-ca", &opts.ClientCA, fromCfg.ClientCA)
	overlayStr("attach-token", &opts.TokenEnv, fromCfg.TokenEnv)
	overlayBool("attach-readonly", &opts.ReadOnly, fromCfg.ReadOnly)
	overlayBool("attach-peer-hub", &opts.PeerHub, fromCfg.PeerHub)
	overlayStr("attach-register-to", &opts.RegisterTo, fromCfg.RegisterTo)
	overlayStr("attach-register-endpoint", &opts.RegisterEndpoint, fromCfg.RegisterEndpoint)
	overlayStr("attach-register-name", &opts.RegisterName, fromCfg.RegisterName)
	return opts
}

// teardownStepTimeout bounds each individual shutdown defer in run()
// that talks to something external (OTLP flush, Prometheus/metrics
// shutdown, Vertex context-cache delete). See the teardown-budget
// comment at the otelShutdown defer (#538).
const teardownStepTimeout = 3 * time.Second

func run(prompt, initialPrompt, cfgPath, modelOverride, providerOverride, taskClass string, noBuiltinTools bool, disableTools string, scriptPath string, scriptStrict bool, recordTo string, color string, ask string, sessionDB bool, sessionDBPath string, yolo, noBackgroundAgents bool, allowURLHost string, allowPathEntries []config.PathScopeAllowEntry, contentDirEntries []string, noREPL, noTUI, noPricingRefresh, noCompact, noCheckpoint bool, compactionThreshold float64, guardrails guardrailOpts, toolProfile toolProfileOpts, smallTierParent string, agenticTools bool, agenticSmallModel string, noMCPDigest, mcpAgenticWrapLLM bool, mcpAgenticWrapModel string, noContextCache bool, promptCfg promptOpts, attachCfg attachOpts, cardCfg agentCardOpts, metricsCfg metricsOpts) int {
	// SIGTERM still cancels the whole process via ctx. SIGINT
	// (Ctrl+C) is NOT in this list anymore — the REPL takes over
	// SIGINT for its own double-Ctrl+C-exits state machine, and
	// the per-turn turnInterrupter handles Ctrl+C as a raw byte
	// while a turn is in flight (raw mode disables ISIG). For
	// headless (-p) mode, an uncaught SIGINT terminates the
	// process at exit code 130 — standard one-shot-CLI behavior.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer stop()

	// -i is incompatible with headless (-p runs one turn and exits;
	// -i seeds one turn and stays interactive — they can't both win)
	// and with --no-repl (attach-only daemon; the seed prompt has no
	// operator surface to remain interactive on). Reject early with a
	// config error so operators see the message before startup work.
	if initialPrompt != "" {
		if prompt != "" {
			fmt.Fprintln(os.Stderr, "core-agent: -p and -i are mutually exclusive (headless single-turn vs seeded interactive)")
			return runner.ExitConfigError
		}
		if noREPL {
			fmt.Fprintln(os.Stderr, "core-agent: -i is not compatible with --no-repl (attach-only daemon has no operator to stay interactive for)")
			return runner.ExitConfigError
		}
	}

	// Filter "Error context canceled" out of the default log
	// output. genai's SSE scanner unconditionally log.Printfs
	// every stream error (api_client.go:484), including
	// context.Canceled when the user hits ESC mid-turn. We can't
	// suppress at the source, so we drop the line at the
	// process-wide log writer here.
	installLogFilter(os.Stderr)

	cwd, _ := os.Getwd()
	cfg, agentsDir, err := loadConfig(cfgPath, cwd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "core-agent: %v\n", err)
		return runner.ExitConfigError
	}

	// #322: load the optional env manifest (.agents/env.yaml or
	// .env.json) that declares which env vars the bundle expects.
	// Nil manifest = bundle didn't opt in = pre-#322 behavior (no
	// interpolation, no validation). Required-var-missing is fail-
	// loud; drift diagnostics warn but never block startup.
	envManifest, err := agentenv.LoadManifest(agentsDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "core-agent: %v\n", err)
		return runner.ExitConfigError
	}
	envResolver := agentenv.NewResolver(envManifest, os.LookupEnv)
	if errs := envResolver.Errors(); len(errs) > 0 {
		for _, e := range errs {
			fmt.Fprintf(os.Stderr, "core-agent: %v\n", e)
		}
		return runner.ExitConfigError
	}

	attachCfg = mergeAttachOpts(attachCfg, cfg.Attach, flag.CommandLine)
	if modelOverride != "" {
		cfg.Model.Name = modelOverride
	}
	if providerOverride != "" {
		cfg.Model.Provider = providerOverride
	}
	if scriptPath != "" {
		cfg.Mock.Script = scriptPath
	}
	if scriptStrict {
		cfg.Mock.Strict = true
	}
	if recordTo != "" {
		cfg.Mock.Record = recordTo
	}
	if allowURLHost != "" {
		for _, h := range strings.Split(allowURLHost, ",") {
			h = strings.TrimSpace(h)
			if h == "" {
				continue
			}
			cfg.URLScope.Allow = append(cfg.URLScope.Allow, h)
		}
	}
	// Explicit --compaction-threshold wins over both the config-file value
	// and any task profile. Applied before the task-class block so that
	// block sees a non-nil threshold and won't overwrite it. Config
	// loading already bounds cfg.Compaction.Threshold; validate the flag
	// here since it lands after Load.
	if compactionThreshold != 0 {
		if compactionThreshold <= 0 || compactionThreshold >= 1 {
			fmt.Fprintf(os.Stderr, "core-agent: --compaction-threshold=%v must be in (0, 1) exclusive\n", compactionThreshold)
			return runner.ExitConfigError
		}
		thr := compactionThreshold
		cfg.Compaction.Threshold = &thr
	}

	// Task class (#123). CLI --task overrides cfg.Session.TaskClass.
	// Apply the resolved profile to whichever flags the operator left
	// unspecified; explicit flags always win. Done BEFORE provider
	// resolution so the task's tier-to-model selection lands before
	// provider.Model(cfg.Model.Name) is called.
	if taskClass != "" {
		cfg.Session.TaskClass = taskClass
	}
	// taskProfile stays at its zero value when no class is declared,
	// which is what makes the tool and plan-first resolution below
	// safe to run unconditionally: an empty profile has no opinion.
	var taskProfile taskclass.Profile
	if cfg.Session.TaskClass != "" {
		profile, ok := taskclass.Resolve(cfg.Session.TaskClass)
		if !ok {
			fmt.Fprintf(os.Stderr, "core-agent: --task=%q: unknown task class (want one of %v)\n",
				cfg.Session.TaskClass, taskclass.Classes())
			return runner.ExitConfigError
		}
		taskProfile = profile
	}
	// Tools (#160): the profile's disables minus anything
	// --enable-tools asked to keep, applied down at the built-in tool
	// block. Run for every start, not just --task ones, so a typo'd or
	// conflicting --enable-tools fails at boot rather than silently
	// no-op'ing when no class happens to be declared.
	profileDisables, err := resolveProfileDisables(taskProfile.DisableTools, cfg.Tools.Disable, splitList(disableTools), splitList(toolProfile.enableTools))
	if err != nil {
		fmt.Fprintf(os.Stderr, "core-agent: %v\n", err)
		return runner.ExitConfigError
	}
	if cfg.Session.TaskClass != "" {
		profile := taskProfile
		// Pick the provider name for tier→model mapping. cfg.Model.Provider
		// may be empty (auto-detect); fall back to env-based auto-detect.
		providerForTier := cfg.Model.Provider
		if providerForTier == "" {
			providerForTier = models.AutoDetectProvider()
		}
		// Model: the profile's tier→model selection only fills in when the
		// operator has expressed no preference. That means BOTH the CLI
		// --model flag is empty AND cfg.Model.Name is still the substrate
		// default — a value present in the config file is operator-set and
		// must be respected (previously --task clobbered it, #395). When
		// the config file pins a model, --task adjusts the other knobs
		// (compaction, ask) but leaves the model alone.
		if modelOverride == "" && cfg.Model.Name == config.DefaultConfig().Model.Name {
			if tierModel := taskclass.ModelForTier(providerForTier, profile.Tier); tierModel != "" {
				cfg.Model.Name = tierModel
			}
		}
		// Compaction threshold: only override if not already set
		// (config-file value wins over task profile).
		if cfg.Compaction.Threshold == nil && profile.CompactionThreshold > 0 {
			thr := profile.CompactionThreshold
			cfg.Compaction.Threshold = &thr
		}
		// Ask mode: only override if CLI --ask is empty. The "auto"
		// the profile picks turns into the existing --ask=auto
		// behavior (stdin TTY → ask, headless → allow).
		if ask == "" && profile.AskMode != "" {
			ask = profile.AskMode
		}
		toolNote := "tools=default"
		switch {
		case noBuiltinTools:
			// The profile's tool opinion is moot here; saying
			// "default−bash" would imply the rest of the suite survived.
			toolNote = "tools=none (--no-builtin-tools)"
		case len(profileDisables) > 0:
			toolNote = "tools=default−" + strings.Join(profileDisables, ",")
		}
		fmt.Fprintf(os.Stderr, "core-agent: task class: %s → model=%s compaction-threshold=%.2f ask=%s %s (override individual knobs with --model / --compaction-threshold / --ask / --enable-tools / --plan-first)\n",
			cfg.Session.TaskClass, cfg.Model.Name, profile.CompactionThreshold, ask, toolNote)
	}

	// Plan-first (#160) is resolved for every run, not just --task
	// ones, because the flag is also the CLI mirror of
	// permissions.require_plan_artifact. The profile can only turn the
	// gate on; --plan-first=false is the way out. Must land before
	// permissions.FromConfig reads cfg.Permissions and before
	// tools.Build decides whether to register record_plan.
	// Whether record_plan will actually be in the catalog. tools.Build
	// needs an agentsDir to write plans into and the built-in suite to
	// be on; the operator can also have disabled the tool outright.
	// Each of those turns plan-first from a stricter posture into a
	// deadlock, so name the specific one rather than saying "off".
	planBlocker := ""
	switch {
	case agentsDir == "":
		planBlocker = "no .agents/ directory resolved, so record_plan has nowhere to write plans"
	case noBuiltinTools:
		planBlocker = "--no-builtin-tools drops record_plan along with the rest of the suite"
	case slices.Contains(cfg.Tools.Disable, "record_plan"):
		planBlocker = "record_plan is in cfg.tools.disable"
	case slices.Contains(splitList(disableTools), "record_plan"):
		planBlocker = "record_plan is in --disable-tools"
	}
	planOn, planSource := resolvePlanFirst(planFirstInputs{
		Flag:          toolProfile.planFirst,
		FlagSet:       toolProfile.planFirstSet,
		Config:        cfg.Permissions.RequirePlanArtifact,
		Profile:       taskProfile.RequirePlanArtifact,
		CanRecordPlan: planBlocker == "",
	})
	cfg.Permissions.RequirePlanArtifact = planOn
	switch {
	case planSource == sourcePlanNoRecorder:
		fmt.Fprintf(os.Stderr, "core-agent: plan-first: off — the %s profile asks for it, but %s\n", cfg.Session.TaskClass, planBlocker)
	case planOn && planBlocker != "":
		// Explicit operator intent, so we honor it rather than
		// silently flipping it off — but with no record_plan, /replan
		// only revokes and nothing grants, so every mutating tool call
		// is denied for the life of the session. Say so at boot rather
		// than letting the operator discover it one denial at a time.
		fmt.Fprintf(os.Stderr, "core-agent: plan-first: on (%s) but %s — every mutating tool call will be denied with no way to clear the gate. Fix that, or pass --plan-first=false.\n", planSource, planBlocker)
	case planOn:
		fmt.Fprintf(os.Stderr, "core-agent: plan-first: on (%s) — mutating tools are denied until the model calls record_plan\n", planSource)
	}

	otelShutdown, err := telemetry.Setup(ctx, cfg.OTEL.Exporter)
	if err != nil {
		fmt.Fprintf(os.Stderr, "core-agent: telemetry setup: %v\n", err)
	}
	// Teardown waits are bounded (#538): every shutdown defer in run()
	// carries its own deadline so a single stalled dependency (OTLP
	// endpoint, Vertex cache delete, wedged subagent) can't eat the
	// supervisor's termination grace period. Budget with defaults:
	// peer deregister 2s + attach drain 5s + background drain 5s +
	// MCP children 3s (parallel) + context-cache/metrics/OTel 3s each
	// ≈ 24s worst case, inside K8s' default 30s
	// terminationGracePeriodSeconds with headroom.
	defer func() {
		shCtx, cancel := context.WithTimeout(context.Background(), teardownStepTimeout)
		defer cancel()
		_ = otelShutdown(shCtx)
	}()

	// Metrics pipeline runs alongside traces but doesn't share init.
	// ADK has no MeterProvider (upstream TODO(#479)), so telemetry.SetupMetrics
	// builds one directly. Off by default; opt-in via cfg.otel.metrics
	// or OTEL_METRICS_EXPORTER. See docs/metrics-design.md.
	//
	// Fail-loudly: init errors abort the daemon rather than silently
	// shipping a binary that emits no metrics. Prometheus bind
	// failures are the most common cause in dev (port already in use).
	metricsShutdown, err := telemetry.SetupMetrics(ctx, cfg.OTEL.Metrics, telemetry.MetricsOptions{
		PrometheusAddr: metricsCfg.Addr,
		ServiceName:    "core-agent",
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "core-agent: metrics setup: %v\n", err)
		return runner.ExitConfigError
	}
	defer func() {
		shCtx, cancel := context.WithTimeout(context.Background(), teardownStepTimeout)
		defer cancel()
		_ = metricsShutdown(shCtx)
	}()

	provider, err := models.Resolve(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "core-agent: %v\n", err)
		return runner.ExitConfigError
	}

	// Vertex explicit context caching (#221 v1). Wire the cache
	// manager BEFORE Model() is called — Provider.Model constructs
	// the builtinsLLM wrapper that reads the cache hooks, so
	// installing hooks after Model() would leave them dangling.
	//
	// Gated three ways:
	//   1. Provider must be *gemini.Provider (only Vertex/Gemini SDK).
	//   2. Config: cfg.Model.Vertex.ContextCache.IsEnabled() (default
	//      ON when the block is absent from config.json).
	//   3. --no-context-cache CLI kill switch takes precedence.
	//
	// Failure to construct the sibling genai.Client is logged and
	// caching is skipped — never breaks agent startup.
	contextCacheManager := compose.MaybeWireContextCache(
		ctx, provider, cfg, noContextCache,
		func(s string) { fmt.Fprintln(os.Stderr, "core-agent: "+s) },
	)
	if contextCacheManager != nil {
		defer func() {
			// Bounded (#538): Delete is a Vertex API call; a slow or
			// unreachable endpoint must not stall teardown. An
			// undeleted CachedContent expires server-side via its TTL.
			shCtx, cancel := context.WithTimeout(context.Background(), teardownStepTimeout)
			defer cancel()
			contextCacheManager.Delete(shCtx)
		}()
	}

	m, err := provider.Model(ctx, cfg.Model.Name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "core-agent: %v\n", err)
		return runner.ExitConfigError
	}
	if cfg.Mock.Record != "" {
		f, err := os.Create(cfg.Mock.Record)
		if err != nil {
			fmt.Fprintf(os.Stderr, "core-agent: --record-to: %v\n", err)
			return runner.ExitConfigError
		}
		defer f.Close()
		m = recording.NewRecorder(m, f)
	}

	userHome, _ := os.UserHomeDir()
	coreHome := ""
	homeAgentsDir := ""
	if userHome != "" {
		coreHome = filepath.Join(userHome, ".core-agent")
		// Portable user-scope root: $HOME/.agents/. Read by the skills,
		// mcp, and instruction loaders as a layer between project-scope
		// (workspace) and ~/.core-agent/. Lets a user park skills /
		// mcp.json / AGENTS.md once and have every session pick them up,
		// even when a harness (e.g. scion) pre-creates an empty
		// workspace .agents/ that would otherwise shadow it.
		homeAgentsDir = filepath.Join(userHome, ".agents")
	}

	if yolo {
		// --yolo overrides the configured mode unconditionally. Done
		// before FromConfig so the mode is consistent with the
		// constructed Gate (and any future code that reads it back).
		cfg.Permissions.Mode = string(permissions.ModeYolo)
	}
	// CLI --allow-path entries layer on top of whatever the config
	// file already lists; CLI > config > nothing. Validated in two
	// places: parseAllowPathSpec rejects malformed flag values at
	// parse time, FromConfig's ParseAccess call rejects bad entries
	// from either source as a defense-in-depth.
	if len(allowPathEntries) > 0 {
		cfg.PathScope.AllowPaths = append(cfg.PathScope.AllowPaths, allowPathEntries...)
	}
	// Search-gate precedence: CLI --bash-search-gate > config
	// safety.bash_search_gate > "enforce". Applied before FromConfig
	// so the constructed Gate and --print-config agree, and validated
	// here because the flag path never goes through config.Validate.
	if guardrails.bashSearchGate != "" {
		switch guardrails.bashSearchGate {
		case config.BashSearchGateEnforce, config.BashSearchGateWarn, config.BashSearchGateAllow:
			cfg.Safety.BashSearchGate = guardrails.bashSearchGate
		default:
			fmt.Fprintf(os.Stderr, "core-agent: --bash-search-gate: unknown value %q (want one of %q, %q, %q)\n",
				guardrails.bashSearchGate, config.BashSearchGateEnforce, config.BashSearchGateWarn, config.BashSearchGateAllow)
			return runner.ExitConfigError
		}
	}
	prompter := resolveGatePrompter(yolo, os.Stdin, os.Stderr)
	template, err := permissions.FromConfig(cfg, cwd, coreHome, prompter)
	if err != nil {
		fmt.Fprintf(os.Stderr, "core-agent: %v\n", err)
		return runner.ExitConfigError
	}
	// Persist "allow always" grants through the config-backed store
	// (#386 PR 3). The gate's DecisionAllowAlways path now owns both
	// halves of the contract — in-memory policy add + disk write —
	// for every prompter (TUI modal, stdin, HTTP broker). Derived
	// per-session sub-gates share the store by reference. Empty
	// agentsDir ⇒ Persist is a no-op (grants stay session-scoped),
	// same fallback the TUI callback used to implement one layer up.
	template.SetGrantStore(&permissions.ConfigGrantStore{AgentsDir: agentsDir})
	// Always-derive: even in single-user mode the agent runs against
	// a per-session sub-gate so per-session state (sessionAllow,
	// planRecorded, etc.) is naturally isolated and the multi-session
	// path is the same code path. The template stays as the daemon-
	// wide configuration source; only the derived gate is consulted
	// at tool-call time. See docs/multi-session-design.md.
	//
	// SessionID is empty at startup because the daemon currently only
	// constructs one agent. Future multi-session-creation flows will
	// derive a fresh sub-gate per session with that session's ID.
	gate := template.DeriveForSession("", prompter)

	projectRoot := cwd
	if agentsDir != "" {
		projectRoot = filepath.Dir(agentsDir)
	}
	// External content roots (docs/external-content-root-design.md): config
	// content_roots plus repeatable --agents-content-dir flags, merged CLI-
	// after-config, resolved once relative to the agents dir (or cwd when the
	// config was not discovered under an agents dir). Each becomes an
	// additional trusted instruction/skill scope, threaded into every loader
	// call below (startup + reload). Empty ⇒ nil ⇒ the loaders are a no-op,
	// i.e. today's behavior exactly.
	contentRootBase := agentsDir
	if contentRootBase == "" {
		contentRootBase = cwd
	}
	contentRoots := resolveContentRoots(cfg.ContentRoots, contentDirEntries, contentRootBase)
	// LoadForSession is the multi-session-aware loader. With an empty
	// callerIdentity (single-user / startup-time), it behaves
	// identically to Load. The per-caller overlay path lights up when
	// a request-time Caller threads through — γ wires the call site;
	// future session-creation flows pass the resolved Caller.Identity.
	loaded, err := instruction.LoadForSession(projectRoot, coreHome, "", cfg.Attach.MultiSession.UsersDir,
		instruction.WithHomeAgentsRoot(homeAgentsDir),
		instruction.WithContentRoots(contentRoots),
		instruction.WithInterpolator(envResolver.InterpolateFunc()))
	if err != nil {
		// Fatal: malformed @include / escaped path / missing target / non-UTF-8
		// content indicates a config bug. Silently shipping a degraded prompt
		// to the agent is worse than refusing to start — the v2 design intent
		// is "typos surface immediately rather than silently shrinking the
		// system prompt." Operators expecting a softer failure mode can fix
		// their AGENTS.md / AGENTS.d/ contents and restart.
		fmt.Fprintf(os.Stderr, "core-agent: instruction load: %v\n", err)
		return runner.ExitConfigError
	}

	send := func(s string) { fmt.Fprintln(os.Stderr, "core-agent: "+s) }

	// Instruction-load visibility. Loading nothing is silently permitted
	// (single-shot -p invocations, mock/scripted runs, freshly-cloned
	// repos legitimately have no AGENTS.md) but operators wiring up a
	// recipe that DOES expect AGENTS.md need a signal when the loader
	// found nothing — otherwise the daemon runs on raw provider
	// defaults and the operator has no visible clue why the model is
	// ignoring their carefully-written instructions. See issue #218
	// (surfaced live during the v2.6 GKE-troubleshoot demo drive).
	if len(loaded.Sources) == 0 {
		send(fmt.Sprintf("instruction: no AGENTS.md found (searched: %s). Model will run without user instructions.",
			strings.Join(loaded.Searched, ", ")))
	} else {
		names := make([]string, 0, len(loaded.Sources))
		for _, s := range loaded.Sources {
			names = append(names, s.Path)
		}
		send(fmt.Sprintf("instruction: loaded %d file(s): %s", len(loaded.Sources), strings.Join(names, ", ")))
	}

	// Small-tier-parent guard (#121). When an interactive session
	// (REPL or attach-listen — anything that isn't `-p` one-shot)
	// resolves to a small-tier parent model (Flash/Haiku-class),
	// surface a notice. Small-tier models work well as agentic_*
	// subtask workers but loop and stall as the parent for long
	// interactive sessions. The 2026-06-08 smoke that motivated this
	// burned ~$80 across three sessions on gemini-3.5-flash as the
	// parent — same bug an Opus-tier session found in a handful of
	// turns.
	//
	// Skipped when: prompt != "" (one-shot; operator may know what
	// they're doing — could be a script invoking Flash on purpose);
	// yolo (trust-the-operator mode); the resolved model's tier
	// doesn't classify (unknown / new model — false-positive risk
	// outweighs the warning value).
	//
	// Mode resolution: CLI --small-tier-parent > config
	// safety.small_tier_parent > default "warn". Place this BEFORE
	// the rest of agent construction so "refuse" can short-circuit
	// without leaking listeners / tracker / etc.
	if smallTierParent != "" {
		cfg.Safety.SmallTierParent = smallTierParent
	}
	stpMode := cfg.Safety.SmallTierParent
	if stpMode == "" {
		stpMode = config.SmallTierParentWarn
	}
	if prompt == "" && !yolo && modeltier.IsSmall(cfg.Model.Name) {
		// Use the task-class per-provider tier→model table to
		// suggest a same-provider frontier model. Falls back to a
		// generic Opus suggestion when the provider isn't in the
		// table (e.g. echo / scripted in tests).
		suggested := taskclass.ModelForTier(provider.Name(), taskclass.TierFrontier)
		if suggested == "" {
			suggested = "claude-opus-4-7"
		}
		notice := fmt.Sprintf(
			"%s is a small-tier model. Small-tier models work well as subtask workers (--agentic-small-model) but loop and stall as the parent for long interactive sessions. Consider a frontier or mid-tier model for the parent — e.g. --model %s --agentic-small-model %s.",
			cfg.Model.Name, suggested, cfg.Model.Name,
		)
		switch stpMode {
		case config.SmallTierParentRefuse:
			fmt.Fprintf(os.Stderr, "core-agent: refuse-on-small-tier-parent: %s Pass --small-tier-parent=warn or --small-tier-parent=allow to proceed anyway.\n", notice)
			return runner.ExitConfigError
		case config.SmallTierParentWarn:
			send("small-tier parent: " + notice + " Pass --small-tier-parent=allow to suppress this notice.")
		case config.SmallTierParentAllow:
			// no-op
		}
	}

	// makeMCPElicitor is build-tagged: in the default build it
	// constructs a tui.Elicitor (and stashes the handle for
	// launchTUI to attach later); in the slim `-tags no_tui` build
	// it returns nil so MCP elicit requests decline server-side.
	//
	// digestOpts wires the pkg/digest structural pruner into every
	// MCP tool response (see docs/digest-design.md, task #84). The
	// LazyStore lets the wrap layer accept a stable Store reference
	// up front — the EventlogStore itself needs a session ID which
	// isn't known until agent.New runs, so we .Set(...) the real
	// backing later. Nil digestOpts disables wrapping entirely
	// (--no-mcp-digest kill switch).
	//
	// agentRef is hoisted above mcp.Build so the LLMFallback closure
	// below can capture it — mcp.Build precedes agent.New (the
	// toolsets feed into agent.New's options), and the LLM subagent
	// path needs a reference to the *Agent to invoke RunSubtask.
	var agentRef *agent.Agent
	tracker := usage.NewTracker()

	// Register the usage/cost observer against whatever MeterProvider
	// telemetry.SetupMetrics installed. When metrics are disabled the
	// global MeterProvider is a noop and RegisterMetrics is
	// effectively free — the callback registers but never fires
	// because no reader collects. Identity fields (SessionID, AppName,
	// UserID) are lazily populated via primaryTracker.SetIdentity in
	// the WithPostConstruct hook below, since a.SessionID() isn't
	// known until agent.New completes.
	primaryTracker := &primaryTrackerProvider{tracker: tracker}
	var usageMetricOpts []usage.RegisterOption
	if !cfg.OTEL.Metrics.SessionLabelsEnabled() {
		// otel.metrics.session_labels=false — fleet operators trade
		// per-session drill-down for bounded series cardinality; the
		// observer aggregates across sessions before export.
		usageMetricOpts = append(usageMetricOpts, usage.WithoutSessionLabels())
	}
	if _, err := usage.RegisterMetrics(otel.GetMeterProvider(), primaryTracker, usageMetricOpts...); err != nil {
		fmt.Fprintf(os.Stderr, "core-agent: metrics: register usage observer: %v\n", err)
		return runner.ExitConfigError
	}
	// Subsystem observers (#338 Phase 3): digest is process-global;
	// the agent observer reuses primaryTracker as its AgentSource
	// (primary agent + attach registry, stamped later like the
	// tracker side). Same fail-loud posture as the usage observer.
	if _, err := digest.RegisterMetrics(otel.GetMeterProvider()); err != nil {
		fmt.Fprintf(os.Stderr, "core-agent: metrics: register digest observer: %v\n", err)
		return runner.ExitConfigError
	}
	if _, err := agent.RegisterMetrics(otel.GetMeterProvider(), primaryTracker); err != nil {
		fmt.Fprintf(os.Stderr, "core-agent: metrics: register agent observer: %v\n", err)
		return runner.ExitConfigError
	}
	var digestStore *digest.LazyStore
	var digestOpts *mcp.DigestOptions
	if !noMCPDigest {
		digestStore = &digest.LazyStore{}
		digestOpts = &mcp.DigestOptions{Store: digestStore}
		// Per-call digest savings are observed on the AGENT side now
		// — see pkg/agent/tool_savings_observer.go. The previous
		// wiring here (DigestOptions.OnResult → this process-level
		// tracker) only worked in single-session mode: multi-session
		// gives each on-demand session its own tracker, and OnResult
		// still fired against the closure-captured process tracker,
		// so per-session /context reads were always empty. The
		// agent-side observer reads the `savings` sidecar off each
		// FunctionResponse it processes and appends to the session's
		// own tracker — one code path, both modes fixed.

		// LLM subagent second-chance path (#223). Opt-in from either
		// source: the CLI flag OR mcp.json's agentic_wrap_llm. Either
		// on → build the closure. Peek at mcp.json up front so config
		// alone can enable this without touching CLI flags (config is
		// the persistent source for per-project defaults).
		//
		// Load errors surface later inside mcp.Build itself; here we
		// just default to "no config-side opt-in" so a misconfigured
		// mcp.json doesn't accidentally light up the LLM subagent.
		mcpCfg, _ := mcp.LoadAll(agentsDir, homeAgentsDir)
		llmEnabled := mcpAgenticWrapLLM || mcpCfg.AgenticWrapLLMEnabled()
		llmModelOverride := mcpAgenticWrapModel
		if llmModelOverride == "" {
			llmModelOverride = mcpCfg.AgenticWrapModel
		}
		if llmEnabled {
			resolvedMCPModel := models.ResolveMCPSmallModel(provider, llmModelOverride, agenticSmallModel)
			digestOpts.LLMFallback = compose.BuildMCPDigestLLMFallback(&agentRef, provider, resolvedMCPModel)
			switch {
			case resolvedMCPModel == "":
				send(fmt.Sprintf("mcp agentic wrap: LLM subagent on, inherits parent (%s — no cheap-tier default for provider %q)", cfg.Model.Name, provider.Name()))
			case llmModelOverride != "":
				send(fmt.Sprintf("mcp agentic wrap: LLM subagent on, %s (mcp-specific override)", resolvedMCPModel))
			case agenticSmallModel != "":
				send(fmt.Sprintf("mcp agentic wrap: LLM subagent on, %s (via --agentic-small-model)", resolvedMCPModel))
			default:
				send(fmt.Sprintf("mcp agentic wrap: LLM subagent on, %s (provider default)", resolvedMCPModel))
			}
		}
	}
	// Construct the MCP elicitor ONCE and reuse it for both the parent and
	// any rooted subagents' mcp.Build. makeMCPElicitor has a side effect in
	// the TUI build — it stashes the constructed handle in the package-global
	// pkgCoreElicitor that launchTUIv2 later attaches to the bubble-tea
	// program — so calling it a second time would leave the TUI wired to a
	// different elicitor than the servers actually captured.
	mcpElicitor := makeMCPElicitor()
	mcpServers, mcpToolsets, mcpErr := mcp.Build(ctx, agentsDir, homeAgentsDir, send, gate, mcpElicitor, digestOpts)
	if mcpErr != nil {
		fmt.Fprintf(os.Stderr, "core-agent: mcp: %v\n", mcpErr)
	}
	// Terminate stdio MCP children on the way out (#538): SIGTERM,
	// 3s grace, SIGKILL — concurrently across servers. Without this,
	// children were orphaned at exit and died only via stdio pipe
	// closure, which leaves servers that ignore EOF running forever.
	defer mcp.CloseAll(mcpServers)
	// Status-gauge registration is deferred until after rooted subagents
	// stand up their own servers (below): mcp.RegisterMetrics creates a
	// same-named ObservableGauge each call, so it must run ONCE over the
	// combined parent + subagent-root slice (#338).
	loadedSkills, skillsErr := skills.LoadAll(ctx, agentsDir, coreHome, gate,
		skills.WithHomeAgentsSkillsDir(homeAgentsDir),
		skills.WithContentRoots(contentRoots),
		skills.WithInterpolator(envResolver.InterpolateFunc()))
	if skillsErr != nil {
		fmt.Fprintf(os.Stderr, "core-agent: skills: %v\n", skillsErr)
	}

	// #322: surface drift diagnostics AFTER instruction + skill loading
	// so ReportDrift can see every reference the bundle actually made.
	// Warnings only — never blocks startup.
	if envResolver != nil {
		for _, w := range envResolver.ReportDrift() {
			send(w)
		}
	}

	// Startup config summary (#212 part 1). Emits the resolved state
	// of every load-bearing subsystem — config source, agentsDir,
	// model+provider, MCP servers, skills, multi-session auth — so
	// operators can verify what the daemon actually loaded via a
	// grep rather than `kubectl debug` + /proc/1/root inspection.
	// Fires unconditionally at this point (both single-shot -p and
	// attach modes), independent of the attach branch further down.
	for _, line := range compose.FormatStartupSummary(compose.StartupSummaryInputs{
		CfgPath:      cfgPath,
		Cfg:          cfg,
		AgentsDir:    agentsDir,
		ProviderName: provider.Name(),
		MCPServers:   mcpServers,
		LoadedSkills: loadedSkills,
	}) {
		send(line)
	}

	allToolsets := append([]adktool.Toolset{}, mcpToolsets...)
	if !loadedSkills.Empty() {
		allToolsets = append(allToolsets, loadedSkills.Toolset)
	}

	// Built-in tools (read_file, write_file, edit_file, list_dir,
	// bash, todo) ship on by default. --no-builtin-tools disables
	// the whole suite; --disable-tools / cfg.tools.disable turn off
	// specific entries (composed by union).
	var builtinTools []adktool.Tool
	if !noBuiltinTools {
		b := tools.Default()
		for _, name := range cfg.Tools.Disable {
			if err := b.Disable(name); err != nil {
				fmt.Fprintf(os.Stderr, "core-agent: config tools.disable: %v\n", err)
				return runner.ExitConfigError
			}
		}
		for _, name := range strings.Split(disableTools, ",") {
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			if err := b.Disable(name); err != nil {
				fmt.Fprintf(os.Stderr, "core-agent: --disable-tools: %v\n", err)
				return runner.ExitConfigError
			}
		}
		// Task-profile disables (#160) come last and are already
		// filtered by --enable-tools. Names are profile-authored, so a
		// failure here is a table bug, not operator input — surface it
		// as such.
		for _, name := range profileDisables {
			if err := b.Disable(name); err != nil {
				fmt.Fprintf(os.Stderr, "core-agent: task class %q profile: %v\n", cfg.Session.TaskClass, err)
				return runner.ExitConfigError
			}
		}
		reg, err := tools.Build(cfg, gate, agentsDir, b)
		if err != nil {
			fmt.Fprintf(os.Stderr, "core-agent: built-in tools: %v\n", err)
			return runner.ExitConfigError
		}
		builtinTools = reg.Tools
		// Build taught the *derived* gate which native search tools
		// exist (#158). Sessions created later through POST /sessions
		// derive from the template, not from this gate, and a session
		// gate that didn't know would fall back to "assume registered"
		// and refuse a bash grep by naming a tool this build dropped.
		template.SetNativeSearchTools(gate.NativeSearchTools())
	}

	askTool, err := resolveAskUserTool(ask, os.Stdin, os.Stderr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "core-agent: %v\n", err)
		return runner.ExitConfigError
	}
	if askTool != nil {
		builtinTools = append(builtinTools, askTool)
	}

	// Daily pricing refresh (PR B): pull LiteLLM's pricing JSON
	// into ~/.core-agent/pricing.json's external section. Skipped
	// when --no-pricing-refresh is set, when cfg.pricing.refresh is
	// false, or when coreHome is empty (no place to cache). Network
	// failures are non-fatal — existing cache stays in place; the
	// refresher's stderr line tells the operator the rates may be
	// stale ("using N-day-old cache; network: …").
	refreshPricing := !noPricingRefresh && coreHome != ""
	if cfg.Pricing.Refresh != nil && !*cfg.Pricing.Refresh {
		refreshPricing = false
	}
	if refreshPricing {
		outcome, perr := pricing.Refresh(ctx, coreHome, pricing.RefreshOptions{
			Source: cfg.Pricing.Source,
		})
		if perr != nil {
			fmt.Fprintf(os.Stderr, "core-agent: pricing refresh: %v\n", perr)
		} else {
			compose.DescribeRefresh(os.Stderr, outcome)
		}
	}

	// Install the layered pricing catalog before any cost lookups
	// happen. Per docs/pricing-design.md:
	//   cfg.Model.Pricing override → .agents/pricing.json
	//   → ~/.core-agent/pricing.json (manual + external)
	//   → compiled-in builtin → longest-prefix → unknown.
	// PR C adds /pricing refresh + /pricing set slash commands.
	if catalog, perr := pricing.NewCatalog(pricing.Options{
		CfgOverride: compose.CfgToCatalogOverride(cfg.Model.Pricing),
		AgentsDir:   agentsDir,
		UserHome:    coreHome,
	}); perr != nil {
		fmt.Fprintf(os.Stderr, "core-agent: pricing: %v\n", perr)
		// Non-fatal: missing/corrupt files fall back to builtin via
		// usage.PriceFor's no-catalog path. Just continue.
	} else {
		usage.SetCatalog(catalog)
	}

	// Resolve the parent model's pricing rate now that the layered
	// catalog is installed. tracker was hoisted earlier for symmetry
	// with the LLMFallback + agentRef late-binding above.
	pricingRate := usage.PriceFor(cfg.Model.Name, cfg)

	// Background subagent spawning. Constructed before agent.New so
	// the spawn tools can be registered alongside the built-in tools.
	// Manager is attached to the parent agent inside agent.New via
	// WithBackgroundManager; the agent's pre-turn alert drain
	// surfaces background reports to the parent's model.
	var bgMgr *background.Manager
	// bgRecipe carries the same recipe down to every multi-session
	// session so each gets its OWN manager (#637). Stays zero (and so
	// inert) under --no-background-agents.
	var bgRecipe sessionBackgroundRecipe
	if !noBackgroundAgents {
		var err error
		// allow_adhoc gates inline-persona spawns (the model authoring a
		// fresh system_prompt at spawn time). Off for the attach-only
		// daemon (--no-repl), which has no operator steering it and should
		// only spawn operator-vetted subagents; on for interactive REPL
		// and one-shot -p runs, preserving the pre-#626 behavior where any
		// spawn_agent call could author a persona inline. Referencing a
		// preconfigured/declarative subagent by name works regardless.
		allowAdhoc := !noREPL
		// The small-tier model the "small" per-spawn model override resolves
		// to — same resolution the agentic subtasks use below.
		bgSmallModel := models.ResolveSmallModel(provider, agenticSmallModel)
		bgMgr, err = background.NewManager(
			background.WithProvider(provider, cfg.Model.Name),
			background.WithGate(gate),
			background.WithCatalog(builtinTools),
			background.WithAllowAdhoc(allowAdhoc),
			background.WithSmallModelID(bgSmallModel),
			// A synchronous spawn (spawn_agent {wait: true}) holds the
			// parent turn open, so cap it tighter than the async
			// fire-and-continue wall-clock; on timeout the subagent keeps
			// running and its result is pushed on a later turn (#626/D5).
			background.WithSyncWaitTimeout(defaultSyncWaitTimeout),
		)
		if err != nil {
			fmt.Fprintf(os.Stderr, "core-agent: background agents: %v\n", err)
			return runner.ExitConfigError
		}
		defer func() {
			// Close is bounded internally (closeDrainTimeout); a
			// non-nil error means stragglers were abandoned — log
			// them so a wedged-subagent pattern is visible instead
			// of silently absorbed into pod-restart latency.
			if err := bgMgr.Close(); err != nil {
				fmt.Fprintf(os.Stderr, "core-agent: shutdown: %v\n", err)
			}
		}()
		spawnTools := background.NewSpawnTools(bgMgr)
		builtinTools = append(builtinTools, spawnTools...)
		bgRecipe = sessionBackgroundRecipe{
			provider:       provider,
			smallModelID:   bgSmallModel,
			allowAdhoc:     allowAdhoc,
			syncWait:       defaultSyncWaitTimeout,
			spawnToolNames: make(map[string]struct{}, len(spawnTools)),
			live:           newSessionManagerSet(),
		}
		defer func() {
			// Multi-session sessions each own a manager; drain them on the
			// way out exactly like the daemon's own above, so a SIGTERM
			// cancels their in-flight subagents instead of dropping them
			// mid-tool-call at process exit.
			bgRecipe.live.closeAll()
		}()
		for _, t := range spawnTools {
			bgRecipe.spawnToolNames[t.Name()] = struct{}{}
		}
	}

	// retrieve_raw built-in: model-facing escape hatch to fetch back
	// the un-digested MCP payload when a digest looks suspicious
	// (docs/digest-design.md CCR store). Registered only when the
	// digest wrap is on AND we have a store to back it — otherwise
	// every call would return "no raw payload stored," which just
	// confuses the model.
	//
	// The LazyStore's inner delegate is bound below, once the
	// eventlog handle is open. Registering the tool here (with the
	// LazyStore) means retrieve_raw becomes usable the moment the
	// binding fires.
	if digestStore != nil {
		rtTool, err := tools.NewRetrieveRawTool(tools.RetrieveRawOptions{Store: digestStore})
		if err != nil {
			fmt.Fprintf(os.Stderr, "core-agent: retrieve_raw: %v\n", err)
			return runner.ExitConfigError
		}
		builtinTools = append(builtinTools, rtTool)
	}

	// Agentic tool wrappers (docs/context-management-design.md
	// Mechanism B). On by default since v2.1; disable via
	// --agentic-tools=false. Each wrapper routes its operation
	// through Agent.RunSubtask so only the digest reaches the
	// parent's context — raw tool output stays in the subtask.
	// Late-bound *Agent via agentRef closure; agentRef was hoisted
	// above mcp.Build (the MCP digest LLM fallback needs the same
	// late binding) and is populated inside the WithPostConstruct
	// hook below. The inner tools the subtask runs are pulled from
	// builtinTools by canonical name, so the subtask shares the
	// parent's gate + output caps.
	if agenticTools {
		resolvedSmallModel := models.ResolveSmallModel(provider, agenticSmallModel)
		switch {
		case resolvedSmallModel == "":
			send(fmt.Sprintf("agentic subtasks: inherit parent (%s — no cheap-tier default for provider %q)", cfg.Model.Name, provider.Name()))
		case agenticSmallModel != "":
			send(fmt.Sprintf("agentic subtasks: %s (operator override)", resolvedSmallModel))
		default:
			send(fmt.Sprintf("agentic subtasks: %s (provider default)", resolvedSmallModel))
		}
		agTools, err := compose.BuildAgenticTools(builtinTools, func() *agent.Agent { return agentRef }, provider, resolvedSmallModel)
		if err != nil {
			fmt.Fprintf(os.Stderr, "core-agent: agentic tools: %v\n", err)
			return runner.ExitConfigError
		}
		builtinTools = append(builtinTools, agTools...)
	}

	// System-prompt layer resolution (#459): memory enters as layer 4
	// (AFTER the core — the intended precedence flip from the old
	// prefix arrangement); operator append is layer 5; a full-replace
	// file skips layers 1–3. Flags beat config.
	appendPrompt := cfg.Agent.AppendSystemPrompt
	if promptCfg.appendSystemPrompt != "" {
		appendPrompt = promptCfg.appendSystemPrompt
	}
	if strings.HasPrefix(appendPrompt, "@") {
		raw, err := os.ReadFile(strings.TrimPrefix(appendPrompt, "@")) //nolint:gosec // operator-supplied CLI flag naming a file on the operator's own machine — reading it is the feature, same trust model as --system-prompt-file and every other path flag
		if err != nil {
			fmt.Fprintf(os.Stderr, "core-agent: --append-system-prompt: %v\n", err)
			return runner.ExitConfigError
		}
		appendPrompt = string(raw)
	}
	replaceFile := cfg.Agent.SystemPromptFile
	if promptCfg.systemPromptFile != "" {
		replaceFile = promptCfg.systemPromptFile
	}
	var replacePrompt string
	if replaceFile != "" {
		raw, err := os.ReadFile(replaceFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "core-agent: --system-prompt-file: %v\n", err)
			return runner.ExitConfigError
		}
		replacePrompt = string(raw)
		fmt.Fprintln(os.Stderr, "core-agent: system prompt replaced from file — the built-in harness contract (compaction framing, tool dispatch rules) is NOT included; tool-use degradation is on you")
	}

	// Declarative subagents (docs/declarative-subagents-design.md): build
	// each config-declared subagent into a callable *agent.Agent before
	// assembling the parent options, then register them via
	// agent.WithSubagents below. Constructed after builtinTools/mcpServers/
	// loadedSkills are final so a subagent can inherit the parent's full
	// surface whole (nil refs) or take a name-scoped subset (inline refs).
	mcpNamed := make([]namedToolset, 0, len(mcpServers))
	for _, s := range mcpServers {
		if s == nil {
			continue
		}
		mcpNamed = append(mcpNamed, namedToolset{name: s.Name, toolset: s.Toolset()})
	}
	declaredSubagents, subagentTemplates, subagentServers, err := buildDeclaredSubagents(ctx, cfg, provider, projectRoot, parentSurface{
		builtinTools: builtinTools,
		mcpToolsets:  mcpNamed,
		skills:       loadedSkills,
	}, subagentDeps{
		gate:       gate,
		elicitor:   mcpElicitor,
		digestOpts: digestOpts,
		interp:     envResolver.InterpolateFunc(),
		send:       send,
		rootBase:   contentRootBase,
	})
	// Terminate any stdio children the rooted subagents' servers own on the
	// way out — set before the error check so a partial failure still cleans
	// up whatever started (buildDeclaredSubagents returns them on error too).
	defer mcp.CloseAll(subagentServers)
	if err != nil {
		fmt.Fprintf(os.Stderr, "core-agent: subagents: %v\n", err)
		return runner.ExitConfigError
	}

	// Register the declarative subagents as async-spawn templates on the
	// background manager, so the same subagent the parent can call
	// synchronously (agent.WithSubagents, below) is also spawnable by
	// reference via spawn_agent {agent: "<name>"} (#626, option B). Done
	// here — after the builder produced them — because the manager (and
	// its spawn tools) had to exist first.
	if bgMgr != nil && len(subagentTemplates) > 0 {
		if err := bgMgr.SetSubagentTemplates(subagentTemplates); err != nil {
			fmt.Fprintf(os.Stderr, "core-agent: subagents: register async templates: %v\n", err)
			return runner.ExitConfigError
		}
		// Same roster for every multi-session session's own manager.
		bgRecipe.templates = subagentTemplates
	}

	// One status-gauge registration over every MCP server this process owns
	// — parent plus rooted-subagent servers. RegisterMetrics registers a
	// same-named ObservableGauge per call, so registering the combined slice
	// exactly once avoids duplicate-instrument errors (#338).
	if allServers := append(append([]*mcp.Server{}, mcpServers...), subagentServers...); len(allServers) > 0 {
		if _, err := mcp.RegisterMetrics(otel.GetMeterProvider(), allServers); err != nil {
			fmt.Fprintf(os.Stderr, "core-agent: metrics: register mcp observer: %v\n", err)
			return runner.ExitConfigError
		}
	}

	opts := []agent.Option{
		agent.WithTools(builtinTools),
		agent.WithToolsets(allToolsets),
		agent.WithUserInstruction(loaded.Instruction),
		agent.WithGate(gate),
		// One source of truth for the agent's one-line description:
		// .agents/config.json's `agent.description`. Flows to both
		// ADK's system prompt (this WithDescription) and the
		// /.well-known/agent-card.json card (via resolveAgentCardConfig
		// below, which uses cfg.Agent.Description as the default for
		// AgentCardConfig.Description).
		agent.WithDescription(cfg.Agent.Description),
		// Share the usage.Tracker the host already keeps (for /stats,
		// per-turn cost footer, status sidebar). Agent-level callers
		// — chiefly the compactor's threshold check — read context-
		// window state from this same tracker so there's one source
		// of truth.
		agent.WithUsageTracker(tracker),
	}
	if len(declaredSubagents) > 0 {
		opts = append(opts, agent.WithSubagents(declaredSubagents))
	}
	if replacePrompt != "" {
		opts = append(opts, agent.WithInstruction(replacePrompt))
	}
	if appendPrompt != "" {
		opts = append(opts, agent.WithExtraInstruction(appendPrompt))
	}
	// Attach-extras snapshot funcs, collected separately since the
	// pkg/agent split (#388 phase 4): they configure the
	// *attachadapter.Adapter that wraps the agent at construction
	// time, not the agent itself. The adapter satisfies the
	// MemoryProvider / SkillsProvider / MCPProvider interfaces via
	// these closures, so the remote /memory /skills /mcp endpoints
	// return the same state the in-process TUI sees.
	adapterOpts := []attachadapter.Option{
		attachadapter.WithMemoryProvider(func() []attach.MemorySource {
			// Re-walk on every call so a fresh AGENTS.md / CLAUDE.md /
			// GEMINI.md picked up between turns (or written by the
			// agent itself) surfaces without a daemon restart. Cheap
			// — a few file stats + reads of small files capped at
			// 32 KiB each.
			fresh, _ := instruction.Load(projectRoot, coreHome, instruction.WithHomeAgentsRoot(homeAgentsDir), instruction.WithContentRoots(contentRoots))
			out := make([]attach.MemorySource, 0, len(fresh.Sources)+1)
			// First row: which system-prompt layers are active (#459)
			// so operators can see the assembled shape from /memory
			// without reading code.
			out = append(out, attach.MemorySource{Scope: "system-prompt", Path: describePromptLayers(cfg.Model.Name, replacePrompt != "", appendPrompt != "", len(fresh.Sources))})
			for _, s := range fresh.Sources {
				out = append(out, attach.MemorySource{Scope: s.Scope, Path: s.Path, Size: s.Bytes})
			}
			return out
		}),
		attachadapter.WithSkillsProvider(func() []attach.SkillInfo {
			// Re-walk on every call so newly-dropped SKILL.md bundles
			// surface without restart. The merge across project +
			// user-global sources happens inside skills.LoadAll.
			fresh, err := skills.LoadAll(ctx, agentsDir, coreHome, gate, skills.WithHomeAgentsSkillsDir(homeAgentsDir), skills.WithContentRoots(contentRoots))
			if err != nil {
				return nil
			}
			out := make([]attach.SkillInfo, 0, len(fresh.Infos))
			for _, s := range fresh.Infos {
				out = append(out, attach.SkillInfo{Name: s.Name, Description: s.Description})
			}
			return out
		}),
		attachadapter.WithPricingProvider(func() attach.PricingInfo {
			// Re-resolve on every call so a fresh /pricing refresh
			// during the session is reflected immediately — pricingRate
			// captured at startup would go stale. Also lets Source +
			// UpdatedAt reflect wherever the winning layer landed.
			currentRate, source := usage.PriceForWithSource(cfg.Model.Name, cfg)
			info := attach.PricingInfo{
				CurrentModel: cfg.Model.Name,
				KnownModels:  usage.KnownModelsCount(),
				Source:       source,
			}
			if !currentRate.IsZero() {
				info.Current = &attach.ModelPricing{
					InputUSDPerMTok:  currentRate.InputPerMTok,
					OutputUSDPerMTok: currentRate.OutputPerMTok,
					CachedUSDPerMTok: currentRate.CachedInputPerMTok,
					UpdatedAt:        currentRate.UpdatedAt,
				}
			}
			return info
		}),
		attachadapter.WithRefreshPricer(func(ctx context.Context) (attach.PricingRefreshResponse, error) {
			if coreHome == "" {
				return attach.PricingRefreshResponse{}, fmt.Errorf("pricing refresh: $HOME unavailable, no user file to write")
			}
			summary, err := compose.RefreshPricing(ctx, cfg, agentsDir, coreHome)
			if err != nil {
				return attach.PricingRefreshResponse{}, err
			}
			return attach.PricingRefreshResponse{
				Updated:     true,
				LastRefresh: time.Now(),
				Detail:      summary,
			}, nil
		}),
		attachadapter.WithPricingSetter(func(req attach.PricingSetRequest) error {
			if coreHome == "" {
				return fmt.Errorf("pricing set: $HOME unavailable, no user file to write")
			}
			_, err := compose.SetPricing(cfg, agentsDir, coreHome, req.Model, req.InputUSDPerMTok, req.OutputUSDPerMTok)
			return err
		}),
		attachadapter.WithReloader(func(_ context.Context) attach.ReloadResponse {
			// Best-effort re-walks: instruction + skills snapshots
			// are reported per-surface so the operator sees which
			// parts parsed cleanly after a .agents/ edit. MCP server
			// lifecycle restart + system-prompt rebuild would require
			// reconstructing the running agent (tracked separately);
			// for now MCP comes back as a configuration-only re-read
			// note.
			out := attach.ReloadResponse{}
			if _, err := instruction.Load(projectRoot, coreHome, instruction.WithHomeAgentsRoot(homeAgentsDir), instruction.WithContentRoots(contentRoots)); err != nil {
				out.Errors = append(out.Errors, fmt.Sprintf("memory: %v", err))
			} else {
				out.Memory = true
			}
			if _, err := skills.LoadAll(ctx, agentsDir, coreHome, gate, skills.WithHomeAgentsSkillsDir(homeAgentsDir), skills.WithContentRoots(contentRoots)); err != nil {
				out.Errors = append(out.Errors, fmt.Sprintf("skills: %v", err))
			} else {
				out.Skills = true
			}
			// MCP: confirm the on-disk config still parses; surface
			// the limitation so the operator doesn't expect a live
			// server restart.
			if _, err := mcp.LoadAll(agentsDir, homeAgentsDir); err != nil {
				out.Errors = append(out.Errors, fmt.Sprintf("mcp config: %v", err))
			}
			out.MCP = false
			out.Errors = append(out.Errors, "mcp: live server restart requires daemon restart (tracked for v2.3)")
			return out
		}),
		attachadapter.WithReplanner(func(_ context.Context, _ attach.ReplanRequest) (attach.ReplanResponse, error) {
			// Wired unconditionally; the agent-side handler 501s
			// the slash when require_plan_artifact is off
			// (RevokeLatestPlan returns "" with no error and the
			// gate flag was never set, so the response just says
			// "no plan to revoke").
			if agentsDir == "" {
				return attach.ReplanResponse{
					Message: "/replan unavailable: no .agents/ directory resolved (plan artifacts have nowhere to live)",
				}, nil
			}
			archived, err := tools.RevokeLatestPlan(gate, agentsDir)
			if err != nil {
				return attach.ReplanResponse{}, err
			}
			resp := attach.ReplanResponse{
				ArchivedPath:  archived,
				PlanWasActive: archived != "",
			}
			if archived == "" {
				resp.Message = "/replan: no active plan to revoke (gate flag is clear)."
			} else {
				resp.Message = fmt.Sprintf("Plan revoked. Archived to %s. The next mutating tool call will be denied until the agent calls record_plan again.", archived)
			}
			return resp, nil
		}),
		attachadapter.WithMCPProvider(func() attach.MCPInfo {
			servers := make([]attach.MCPServerInfo, 0, len(mcpServers))
			for _, s := range mcpServers {
				tools := make([]attach.MCPToolInfo, 0, len(s.ToolInfos))
				for _, t := range s.ToolInfos {
					tools = append(tools, attach.MCPToolInfo{Name: t.Name, Description: t.Description})
				}
				// pkg/mcp uses "ok" / "error" internally; the attach
				// wire format documents "running" / "starting" /
				// "failed" / "stopped". Map them here so the remote
				// TUI's coretui projection (Connected = Status ==
				// "running") works as intended.
				status := "running"
				if s.Status == mcp.StatusError {
					status = "failed"
				}
				servers = append(servers, attach.MCPServerInfo{
					Name:      s.Name,
					Status:    status,
					Transport: "", // not surfaced on mcp.Server today
					Tools:     tools,
				})
			}
			return attach.MCPInfo{Servers: servers}
		}),
	}
	if bgMgr != nil {
		opts = append(opts, agent.WithBackgroundManager(bgMgr))
	}
	// Context-window compaction (docs/context-management-design.md
	// Mechanism A). Default-on; disable via --no-compact. Post-turn
	// hook checks DefaultCompactor.ShouldCompact (threshold 0.85)
	// and flags the next Run for pre-turn compaction. /compact slash
	// remains available regardless of this flag — disabling only
	// turns off the automatic trigger.
	if !noCompact {
		opts = append(opts, agent.WithCompactor(compose.BuildCompactor(cfg.Compaction)))
	}
	// Task-boundary checkpoints (docs/context-management-design.md
	// Mechanism C). Default-on; disable via --no-checkpoint.
	// Registers the mark_task_done model-facing tool + enables the
	// /done slash; the model can self-signal task completion at
	// natural boundaries, and the next Run drains the pending
	// checkpoint by writing a richer handover record.
	// Runaway backstops: the behavioral watchdog (#123/#623) and the
	// cost ceilings (#145). Resolution is a pure function so the
	// per-mode default posture is table-tested — see guardrails.go.
	//
	// "Unattended" is the load-bearing input (#642): -p one-shot, a
	// --no-repl daemon, or a piped stdin all mean nobody is watching
	// the alert stream, so warn-mode is indistinguishable from off and
	// an un-ceilinged session can spend without anyone noticing.
	unattended := prompt != "" || noREPL || !term.IsTerminal(int(os.Stdin.Fd()))
	guard, err := resolveGuardrails(guardrailInputs{
		WatchdogFlag:       guardrails.watchdogMode,
		WatchdogConfig:     cfg.Safety.Watchdog,
		SessionCostFlag:    guardrails.maxSessionCostUSD,
		SessionCostFlagSet: guardrails.maxSessionCostSet,
		SessionCostConfig:  cfg.Agent.MaxSessionCostUSD,
		Unattended:         unattended,
	})
	if err != nil {
		log.Printf("%v", err)
		return runner.ExitConfigError
	}
	// Cost-ceiling kill switch (#145). CLI flag > config field >
	// unattended default > disabled. The per-turn bound keeps its
	// simpler "positive value wins" rule — it has no mode default, so
	// an explicit 0 and an unset flag mean the same thing.
	if guardrails.maxTurnCostUSD > 0 {
		cfg.Agent.MaxTurnCostUSD = &guardrails.maxTurnCostUSD
	}
	cfg.Agent.MaxSessionCostUSD = &guard.SessionCostUSD
	ceiling := agent.CostCeiling{MaxSessionUSD: guard.SessionCostUSD}
	if cfg.Agent.MaxTurnCostUSD != nil {
		ceiling.MaxTurnUSD = *cfg.Agent.MaxTurnCostUSD
	}
	if ceiling.MaxTurnUSD > 0 || ceiling.MaxSessionUSD > 0 {
		opts = append(opts, agent.WithCostCeiling(ceiling))
		// 0 means "no ceiling", so print that rather than "$0.0000",
		// which reads like a ceiling that trips on the first token.
		sessionCeiling := "disabled"
		if ceiling.MaxSessionUSD > 0 {
			sessionCeiling = fmt.Sprintf("$%.4f", ceiling.MaxSessionUSD)
		}
		send(fmt.Sprintf("cost ceiling: per-turn=$%.4f per-session=%s [session ceiling from %s] (refuses new turns when exceeded; clear with /guardrail reset, or POST /sessions/{id}/guardrails/reset — a per-session trip needs +budget)", ceiling.MaxTurnUSD, sessionCeiling, guard.SessionCostSource))
	}
	// Behavioral watchdog (#123 PR 2), a ladder rather than a set of
	// alternatives: "warn" observes and logs to the operator, "feedback"
	// adds the model-facing injection (#159), "enforce" adds the halt
	// (#623). Options are accumulated in that order so the stronger
	// modes can't drift from the weaker ones they contain.
	if guard.Watchdog == config.WatchdogOff {
		send(fmt.Sprintf("watchdog: off [%s] (no runaway-pattern observation)", guard.WatchdogSource))
	} else {
		w := watchdog.NewDefaultWatchdog()
		opts = append(opts, agent.WithWatchdog(w, func(a watchdog.Alert) {
			send(fmt.Sprintf("watchdog %s", a.String()))
		}))
		detail := "observes tool-call stream; logs structured alerts on runaway patterns"
		if guard.Watchdog == config.WatchdogFeedback || guard.Watchdog == config.WatchdogEnforce {
			opts = append(opts, agent.WithWatchdogFeedback())
			detail += "; injects the observation into the model's next turn"
		}
		if guard.Watchdog == config.WatchdogEnforce {
			opts = append(opts, agent.WithWatchdogEnforce())
			detail += "; halts the agent and refuses new turns until cleared with /guardrail reset, or POST /sessions/{id}/guardrails/reset"
		}
		send(fmt.Sprintf("watchdog: %s mode [%s] (%s)", guard.Watchdog, guard.WatchdogSource, detail))
	}
	// Bash search gate (#158). Reported on every boot, including the
	// default: "which guardrails are actually armed" is the question
	// this milestone exists to make answerable, and a posture that
	// only prints when weakened is one an operator has to infer.
	// Silent when bash isn't registered at all — there is nothing to
	// gate, and a line about it would be its own small false claim.
	if hasToolNamed(builtinTools, "bash") {
		// The binary list is read back off the gate rather than
		// hand-kept, so it reflects both the gated set and the natives
		// this build actually registered — the line can't advertise a
		// bigger gate than the one that's armed. Empty means enforce
		// would refuse nothing, which is worth saying out loud.
		active := gate.ActiveSearchBinaries()
		gated := strings.Join(active, "/")
		// Name the natives this build kept rather than a literal
		// "grep/glob": with only one of them registered the other half
		// of that phrase is advice the operator can't follow.
		nativeList := gate.ActiveNativeSearchTools()
		natives := strings.Join(nativeList, "/") + " tool"
		if len(nativeList) > 1 {
			natives += "s"
		}
		switch {
		case gate.BashSearchGate() == config.BashSearchGateAllow:
			send("bash search gate: allow (search-shaped bash commands are not gated)")
		case len(active) == 0:
			send(fmt.Sprintf("bash search gate: %s but INERT (no native grep/glob tool is registered, so there is nothing to redirect a search-shaped bash command to)", gate.BashSearchGate()))
		case gate.BashSearchGate() == config.BashSearchGateWarn:
			send(fmt.Sprintf("bash search gate: warn (bash %s still run, but the result carries a pointer to the native %s)", gated, natives))
		default:
			send(fmt.Sprintf("bash search gate: enforce (bash %s refused; use the native %s. --bash-search-gate=allow to disable)", gated, natives))
		}
	}
	if !noCheckpoint {
		opts = append(opts, agent.WithCheckpointer(agent.NewDefaultCheckpointer()))
	}
	// Config-driven hook dispatch (pkg/hooks). Fires operator-configured
	// shell commands on tool/model/turn boundaries. Most consumers won't
	// set cfg.Hooks — the dispatcher becomes a no-op and we skip wiring
	// so the tap loop stays hot-path-free. Primary consumer: Scion, via
	// `sciontool hook --dialect=core-agent`.
	//
	// session_id is left empty in the envelope: the agent's session ID
	// isn't known until agent.New returns (a.SessionID()), and the
	// primary consumer (Scion) doesn't require it. Late-binding via
	// WithPostConstruct is a follow-up if a consumer asks for the
	// correlation.
	if hookDispatcher := hooks.New(cfg.Hooks, "", os.Stderr, gate); !hookDispatcher.Empty() {
		opts = append(opts, agent.WithEventHook(hookDispatcher.OnEvent, hookDispatcher.OnTurnEnd))
		send(fmt.Sprintf("hooks: %d event(s) wired", len(cfg.Hooks)))
	}
	// Late-bind agentRef for the agentic tool wrappers (Mechanism
	// B). The wrappers were registered above with an AgentGetter
	// closure that captures &agentRef; once agent.New finishes
	// constructing the *Agent, this hook fires so the closure
	// resolves to a non-nil pointer on the model's first call. No-
	// op when --agentic-tools was off (agentRef is unused but
	// captured into a closure that nothing ever invokes).
	// Durable sessions + audit log. Either flag enables: --session-db
	// alone uses the default path (~/.<binary>/sessions.db);
	// --session-db-path enables and overrides the path. Off by default
	// to preserve historical CLI behavior (in-memory, ephemeral).
	//
	// handle is hoisted to the outer scope so the multi-session
	// SessionFactory closure below (which constructs on-demand
	// agents from POST /sessions) can reference the same eventlog
	// without re-opening it. Declared here so the PostConstruct
	// closure below can capture it before the eventlog block runs.
	var eventlogHandle *eventlog.Handle

	opts = append(opts, agent.WithPostConstruct(func(a *agent.Agent) {
		agentRef = a
		// Stamp the primary session's identity into the metrics
		// observer so cumulative counters get session.id + app.name +
		// user.id attributes from the first turn onward. Before this
		// point the tracker has no recorded turns, so the observer
		// callback produces no data — the empty-identity window is
		// harmless.
		primaryTracker.SetIdentity(a)
		// Bind the MCP digest LazyStore now that the agent knows its
		// session ID. EventlogStore is session-scoped, so it can't be
		// constructed at mcp.Build time (session ID = empty). Binding
		// here lights up retrieve_raw against the correct session
		// from the model's first tool call onward.
		//
		// Non-fatal if it fails: the digest wrap continues without a
		// store, retrieve_raw returns "no raw payload stored" — the
		// model handles this cleanly per the tool's error contract.
		if digestStore != nil && eventlogHandle != nil {
			es, err := digest.NewEventlogStore(eventlogHandle, a.AppName(), a.UserID(), a.SessionID())
			if err != nil {
				fmt.Fprintf(os.Stderr, "core-agent: mcp digest store: %v (retrieve_raw disabled)\n", err)
				return
			}
			digestStore.Set(es)
			// Positive log — surfaces to the same stderr operators
			// grep at startup, symmetric with the failure line above.
			// Without this, a healthy digest wire looked identical to
			// "wrap disabled" during the 2026-07-15 demo drive; the
			// only way to confirm was to inspect /tools for
			// retrieve_raw.
			fmt.Fprintf(os.Stderr, "core-agent: mcp digest store: bound EventlogStore for session %s (retrieve_raw enabled)\n", a.SessionID())
		}
	}))
	if sessionDB || sessionDBPath != "" {
		path, err := resolveSessionDBPath(sessionDBPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "core-agent: --session-db-path: %v\n", err)
			return runner.ExitConfigError
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "core-agent: session db dir: %v\n", err)
			return runner.ExitConfigError
		}
		// WithMetadataExtractor wires the auth-aware extractor that
		// reads auth.Caller / proxy_by off the request context and
		// stamps them onto each eventlog row's Metadata sidecar (see
		// docs/multi-session-design.md). In single-user mode the
		// extractor returns nil maps (no Caller on context) → no
		// sidecar JSON is written; multi-session deployments get
		// per-event identity threading in the audit log automatically.
		handle, err := eventlog.Open(ctx, sqlite.Open(path),
			eventlog.WithMetadataExtractor(agent.EventlogMetadataExtractor()),
		)
		if err != nil {
			fmt.Fprintf(os.Stderr, "core-agent: open session db %s: %v\n", path, err)
			return runner.ExitConfigError
		}
		defer func() { _ = handle.Close() }()
		// On Gemini/Vertex, wrap the session.Service with the
		// GoogleSearch grounding projection so queries + grounded
		// sources land as queryable rows in the eventlog
		// (Author="gemini/google_search") alongside the original
		// model event that carried the grounding metadata.
		switch cfg.Model.Provider {
		case config.ProviderGemini, config.ProviderVertex:
			handle.Service = gemini.GroundingProjection(handle.Service)
		}
		opts = append(opts, agent.WithEventLog(handle))
		eventlogHandle = handle
		fmt.Fprintf(os.Stderr, "core-agent: session db: %s\n", path)

		// digestStore is bound below, after agent.New — the
		// EventlogStore constructor rejects empty session identity
		// and the session ID isn't known until ADK assigns one
		// inside agent.New. Deferred to the post-construct hook.
	}

	// Auto-continue config (#539, #558, #559): resolved once here,
	// consumed by two triggers — the multi-session lazy/boot-scan path
	// inside the attach block, and the --no-repl startup-session path
	// below it. resolveAutoContinue owns the precondition-gated default
	// (unset → on for daemons with a durable eventlog, off elsewhere),
	// the explicit-false opt-out, the explicit-true warn-and-ignore, and
	// the freshness / retry-interval duration parsing. Interactive modes
	// (REPL/TUI) never enable it: a human is present there by definition.
	acRes, acErr := resolveAutoContinue(cfg.Agent.AutoContinue, cfg.Attach.MultiSession.Enabled, noREPL, eventlogHandle != nil, os.Stderr)
	if acErr != nil {
		fmt.Fprintf(os.Stderr, "core-agent: %v\n", acErr)
		return runner.ExitConfigError
	}
	autoContinueEnabled := acRes.enabled
	autoContinueFreshness := acRes.freshness
	autoContinueRetry := acRes.retry
	autoContinueRetryInterval := acRes.retryInterval
	// In-lifetime auto-continue retry driver lifecycle (#575 defect B).
	// The driver(s) get a dedicated child context cancelled in the defer
	// below, and the WaitGroup is joined there too. Registered AFTER the
	// eventlog handle's Close defer (above) so LIFO ordering stops the
	// drivers and joins them BEFORE the eventlog closes — a mid-tick
	// RecordBoot / resume must never race DB teardown (clean-shutdown
	// milestone). Cancelling our own child ctx (not just relying on the
	// parent) means Wait() can never deadlock on an early return.
	autoContinueDriverCtx, stopAutoContinueDrivers := context.WithCancel(ctx)
	var autoContinueWG sync.WaitGroup
	defer func() {
		stopAutoContinueDrivers()
		autoContinueWG.Wait()
	}()

	// Attach-mode wiring. Must come after the eventlog is set up
	// (broadcaster requires a Stream) and before the agent is
	// constructed (so the registry is in opts).
	// attachRegistry is non-nil exactly when attach-mode is enabled;
	// each agent-construction site below registers its adapter with
	// it. Hoisted out of the attach block so the TUI / --no-repl /
	// REPL branches (which run after the block) can see it.
	var attachRegistry *attach.SessionRegistry
	if attachCfg.Listen != "" || attachCfg.UnixSocket != "" {
		if !sessionDB && sessionDBPath == "" {
			fmt.Fprintln(os.Stderr, "core-agent: --attach-listen / --attach-unix-socket requires --session-db (broadcaster pumps from the event log)")
			return runner.ExitConfigError
		}
		// Session ACL persistence (Phase 1 of session-resume,
		// docs/session-resume-design.md). Backed by the eventlog's
		// GORM connection — no separate DB. When multi-session
		// isn't enabled, the store is still wired but RegisterOwned
		// is never called (the legacy Register path doesn't
		// persist), so the table stays empty and there's no cost.
		var aclStore attach.SessionACLStore
		if eventlogHandle != nil && eventlogHandle.DB != nil {
			s, err := attach.NewSessionACLStore(ctx, eventlogHandle.DB)
			if err != nil {
				fmt.Fprintf(os.Stderr, "core-agent: session ACL store: %v\n", err)
				return runner.ExitConfigError
			}
			aclStore = s
		}
		attachReg := attach.NewSessionRegistryWithStore(aclStore)
		// Every construction site below (TUI, --no-repl, REPL
		// fallback) wraps the agent in an attachadapter.Adapter and
		// registers it here — registration moved out of agent.New
		// with the pkg/agent split (#388 phase 4).
		attachRegistry = attachReg
		// Attach-created sessions join the usage + agent metrics
		// observers through the registry adapters (#338 — closes the
		// "only the primary session is observed" gap). primaryTracker
		// dedups the primary session, which registers in both places.
		primaryTracker.SetRegistry(compose.RegistryTrackerProvider(attachReg))
		primaryTracker.SetAgentRegistry(func() []*agent.Agent { return compose.RegistryAgents(attachReg) })

		// PR D — HTTP-driven permission prompts. Construct the
		// broker now and register it as the gate's prompter so the
		// remote operator's /perms/stream subscription sees prompts
		// the daemon's tool calls trigger. The in-process TUI
		// (launchTUIv2) overrides gate.SetPrompter with its own
		// in-modal prompter when it starts, so this only takes
		// effect for headless attach-only daemons (--no-repl) or
		// when the TUI hasn't taken over yet. Defer Close so
		// pending AskApproval calls unblock with a clean error on
		// process shutdown.
		promptBroker := attach.NewPromptBroker()
		defer promptBroker.Close()
		adapterOpts = append(adapterOpts, attachadapter.WithPromptBroker(promptBroker))
		gate.SetPrompter(promptBroker)

		token := ""
		if attachCfg.TokenEnv != "" {
			token = os.Getenv(attachCfg.TokenEnv)
			if token == "" {
				fmt.Fprintf(os.Stderr, "core-agent: --attach-token=%s is empty in the environment\n", attachCfg.TokenEnv)
				return runner.ExitConfigError
			}
		}
		var peerReg *attach.PeerRegistry
		if attachCfg.PeerHub {
			peerReg = attach.NewPeerRegistry()
			defer func() { _ = peerReg.Close() }()
		}
		cardConfig, err := resolveAgentCardConfig(agentsDir, cardCfg, cfg.Agent.Description)
		if err != nil {
			fmt.Fprintf(os.Stderr, "core-agent: agent card: %v\n", err)
			return runner.ExitConfigError
		}
		// Multi-session wiring (γ of #162). When the operator enables
		// multi_session in config, the listener resolves a per-caller
		// Caller from the request, enforces per-session ACL on every
		// session-scoped handler, and runs the proxy-header path for
		// chat-bot integrations. Single-user mode (the default) leaves
		// these fields zero — the attach server behaves as it always
		// has end-to-end.
		authn, defaultCaller, authErr := compose.BuildMultiSessionAuthn(cfg.Attach.MultiSession)
		if authErr != nil {
			fmt.Fprintf(os.Stderr, "core-agent: multi-session auth: %v\n", authErr)
			return runner.ExitConfigError
		}
		// SessionFactory enables POST /sessions — on-demand creation
		// of caller-owned sessions. Only wired when multi-session is
		// enabled, since the endpoint relies on per-caller auth to
		// stamp the new session's ACL.Owner. v0 spike: substrate
		// essentials only (tools, eventlog, per-session sub-gate, per-
		// caller instruction overlay, per-session prompter). Operator
		// features have since been wired: Compactor / Checkpointer /
		// CostCeiling, Watchdog, and a per-session BackgroundManager
		// (#637). The deferrals that remain are listed on
		// compose.SessionFactoryDeps.
		var sessionFactory attach.SessionFactory
		var sessionResumer attach.SessionResumer
		var autoContinueBootScan func()
		if cfg.Attach.MultiSession.Enabled {
			factoryDeps := compose.SessionFactoryDeps{
				DaemonCtx:             ctx,
				Model:                 m,
				Template:              template,
				PricingRate:           pricingRate,
				AgentsDir:             agentsDir,
				Cfg:                   cfg,
				MCPServers:            mcpServers,
				BuiltinTools:          builtinTools,
				Toolsets:              allToolsets,
				EventlogHandle:        eventlogHandle,
				ProjectRoot:           projectRoot,
				UserRoot:              coreHome,
				HomeAgentsDir:         homeAgentsDir,
				ContentRoots:          contentRoots,
				UsersDir:              cfg.Attach.MultiSession.UsersDir,
				EnvInterp:             envResolver.InterpolateFunc(),
				Registry:              attachReg,
				ACLStore:              aclStore,
				NoCompact:             noCompact,
				NoCheckpoint:          noCheckpoint,
				WatchdogMode:          guard.Watchdog,
				AutoContinueEnabled:   autoContinueEnabled,
				AutoContinueFreshness: autoContinueFreshness,
				SessionBackground:     bgRecipe.factory(),
			}
			sessionFactory = compose.BuildSessionFactory(factoryDeps)
			// Session resume: reconstructs sessions persisted in
			// agent_session_acl that aren't in the in-memory
			// registry yet (post-daemon-restart, post-eviction).
			// nil when aclStore is nil — pre-v2.5 deployments
			// without persisted ACLs keep their legacy 404-on-miss
			// behavior. Wired into attach.NewServer's Options.Resumer
			// below.
			sessionResumer = compose.BuildSessionResumer(factoryDeps)
			// Boot-time auto-continue scan (#539 PR 2): launched after
			// the attach listener is serving (below) so continued
			// sessions are immediately attachable. No-op unless
			// auto-continue is enabled; internally guarded by the
			// crash-loop breaker in agent_boot_log.
			if autoContinueEnabled {
				maxPerBoot := 0
				if ac := cfg.Agent.AutoContinue; ac != nil {
					maxPerBoot = ac.MaxPerBoot
				}
				autoContinueBootScan = func() { compose.AutoContinueBootScan(factoryDeps, maxPerBoot) }
			}
		}
		// Resolve --ui / --ui-dir into an fs.FS. --ui-dir wins when
		// both are set (operator passed an explicit override; that's
		// the local-dev iteration path). --ui alone uses the embedded
		// bundle; if the bundle's empty (no fetch-mast-web run),
		// refuse to start so the operator notices instead of seeing
		// 404s in the browser.
		var uiAssets fs.FS
		if attachCfg.UIDir != "" {
			uiAssets = os.DirFS(attachCfg.UIDir)
			fmt.Fprintf(os.Stderr, "core-agent: --ui-dir: serving %s at /ui/\n", attachCfg.UIDir)
		} else if attachCfg.UI {
			if !webui.HasAssets() {
				fmt.Fprintln(os.Stderr, "core-agent: --ui requested but the embedded mast-web bundle is empty.")
				fmt.Fprintln(os.Stderr, "  Run `dev/tools/fetch-mast-web` before `go build` to populate internal/webui/dist/,")
				fmt.Fprintln(os.Stderr, "  or pass --ui-dir <path> to serve from a local mast-web checkout instead.")
				return runner.ExitConfigError
			}
			f, ferr := webui.FS()
			if ferr != nil {
				fmt.Fprintf(os.Stderr, "core-agent: --ui: %v\n", ferr)
				return runner.ExitConfigError
			}
			uiAssets = f
			fmt.Fprintln(os.Stderr, "core-agent: --ui: serving embedded mast-web bundle at /ui/")
		}
		// Idle-session eviction sweep (Phase 3 of session-resume).
		// Parse the operator's session_idle_timeout string:
		//   omitted / empty → default 24h
		//   explicit "0s"   → disabled (sweep never runs)
		//   any other       → parsed as time.Duration
		// Only active when multi-session is enabled AND the sweep
		// value resolves > 0; single-user daemons skip it entirely.
		var sessionIdleTimeout time.Duration
		if cfg.Attach.MultiSession.Enabled {
			raw := cfg.Attach.MultiSession.SessionIdleTimeout
			switch raw {
			case "":
				sessionIdleTimeout = 24 * time.Hour
			default:
				d, perr := time.ParseDuration(raw)
				if perr != nil {
					fmt.Fprintf(os.Stderr, "core-agent: parse session_idle_timeout=%q: %v\n", raw, perr)
					return runner.ExitConfigError
				}
				sessionIdleTimeout = d // may be 0 (disabled by design)
			}
		}
		// Per-caller cost rate limit: config-tunable since #463's
		// follow-up; nil config keeps the library defaults (burst 5,
		// 10/min per caller), which apply to every daemon.
		var costLimit attach.CostRateLimit
		if rl := cfg.Attach.CostRateLimit; rl != nil {
			costLimit = attach.CostRateLimit{
				PerMinute: rl.PerMinute,
				Burst:     rl.Burst,
				Disabled:  rl.Disabled,
			}
		}
		// Attach graceful-shutdown cap: config-tunable since #538;
		// empty keeps the library default (5s). Same duration-string
		// convention as session_idle_timeout above.
		var attachShutdownTimeout time.Duration
		if raw := cfg.Attach.ShutdownTimeout; raw != "" {
			d, perr := time.ParseDuration(raw)
			if perr != nil {
				fmt.Fprintf(os.Stderr, "core-agent: parse attach.shutdown_timeout=%q: %v\n", raw, perr)
				return runner.ExitConfigError
			}
			// NewServer promotes 0 to the library default, so an
			// explicit "0s" would silently become 5s — reject it (and
			// negatives, which would mean zero drain) instead of
			// inverting the operator's intent.
			if d <= 0 {
				fmt.Fprintf(os.Stderr, "core-agent: attach.shutdown_timeout=%q: must be > 0 (omit the field to keep the 5s default)\n", raw)
				return runner.ExitConfigError
			}
			attachShutdownTimeout = d
		}
		attachSrv, err := attach.NewServer(attach.Options{
			Registry:        attachReg,
			PeerRegistry:    peerReg,
			DaemonCtx:       ctx,
			Addr:            attachCfg.Listen,
			UnixSocket:      attachCfg.UnixSocket,
			CostRateLimit:   costLimit,
			ShutdownTimeout: attachShutdownTimeout,
			Auth: attach.AuthConfig{
				TLSCertFile:  attachCfg.TLSCert,
				TLSKeyFile:   attachCfg.TLSKey,
				ClientCAFile: attachCfg.ClientCA,
				BearerToken:  token,
				ReadOnly:     attachCfg.ReadOnly,
			},
			AgentCard:           cardConfig,
			Authenticator:       authn,
			DefaultCaller:       defaultCaller,
			MultiSessionEnabled: cfg.Attach.MultiSession.Enabled,
			AllowAnonymous:      cfg.Attach.MultiSession.AllowAnonymous,
			ProxyHeader:         cfg.Attach.MultiSession.AssertedCallerHeader,
			UI:                  uiAssets,
			SessionFactory:      sessionFactory,
			Resumer:             sessionResumer,
			SessionIdleTimeout:  sessionIdleTimeout,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "core-agent: attach server: %v\n", err)
			return runner.ExitConfigError
		}
		// Bind synchronously so port-in-use (or any other listener
		// error) is fatal instead of silently degrading to REPL while
		// the operator's TUI talks to the OLD process holding the port.
		if err := attachSrv.Bind(); err != nil {
			fmt.Fprintf(os.Stderr, "core-agent: attach listener: %v\n", err)
			return runner.ExitConfigError
		}
		// Attach observers (#338): sessions/subscribers/drops/peers.
		if _, err := attach.RegisterMetrics(otel.GetMeterProvider(), attachSrv); err != nil {
			fmt.Fprintf(os.Stderr, "core-agent: metrics: register attach observer: %v\n", err)
			return runner.ExitConfigError
		}
		endpoint := attachCfg.Listen
		if endpoint == "" {
			endpoint = "unix://" + attachCfg.UnixSocket
		}
		extras := ""
		if peerReg != nil {
			extras = " (peer-hub enabled)"
		}
		fmt.Fprintf(os.Stderr, "core-agent: attach listener on %s%s\n", endpoint, extras)
		go func() {
			if err := attachSrv.Serve(); err != nil {
				fmt.Fprintf(os.Stderr, "core-agent: attach server: %v\n", err)
			}
		}()
		defer func() { _ = attachSrv.Close() }()
		if autoContinueBootScan != nil {
			go autoContinueBootScan()
			// In-lifetime retry driver (#575 defect B): re-runs the
			// boot-scan pass on a fixed interval so a stranded
			// continuation self-heals without a reboot. Bounded by the
			// same breaker + per-session cap as the boot scan; a
			// daemon-killing turn kills this goroutine too, so only
			// survivable failures are ever re-fired.
			if autoContinueRetry {
				autoContinueWG.Add(1)
				go func() {
					defer autoContinueWG.Done()
					compose.AutoContinueRetryLoop(autoContinueDriverCtx, autoContinueRetryInterval, autoContinueBootScan)
				}()
			}
		}
	}

	// Peer registration: this agent registers with a remote hub. Lives
	// alongside the local listener (the agent CAN be both a hub and a
	// peer of another hub, though that's unusual). The hub records
	// RegisterEndpoint as the reachable address, not Listen — operators
	// commonly bind 0.0.0.0 for Listen but need to publish a specific
	// pod IP to the hub.
	if attachCfg.RegisterTo != "" {
		if attachCfg.RegisterEndpoint == "" {
			fmt.Fprintln(os.Stderr, "core-agent: --attach-register-to requires --attach-register-endpoint (the URL the hub should record for this agent)")
			return runner.ExitConfigError
		}
		regName := attachCfg.RegisterName
		if regName == "" {
			if h, herr := os.Hostname(); herr == nil {
				regName = h
			} else {
				regName = "core-agent"
			}
		}
		peerClientOpts := []attach.PeerClientOption{}
		if attachCfg.TokenEnv != "" {
			if tok := os.Getenv(attachCfg.TokenEnv); tok != "" {
				peerClientOpts = append(peerClientOpts, attach.WithPeerBearerToken(tok))
			}
		}
		peerClient := attach.NewPeerClient(attachCfg.RegisterTo, peerClientOpts...)
		regCtx, regCancel := context.WithTimeout(ctx, 10*time.Second)
		stopPeer, err := peerClient.RegisterAndHeartbeat(regCtx, attach.RegisterRequest{
			Name:     regName,
			Endpoint: attachCfg.RegisterEndpoint,
			Labels:   map[string]string{"core-agent-version": "dev"},
		})
		regCancel()
		if err != nil {
			fmt.Fprintf(os.Stderr, "core-agent: register with hub %s: %v\n", attachCfg.RegisterTo, err)
			return runner.ExitConfigError
		}
		fmt.Fprintf(os.Stderr, "core-agent: registered with hub %s as %q (endpoint=%s)\n",
			attachCfg.RegisterTo, regName, attachCfg.RegisterEndpoint)
		defer stopPeer()
	}

	colorOn, err := resolveColor(color, os.Stdout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "core-agent: %v\n", err)
		return runner.ExitConfigError
	}
	eventsOpts := []runner.EventsOption{runner.WithColor(colorOn)}

	var code int
	if prompt != "" {
		code, err = runner.Headless(ctx, m, prompt, os.Stdout, os.Stderr, tracker, pricingRate, opts, eventsOpts...)
		if err != nil {
			fmt.Fprintf(os.Stderr, "core-agent: %v\n", err)
		}
		if code == runner.ExitOK {
			runner.WriteSummary(os.Stderr, tracker, m.Name())
			persistTranscript(agentsDir, m.Name(), prompt, tracker)
		}
		return code
	}

	if noREPL {
		// Attach-only daemon mode: construct the agent (which
		// registers it with the attach session registry so the
		// picker shows a session to attach to) and block on ctx
		// cancellation. Required for `core-agent-tui --local`
		// spawns (and any other "headless server, attach is the
		// only surface" deployment), since the default REPL
		// reads stdin which is /dev/null for spawned children —
		// scanner.Scan() returns false immediately, REPL exits,
		// and the agent dies before the operator can attach.
		if attachCfg.Listen == "" && attachCfg.UnixSocket == "" {
			fmt.Fprintln(os.Stderr, "core-agent: --no-repl requires --attach-listen or --attach-unix-socket")
			return runner.ExitConfigError
		}
		a, _, err := buildAttachedAgent(m, opts, adapterOpts, attachRegistry)
		if err != nil {
			fmt.Fprintf(os.Stderr, "core-agent: %v\n", err)
			return runner.ExitAgentError
		}
		fmt.Fprintf(os.Stderr,
			"core-agent: --no-repl: attach-only mode, session %s (Ctrl-C or SIGTERM to exit)\n",
			a.SessionID())
		// Auto-continue for the startup session (#558): a headless
		// single-user daemon has no human present, so a restart-
		// interrupted turn deserves the same continuation the
		// multi-session paths get. Runs before the wake loop so the
		// injected note latches the wake signal and drains as the
		// first turn. Guarded internally by the boot-log breaker /
		// attempt caps + run lock; no-op on a clean tail.
		//
		// Single-user mode only: a multi-session --no-repl daemon
		// already runs the boot scan, and both triggers writing
		// agent_boot_log rows would double-count boots in the
		// breaker math (its bootstrap `default` session has no ACL
		// row and stays uncovered — status quo since #539).
		if autoContinueEnabled && !cfg.Attach.MultiSession.Enabled {
			compose.AutoContinueStartupSession(ctx, eventlogHandle, a, autoContinueFreshness)
			// In-lifetime retry driver (#575 defect B) for the headless
			// single-user session: re-runs the startup-session pass so a
			// stranded continuation self-heals without a reboot. Same
			// breaker + cap bounds; joined via autoContinueWG before the
			// eventlog closes. Runs concurrently with the WakeLoop below,
			// which blocks until ctx cancels.
			if autoContinueRetry {
				startupAgent := a
				autoContinueWG.Add(1)
				go func() {
					defer autoContinueWG.Done()
					compose.AutoContinueRetryLoop(autoContinueDriverCtx, autoContinueRetryInterval, func() {
						compose.AutoContinueStartupSession(autoContinueDriverCtx, eventlogHandle, startupAgent, autoContinueFreshness)
					})
				}()
			}
		}
		// Wake-driven inbox loop, consolidated behind
		// runner.WakeLoop (#386 PR 4): blocks until an attach
		// client's POST /inject fires WakeRequested, drains the
		// inbox through an empty-prompt turn, accounts usage via
		// the shared usage.TurnTap discipline, repeats until ctx
		// cancels. Errors are surfaced per-turn; the loop stays up.
		runner.WakeLoop(ctx, a, runner.WakeLoopOptions{
			Tracker: tracker,
			Model:   m.Name(),
			Pricing: pricingRate,
			OnTurnError: func(err error) {
				fmt.Fprintf(os.Stderr, "core-agent: turn: %v\n", err)
			},
			Debugf: func(format string, args ...any) {
				debugf("--no-repl: "+format, args...)
			},
		})
		return runner.ExitOK
	}

	// TUI launch branch: when stdin is a real terminal and --no-tui
	// wasn't passed, take over the terminal with the in-process
	// bubble-tea TUI lifted from cogo (docs/embedded-tui-design-v2.md).
	// The REPL stays as the fallback for non-TTY (piped/CI), explicit
	// --no-tui, or any --tags no_tui slim build that excludes the
	// TUI package. Defaults follow Claude Code: bare `core-agent` in
	// a terminal lands in the TUI.
	if !noTUI && term.IsTerminal(int(os.Stdin.Fd())) {
		// core-tui is the only TUI codepath since v2.1; the
		// CORE_AGENT_TUI=internal escape hatch and the lifted
		// internal/tui/ tree are gone. Slim build (no_tui) still
		// stubs launchTUIv2 to no-op + REPL fall-through.
		didRun, code, err := launchTUIv2(ctx, tuiDeps{
			Cfg:           cfg,
			Model:         m,
			AgentOpts:     opts,
			AdapterOpts:   adapterOpts,
			AttachReg:     attachRegistry,
			Provider:      provider,
			Gate:          gate,
			Tracker:       tracker,
			Memory:        loaded,
			MCPServers:    mcpServers,
			LoadedSkills:  loadedSkills,
			AgentsDir:     agentsDir,
			CoreHome:      coreHome,
			HomeAgentsDir: homeAgentsDir,
			ProjectRoot:   projectRoot,
			ContentRoots:  contentRoots,
			EnvInterp:     envResolver.InterpolateFunc(),
			InitialPrompt: initialPrompt,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "core-agent: %v\n", err)
			if !didRun {
				return runner.ExitAgentError
			}
		}
		if didRun {
			if code == runner.ExitOK {
				runner.WriteSummary(os.Stderr, tracker, m.Name())
			}
			return code
		}
		// didRun=false in the slim build (-tags no_tui) — fall
		// through to the REPL fallback below.
	}

	replAgent, _, err := buildAttachedAgent(m, opts, adapterOpts, attachRegistry)
	if err != nil {
		fmt.Fprintf(os.Stderr, "core-agent: %v\n", err)
		return runner.ExitAgentError
	}
	if initialPrompt != "" {
		code, err = runner.Run(ctx, runner.RunOptions{
			Agent: replAgent, InitialPrompt: initialPrompt,
			Tracker: tracker, Pricing: pricingRate, EventsOptions: eventsOpts,
		})
	} else {
		code, err = runner.Run(ctx, runner.RunOptions{
			Agent:   replAgent,
			Tracker: tracker, Pricing: pricingRate, EventsOptions: eventsOpts,
		})
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "core-agent: %v\n", err)
	}
	if code == runner.ExitOK {
		runner.WriteSummary(os.Stderr, tracker, m.Name())
	}
	return code
}

// loadConfig resolves the config from cfgPath (when set) or by walking
// up from cwd looking for .agents/. Returns the config plus the
// resolved agentsDir (empty when none was found).
func loadConfig(cfgPath, cwd string) (*config.Config, string, error) {
	if cfgPath != "" {
		cfg := config.DefaultConfig()
		body, err := os.ReadFile(cfgPath)
		if err != nil {
			return nil, "", fmt.Errorf("read %s: %w", cfgPath, err)
		}
		if err := json.Unmarshal(body, cfg); err != nil {
			return nil, "", fmt.Errorf("parse %s: %w", cfgPath, err)
		}
		if err := cfg.Validate(); err != nil {
			return nil, "", err
		}
		// Treat the directory containing -c as the agentsDir so MCP /
		// skills resolve relative to it.
		return cfg, filepath.Dir(cfgPath), nil
	}
	return config.LoadOrDefault(cwd)
}

// installLogFilter replaces log.Default()'s output with
// compose.NewFilteredLogWriter, which drops lines matching
// known-noisy patterns the bundled CLI doesn't want surfaced to
// users (see pkg/compose/logfilter.go).
//
// Anything that isn't filtered passes through to fallback (typically
// os.Stderr) unchanged, so consumer-supplied log lines still appear.
func installLogFilter(fallback io.Writer) {
	log.SetOutput(compose.NewFilteredLogWriter(fallback))
	// Strip the default date/time prefix so any line that DOES make
	// it through reads like a normal stderr message rather than a
	// log entry. Genai's own log.Printf will pick up our flags;
	// fortunately the line we're filtering is the noisy one.
	log.SetFlags(0)
}

// resolveSessionDBPath returns the path to use for the session
// database. An explicit override wins; otherwise the default is
// ~/.<binary>/sessions.db where <binary> is derived from
// os.Executable() so forks and adapters land in their own directory.
func resolveSessionDBPath(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("user home: %w", err)
	}
	return filepath.Join(home, "."+binaryName(), "sessions.db"), nil
}

// binaryName returns the name of the running executable (without
// directory or .exe suffix) so default paths sort by binary identity.
// Falls back to "core-agent" if os.Executable fails for some reason.
func binaryName() string {
	if exe, err := os.Executable(); err == nil {
		return strings.TrimSuffix(filepath.Base(exe), ".exe")
	}
	return "core-agent"
}

// resolveColor parses the --color flag value into a bool. "auto"
// detects whether w is a TTY via runner.IsTerminal; "always" forces
// on; "never" forces off. Anything else is a config error.
func resolveColor(mode string, w io.Writer) (bool, error) {
	switch mode {
	case "auto", "":
		return runner.IsTerminal(w), nil
	case "always":
		return true, nil
	case "never":
		return false, nil
	default:
		return false, fmt.Errorf("--color: unknown value %q (want auto|always|never)", mode)
	}
}

// resolveAskUserTool turns the --ask flag value into a registered
// ask_user tool (or nil to skip). "off" returns nil. "stdin" wires
// tools.StdinPrompter unconditionally. "auto" picks stdin when the
// agent's stdin is a TTY (interactive REPL or pty-backed run) and
// tools.RefusePrompter otherwise — so the model gets a clear "no
// user available" tool result and adapts in headless/piped runs.
// hasToolNamed reports whether the assembled tool slice contains a
// tool with the given name. Used to keep boot lines honest about
// guardrails that only apply to a tool the run actually registered.
func hasToolNamed(ts []adktool.Tool, name string) bool {
	for _, t := range ts {
		if t != nil && t.Name() == name {
			return true
		}
	}
	return false
}

func resolveAskUserTool(mode string, in io.Reader, out io.Writer) (adktool.Tool, error) {
	var prompter tools.Prompter
	switch mode {
	case "off", "":
		return nil, nil
	case "stdin":
		prompter = tools.StdinPrompter(in, out)
	case "auto":
		if f, ok := in.(*os.File); ok && runner.IsTerminal(f) {
			prompter = tools.StdinPrompter(in, out)
		} else {
			prompter = tools.RefusePrompter("running unattended; proceed with reasonable defaults and explain in your final response")
		}
	default:
		return nil, fmt.Errorf("--ask: unknown value %q (want off|stdin|auto)", mode)
	}
	return tools.NewAskUserTool(tools.AskUserOptions{Prompter: prompter})
}

// resolveGatePrompter returns the Prompter wired into the
// permissions gate. When --yolo is set the gate runs in yolo mode
// and prompting never happens, so we skip the prompter. When stdin
// isn't a TTY (piped input, daemon, CI) we also skip — the gate's
// ErrNoPrompter message points at --yolo and the config knobs so
// the failure mode is recoverable. Otherwise we wire a stdin
// prompter that renders requests to stderr (keeping stdout clean
// for the model's reply).
func resolveGatePrompter(yolo bool, in *os.File, out io.Writer) permissions.Prompter {
	if yolo {
		return nil
	}
	if !runner.IsTerminal(in) {
		return nil
	}
	return permissions.StdinPrompter(in, out)
}

// autoContinueResolution is the decided auto-continue enablement plus the
// parsed freshness window and retry-driver settings, produced by
// resolveAutoContinue and consumed by both the multi-session boot-scan
// path and the --no-repl startup path.
type autoContinueResolution struct {
	enabled       bool
	freshness     time.Duration
	retry         bool
	retryInterval time.Duration
}

// resolveAutoContinue decides whether restart-interrupted turns are
// auto-continued (#539/#559), given the parsed config block and the run
// context. The default — config left unset (nil block or nil enabled) —
// is ON when the feature can apply: a multi-session daemon or a --no-repl
// single-user daemon, with a durable eventlog. It is OFF, silently,
// otherwise, so interactive REPL/TUI runs are never surprised by
// unattended token spend. An explicit enabled:false is a hard opt-out; an
// explicit enabled:true forces it on where it can apply and otherwise
// warns and is ignored (the pre-#559 behavior). When enabled by default
// (not by an explicit true) a one-line notice is emitted so the spend is
// never invisible. Freshness / retry_interval overrides follow the same
// duration-string convention as session_idle_timeout and are parsed only
// when the feature ends up on; a parse or validation failure returns a
// non-nil error (mapped to ExitConfigError by the caller).
func resolveAutoContinue(ac *config.AutoContinueConfig, multiSession, noREPL, haveEventlog bool, stderr io.Writer) (autoContinueResolution, error) {
	res := autoContinueResolution{freshness: time.Hour, retryInterval: 5 * time.Minute}
	applies := (multiSession || noREPL) && haveEventlog

	// Intent: an explicit enabled value wins; unset defaults to the
	// precondition gate.
	explicit := ac != nil && ac.Enabled != nil
	want := applies
	if explicit {
		want = *ac.Enabled
	}
	if !want {
		// Off: explicit opt-out, or unset in a mode where the feature
		// cannot apply. The latter is intentionally silent — a REPL/TUI
		// user has no reason to hear about a daemon feature.
		return res, nil
	}
	if !applies {
		// Reachable only via an explicit true (the default path set
		// want=applies, so a false want already returned above).
		// Preserve the pre-#559 warn-and-ignore, keeping the two
		// distinct diagnostics; mode check first so the missing
		// session-db message never points at the wrong knob.
		switch {
		case !multiSession && !noREPL:
			fmt.Fprintln(stderr, "core-agent: agent.auto_continue has no effect in this mode; it applies to multi-session daemons and --no-repl single-user daemons — ignoring")
		case !haveEventlog:
			fmt.Fprintln(stderr, "core-agent: agent.auto_continue requires --session-db (durable eventlog); ignoring")
		}
		return res, nil
	}

	// On. Parse freshness / retry-interval overrides (nil block → all
	// defaults, retry default-on).
	res.enabled = true
	if ac != nil && ac.Freshness != "" {
		d, err := time.ParseDuration(ac.Freshness)
		if err != nil {
			return res, fmt.Errorf("parse agent.auto_continue.freshness=%q: %w", ac.Freshness, err)
		}
		if d < 0 {
			return res, fmt.Errorf("agent.auto_continue.freshness=%q: must be >= 0 (\"0s\" disables the window)", ac.Freshness)
		}
		res.freshness = d // 0 = disabled window, by design
	}
	// In-lifetime retry driver (#575 defect B). Default-on wherever
	// auto-continue is enabled — including the unset (nil block) default;
	// an explicit retry:false keeps the one-shot-then-wait contract.
	res.retry = ac == nil || ac.RetryEnabled()
	if ac != nil && ac.RetryInterval != "" {
		d, err := time.ParseDuration(ac.RetryInterval)
		if err != nil {
			return res, fmt.Errorf("parse agent.auto_continue.retry_interval=%q: %w", ac.RetryInterval, err)
		}
		if d <= 0 {
			return res, fmt.Errorf("agent.auto_continue.retry_interval=%q: must be > 0", ac.RetryInterval)
		}
		res.retryInterval = d
	}

	if !explicit {
		fmt.Fprintln(stderr, "core-agent: auto_continue on by default (multi-session/--no-repl + durable eventlog); set agent.auto_continue.enabled=false to opt out")
	}
	return res, nil
}

func persistTranscript(agentsDir, model, prompt string, tracker *usage.Tracker) {
	if agentsDir == "" {
		return
	}
	tot := tracker.Totals()
	_, _ = transcript.Save(agentsDir, transcript.Transcript{
		Model: model,
		Messages: []transcript.Message{
			{Role: "user", Text: prompt},
		},
		Usage: transcript.Usage{
			Turns:        tot.Turns,
			InputTokens:  tot.InputTokens,
			OutputTokens: tot.OutputTokens,
			CostUSD:      tot.CostUSD,
		},
	})
}
