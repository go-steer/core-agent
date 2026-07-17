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
	"bytes"
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
	"strings"
	"syscall"
	"time"

	"github.com/glebarez/sqlite"
	"golang.org/x/term"
	adktool "google.golang.org/adk/tool"

	"github.com/go-steer/core-agent/internal/pricing"
	"github.com/go-steer/core-agent/internal/version"
	"github.com/go-steer/core-agent/internal/webui"
	"github.com/go-steer/core-agent/pkg/agent"
	"github.com/go-steer/core-agent/pkg/attach"
	"github.com/go-steer/core-agent/pkg/config"
	"github.com/go-steer/core-agent/pkg/digest"
	"github.com/go-steer/core-agent/pkg/eventlog"
	"github.com/go-steer/core-agent/pkg/instruction"
	"github.com/go-steer/core-agent/pkg/mcp"
	"github.com/go-steer/core-agent/pkg/models"
	_ "github.com/go-steer/core-agent/pkg/models/anthropic"
	"github.com/go-steer/core-agent/pkg/models/gemini"
	_ "github.com/go-steer/core-agent/pkg/models/mock"
	"github.com/go-steer/core-agent/pkg/modeltier"
	"github.com/go-steer/core-agent/pkg/permissions"
	"github.com/go-steer/core-agent/pkg/recording"
	"github.com/go-steer/core-agent/pkg/runner"
	"github.com/go-steer/core-agent/pkg/session"
	"github.com/go-steer/core-agent/pkg/skills"
	"github.com/go-steer/core-agent/pkg/taskclass"
	"github.com/go-steer/core-agent/pkg/telemetry"
	"github.com/go-steer/core-agent/pkg/tools"
	"github.com/go-steer/core-agent/pkg/usage"
	"github.com/go-steer/core-agent/pkg/watchdog"
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
	modelOverride := flag.String("m", "", "override model name from config")
	providerOverride := flag.String("provider", "", "override model.provider (gemini|vertex|anthropic|anthropic-vertex|echo|scripted)")
	noBuiltinTools := flag.Bool("no-builtin-tools", false, "disable the built-in tool suite ("+strings.Join(tools.BuiltinToolNames(), ", ")+")")
	disableTools := flag.String("disable-tools", "", "comma-separated list of built-in tools to disable (e.g. bash,write_file). Composes with cfg.tools.disable; ignored when --no-builtin-tools is set.")
	scriptPath := flag.String("script", "", "JSONL transcript for --provider=scripted (overrides cfg.mock.script)")
	scriptStrict := flag.Bool("script-strict", false, "scripted: assert each incoming request matches the recorded one (overrides cfg.mock.strict)")
	recordTo := flag.String("record-to", "", "write a JSONL recording of all LLM turns to this path (overrides cfg.mock.record)")
	color := flag.String("color", "auto", "ANSI color in streamed output: auto|always|never (auto = TTY-detect on stdout)")
	ask := flag.String("ask", "off", "register an ask_user tool the model can call when its instructions tell it to ask: off|stdin|auto (auto = stdin if interactive, refuse otherwise)")
	sessionDB := flag.Bool("session-db", false, "persist sessions + audit log to a durable database (default off; in-memory)")
	sessionDBPath := flag.String("session-db-path", "", "override the database path used when --session-db is set (default: ~/.<binary>/sessions.db)")
	yolo := flag.Bool("yolo", false, "bypass the permissions gate entirely (every tool call runs without approval). Equivalent to permissions.mode=\"yolo\" in config.")
	noBackgroundAgents := flag.Bool("no-background-agents", false, "disable the spawn_agent / list_agents / check_agent / stop_agent tools (model can't spawn background subagents). Default: enabled.")
	allowURLHost := flag.String("allow-url-host", "", "comma-separated host patterns appended to url_scope.allow for the fetch_url tool (e.g. \"github.com,*.googleapis.com\"). HTTPS only unless the pattern carries an http:// prefix. Disable the tool entirely with --disable-tools=fetch_url.")
	var allowPathEntries []config.PathScopeAllowEntry
	flag.Func("allow-path", "grant file access to a path tree outside the project + user-home roots, e.g. --allow-path /home/me/sibling-repo:rw (repeatable). Explicit access is required: r, w, or rw (long forms read/write/readwrite accepted). Skip the permission prompt for matching paths; unmatched paths still prompt.", func(s string) error {
		e, err := parseAllowPathSpec(s)
		if err != nil {
			return err
		}
		allowPathEntries = append(allowPathEntries, e)
		return nil
	})
	attachListen := flag.String("attach-listen", "", "enable attach-mode HTTP listener on this address (e.g. :7777). Requires --session-db.")
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
	noCompact := flag.Bool("no-compact", false, "disable automatic context-window compaction. /compact slash still works for manual summarization, but the post-turn threshold trigger is off. Use when running headless against a model whose window is huge enough that compaction would never fire anyway, or when debugging an issue where you don't want history rewrites in play.")
	noCheckpoint := flag.Bool("no-checkpoint", false, "disable task-boundary checkpoints. /done slash + the model-facing mark_task_done tool are both removed. Use when running headless where the model shouldn't self-signal task completion, or when debugging an issue where you don't want auto-slicing in play.")
	taskClass := flag.String("task", "", "operator-declared task class — picks a bundle of defaults (model tier, compaction threshold, agentic-tools posture, ask mode) tuned for the kind of work being done. One of: debug, implement, chat, research, review. Empty = no task class applied (substrate defaults). Explicit flags (--model, --ask, etc.) always win over the task profile. Per docs/model-selection-design.md / issue #123. Config-file equivalent: session.task_class.")
	maxTurnCostUSD := flag.Float64("max-turn-cost-usd", 0, "per-turn spend ceiling in USD. When a single conversation turn's cumulative cost (across all model calls + subtask costs) meets or exceeds this value, the agent emits a structured turn-error (kind=cost_ceiling) and refuses new turns until the operator runs /resume-after-cost-ceiling. 0 = disabled (default). Defense against runaway tool-loops within one turn (e.g. issue #144). Pairs with --max-session-cost-usd; either or both can be set. Overrides config.agent.max_turn_cost_usd when set.")
	maxSessionCostUSD := flag.Float64("max-session-cost-usd", 0, "session-level spend ceiling in USD. Cumulative across every turn including subtasks; same trip + refuse behavior as --max-turn-cost-usd. 0 = disabled (default). Useful for long-running autonomous deploys where per-turn cost is reasonable but the session total adds up. Overrides config.agent.max_session_cost_usd when set.")
	smallTierParent := flag.String("small-tier-parent", "", "what to do when an interactive session starts on a small-tier parent model (Flash/Haiku-class). One of warn|refuse|allow. warn (default when unset) logs a one-line operator notice but proceeds; refuse exits with a config-error code; allow suppresses the check entirely. Skipped regardless when -p (one-shot), --yolo, or the model's tier doesn't classify. Per docs/model-selection-design.md / issue #121. Config-file equivalent: safety.small_tier_parent.")
	watchdogMode := flag.String("watchdog", "warn", "behavioral watchdog mode (#123 PR 2). 'warn' = observe tool-call stream + log structured alerts to the operator when a runaway pattern is detected (e.g. 5 consecutive identical tool calls — the read_file loop from #144). 'off' = no observation. v1 ships warn-mode + one signal (repeated-tool-call); future modes (prompt, auto) and additional signals (tools-without-text, files-not-touched) are deferred per the design doc.")
	agenticTools := flag.Bool("agentic-tools", true, "register the agentic tool wrappers (agentic_read_file, agentic_fetch_url, agentic_grep, agentic_research) that route through a subtask so only the digest enters the parent's context (docs/context-management-design.md Mechanism B). On by default since v2.1; pass --agentic-tools=false to register only the bare tools.")
	agenticSmallModel := flag.String("agentic-small-model", "", "small/cheap model ID the agentic_* wrappers should route subtasks to (e.g. gemini-2.5-flash, claude-haiku-4-5). When empty, the provider's cheap-tier default is used (gemini-2.5-flash for Gemini/Vertex, claude-haiku-4-5 for Anthropic); providers without a cheap tier (echo, scripted) fall through to inheriting the parent's model. Requires --agentic-tools.")
	noMCPDigest := flag.Bool("no-mcp-digest", false, "disable the structural pkg/digest wrap around MCP tool responses (docs/digest-design.md). Default: enabled. When on, JSON-shaped MCP responses get a deterministic prune (identifier keys preserved, long strings truncated, arrays collapsed head+tail) before reaching the parent context; prose passthroughs are bounded. Also registers retrieve_raw as a built-in tool so the model can fetch back the un-digested payload when a digest looks suspicious. Kill switch for demos / debugging; leave on for production. Also gated per-project by cfg.MCP.AgenticWrap and per-server by mcp.json's agentic_never.")
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
	flag.Parse()

	code := run(*prompt, *initialPrompt, *cfgPath, *modelOverride, *providerOverride, *taskClass, *noBuiltinTools, *disableTools, *scriptPath, *scriptStrict, *recordTo, *color, *ask, *sessionDB, *sessionDBPath, *yolo, *noBackgroundAgents, *allowURLHost, allowPathEntries, *noREPL, *noTUI, *noPricingRefresh, *noCompact, *noCheckpoint, *maxTurnCostUSD, *maxSessionCostUSD, *watchdogMode, *smallTierParent, *agenticTools, *agenticSmallModel, *noMCPDigest, *noContextCache,
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
		})
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
// doesn't grow by 11 more positional args.
type attachOpts struct {
	Listen           string
	UnixSocket       string
	TLSCert          string
	TLSKey           string
	ClientCA         string
	TokenEnv         string
	ReadOnly         bool
	PeerHub          bool
	RegisterTo       string
	RegisterName     string
	RegisterEndpoint string
	// UI enables the /ui/* route on the attach listener serving the
	// mast-web operator UI. Uses the embedded bundle from
	// internal/webui (populated by dev/tools/fetch-mast-web at build
	// time) unless UIDir overrides with a local directory.
	UI    bool
	UIDir string
}

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

	overlayStr := func(name string, dst *string, cfgVal string) {
		if !setOnCLI[name] && *dst == "" {
			*dst = cfgVal
		}
		*dst = os.ExpandEnv(*dst)
	}
	overlayBool := func(name string, dst *bool, cfgVal bool) {
		if !setOnCLI[name] {
			*dst = cfgVal
		}
	}

	overlayStr("attach-listen", &opts.Listen, cfg.Listen)
	overlayStr("attach-unix-socket", &opts.UnixSocket, cfg.UnixSocket)
	overlayStr("attach-tls-cert", &opts.TLSCert, cfg.TLSCert)
	overlayStr("attach-tls-key", &opts.TLSKey, cfg.TLSKey)
	overlayStr("attach-client-ca", &opts.ClientCA, cfg.ClientCA)
	overlayStr("attach-token", &opts.TokenEnv, cfg.TokenEnv)
	overlayBool("attach-readonly", &opts.ReadOnly, cfg.ReadOnly)
	overlayBool("attach-peer-hub", &opts.PeerHub, cfg.PeerHub)
	overlayStr("attach-register-to", &opts.RegisterTo, cfg.RegisterTo)
	overlayStr("attach-register-endpoint", &opts.RegisterEndpoint, cfg.RegisterEndpoint)
	overlayStr("attach-register-name", &opts.RegisterName, cfg.RegisterName)
	return opts
}

