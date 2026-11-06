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
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	adktool "google.golang.org/adk/v2/tool"

	"github.com/go-steer/core-agent/v3/internal/version"
	"github.com/go-steer/core-agent/v3/pkg/agent"
	"github.com/go-steer/core-agent/v3/pkg/attach"
	"github.com/go-steer/core-agent/v3/pkg/compose"
	"github.com/go-steer/core-agent/v3/pkg/config"
	"github.com/go-steer/core-agent/v3/pkg/models"
	_ "github.com/go-steer/core-agent/v3/pkg/models/mock"
	"github.com/go-steer/core-agent/v3/pkg/permissions"
	"github.com/go-steer/core-agent/v3/pkg/runner"
	"github.com/go-steer/core-agent/v3/pkg/tools"
	"github.com/go-steer/core-agent/v3/pkg/transcript"
	"github.com/go-steer/core-agent/v3/pkg/usage"
)

func main() {
	if code, handled := dispatchSubcommandOrVersion(os.Args[1:]); handled {
		os.Exit(code)
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
	flag.StringVar(&cfgPathVal, "c", "", "config file path (default: discover .agents/config.json). Its directory also becomes the agentsDir unless --agents-dir says otherwise")
	flag.StringVar(&cfgPathVal, "config", "", "long-form alias for -c — same behavior")
	cfgPath := &cfgPathVal
	// --agents-dir (#945). The agentsDir is where MCP servers, skills,
	// the env manifest, the grant store, recorded plans and content
	// roots all resolve from, and until now it could only be set
	// indirectly, by choosing where -c lives. That coupling was
	// undiscoverable — a recipe was written against an --agents-dir
	// flag that did not exist — and it forced config.json to sit in
	// the same directory as the tree it configures.
	agentsDirFlag := flag.String("agents-dir", "",
		"directory MCP / skills / plans / content roots resolve from (default: the directory containing -c, else .agents/ discovered from cwd)")
	// -m / --model bind to the same var (same alias pattern as -c/--config,
	// issue #209). Several operator-facing messages suggest `--model ...`
	// (e.g. the small-tier-parent warning), so the long form must exist or
	// copy-pasting them fails with "flag provided but not defined: -model".
	var modelOverrideVal string
	flag.StringVar(&modelOverrideVal, "m", "", "override model name from config")
	flag.StringVar(&modelOverrideVal, "model", "", "long-form alias for -m — same behavior")
	modelOverride := &modelOverrideVal
	providerOverride := flag.String("provider", "", "override model.provider: gemini|vertex|anthropic|anthropic-vertex|echo|scripted, or a provider profile — a core-models built-in (vertex-maas|openai|xai|sap-ai-core|vllm|sglang|ollama|openai-compatible) or one declared under providers in .agents/config.json")
	noBuiltinTools := flag.Bool("no-builtin-tools", false, "disable the built-in tool suite ("+strings.Join(tools.BuiltinToolNames(), ", ")+")")
	disableTools := flag.String("disable-tools", "", "comma-separated list of built-in tools to disable (e.g. bash,write_file). Composes with cfg.tools.disable; ignored when --no-builtin-tools is set.")
	enableTools := flag.String("enable-tools", "", "comma-separated list of built-in tools to add back after a --task profile dropped them (e.g. --task=debug --enable-tools=bash). Cancels the profile's opinion only: it cannot re-enable a tool you turned off in cfg.tools.disable or --disable-tools, and asking for that combination is an error rather than a silent win for either side. Naming a tool the profile never dropped is a no-op.")
	planMode := flag.String("plan-mode", "", "what record_plan does: off (not registered) | advisory (registered, plan artifact persists to .agents/plans/, nothing is ever blocked on it) | required (advisory plus the gate: mutating tool calls — write_file / edit_file / delete_file / bash / fetch_url / spawn_agent — are denied until the model calls record_plan). CLI mirror of permissions.plan_mode, and the way to override a task profile that turns the gate on: --task=debug --plan-mode=advisory. Needs a .agents/ directory to persist plans into; without one record_plan cannot register, which makes advisory inert and required a deadlock.")
	planFirst := flag.Bool("plan-first", false, "deprecated two-state spelling of --plan-mode: true == --plan-mode=required, false == --plan-mode=off. Cannot express advisory. --plan-mode wins when both are passed.")
	scriptPath := flag.String("script", "", "JSONL transcript for --provider=scripted (overrides cfg.mock.script)")
	scriptStrict := flag.Bool("script-strict", false, "scripted: assert each incoming request matches the recorded one (overrides cfg.mock.strict)")
	recordTo := flag.String("record-to", "", "write a JSONL recording of all LLM turns to this path (overrides cfg.mock.record)")
	color := flag.String("color", "auto", "ANSI color in streamed output: auto|always|never (auto = TTY-detect on stdout)")
	outputFormat := flag.String("output-format", "text", "output format for headless (-p) runs: text (default; omits 'asst › ' prefix when stdout is not a terminal) | json (single JSON document on stdout at turn end) | stream-json (NDJSON event stream on stdout)")
	var quietVal bool
	flag.BoolVar(&quietVal, "q", false, "suppress startup config summary and closing turn summary on stderr; errors still print")
	flag.BoolVar(&quietVal, "quiet", false, "long-form alias for -q — same behavior")
	quiet := &quietVal
	// Default "" (not "off") so run() can tell "operator didn't pass --ask"
	// from an explicit --ask=off. A task profile's ask mode only applies to
	// the former; "" is treated as "off" everywhere downstream.
	ask := flag.String("ask", "", "register an ask_user tool the model can call when its instructions tell it to ask: off|stdin|auto (auto = stdin if interactive, refuse otherwise). Empty (default) behaves as off.")
	sessionDB := flag.Bool("session-db", false, "persist sessions + audit log to a durable database (default off; in-memory. attach mode, a non-sql state.transcript.backend, --continue, and --resume turn this on by themselves)")
	sessionDBPath := flag.String("session-db-path", "", "override the database path used when --session-db is set or implied by attach mode / external transcript / --continue / --resume (default: ~/.<binary>/sessions.db)")
	continueSession := flag.Bool("continue", false, "continue the most recent session for this project in the session database (implies --session-db)")
	resumeSession := flag.String("resume", "", "resume a previous session from the session database: pass --resume <session-id> to resume a specific session, or bare --resume to select interactively from this project's sessions (implies --session-db)")
	permissionMode := flag.String("permission-mode", "", "permission gate mode: ask|auto|acceptEdits|plan|allow|yolo (overrides permissions.mode)")
	yolo := flag.Bool("yolo", false, "bypass the permissions gate entirely (every tool call runs without approval). Equivalent to permissions.mode=\"yolo\" in config.")
	sandboxFlag := flag.String("sandbox", "", "execution sandbox mode or driver: auto|enforce|off|bwrap|seatbelt|container (overrides safety.sandbox.mode / driver)")
	noSandbox := flag.Bool("no-sandbox", false, "disable execution sandboxing and workspace write containment (equivalent to --sandbox=off or safety.sandbox.mode=\"off\")")
	trustWorkspace := flag.Bool("trust", false, "trust the discovered .agents/ workspace configuration (stdio MCP servers, command hooks, elevated permission modes, rooted subagents, and content_roots) and remember its SHA-256 content hash in ~/.core-agent/trusted_workspaces.json (#1353).")
	noBackgroundAgents := flag.Bool("no-background-agents", false, "disable the spawn_agent / stop_agent tools (model can't spawn background subagents). Default: enabled.")
	subagentSyncWait := flag.String("subagent-sync-wait", "", "how long spawn_agent {wait: true} holds the parent's turn open, as a duration (\"10m\"). Default 5m. The cap is on the wait, not the subagent: past it the tool returns and the subagent's result arrives later as a pushed report. Raise it for deep diagnostics, where a parent that gets a timeout tends to redo the work itself. \"0s\" removes the cap and leaves the subagent's own turn/wallclock budgets as the only bound. Config-file equivalent: tools.spawn_agent.sync_wait_timeout.")
	var memFlags memoryFlags
	registerMemoryFlags(&memFlags)
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
	attachListen := flag.String("attach-listen", "", "enable attach-mode HTTP listener on this address (e.g. 127.0.0.1:7777). Requires --session-db. Non-loopback binds (:7777, 0.0.0.0:7777, ...) refuse to start without authentication — set --attach-token-file or --attach-token (or mTLS / enforced multi-session auth). A loopback bind is refused too when the agent has the bash tool, unless --attach-allow-unauthenticated-local.")
	attachUnixSocket := flag.String("attach-unix-socket", "", "enable attach-mode on a Unix socket at this path. Mutually exclusive with --attach-listen. Without a token (or mTLS / enforced multi-session auth) it is refused when the agent has the bash tool, unless --attach-allow-unauthenticated-local: the socket's 0600 mode does not stop the agent's own user.")
	attachTLSCert := flag.String("attach-tls-cert", "", "TLS server certificate (PEM) for --attach-listen. Pair with --attach-tls-key.")
	attachTLSKey := flag.String("attach-tls-key", "", "TLS server key (PEM) for --attach-listen.")
	attachClientCA := flag.String("attach-client-ca", "", "CA PEM for client-cert verification (mTLS). When set, clients must present a cert signed by this CA.")
	attachTokenEnv := flag.String("attach-token", "", "env var name holding the bearer token clients must present in Authorization: Bearer <token>. Empty disables bearer-token auth. Mutually exclusive with --attach-token-file, which keeps the token out of every process environment.")
	attachTokenFile := flag.String("attach-token-file", "", "read the bearer token clients must present from this file (or a pipe), once, at startup. Unlike --attach-token the token never sits in any process environment, including the shell that launched the daemon. A regular file must not be accessible to other users and is never deleted, so against the agent it is a credential only if the agent's user cannot read it; prefer a pipe, e.g. <(pass show attach | head -n1). Mutually exclusive with --attach-token.")
	attachAllowUnauthLocal := flag.Bool("attach-allow-unauthenticated-local", false, "start a loopback or Unix-socket attach listener with no authentication even though the agent has the bash tool. Without this the daemon refuses: the agent's bash runs as this same user and could answer its own permission prompts over the listener. Prefer a token the agent cannot read, or --attach-readonly.")
	attachReadonly := flag.Bool("attach-readonly", false, "attach-mode: disable POST /inject and /wake. Read endpoints (GET /sessions, GET /events) remain open.")
	attachPeerHub := flag.Bool("attach-peer-hub", false, "enable peer-registration endpoints (POST/GET /peers + heartbeat) on the attach listener — this agent becomes a discovery hub for other peers.")
	attachPeerStateFile := flag.String("attach-peer-state-file", "", "make the peer registry durable across restarts by snapshotting it to this JSONL file (requires --attach-peer-hub). Reloaded at startup, so a hub restart doesn't blank the fleet until every peer re-registers. Holds registration IDs: written 0600, put it on a volume that outlives the pod.")
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
	checkpointMode := flag.String("checkpoint", "", "which parties may declare a task boundary (#905). 'model' (default) registers the model-facing mark_task_done tool alongside the operator's /done and the post-turn heuristic. 'operator' keeps /done and the heuristic but does NOT register mark_task_done, so the model cannot self-declare completion — the posture for a long-lived service: mark_task_done's description tells the model to use it 'generously at natural task boundaries' and its detail arg asks for a completion summary, framing written for an interactive coding session, and a daemon consuming machine signals sees a boundary everywhere. 'off' disables checkpointing entirely; /done then reports no checkpointer. Automatic context-window compaction is a separate mechanism and is unaffected by any of these — use --no-compact for that. Config-file equivalent: checkpoint.mode.")
	noCheckpoint := flag.Bool("no-checkpoint", false, "DEPRECATED alias for --checkpoint=off. Removes the /done slash and the post-turn heuristic as well as the model-facing mark_task_done tool — broader than this flag's original description implied. Prefer --checkpoint=off, or --checkpoint=operator to withhold only the model's trigger.")
	taskClass := flag.String("task", "", "operator-declared task class — picks a bundle of defaults (model tier, compaction threshold, agentic-tools posture, ask mode) tuned for the kind of work being done. One of: debug, implement, chat, research, review. Empty = no task class applied (substrate defaults). Explicit flags (--model, --ask, etc.) always win over the task profile. Per docs/model-selection-design.md / issue #123. Config-file equivalent: session.task_class.")
	maxTurnCostUSD := flag.Float64("max-turn-cost-usd", 0, "per-turn spend ceiling in USD. When a single conversation turn's cumulative cost (across all model calls + subtask costs) meets or exceeds this value, the agent emits a guardrail-trip (guardrail=cost_ceiling) and ENDS THAT TURN. The session is not halted and needs no reset: a further turn starts from a fresh per-turn budget, because one expensive turn is not a reason to end an eight-hour unattended run (#1049). Nothing starts that turn on its own, though — a one-shot (-p) run ends when its turn is cut, exit 1 (#1140). Three turns in a row that each trip it do halt the session, which is what stops a re-driving wake loop from spending the ceiling per turn forever; that halt clears with /guardrail reset in the TUI, or POST /sessions/{id}/guardrails/reset over attach. 0 = disabled (default). Defense against runaway tool-loops within one turn (e.g. issue #144). Pairs with --max-session-cost-usd; either or both can be set. Overrides config.agent.max_turn_cost_usd when set.")
	maxSessionCostUSD := flag.Float64("max-session-cost-usd", 0, fmt.Sprintf("session-level spend ceiling in USD. Cumulative across every turn including subtasks. Unlike --max-turn-cost-usd, a trip halts the SESSION — the agent refuses new turns until an operator resets it WITH additional budget (/guardrail reset +N in the TUI, or POST /sessions/{id}/guardrails/reset with additional_budget_usd over attach; a bare reset would re-trip immediately, since the accumulator is already past the bound). Useful for long-running autonomous deploys where per-turn cost is reasonable but the session total adds up. Overrides config.agent.max_session_cost_usd when set, including an explicit 0. When neither is set, unattended runs (-p, --no-repl, or a non-TTY stdin) default to $%.2f and interactive runs default to disabled (#642); pass --max-session-cost-usd=0 to opt an unattended run back out.", DefaultUnattendedSessionCostUSD))
	smallTierParent := flag.String("small-tier-parent", "", "what to do when an interactive session starts on a small-tier parent model (Flash/Haiku-class). One of warn|refuse|allow. warn (default when unset) logs a one-line operator notice but proceeds; refuse exits with a config-error code; allow suppresses the check entirely. Skipped regardless when -p (one-shot), --yolo, or the model's tier doesn't classify. Per docs/model-selection-design.md / issue #121. Config-file equivalent: safety.small_tier_parent.")
	watchdogMode := flag.String("watchdog", "", "behavioral watchdog mode (#123 PR 2). A ladder — each mode includes the one before it. 'warn' = observe tool-call stream + log structured alerts to the operator when a runaway pattern is detected (e.g. 5 consecutive identical tool calls — the read_file loop from #144). 'feedback' = same, plus the observation is injected into the model's next-turn context as a '[watchdog]' block, so the party actually making the looping call finds out about it (#159); a correction, not a backstop — nothing halts a model that reads it and loops anyway. 'enforce' = all of that, but a runaway also halts the turn in flight, trips a turn-error (kind=watchdog), and the agent refuses new turns until the operator resets it (/guardrail reset in the TUI, or POST /sessions/{id}/guardrails/reset over attach) (#623 — the hard backstop against tool loops an auto-continue re-drive would otherwise re-issue). 'off' = no observation. Empty (default) resolves per mode: 'enforce' for unattended runs (-p, --no-repl, or a non-TTY stdin — nobody is reading the warn-mode log there) and 'warn' for interactive REPL/TUI runs (#642). Eight signals ship today: four Critical (the args-keyed loop detectors repeated-tool-call, alternating-tool-cycle and dominant-tool-call, plus no-op-streak, which trips on 3 tool results in a row that each report changing nothing) and four Warn-level, repeated-tool-name, tool-failure-streak, no-new-state and tools-without-text (12 tool calls in a row with no assistant text between them), which never halt. Future modes (prompt, auto) are deferred per the design doc; its other proposed signals (files-not-touched, context-growth-rate, cost-burn-rate) are closed rather than deferred — see pkg/watchdog for which shipped component already covers each. Config-file equivalent: safety.watchdog.")
	bashSearchGate := flag.String("bash-search-gate", "", "what to do when the model runs a search-shaped shell command (grep/egrep/fgrep/rgrep/rg/ag/ack/fd/find) while the native grep/glob tools are registered (#158). 'enforce' (default) refuses the call with a structured error naming the native replacement — bash-as-grep is a training prior strong enough that the existing 'PREFERRED over bash grep' tool descriptions bounce off it (measured: one Gemini variant picked bash for search 15/27 times anyway), and a refusal is the only feedback the model gets at the moment it makes the wrong choice. 'warn' runs the command but attaches the same advice to the tool result. 'allow' disables the check. Piping into a search binary is never gated ('go test ./... | grep -v ok' filters a stream, which the native tool cannot do), and 'find' with an action predicate (-delete, -exec, ...) is a file operation rather than a lookup, so it passes. Tests, builds, git and every other bash use are untouched — this is the surgical version of --disable-tools=bash. Config-file equivalent: safety.bash_search_gate.")
	approvalTimeout := flag.String("approval-timeout", "", fmt.Sprintf("how long a permission prompt waits for an operator answer before denying the tool call, as a Go duration (e.g. \"15m\"). Overrides permissions.approval_timeout when set. When neither is set, unattended runs in \"ask\" mode with a prompt surface (e.g. --attach-listen) default to %s so an unanswered prompt cannot hold a session's turn lock and wake-loop goroutine open forever (#1364); pass --approval-timeout=0s to wait indefinitely.", DefaultUnattendedApprovalTimeout))
	agenticTools := flag.Bool("agentic-tools", true, "register the agentic tool wrappers (agentic_read_file, agentic_fetch_url, agentic_grep, agentic_research) that route through a subtask so only the digest enters the parent's context (docs/context-management-design.md Mechanism B). On by default since v2.1; pass --agentic-tools=false to register only the bare tools.")
	agenticSmallModel := flag.String("agentic-small-model", "", "small/cheap model ID the agentic_* wrappers should route subtasks to (e.g. gemini-3.5-flash-lite, claude-haiku-4-5). When empty, the provider's cheap-tier default is used (gemini-3.5-flash-lite for Gemini/Vertex, claude-haiku-5-5 for Anthropic); providers without a cheap tier (vertex-maas, custom profiles without tiers.small, echo, scripted) skip registering the agentic_* wrappers unless --agentic-small-model is set explicitly (#1377). Requires --agentic-tools.")
	noMCPDigest := flag.Bool("no-mcp-digest", false, "disable the structural pkg/digest wrap around MCP tool responses (docs/digest-design.md). Default: enabled. When on, JSON-shaped MCP responses get a deterministic prune (identifier keys preserved, long strings truncated, arrays collapsed head+tail) before reaching the parent context; prose passthroughs are bounded. Also registers retrieve_raw as a built-in tool so the model can fetch back the un-digested payload when a digest looks suspicious. Despite the name this switch governs the whole digest layer: the same wrap covers the survey-shaped built-ins (read_many_files, grep, glob, list_dir) once their response clears the threshold, so a filesystem read and an MCP call cost the same for the same payload size. Kill switch for demos / debugging; leave on for production. The MCP half is additionally gated per-project by cfg.MCP.AgenticWrap and per-server by mcp.json's agentic_never.")
	mcpAgenticWrapLLM := flag.Bool("mcp-agentic-wrap-llm", false, "enable the LLM subagent second-chance path for MCP responses the structural pruner can't reduce below threshold (docs/agentic-mcp-design.md #223). Default off — opt-in until the operator has confirmed the cost trade-off works for their MCP surface. Layered on top of --no-mcp-digest: structural runs first regardless, and the LLM subagent only fires when structural leaves the response above threshold. Config-file equivalent: mcp.json's agentic_wrap_llm.")
	mcpAgenticWrapModel := flag.String("mcp-agentic-wrap-model", "", "MCP-specific small-model override for the --mcp-agentic-wrap-llm subagent. When empty, falls through to --agentic-small-model, then to the provider's cheap-tier default. Motivation: MCP responses can be shaped differently enough from built-in-tool wrappers that one tier works well for one surface but not the other. Requires --mcp-agentic-wrap-llm. Config-file equivalent: mcp.json's agentic_wrap_model.")
	noContextCache := flag.Bool("no-context-cache", false, "disable Vertex explicit context caching for the stable request prefix (system instruction + tools). Default: enabled on Vertex. When on, the daemon creates a CachedContent resource after turn 1 and stamps it onto every subsequent GenerateContent call so the prefix bills at ~10% of the input rate. Kill switch for demos / debugging Vertex issues; leave on for production. See docs/vertex-context-caching-design.md. Also gated per-project by cfg.Model.Vertex.ContextCache.enabled.")
	promptCacheTTL := flag.String("prompt-cache-ttl", "", "Anthropic prompt-cache breakpoint lifetime: \"5m\" (default) or \"1h\". The 1-hour breakpoint bills cache writes at 2x the base input rate where the 5-minute one bills 1.25x, so it pays only when consecutive turns are routinely more than five minutes apart — an operator approving each tool call, a cron-driven run, a human-paced review. A tight agentic loop refreshes the 5-minute window on every turn and should leave this alone. Both TTLs are priced separately and the response reports which one each write used, so the cost ledger is accurate either way. Ignored when --no-prompt-cache is set or the provider isn't Anthropic. Config-file equivalent: cfg.model.anthropic.prompt_cache.ttl.")
	noPromptCache := flag.Bool("no-prompt-cache", false, "disable Anthropic prompt caching (cache_control breakpoints on the system prefix and the conversation tail). Default: enabled on the anthropic and anthropic-vertex providers. When on, the repeated prefix bills at 5–10% of the input rate (for example, 5% on claude-opus-5-5 at $0.20 vs $4/MTok, and 10% on earlier Claude models) instead of full price, at the cost of a 1.25x premium on the tokens each entry stores. Distinct from --no-context-cache, which is the Vertex/Gemini CachedContent resource; the two providers' mechanisms share nothing. Kill switch for debugging or for workloads whose prefix changes every call. See docs/anthropic-prompt-caching-design.md. Also gated per-project by cfg.model.anthropic.prompt_cache.enabled.")

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
	flag.CommandLine.Usage = func() {
		writeCLIUsage(flag.CommandLine.Output(), flag.CommandLine)
	}
	os.Args = append(os.Args[:1], normalizeResumeArgs(os.Args[1:], flag.CommandLine)...)
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

	if err := validatePromptCacheTTL(*promptCacheTTL); err != nil {
		fmt.Fprintf(os.Stderr, "core-agent: %v\n", err)
		os.Exit(runner.ExitConfigError)
	}

	code := run(runOptions{
		driver: driverOpts{
			prompt:        *prompt,
			initialPrompt: *initialPrompt,
			color:         *color,
			ask:           *ask,
			yolo:          *yolo,
			noREPL:        *noREPL,
			noTUI:         *noTUI,
		},
		workspace: workspaceOpts{
			cfgPath:           *cfgPath,
			agentsDirFlag:     *agentsDirFlag,
			allowURLHost:      *allowURLHost,
			allowPathEntries:  allowPathEntries,
			contentDirEntries: contentDirEntries,
		},
		model: modelOpts{
			modelOverride:    *modelOverride,
			providerOverride: *providerOverride,
			taskClass:        *taskClass,
			scriptPath:       *scriptPath,
			scriptStrict:     *scriptStrict,
			recordTo:         *recordTo,
			smallTierParent:  *smallTierParent,
			noPricingRefresh: *noPricingRefresh,
			noContextCache:   *noContextCache,
			noPromptCache:    *noPromptCache,
			promptCacheTTL:   *promptCacheTTL,
		},
		toolCatalog: toolCatalogOpts{
			noBuiltinTools:      *noBuiltinTools,
			disableTools:        *disableTools,
			noBackgroundAgents:  *noBackgroundAgents,
			agenticTools:        *agenticTools,
			agenticSmallModel:   *agenticSmallModel,
			noMCPDigest:         *noMCPDigest,
			mcpAgenticWrapLLM:   *mcpAgenticWrapLLM,
			mcpAgenticWrapModel: *mcpAgenticWrapModel,
			noCompact:           *noCompact,
			compactionThreshold: *compactionThreshold,
		},
		session: sessionOpts{
			sessionDB:       *sessionDB,
			sessionDBSet:    flagWasSet(flag.CommandLine, "session-db"),
			sessionDBPath:   *sessionDBPath,
			continueSession: *continueSession,
			resumeSession:   *resumeSession,
		},
		checkpoint: checkpointOpts{
			mode:            *checkpointMode,
			noCheckpoint:    *noCheckpoint,
			noCheckpointSet: flagWasSet(flag.CommandLine, "no-checkpoint"),
		},
		subagent: subagentOpts{syncWait: *subagentSyncWait, memory: memFlags},
		guardrails: guardrailOpts{
			watchdogMode:      *watchdogMode,
			maxTurnCostUSD:    *maxTurnCostUSD,
			maxSessionCostUSD: *maxSessionCostUSD,
			maxSessionCostSet: flagWasSet(flag.CommandLine, "max-session-cost-usd"),
			bashSearchGate:    *bashSearchGate,
			approvalTimeout:   *approvalTimeout,
			trust:             *trustWorkspace,
			permissionMode:    *permissionMode,
			sandbox:           *sandboxFlag,
			noSandbox:         *noSandbox,
		},
		toolProfile: toolProfileOpts{
			enableTools:  *enableTools,
			planFirst:    *planFirst,
			planFirstSet: flagWasSet(flag.CommandLine, "plan-first"),
			planMode:     *planMode,
		},
		prompt: promptOpts{
			appendSystemPrompt: *appendSystemPrompt,
			systemPromptFile:   *systemPromptFile,
			outputFormat:       *outputFormat,
			quiet:              *quiet,
		},
		attach: attachOpts{
			Listen:                    *attachListen,
			UnixSocket:                *attachUnixSocket,
			TLSCert:                   *attachTLSCert,
			TLSKey:                    *attachTLSKey,
			ClientCA:                  *attachClientCA,
			TokenEnv:                  *attachTokenEnv,
			TokenFile:                 *attachTokenFile,
			ReadOnly:                  *attachReadonly,
			PeerHub:                   *attachPeerHub,
			PeerStateFile:             *attachPeerStateFile,
			RegisterTo:                *attachRegisterTo,
			RegisterName:              *attachRegisterName,
			RegisterEndpoint:          *attachRegisterEndpoint,
			UI:                        *attachUI || *attachUIDir != "",
			UIDir:                     *attachUIDir,
			AllowUnauthenticatedLocal: *attachAllowUnauthLocal,
		},
		agentCard: agentCardOpts{
			ConfigPath:       *agentCardConfigPath,
			Name:             *agentCardName,
			Description:      *agentCardDescription,
			ExternalURL:      *agentCardExternalURL,
			Version:          *agentCardVersion,
			ProviderOrg:      *agentCardProviderOrg,
			ProviderURL:      *agentCardProviderURL,
			DocumentationURL: *agentCardDocsURL,
		},
		metrics: metricsOpts{Addr: *metricsAddr},
	})
	os.Exit(code)
}

func registerMemoryFlags(dst *memoryFlags) {
	flag.StringVar(&dst.ref, "memory-ref", "", "long-term memory backend URI (e.g. memorybank://projects/p/locations/l/reasoningEngines/123, sqlite:///path/memory.db, inmemory://default). Overrides CORE_AGENT_MEMORY_REF and memory.ref (#948).")
	flag.StringVar(&dst.agentEngine, "agent-engine", "", "Vertex AI ReasoningEngine resource (projects/{p}/locations/{l}/reasoningEngines/{id} or {p}/{l}/{id}) shared by session store and memory bank. Overrides CORE_AGENT_AGENT_ENGINE and agent_engine.resource (#948).")
	flag.StringVar(&dst.namespace, "memory-namespace", "", "logical memory namespace partitioning scopes across environments or tenants (default: \"default\"). Overrides CORE_AGENT_MEMORY_NAMESPACE and memory.namespace (#948).")
	flag.StringVar(&dst.agent, "memory-agent", "", "agent private memory scope mount (<id>[:rw|ro]|rw|ro|off). Overrides CORE_AGENT_MEMORY_AGENT and memory.agent (#948).")
	flag.StringVar(&dst.user, "memory-user", "", "explicit user identity for user-partitioned memory scopes in stateless or headless runs. Overrides CORE_AGENT_MEMORY_USER (#948).")
	flag.Func("memory-shared", "mount a named shared memory pool (<pool>[:ro|rw], default ro; repeatable). Overrides CORE_AGENT_MEMORY_SHARED and merges onto memory.shared (#948).", func(s string) error {
		if strings.TrimSpace(s) == "" {
			return fmt.Errorf("--memory-shared requires <pool>[:ro|rw]")
		}
		dst.shared = append(dst.shared, s)
		return nil
	})
	flag.StringVar(&dst.preload, "memory-preload", "", "pre-turn memory recall injection mode: off (default) | session_start | turn_start. Overrides CORE_AGENT_MEMORY_PRELOAD and memory.preload (#948).")
	flag.StringVar(&dst.consolidate, "memory-consolidate", "", "opt-in conversation memory extraction trigger: off (default) | turn_end | session_end. Overrides CORE_AGENT_MEMORY_CONSOLIDATE and memory.consolidate (#948).")
}

func dispatchSubcommandOrVersion(args []string) (int, bool) {
	if len(args) >= 1 {
		switch args[0] {
		case "attach":
			return runAttachSubcommand(args[1:]), true
		case "ls":
			return runLsSubcommand(args[1:]), true
		case "auth":
			return runAuthSubcommand(args[1:]), true
		case "config":
			return runConfigSubcommand(args[1:], os.Stdout, os.Stderr), true
		}
	}
	for _, a := range args {
		if a == "--version" || a == "-version" {
			fmt.Println(version.String("core-agent"))
			return 0, true
		}
	}
	return 0, false
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
	// The token's two sources are one setting: naming either on the CLI
	// replaces the config's choice of source, whichever field it used.
	// Overlaying them independently would let a config token_env survive
	// a CLI --attach-token-file and turn a deliberate override into a
	// "mutually exclusive" startup error.
	if setOnCLI["attach-token"] || setOnCLI["attach-token-file"] {
		fromCfg.TokenEnv, fromCfg.TokenFile = "", ""
	}
	overlayStr("attach-token", &opts.TokenEnv, fromCfg.TokenEnv)
	overlayStr("attach-token-file", &opts.TokenFile, fromCfg.TokenFile)
	overlayBool("attach-readonly", &opts.ReadOnly, fromCfg.ReadOnly)
	overlayBool("attach-peer-hub", &opts.PeerHub, fromCfg.PeerHub)
	overlayStr("attach-peer-state-file", &opts.PeerStateFile, fromCfg.PeerStateFile)
	overlayStr("attach-register-to", &opts.RegisterTo, fromCfg.RegisterTo)
	overlayStr("attach-register-endpoint", &opts.RegisterEndpoint, fromCfg.RegisterEndpoint)
	overlayStr("attach-register-name", &opts.RegisterName, fromCfg.RegisterName)
	return opts
}

// validatePromptCacheTTL rejects an unknown --prompt-cache-ttl rather
// than falling back to the 5-minute default. The two values differ by
// 60% on write cost, so an operator who typed "1hr" or "3600s" has a
// budget expectation this run would quietly fail to meet — and they
// would only find out on the invoice. Mirrors the config-side check in
// config.Validate, which guards the same value arriving from a file.
func validatePromptCacheTTL(ttl string) error {
	switch ttl {
	case "", config.PromptCacheTTL5m, config.PromptCacheTTL1h:
		return nil
	default:
		return fmt.Errorf("--prompt-cache-ttl %q must be %q or %q", ttl, config.PromptCacheTTL5m, config.PromptCacheTTL1h)
	}
}

// teardownStepTimeout bounds each individual shutdown defer in run()
// that talks to something external (OTLP flush, Prometheus/metrics
// shutdown, Vertex context-cache delete). See the teardown-budget
// comment at the otelShutdown defer (#538).
const teardownStepTimeout = 3 * time.Second

func run(opts runOptions) int {
	// SIGTERM still cancels the whole process via ctx. SIGINT
	// (Ctrl+C) is NOT in this list anymore — the REPL takes over
	// SIGINT for its own double-Ctrl+C-exits state machine, and
	// the per-turn turnInterrupter handles Ctrl+C as a raw byte
	// while a turn is in flight (raw mode disables ISIG). For
	// headless (-p) mode, an uncaught SIGINT terminates the
	// process at exit code 130 — standard one-shot-CLI behavior.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer stop()

	// Filter "Error context canceled" out of the default log
	// output. genai's SSE scanner unconditionally log.Printfs
	// every stream error (api_client.go:484), including
	// context.Canceled when the user hits ESC mid-turn. We can't
	// suppress at the source, so we drop the line at the
	// process-wide log writer here.
	installLogFilter(os.Stderr)

	state := newRunState(ctx, stop, opts)
	defer state.close()

	if code := runStartupSteps(state, startupSteps); code != runner.ExitOK {
		return code
	}
	return state.executeDriver()
}

func registerWithPeerHub(ctx context.Context, attachCfg attachOpts, attachToken string, send func(string)) (func(), error) {
	if attachCfg.RegisterEndpoint == "" {
		return nil, fmt.Errorf("--attach-register-to requires --attach-register-endpoint (the URL the hub should record for this agent)")
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
	if attachToken != "" {
		peerClientOpts = append(peerClientOpts, attach.WithPeerBearerToken(attachToken))
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
		return nil, fmt.Errorf("register with hub %s: %w", attachCfg.RegisterTo, err)
	}
	send(fmt.Sprintf("registered with hub %s as %q (endpoint=%s)",
		attachCfg.RegisterTo, regName, attachCfg.RegisterEndpoint))
	return stopPeer, nil
}

// loadConfig resolves the config from cfgPath (when set) or by walking
// up from cwd looking for .agents/. Returns the config plus the
// resolved agentsDir (empty when none was found). When agentsDirFlag is
// a valid directory, Markdown subagent discovery and relative schema
// resolution bind to it (#1450).
func loadConfig(cfgPath, agentsDirFlag, cwd string) (*config.Config, string, error) {
	var overrideAgentsDir string
	if agentsDirFlag != "" {
		if info, err := os.Stat(agentsDirFlag); err == nil && info.IsDir() {
			overrideAgentsDir = agentsDirFlag
		}
	}
	if cfgPath != "" {
		derived := filepath.Dir(cfgPath)
		effectiveDir := derived
		if overrideAgentsDir != "" {
			effectiveDir = overrideAgentsDir
		}
		cfg, err := config.LoadFileWithAgentsDir(cfgPath, effectiveDir)
		if err != nil {
			return nil, "", err
		}
		return cfg, derived, nil
	}
	cfg, dir, err := config.LoadOrDefault(cwd)
	if err != nil {
		return nil, "", err
	}
	if overrideAgentsDir != "" && !sameDir(overrideAgentsDir, dir) {
		if err := cfg.ApplyAgentsDir(overrideAgentsDir); err != nil {
			return nil, "", err
		}
	}
	return cfg, dir, nil
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

// resolveSessionDB decides whether this run gets a durable session
// database, and reports whether that decision was the operator's or
// ours. Attach mode implies one (#973): it is not a preference there,
// it is a precondition. The broadcaster pumps from the event log,
// /events replays from it, subagent history reads it, and guardrail
// state and auto-continue restore from it. Attach without it could
// never have worked, so requiring the operator to say so was a flag
// that existed only to be mandatory — and it was enforced at the END
// of boot, after provider detection, MCP loading and skill discovery.
//
// The implication only fires when NEITHER flag was given. An operator
// who passes --session-db-path keeps their path; nobody's database
// moves. Outside the two implying shapes (attach mode, and an external
// transcript — see below) nothing is implied: a one-shot run that
// wanted durability would have asked for it.
//
// conflict reports the one case the implication must not swallow:
// --session-db=false on an attach shape. A plain bool cannot tell
// "unset" from "explicitly off", so sessionDBSet (flag.Visit) carries
// that, and an operator who said no gets told why the answer cannot
// be no rather than watching a database appear anyway. It is not a
// conflict alongside --session-db-path, which is itself a request for
// a durable log at a named location.
//
// An external transcript (state.transcript.backend other than sql,
// #1192) implies the database the same way: the transcript lives
// elsewhere, but the run lock, claims, guardrail state, spend ledger
// and session ACLs still live in the local file, and the backend
// cannot be opened without it.
func resolveSessionDB(sessionDB, sessionDBSet bool, sessionDBPath, attachListen, attachUnixSocket string, externalTranscript bool) (enabled, implied, conflict bool) {
	implying := attachListen != "" || attachUnixSocket != "" || externalTranscript
	if implying && sessionDBSet && !sessionDB && sessionDBPath == "" {
		return false, false, true
	}
	if sessionDB || sessionDBPath != "" {
		return sessionDB, false, false
	}
	if !implying {
		return false, false, false
	}
	return true, true, false
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

// wakeLoopDrainTimeout bounds how long daemon shutdown waits for
// per-session wake loops to return before closing the eventlog handle
// they query (#751). Generous enough for a turn that is finishing up,
// short enough that a wedged session can't hold SIGTERM open past a
// container runtime's own kill grace period.
const wakeLoopDrainTimeout = 5 * time.Second

// primarySessionID returns a getter for the primary agent's session id,
// safe to call before — and on a multi-session daemon, instead of — the
// agent ever existing.
//
// It takes the address of the late-bound agentRef rather than the agent
// itself because the only correct time to read a session id is when
// something needs it. Everything wired around the prompt broker is
// constructed while the agent is still a list of options: agent.New runs
// inside runner.Run, and on a multi-session daemon it never runs for a
// primary at all, because POST /sessions builds each agent on demand.
// A caller that resolves the id eagerly does not get a blank field, it
// gets a nil dereference at startup, before the daemon has served
// anything — which is how the #647 notifier wiring first shipped and why
// this is a function.
func primarySessionID(ref **agent.Agent) func() string {
	return func() string {
		if ref == nil || *ref == nil {
			return ""
		}
		return (*ref).SessionID()
	}
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

// resolveAgenticTools resolves the small-tier model for the agentic_*
// wrapper tools and builds them only when a cheap-tier model is
// actually available (#1377). When models.ResolveSmallModel returns ""
// (providers with no cheap-tier default such as vertex-maas, custom
// profiles without tiers.small, echo, or scripted, and no explicit
// --agentic-small-model override), registering the wrappers would run
// both the parent and the subtask at frontier rates and multiply cost
// instead of saving it — so we skip registration and report why.
func resolveAgenticTools(
	builtinTools []adktool.Tool,
	agentGetter func() *agent.Agent,
	provider models.Provider,
	smallModelOverride string,
) ([]adktool.Tool, string, error) {
	resolvedSmallModel := models.ResolveSmallModel(provider, smallModelOverride)
	if resolvedSmallModel == "" {
		provName := ""
		if provider != nil {
			provName = provider.Name()
		}
		return nil, fmt.Sprintf("agentic subtasks: off (no cheap-tier default for provider %q; pass --agentic-small-model to enable)", provName), nil
	}
	bootLine := fmt.Sprintf("agentic subtasks: %s (provider default)", resolvedSmallModel)
	if smallModelOverride != "" {
		bootLine = fmt.Sprintf("agentic subtasks: %s (operator override)", resolvedSmallModel)
	}
	agTools, err := compose.BuildAgenticTools(builtinTools, agentGetter, provider, resolvedSmallModel)
	if err != nil {
		return nil, "", err
	}
	return agTools, bootLine, nil
}
