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
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/glebarez/sqlite"
	adktool "google.golang.org/adk/tool"

	"github.com/go-steer/core-agent/agent"
	"github.com/go-steer/core-agent/attach"
	"github.com/go-steer/core-agent/config"
	"github.com/go-steer/core-agent/eventlog"
	"github.com/go-steer/core-agent/instruction"
	"github.com/go-steer/core-agent/mcp"
	"github.com/go-steer/core-agent/models"
	_ "github.com/go-steer/core-agent/models/anthropic"
	"github.com/go-steer/core-agent/models/gemini"
	_ "github.com/go-steer/core-agent/models/mock"
	"github.com/go-steer/core-agent/permissions"
	"github.com/go-steer/core-agent/recording"
	"github.com/go-steer/core-agent/runner"
	"github.com/go-steer/core-agent/session"
	"github.com/go-steer/core-agent/skills"
	"github.com/go-steer/core-agent/telemetry"
	"github.com/go-steer/core-agent/tools"
	"github.com/go-steer/core-agent/usage"
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

	prompt := flag.String("p", "", "single prompt; runs one turn and exits (REPL otherwise)")
	cfgPath := flag.String("c", "", "config file path (default: discover .agents/config.json)")
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
	attachListen := flag.String("attach-listen", "", "enable attach-mode HTTP listener on this address (e.g. :7777). Requires --session-db.")
	attachUnixSocket := flag.String("attach-unix-socket", "", "enable attach-mode on a Unix socket at this path. Mutually exclusive with --attach-listen.")
	attachTLSCert := flag.String("attach-tls-cert", "", "TLS server certificate (PEM) for --attach-listen. Pair with --attach-tls-key.")
	attachTLSKey := flag.String("attach-tls-key", "", "TLS server key (PEM) for --attach-listen.")
	attachClientCA := flag.String("attach-client-ca", "", "CA PEM for client-cert verification (mTLS). When set, clients must present a cert signed by this CA.")
	attachTokenEnv := flag.String("attach-token", "", "env var name holding the bearer token clients must present in Authorization: Bearer <token>. Empty disables bearer-token auth.")
	attachReadonly := flag.Bool("attach-readonly", false, "attach-mode: disable POST /inject and /wake. Read endpoints (GET /sessions, GET /events) remain open.")
	flag.Parse()

	code := run(*prompt, *cfgPath, *modelOverride, *providerOverride, *noBuiltinTools, *disableTools, *scriptPath, *scriptStrict, *recordTo, *color, *ask, *sessionDB, *sessionDBPath, *yolo, *noBackgroundAgents, *allowURLHost,
		attachOpts{
			Listen:     *attachListen,
			UnixSocket: *attachUnixSocket,
			TLSCert:    *attachTLSCert,
			TLSKey:     *attachTLSKey,
			ClientCA:   *attachClientCA,
			TokenEnv:   *attachTokenEnv,
			ReadOnly:   *attachReadonly,
		})
	os.Exit(code)
}

// attachOpts bundles the attach-mode CLI flags so run()'s signature
// doesn't grow by 7 more positional args.
type attachOpts struct {
	Listen     string
	UnixSocket string
	TLSCert    string
	TLSKey     string
	ClientCA   string
	TokenEnv   string
	ReadOnly   bool
}