func run(prompt, initialPrompt, cfgPath, modelOverride, providerOverride, taskClass string, noBuiltinTools bool, disableTools string, scriptPath string, scriptStrict bool, recordTo string, color string, ask string, sessionDB bool, sessionDBPath string, yolo, noBackgroundAgents bool, allowURLHost string, allowPathEntries []config.PathScopeAllowEntry, noREPL, noTUI, noPricingRefresh, noCompact, noCheckpoint bool, maxTurnCostUSD, maxSessionCostUSD float64, watchdogMode, smallTierParent string, agenticTools bool, agenticSmallModel string, noMCPDigest bool, noContextCache bool, attachCfg attachOpts, cardCfg agentCardOpts) int {
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

	// Task class (#123). CLI --task overrides cfg.Session.TaskClass.
	// Apply the resolved profile to whichever flags the operator left
	// unspecified; explicit flags always win. Done BEFORE provider
	// resolution so the task's tier-to-model selection lands before
	// provider.Model(cfg.Model.Name) is called.
	if taskClass != "" {
		cfg.Session.TaskClass = taskClass
	}
	if cfg.Session.TaskClass != "" {
		profile, ok := taskclass.Resolve(cfg.Session.TaskClass)
		if !ok {
			fmt.Fprintf(os.Stderr, "core-agent: --task=%q: unknown task class (want one of %v)\n",
				cfg.Session.TaskClass, taskclass.Classes())
			return runner.ExitConfigError
		}
		// Pick the provider name for tier→model mapping. cfg.Model.Provider
		// may be empty (auto-detect); fall back to env-based auto-detect.
		providerForTier := cfg.Model.Provider
		if providerForTier == "" {
			providerForTier = models.AutoDetectProvider()
		}
		// Model: if neither operator's --model nor cfg.Model.Name is
		// the value the profile would pick, use the profile's tier
		// selection. modelOverride == "" means CLI didn't specify;
		// the existing cfg.Model.Name was either the config-file value
		// or the substrate default — we treat the substrate default
		// (DefaultConfig's gemini-3.1-pro-preview-customtools) as
		// overridable by --task, but a config-file value as
		// operator-set and respect it. Distinguishing those today
		// requires comparing against DefaultConfig().Model.Name; for
		// v1 we use a simpler rule: --task only fills in when the CLI
		// --model flag is empty.
		if modelOverride == "" {
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
		fmt.Fprintf(os.Stderr, "core-agent: task class: %s → model=%s compaction-threshold=%.2f ask=%s (override individual knobs with --model / --compaction-threshold / --ask)\n",
			cfg.Session.TaskClass, cfg.Model.Name, profile.CompactionThreshold, ask)
	}

	otelShutdown, err := telemetry.Setup(ctx, cfg.OTEL.Exporter)
	if err != nil {
		fmt.Fprintf(os.Stderr, "core-agent: telemetry setup: %v\n", err)
	}
	defer func() { _ = otelShutdown(context.Background()) }()

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
	contextCacheManager := maybeWireContextCache(
		ctx, provider, cfg, noContextCache,
		func(s string) { fmt.Fprintln(os.Stderr, "core-agent: "+s) },
	)
	if contextCacheManager != nil {
		defer contextCacheManager.Delete(context.Background())
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
	if userHome != "" {
		coreHome = filepath.Join(userHome, ".core-agent")
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
	prompter := resolveGatePrompter(yolo, os.Stdin, os.Stderr)
	template, err := permissions.FromConfig(cfg, cwd, coreHome, prompter)
	if err != nil {
		fmt.Fprintf(os.Stderr, "core-agent: %v\n", err)
		return runner.ExitConfigError
	}
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
	// LoadForSession is the multi-session-aware loader. With an empty
	// callerIdentity (single-user / startup-time), it behaves
	// identically to Load. The per-caller overlay path lights up when
	// a request-time Caller threads through — γ wires the call site;
	// future session-creation flows pass the resolved Caller.Identity.
	loaded, err := instruction.LoadForSession(projectRoot, coreHome, "", cfg.Attach.MultiSession.UsersDir)
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
	var digestStore *digest.LazyStore
	var digestOpts *mcp.DigestOptions
	if !noMCPDigest {
		digestStore = &digest.LazyStore{}
		digestOpts = &mcp.DigestOptions{Store: digestStore}
	}
	mcpServers, mcpToolsets, mcpErr := mcp.Build(ctx, agentsDir, send, gate, makeMCPElicitor(), digestOpts)
	if mcpErr != nil {
		fmt.Fprintf(os.Stderr, "core-agent: mcp: %v\n", mcpErr)
	}
	loadedSkills, skillsErr := skills.LoadAll(ctx, agentsDir, coreHome, gate)
	if skillsErr != nil {
		fmt.Fprintf(os.Stderr, "core-agent: skills: %v\n", skillsErr)
	}

	// Startup config summary (#212 part 1). Emits the resolved state
	// of every load-bearing subsystem — config source, agentsDir,
	// model+provider, MCP servers, skills, multi-session auth — so
	// operators can verify what the daemon actually loaded via a
	// grep rather than `kubectl debug` + /proc/1/root inspection.
	// Fires unconditionally at this point (both single-shot -p and
	// attach modes), independent of the attach branch further down.
	for _, line := range formatStartupSummary(startupSummaryInputs{
		cfgPath:      cfgPath,
		cfg:          cfg,
		agentsDir:    agentsDir,
		providerName: provider.Name(),
		mcpServers:   mcpServers,
		loadedSkills: loadedSkills,
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
		reg, err := tools.Build(cfg, gate, agentsDir, b)
		if err != nil {
			fmt.Fprintf(os.Stderr, "core-agent: built-in tools: %v\n", err)
			return runner.ExitConfigError
		}
		builtinTools = reg.Tools
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
			describeRefresh(os.Stderr, outcome)
		}
	}

	// Install the layered pricing catalog before any cost lookups
	// happen. Per docs/pricing-design.md:
	//   cfg.Model.Pricing override → .agents/pricing.json
	//   → ~/.core-agent/pricing.json (manual + external)
	//   → compiled-in builtin → longest-prefix → unknown.
	// PR C adds /pricing refresh + /pricing set slash commands.
	if catalog, perr := pricing.NewCatalog(pricing.Options{
		CfgOverride: cfgToCatalogOverride(cfg.Model.Pricing),
		AgentsDir:   agentsDir,
		UserHome:    coreHome,
	}); perr != nil {
		fmt.Fprintf(os.Stderr, "core-agent: pricing: %v\n", perr)
		// Non-fatal: missing/corrupt files fall back to builtin via
		// usage.PriceFor's no-catalog path. Just continue.
	} else {
		usage.SetCatalog(catalog)
	}

	tracker := usage.NewTracker()
	pricingRate := usage.PriceFor(cfg.Model.Name, cfg)

	// Background subagent spawning. Constructed before agent.New so
	// the spawn tools can be registered alongside the built-in tools.
	// Manager is attached to the parent agent inside agent.New via
	// WithBackgroundManager; the agent's pre-turn alert drain
	// surfaces background reports to the parent's model.
	var bgMgr *agent.BackgroundAgentManager
	if !noBackgroundAgents {
		var err error
		bgMgr, err = agent.NewBackgroundAgentManager(
			agent.WithBackgroundProvider(provider, cfg.Model.Name),
			agent.WithBackgroundGate(gate),
			agent.WithBackgroundCatalog(builtinTools),
		)
		if err != nil {
			fmt.Fprintf(os.Stderr, "core-agent: background agents: %v\n", err)
			return runner.ExitConfigError
		}
		defer func() { _ = bgMgr.Close() }()
		builtinTools = append(builtinTools, agent.NewBackgroundSpawnTools(bgMgr)...)
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
	// parent's context — raw tool output stays in the subtask. Late-bound *Agent via agentRef closure;
	// agentRef is populated after agent.New returns. The inner
	// tools the subtask runs are pulled from builtinTools by
	// canonical name, so the subtask shares the parent's gate +
	// output caps.
	var agentRef *agent.Agent
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
		agTools, err := buildAgenticTools(builtinTools, func() *agent.Agent { return agentRef }, provider, resolvedSmallModel)
		if err != nil {
			fmt.Fprintf(os.Stderr, "core-agent: agentic tools: %v\n", err)
			return runner.ExitConfigError
		}
		builtinTools = append(builtinTools, agTools...)
	}

	opts := []agent.Option{
		agent.WithTools(builtinTools),
		agent.WithToolsets(allToolsets),
		agent.WithSystemInstructionPrefix(loaded.Instruction),
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
		// Attach-extras snapshot funcs. The agent itself satisfies the
		// MemoryProvider / SkillsProvider / MCPProvider interfaces via
		// these closures, so the remote /memory /skills /mcp endpoints
		// return the same state the in-process TUI sees.
		agent.WithAttachMemoryProvider(func() []attach.MemorySource {
			// Re-walk on every call so a fresh AGENTS.md / CLAUDE.md /
			// GEMINI.md picked up between turns (or written by the
			// agent itself) surfaces without a daemon restart. Cheap
			// — a few file stats + reads of small files capped at
			// 32 KiB each.
			fresh, _ := instruction.Load(projectRoot, coreHome)
			out := make([]attach.MemorySource, 0, len(fresh.Sources))
			for _, s := range fresh.Sources {
				out = append(out, attach.MemorySource{Scope: s.Scope, Path: s.Path, Size: s.Bytes})
			}
			return out
		}),
		agent.WithAttachSkillsProvider(func() []attach.SkillInfo {
			// Re-walk on every call so newly-dropped SKILL.md bundles
			// surface without restart. The merge across project +
			// user-global sources happens inside skills.LoadAll.
			fresh, err := skills.LoadAll(ctx, agentsDir, coreHome, gate)
			if err != nil {
				return nil
			}
			out := make([]attach.SkillInfo, 0, len(fresh.Infos))
			for _, s := range fresh.Infos {
				out = append(out, attach.SkillInfo{Name: s.Name, Description: s.Description})
			}
			return out
		}),
		agent.WithAttachPricingProvider(func() attach.PricingInfo {
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
		agent.WithAttachRefreshPricer(func(ctx context.Context) (attach.PricingRefreshResponse, error) {
			if coreHome == "" {
				return attach.PricingRefreshResponse{}, fmt.Errorf("pricing refresh: $HOME unavailable, no user file to write")
			}
			summary, err := refreshPricingForTUI(ctx, cfg, agentsDir, coreHome)
			if err != nil {
				return attach.PricingRefreshResponse{}, err
			}
			return attach.PricingRefreshResponse{
				Updated:     true,
				LastRefresh: time.Now(),
				Detail:      summary,
			}, nil
		}),
		agent.WithAttachPricingSetter(func(req attach.PricingSetRequest) error {
			if coreHome == "" {
				return fmt.Errorf("pricing set: $HOME unavailable, no user file to write")
			}
			_, err := setPricingForTUI(cfg, agentsDir, coreHome, req.Model, req.InputUSDPerMTok, req.OutputUSDPerMTok)
			return err
		}),
		agent.WithAttachReloader(func(_ context.Context) attach.ReloadResponse {
			// Best-effort re-walks: instruction + skills snapshots
			// are reported per-surface so the operator sees which
			// parts parsed cleanly after a .agents/ edit. MCP server
			// lifecycle restart + system-prompt rebuild would require
			// reconstructing the running agent (tracked separately);
			// for now MCP comes back as a configuration-only re-read
			// note.
			out := attach.ReloadResponse{}
			if _, err := instruction.Load(projectRoot, coreHome); err != nil {
				out.Errors = append(out.Errors, fmt.Sprintf("memory: %v", err))
			} else {
				out.Memory = true
			}
			if _, err := skills.LoadAll(ctx, agentsDir, coreHome, gate); err != nil {
				out.Errors = append(out.Errors, fmt.Sprintf("skills: %v", err))
			} else {
				out.Skills = true
			}
			// MCP: confirm the on-disk config still parses; surface
			// the limitation so the operator doesn't expect a live
			// server restart.
			if _, err := mcp.Load(agentsDir); err != nil {
				out.Errors = append(out.Errors, fmt.Sprintf("mcp config: %v", err))
			}
			out.MCP = false
			out.Errors = append(out.Errors, "mcp: live server restart requires daemon restart (tracked for v2.3)")
			return out
		}),
		agent.WithAttachReplanner(func(_ context.Context, _ attach.ReplanRequest) (attach.ReplanResponse, error) {
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
		agent.WithAttachMCPProvider(func() attach.MCPInfo {
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
		opts = append(opts, agent.WithCompactor(buildCompactor(cfg.Compaction)))
	}
	// Task-boundary checkpoints (docs/context-management-design.md
	// Mechanism C). Default-on; disable via --no-checkpoint.
	// Registers the mark_task_done model-facing tool + enables the
	// /done slash; the model can self-signal task completion at
	// natural boundaries, and the next Run drains the pending
	// checkpoint by writing a richer handover record.
	// Cost-ceiling kill switch (#145). CLI flags > config fields >
	// config default (unset → disabled). Both bounds are independent;
	// the agent enforces whichever is configured.
	if maxTurnCostUSD > 0 {
		cfg.Agent.MaxTurnCostUSD = &maxTurnCostUSD
	}
	if maxSessionCostUSD > 0 {
		cfg.Agent.MaxSessionCostUSD = &maxSessionCostUSD
	}
	ceiling := agent.CostCeiling{}
	if cfg.Agent.MaxTurnCostUSD != nil {
		ceiling.MaxTurnUSD = *cfg.Agent.MaxTurnCostUSD
	}
	if cfg.Agent.MaxSessionCostUSD != nil {
		ceiling.MaxSessionUSD = *cfg.Agent.MaxSessionCostUSD
	}
	if ceiling.MaxTurnUSD > 0 || ceiling.MaxSessionUSD > 0 {
		opts = append(opts, agent.WithCostCeiling(ceiling))
		send(fmt.Sprintf("cost ceiling: per-turn=$%.4f per-session=$%.4f (refuses new turns when exceeded; clear via Agent.ResetCostCeiling)", ceiling.MaxTurnUSD, ceiling.MaxSessionUSD))
	}
	// Behavioral watchdog (#123 PR 2). Off when --watchdog=off;
	// observe + log when --watchdog=warn (default). Alerts go to
	// the operator via send(); future versions can route to SSE
	// turn-error events or prompt the operator interactively.
	switch strings.ToLower(strings.TrimSpace(watchdogMode)) {
	case "off":
		// no-op
	case "warn", "":
		w := watchdog.NewDefaultWatchdog()
		opts = append(opts, agent.WithWatchdog(w, func(a watchdog.Alert) {
			send(fmt.Sprintf("watchdog %s", a.String()))
		}))
		send("watchdog: warn mode (observes tool-call stream; logs structured alerts on runaway patterns)")
	default:
		log.Printf("invalid --watchdog mode %q (want warn|off)", watchdogMode)
		return runner.ExitConfigError
	}
	if !noCheckpoint {
		opts = append(opts, agent.WithCheckpointer(agent.NewDefaultCheckpointer()))
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

	// Attach-mode wiring. Must come after the eventlog is set up
	// (broadcaster requires a Stream) and before the agent is
	// constructed (so the registry is in opts).
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
		opts = append(opts, agent.WithSessionRegistry(attach.NewAgentRegistrarAdapter(attachReg)))

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
		opts = append(opts, agent.WithAttachPromptBroker(promptBroker))
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
		authn, defaultCaller, authErr := buildMultiSessionAuthn(cfg.Attach.MultiSession)
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
		// features (BackgroundManager, Compactor, Watchdog, etc.) are
		// not yet wired into on-demand sessions; document follow-up.
		var sessionFactory attach.SessionFactory
		var sessionResumer attach.SessionResumer
		if cfg.Attach.MultiSession.Enabled {
			factoryDeps := sessionFactoryDeps{
				daemonCtx:      ctx,
				model:          m,
				template:       template,
				pricingRate:    pricingRate,
				agentsDir:      agentsDir,
				cfg:            cfg,
				mcpServers:     mcpServers,
				builtinTools:   builtinTools,
				toolsets:       allToolsets,
				eventlogHandle: eventlogHandle,
				projectRoot:    projectRoot,
				userRoot:       coreHome,
				usersDir:       cfg.Attach.MultiSession.UsersDir,
				registry:       attachReg,
				aclStore:       aclStore,
				noCompact:      noCompact,
				noCheckpoint:   noCheckpoint,
			}
			sessionFactory = buildSessionFactory(factoryDeps)
			// Session resume: reconstructs sessions persisted in
			// agent_session_acl that aren't in the in-memory
			// registry yet (post-daemon-restart, post-eviction).
			// nil when aclStore is nil — pre-v2.5 deployments
			// without persisted ACLs keep their legacy 404-on-miss
			// behavior. Wired into attach.NewServer's Options.Resumer
			// below.
			sessionResumer = buildSessionResumer(factoryDeps)
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
		attachSrv, err := attach.NewServer(attach.Options{
			Registry:     attachReg,
			PeerRegistry: peerReg,
			Addr:         attachCfg.Listen,
			UnixSocket:   attachCfg.UnixSocket,
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
		a, err := agent.New(m, opts...)
		if err != nil {
			fmt.Fprintf(os.Stderr, "core-agent: %v\n", err)
			return runner.ExitAgentError
		}
		fmt.Fprintf(os.Stderr,
			"core-agent: --no-repl: attach-only mode, session %s (Ctrl-C or SIGTERM to exit)\n",
			a.SessionID())
		debugf("--no-repl: wake loop starting (session=%s model=%s)", a.SessionID(), m.Name())
		// Wake-driven inbox loop: when an attach client POSTs
		// /inject, agent.Inject appends to the inbox + fires
		// WakeRequested. We consume the event iterator from
		// a.Run so the turn actually completes; the events also
		// hit the eventlog → attach broadcaster, which is what
		// the operator's TUI is rendering. Empty prompt means
		// "no user text this turn, just drain the inbox" — same
		// path REPL uses for the same case.
		//
		// Per-turn usage tap mirrors runner/headless.go's tapUsage:
		// the loop watches each event's UsageMetadata, remembers
		// the latest in/out counts, and on iterator end calls
		// tracker.Append once. Without this the /stats and status-
		// banner cumulative totals stay at zero in --no-repl mode
		// because the tracker is only otherwise driven by
		// agent/autonomous.go and agent/subtask.go.
		for {
			select {
			case <-ctx.Done():
				debugf("--no-repl: wake loop ending (ctx cancelled)")
				return runner.ExitOK
			case <-a.WakeRequested():
				debugf("--no-repl: wake fired; calling Run")
				var lastUsage usage.TurnUsage
				var evCount int
				for ev, runErr := range a.Run(ctx, "") {
					evCount++
					if ev != nil && ev.UsageMetadata != nil {
						lastUsage = usage.TurnUsageFromGenaiMetadata(ev.UsageMetadata)
					}
					if runErr != nil {
						fmt.Fprintf(os.Stderr, "core-agent: turn: %v\n", runErr)
						debugf("--no-repl: Run yielded error: %v", runErr)
					}
				}
				debugf("--no-repl: Run finished (events=%d lastIn=%d lastOut=%d)", evCount, lastUsage.InputTokens, lastUsage.OutputTokens)
				if tracker != nil && (lastUsage.InputTokens > 0 || lastUsage.OutputTokens > 0) {
					tracker.AppendUsage(m.Name(), lastUsage, pricingRate)
				}
			}
		}
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
			Provider:      provider,
			Gate:          gate,
			Tracker:       tracker,
			Memory:        loaded,
			MCPServers:    mcpServers,
			LoadedSkills:  loadedSkills,
			AgentsDir:     agentsDir,
			CoreHome:      coreHome,
			ProjectRoot:   projectRoot,
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

	if initialPrompt != "" {
		code, err = runner.REPLWithInitialPrompt(ctx, m, initialPrompt, os.Stdin, os.Stdout, os.Stderr, tracker, pricingRate, opts, eventsOpts...)
	} else {
		code, err = runner.REPL(ctx, m, os.Stdin, os.Stdout, os.Stderr, tracker, pricingRate, opts, eventsOpts...)
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

// installLogFilter replaces log.Default()'s output with a writer
// that drops lines matching known-noisy patterns the bundled CLI
// doesn't want surfaced to users. Today the only filtered line is
// `Error context canceled` from genai's SSE scanner, which fires
// every time the user hits ESC mid-turn (genai/api_client.go:484
// log.Printf's it unconditionally).
//
// Anything that isn't filtered passes through to fallback (typically
// os.Stderr) unchanged, so consumer-supplied log lines still appear.
func installLogFilter(fallback io.Writer) {
	log.SetOutput(&filteredLogWriter{w: fallback})
	// Strip the default date/time prefix so any line that DOES make
	// it through reads like a normal stderr message rather than a
	// log entry. Genai's own log.Printf will pick up our flags;
	// fortunately the line we're filtering is the noisy one.
	log.SetFlags(0)
}

// filteredLogWriter drops noisy log lines from genai/ADK that the
// bundled CLI doesn't want to expose.
type filteredLogWriter struct{ w io.Writer }

// drop is the set of substrings that mark a line for filtering.
// Kept small + literal so we don't accidentally suppress something
// users need to see.
var droppedLogPatterns = [][]byte{
	[]byte("Error context canceled"),
	[]byte("Error context deadline exceeded"),
}

func (f *filteredLogWriter) Write(p []byte) (int, error) {
	for _, pat := range droppedLogPatterns {
		if bytes.Contains(p, pat) {
			// Return the full length so log.Output() doesn't see a
			// short write and retry. The semantic is "consumed".
			return len(p), nil
		}
	}
	return f.w.Write(p)
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

func persistTranscript(agentsDir, model, prompt string, tracker *usage.Tracker) {
	if agentsDir == "" {
		return
	}
	tot := tracker.Totals()
	_, _ = session.Save(agentsDir, session.Transcript{
		Model: model,
		Messages: []session.Message{
			{Role: "user", Text: prompt},
		},
		Usage: session.Usage{
			Turns:        tot.Turns,
			InputTokens:  tot.InputTokens,
			OutputTokens: tot.OutputTokens,
			CostUSD:      tot.CostUSD,
		},
	})
}