func run(prompt, cfgPath, modelOverride, providerOverride string, noBuiltinTools bool, disableTools string, scriptPath string, scriptStrict bool, recordTo string, color string, ask string, sessionDB bool, sessionDBPath string, yolo, noBackgroundAgents bool, allowURLHost string, attachCfg attachOpts) int {
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

	cwd, _ := os.Getwd()
	cfg, agentsDir, err := loadConfig(cfgPath, cwd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "core-agent: %v\n", err)
		return runner.ExitConfigError
	}
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
	gate, err := permissions.FromConfig(cfg, cwd, coreHome, resolveGatePrompter(yolo, os.Stdin, os.Stderr))
	if err != nil {
		fmt.Fprintf(os.Stderr, "core-agent: %v\n", err)
		return runner.ExitConfigError
	}

	projectRoot := cwd
	if agentsDir != "" {
		projectRoot = filepath.Dir(agentsDir)
	}
	loaded, err := instruction.Load(projectRoot, coreHome)
	if err != nil {
		fmt.Fprintf(os.Stderr, "core-agent: instruction load: %v\n", err)
	}

	send := func(s string) { fmt.Fprintln(os.Stderr, "core-agent: "+s) }
	_, mcpToolsets, mcpErr := mcp.Build(ctx, agentsDir, send, gate, nil)
	if mcpErr != nil {
		fmt.Fprintf(os.Stderr, "core-agent: mcp: %v\n", mcpErr)
	}
	loadedSkills, skillsErr := skills.Load(ctx, agentsDir, gate)
	if skillsErr != nil {
		fmt.Fprintf(os.Stderr, "core-agent: skills: %v\n", skillsErr)
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
		reg, err := tools.Build(cfg, gate, b)
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

	tracker := usage.NewTracker()
	pricing := usage.PriceFor(cfg.Model.Name, cfg)

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

	opts := []agent.Option{
		agent.WithTools(builtinTools),
		agent.WithToolsets(allToolsets),
		agent.WithSystemInstructionPrefix(loaded.Instruction),
	}
	if bgMgr != nil {
		opts = append(opts, agent.WithBackgroundManager(bgMgr))
	}

	// Durable sessions + audit log. Either flag enables: --session-db
	// alone uses the default path (~/.<binary>/sessions.db);
	// --session-db-path enables and overrides the path. Off by default
	// to preserve historical CLI behavior (in-memory, ephemeral).
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
		handle, err := eventlog.Open(ctx, sqlite.Open(path))
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
		fmt.Fprintf(os.Stderr, "core-agent: session db: %s\n", path)
	}

	// Attach-mode wiring. Must come after the eventlog is set up
	// (broadcaster requires a Stream) and before the agent is
	// constructed (so the registry is in opts).
	if attachCfg.Listen != "" || attachCfg.UnixSocket != "" {
		if !sessionDB && sessionDBPath == "" {
			fmt.Fprintln(os.Stderr, "core-agent: --attach-listen / --attach-unix-socket requires --session-db (broadcaster pumps from the event log)")
			return runner.ExitConfigError
		}
		attachReg := attach.NewSessionRegistry()
		opts = append(opts, agent.WithSessionRegistry(attach.NewAgentRegistrarAdapter(attachReg)))
		token := ""
		if attachCfg.TokenEnv != "" {
			token = os.Getenv(attachCfg.TokenEnv)
			if token == "" {
				fmt.Fprintf(os.Stderr, "core-agent: --attach-token=%s is empty in the environment\n", attachCfg.TokenEnv)
				return runner.ExitConfigError
			}
		}
		attachSrv, err := attach.NewServer(attach.Options{
			Registry:   attachReg,
			Addr:       attachCfg.Listen,
			UnixSocket: attachCfg.UnixSocket,
			Auth: attach.AuthConfig{
				TLSCertFile:  attachCfg.TLSCert,
				TLSKeyFile:   attachCfg.TLSKey,
				ClientCAFile: attachCfg.ClientCA,
				BearerToken:  token,
				ReadOnly:     attachCfg.ReadOnly,
			},
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "core-agent: attach server: %v\n", err)
			return runner.ExitConfigError
		}
		go func() {
			endpoint := attachCfg.Listen
			if endpoint == "" {
				endpoint = "unix://" + attachCfg.UnixSocket
			}
			fmt.Fprintf(os.Stderr, "core-agent: attach listener on %s\n", endpoint)
			if err := attachSrv.ListenAndServe(); err != nil {
				fmt.Fprintf(os.Stderr, "core-agent: attach server: %v\n", err)
			}
		}()
		defer func() { _ = attachSrv.Close() }()
	}

	colorOn, err := resolveColor(color, os.Stdout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "core-agent: %v\n", err)
		return runner.ExitConfigError
	}
	eventsOpts := []runner.EventsOption{runner.WithColor(colorOn)}

	var code int
	if prompt != "" {
		code, err = runner.Headless(ctx, m, prompt, os.Stdout, os.Stderr, tracker, pricing, opts, eventsOpts...)
		if err != nil {
			fmt.Fprintf(os.Stderr, "core-agent: %v\n", err)
		}
		if code == runner.ExitOK {
			runner.WriteSummary(os.Stderr, tracker, m.Name())
			persistTranscript(agentsDir, m.Name(), prompt, tracker)
		}
		return code
	}

	code, err = runner.REPL(ctx, m, os.Stdin, os.Stdout, os.Stderr, tracker, pricing, opts, eventsOpts...)
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
