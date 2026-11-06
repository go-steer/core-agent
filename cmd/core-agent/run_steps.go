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
	"flag"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"golang.org/x/term"
	adkmodel "google.golang.org/adk/v2/model"
	adktool "google.golang.org/adk/v2/tool"

	"github.com/go-steer/core-agent/v3/internal/approvalnotify"
	"github.com/go-steer/core-agent/v3/internal/webui"
	"github.com/go-steer/core-agent/v3/pkg/agent"
	"github.com/go-steer/core-agent/v3/pkg/agent/background"
	"github.com/go-steer/core-agent/v3/pkg/agentenv"
	"github.com/go-steer/core-agent/v3/pkg/attach"
	"github.com/go-steer/core-agent/v3/pkg/attachadapter"
	"github.com/go-steer/core-agent/v3/pkg/compose"
	"github.com/go-steer/core-agent/v3/pkg/config"
	"github.com/go-steer/core-agent/v3/pkg/digest"
	"github.com/go-steer/core-agent/v3/pkg/eventlog"
	"github.com/go-steer/core-agent/v3/pkg/hooks"
	"github.com/go-steer/core-agent/v3/pkg/instruction"
	"github.com/go-steer/core-agent/v3/pkg/mcp"
	"github.com/go-steer/core-agent/v3/pkg/models"
	"github.com/go-steer/core-agent/v3/pkg/models/profiles"
	"github.com/go-steer/core-agent/v3/pkg/permissions"
	"github.com/go-steer/core-agent/v3/pkg/pricing"
	"github.com/go-steer/core-agent/v3/pkg/recording"
	"github.com/go-steer/core-agent/v3/pkg/runner"
	"github.com/go-steer/core-agent/v3/pkg/skills"
	"github.com/go-steer/core-agent/v3/pkg/taskclass"
	"github.com/go-steer/core-agent/v3/pkg/telemetry"
	"github.com/go-steer/core-agent/v3/pkg/tools"
	"github.com/go-steer/core-agent/v3/pkg/tools/alert"
	"github.com/go-steer/core-agent/v3/pkg/tools/vcs"
	"github.com/go-steer/core-agent/v3/pkg/usage"
)

// driverOpts bundles interactive vs headless execution-driver flags (#685).
type driverOpts struct {
	prompt        string
	initialPrompt string
	color         string
	ask           string
	yolo          bool
	noREPL        bool
	noTUI         bool
}

// workspaceOpts bundles config discovery, workspace roots, and scope overrides (#685).
type workspaceOpts struct {
	cfgPath           string
	agentsDirFlag     string
	allowURLHost      string
	allowPathEntries  []config.PathScopeAllowEntry
	contentDirEntries []string
}

// modelOpts bundles model, provider, caching, and mock/recording flags (#685).
type modelOpts struct {
	modelOverride    string
	providerOverride string
	taskClass        string
	scriptPath       string
	scriptStrict     bool
	recordTo         string
	smallTierParent  string
	noPricingRefresh bool
	noContextCache   bool
	noPromptCache    bool
	promptCacheTTL   string
}

// toolCatalogOpts bundles built-in tool, subagent, digest, and compaction flags (#685).
type toolCatalogOpts struct {
	noBuiltinTools      bool
	disableTools        string
	noBackgroundAgents  bool
	agenticTools        bool
	agenticSmallModel   string
	noMCPDigest         bool
	mcpAgenticWrapLLM   bool
	mcpAgenticWrapModel string
	noCompact           bool
	compactionThreshold float64
}

// runOptions groups all CLI flag families passed into run() (#685).
type runOptions struct {
	driver      driverOpts
	workspace   workspaceOpts
	model       modelOpts
	toolCatalog toolCatalogOpts
	session     sessionOpts
	checkpoint  checkpointOpts
	subagent    subagentOpts
	guardrails  guardrailOpts
	toolProfile toolProfileOpts
	prompt      promptOpts
	attach      attachOpts
	agentCard   agentCardOpts
	metrics     metricsOpts
}

// runState holds the resolved state assembled across startupSteps (#685).
type runState struct {
	ctx      context.Context
	stop     context.CancelFunc
	opts     runOptions
	send     func(string)
	flagSet  *flag.FlagSet
	cleanups []func()

	// Step ordering latches verified by downstream steps so transposing
	// order-dependent steps fails deterministically.
	flagsValidated          bool
	configLoaded            bool
	credentialsWithheld     bool
	taskProfileResolved     bool
	planModeApplied         bool
	telemetryReady          bool
	modelResolved           bool
	gateBuilt               bool
	instructionsLoaded      bool
	mcpBuilt                bool
	skillsLoaded            bool
	toolCatalogBuilt        bool
	pricingCatalogInstalled bool
	subagentsBuilt          bool
	sessionEventlogResolved bool

	outFormat           runner.OutputFormat
	unattended          bool
	cwd                 string
	cfg                 *config.Config
	agentsDir           string
	discoveredAgentsDir string
	agentsDirFrom       agentsDirSource
	envResolver         *agentenv.Resolver
	envInterp           func(string) string
	envResidual         *agentenv.ResidualRefs

	attachToken     string
	taskProfile     taskclass.Profile
	profileDisables []string

	provider  models.Provider
	model     adkmodel.LLM
	titleLLM  adkmodel.LLM
	coreHome  string
	homeAgDir string
	guard     guardrailResolution
	template  *permissions.Gate
	gate      *permissions.Gate

	projectRoot       string
	contentRootBase   string
	contentRoots      []string
	loadedInstruction instruction.Loaded

	agentRef       *agent.Agent
	tracker        *usage.Tracker
	primaryTracker *primaryTrackerProvider
	digestStore    *digest.LazyStore
	digestOpts     *mcp.DigestOptions
	mcpElicitor    mcp.ElicitorFn
	mcpServers     []*mcp.Server
	mcpToolsets    []adktool.Toolset

	builtinSelection tools.BuiltinTools
	loadedSkills     skills.Skills
	allToolsets      []adktool.Toolset
	eventlogHandle   *eventlog.Handle
	builtinTools     []adktool.Tool
	peerReg          *attach.PeerRegistry

	pricingRate       usage.Pricing
	bgMgr             *background.Manager
	ceilingPolicy     atomic.Pointer[ceilingPricing]
	spawnBudgetWarned warnOnce
	bgRecipe          sessionBackgroundRecipe

	appendPrompt      string
	replacePrompt     string
	declaredSubagents []*agent.Agent
	subagentTemplates []background.SubagentTemplate
	subagentServers   []*mcp.Server

	pricingPolicy             ceilingPricing
	ckpt                      checkpointResolution
	agentOpts                 []agent.Option
	adapterOpts               []attachadapter.Option
	autoContinueEnabled       bool
	autoContinueFreshness     time.Duration
	autoContinueRetry         bool
	autoContinueRetryInterval time.Duration
	loopHealth                *runner.LoopHealthSet
	autoContinueDriverCtx     context.Context
	autoContinueWG            sync.WaitGroup

	attachRegistry          *attach.SessionRegistry
	multiSessionFactoryDeps *compose.SessionFactoryDeps
}

func newRunState(ctx context.Context, stop context.CancelFunc, opts runOptions) *runState {
	return &runState{
		ctx:     ctx,
		stop:    stop,
		opts:    opts,
		flagSet: flag.CommandLine,
		send: func(s string) {
			if !opts.prompt.quiet {
				fmt.Fprintln(os.Stderr, "core-agent: "+s)
			}
		},
	}
}

func (s *runState) addCleanup(fn func()) {
	if fn != nil {
		s.cleanups = append(s.cleanups, fn)
	}
}

func (s *runState) close() {
	for i := len(s.cleanups) - 1; i >= 0; i-- {
		s.cleanups[i]()
	}
}

// startupStep is one named, order-checked phase of CLI startup (#685).
type startupStep struct {
	name     string
	requires []string
	run      func(*runState) int
}

// startupSteps is the canonical ordered sequence of startup phases for
// cmd/core-agent (#685). Each step names the upstream steps whose state it
// consumes; validateStartupStepOrder and per-step precondition checks fail
// if order-dependent steps are transposed.
var startupSteps = []startupStep{
	{
		name: "validate-flags",
		run:  stepValidateInteractiveAndOutputFlags,
	},
	{
		name:     "load-config-and-trust",
		requires: []string{"validate-flags"},
		run:      stepLoadConfigAndWorkspaceTrust,
	},
	{
		name:     "apply-cli-and-task-profile",
		requires: []string{"load-config-and-trust"},
		run:      stepApplyCLIAndTaskProfileOverrides,
	},
	{
		name:     "setup-telemetry",
		requires: []string{"load-config-and-trust"},
		run:      stepSetupTelemetryAndMetrics,
	},
	{
		name:     "resolve-model",
		requires: []string{"apply-cli-and-task-profile"},
		run:      stepResolveModelAndProvider,
	},
	{
		name:     "build-permission-gate",
		requires: []string{"apply-cli-and-task-profile", "resolve-model"},
		run:      stepBuildPermissionGate,
	},
	{
		name:     "load-instructions",
		requires: []string{"apply-cli-and-task-profile", "resolve-model", "build-permission-gate"},
		run:      stepLoadInstructionsAndCheckSmallTier,
	},
	{
		name:     "build-mcp",
		requires: []string{"setup-telemetry", "resolve-model", "build-permission-gate", "load-instructions"},
		run:      stepRegisterCoreMetricsAndBuildMCP,
	},
	{
		name:     "load-skills",
		requires: []string{"load-instructions", "build-mcp"},
		run:      stepLoadSkillsAndStartupSummary,
	},
	{
		name:     "build-tools",
		requires: []string{"load-skills"},
		run:      stepBuildToolCatalog,
	},
	{
		name:     "setup-pricing-and-subagent-tools",
		requires: []string{"build-tools"},
		run:      stepSetupPricingAndSubagentTools,
	},
	{
		name:     "build-declarative-subagents",
		requires: []string{"setup-pricing-and-subagent-tools"},
		run:      stepBuildDeclarativeSubagentsAndPromptLayers,
	},
	{
		name:     "assemble-agent-and-eventlog",
		requires: []string{"build-declarative-subagents"},
		run:      stepAssembleAgentAndEventlog,
	},
	{
		name:     "start-attach",
		requires: []string{"assemble-agent-and-eventlog"},
		run:      stepStartAttachAndPeerHub,
	},
}

// validateStartupStepOrder checks that every step's declared prerequisites
// appear earlier in steps.
func validateStartupStepOrder(steps []startupStep) error {
	seen := make(map[string]bool, len(steps))
	for _, st := range steps {
		for _, req := range st.requires {
			if !seen[req] {
				return fmt.Errorf("startup step %q requires %q to run before it", st.name, req)
			}
		}
		seen[st.name] = true
	}
	return nil
}

// runStartupSteps validates step ordering and runs each step in sequence,
// stopping at the first non-zero exit code.
func runStartupSteps(s *runState, steps []startupStep) int {
	if err := validateStartupStepOrder(steps); err != nil {
		fmt.Fprintf(os.Stderr, "core-agent: internal startup step order error: %v\n", err)
		return runner.ExitConfigError
	}
	for _, st := range steps {
		if code := st.run(s); code != runner.ExitOK {
			return code
		}
	}
	return runner.ExitOK
}

// stepValidateInteractiveAndOutputFlags validates -i vs -p / --no-repl and
// resolves --output-format and unattended status.
func stepValidateInteractiveAndOutputFlags(s *runState) int {
	// -i is incompatible with headless (-p runs one turn and exits;
	// -i seeds one turn and stays interactive — they can't both win)
	// and with --no-repl (attach-only daemon; the seed prompt has no
	// operator surface to remain interactive on). Reject early with a
	// config error so operators see the message before startup work.
	if s.opts.driver.initialPrompt != "" {
		if s.opts.driver.prompt != "" {
			fmt.Fprintln(os.Stderr, "core-agent: -p and -i are mutually exclusive (headless single-turn vs seeded interactive)")
			return runner.ExitConfigError
		}
		if s.opts.driver.noREPL {
			fmt.Fprintln(os.Stderr, "core-agent: -i is not compatible with --no-repl (attach-only daemon has no operator to stay interactive for)")
			return runner.ExitConfigError
		}
	}
	outFormat, err := resolveOutputFormat(s.opts.prompt.outputFormat, s.opts.driver.prompt)
	if err != nil {
		fmt.Fprintf(os.Stderr, "core-agent: %v\n", err)
		return runner.ExitConfigError
	}
	s.outFormat = outFormat
	s.unattended = s.opts.driver.prompt != "" || s.opts.driver.noREPL || !term.IsTerminal(int(os.Stdin.Fd()))
	s.flagsValidated = true
	return runner.ExitOK
}

// stepLoadConfigAndWorkspaceTrust loads .agents/config.json, resolves
// --agents-dir, applies workspace trust gating (#1353), and loads the
// optional .agents/env.yaml manifest (#322).
func stepLoadConfigAndWorkspaceTrust(s *runState) int {
	if !s.flagsValidated {
		return runner.ExitConfigError
	}
	s.cwd, _ = os.Getwd()
	cfg, agentsDir, err := loadConfig(s.opts.workspace.cfgPath, s.opts.workspace.agentsDirFlag, s.cwd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "core-agent: %v\n", err)
		return runner.ExitConfigError
	}
	// --agents-dir overrides the derivation (#945). Keep what
	// loadConfig found so splitTreeWarning can tell the operator when
	// the two disagree.
	s.discoveredAgentsDir = agentsDir
	agentsDir, agentsDirFrom, err := resolveAgentsDir(s.opts.workspace.agentsDirFlag, s.opts.workspace.cfgPath, agentsDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "core-agent: %v\n", err)
		return runner.ExitConfigError
	}
	if w := splitTreeWarning(agentsDirFrom, s.opts.workspace.cfgPath, agentsDir, s.discoveredAgentsDir); w != "" {
		fmt.Fprintln(os.Stderr, w)
	}
	s.cfg = cfg
	s.agentsDir = agentsDir
	s.agentsDirFrom = agentsDirFrom

	s.ctx, err = applyWorkspaceTrust(s.ctx, s.cfg, s.agentsDir, s.discoveredAgentsDir, s.agentsDirFrom, s.opts.guardrails.trust, s.unattended, s.send)
	if err != nil {
		fmt.Fprintf(os.Stderr, "core-agent: %v\n", err)
		return runner.ExitConfigError
	}

	// #322: load the optional env manifest (.agents/env.yaml or
	// .env.json) that declares which env vars the bundle expects.
	envManifest, err := agentenv.LoadManifest(s.agentsDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "core-agent: %v\n", err)
		return runner.ExitConfigError
	}
	s.envResolver = agentenv.NewResolver(envManifest, os.LookupEnv)
	// #712: watch loaded content for ${env:...} placeholders that survive
	// the interpolation pass.
	s.envInterp, s.envResidual = newEnvInterpolator(s.envResolver)
	if errs := s.envResolver.Errors(); len(errs) > 0 {
		for _, e := range errs {
			fmt.Fprintf(os.Stderr, "core-agent: %v\n", e)
		}
		return runner.ExitConfigError
	}
	s.configLoaded = true
	return runner.ExitOK
}

// stepApplyCLIAndTaskProfileOverrides merges attach flags, withholds daemon
// credentials and credential files (#1201), applies CLI model/mock/url/compaction
// overrides, resolves the --task profile (#123), and applies --plan-mode (#160).
func stepApplyCLIAndTaskProfileOverrides(s *runState) int {
	if !s.configLoaded || s.cfg == nil {
		return runner.ExitConfigError
	}
	flagSet := s.flagSet
	if flagSet == nil {
		flagSet = flag.CommandLine
	}
	s.opts.attach = mergeAttachOpts(s.opts.attach, s.cfg.Attach, flagSet)
	withholdDaemonCredentials(s.cfg, s.opts.attach.TokenEnv, os.Stderr)
	withholdCredentialFiles(s.cfg, s.opts.attach.TokenFile) // #1201: before the first instruction load below
	s.credentialsWithheld = true

	attachToken, err := attachTokenIfUsed(s.opts.attach, os.Stderr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "core-agent: %v\n", err)
		return runner.ExitConfigError
	}
	s.attachToken = attachToken

	if s.opts.model.modelOverride != "" {
		s.cfg.Model.Name = s.opts.model.modelOverride
	}
	if s.opts.model.providerOverride != "" {
		s.cfg.Model.Provider = s.opts.model.providerOverride
	}
	if s.opts.model.scriptPath != "" {
		s.cfg.Mock.Script = s.opts.model.scriptPath
	}
	if s.opts.model.scriptStrict {
		s.cfg.Mock.Strict = true
	}
	if s.opts.model.recordTo != "" {
		s.cfg.Mock.Record = s.opts.model.recordTo
	}
	if s.opts.workspace.allowURLHost != "" {
		for _, h := range strings.Split(s.opts.workspace.allowURLHost, ",") {
			if h = strings.TrimSpace(h); h != "" {
				s.cfg.URLScope.Allow = append(s.cfg.URLScope.Allow, h)
			}
		}
	}
	if s.opts.toolCatalog.compactionThreshold != 0 {
		if s.opts.toolCatalog.compactionThreshold <= 0 || s.opts.toolCatalog.compactionThreshold >= 1 {
			fmt.Fprintf(os.Stderr, "core-agent: --compaction-threshold=%v must be in (0, 1) exclusive\n", s.opts.toolCatalog.compactionThreshold)
			return runner.ExitConfigError
		}
		thr := s.opts.toolCatalog.compactionThreshold
		s.cfg.Compaction.Threshold = &thr
	}
	return s.applyTaskProfileAndPlanMode()
}

func (s *runState) applyTaskProfileAndPlanMode() int {
	if s.opts.model.taskClass != "" {
		s.cfg.Session.TaskClass = s.opts.model.taskClass
	}
	var taskProfile taskclass.Profile
	if s.cfg.Session.TaskClass != "" {
		profile, ok := taskclass.Resolve(s.cfg.Session.TaskClass)
		if !ok {
			fmt.Fprintf(os.Stderr, "core-agent: --task=%q: unknown task class (want one of %v)\n",
				s.cfg.Session.TaskClass, taskclass.Classes())
			return runner.ExitConfigError
		}
		taskProfile = profile
	}
	profileDisables, err := resolveProfileDisables(taskProfile.DisableTools, s.cfg.Tools.Disable, splitList(s.opts.toolCatalog.disableTools), splitList(s.opts.toolProfile.enableTools))
	if err != nil {
		fmt.Fprintf(os.Stderr, "core-agent: %v\n", err)
		return runner.ExitConfigError
	}
	s.taskProfile = taskProfile
	s.profileDisables = profileDisables

	if s.cfg.Session.TaskClass != "" {
		providerForTier := s.cfg.Model.Provider
		if providerForTier == "" {
			providerForTier = models.AutoDetectProvider()
		}
		if s.opts.model.modelOverride == "" && s.cfg.Model.Name == config.DefaultConfig().Model.Name {
			if tierModel := taskclass.ModelForTier(providerForTier, taskProfile.Tier); tierModel != "" {
				s.cfg.Model.Name = tierModel
			}
		}
		if s.cfg.Compaction.Threshold == nil && taskProfile.CompactionThreshold > 0 {
			thr := taskProfile.CompactionThreshold
			s.cfg.Compaction.Threshold = &thr
		}
		if s.opts.driver.ask == "" && taskProfile.AskMode != "" {
			s.opts.driver.ask = taskProfile.AskMode
		}
		toolNote := "tools=default"
		switch {
		case s.opts.toolCatalog.noBuiltinTools:
			toolNote = "tools=none (--no-builtin-tools)"
		case len(profileDisables) > 0:
			toolNote = "tools=default−" + strings.Join(profileDisables, ",")
		}
		s.send(fmt.Sprintf("task class: %s → model=%s compaction-threshold=%.2f ask=%s %s (override individual knobs with --model / --compaction-threshold / --ask / --enable-tools / --plan-mode)",
			s.cfg.Session.TaskClass, s.cfg.Model.Name, taskProfile.CompactionThreshold, s.opts.driver.ask, toolNote))
	}

	if s.cfg.IsProfileProvider(s.cfg.Model.Provider) && s.opts.model.modelOverride == "" && s.cfg.Model.Name == config.DefaultConfig().Model.Name {
		id, err := profiles.DefaultModel(s.cfg, s.cfg.Model.Provider, taskProfile.Tier)
		if err != nil {
			fmt.Fprintf(os.Stderr, "core-agent: %v\n", err)
			return runner.ExitConfigError
		}
		s.cfg.Model.Name = id
		s.send(fmt.Sprintf("provider profile %s → model=%s (from the profile's tiers; override with --model)", s.cfg.Model.Provider, id))
	}
	s.taskProfileResolved = true

	if err := applyPlanMode(s.cfg, s.agentsDir, s.opts.toolCatalog.noBuiltinTools, s.opts.toolCatalog.disableTools, s.opts.toolProfile, taskProfile, s.send); err != nil {
		fmt.Fprintf(os.Stderr, "core-agent: %v\n", err)
		return runner.ExitConfigError
	}
	s.planModeApplied = true
	return runner.ExitOK
}

// stepSetupTelemetryAndMetrics initializes OTel traces and metrics pipelines (#538).
func stepSetupTelemetryAndMetrics(s *runState) int {
	if !s.configLoaded || s.cfg == nil {
		return runner.ExitConfigError
	}
	otelShutdown, err := telemetry.Setup(s.ctx, s.cfg.OTEL.Exporter)
	if err != nil {
		fmt.Fprintf(os.Stderr, "core-agent: telemetry setup: %v\n", err)
	}
	s.addCleanup(func() {
		shCtx, cancel := context.WithTimeout(context.Background(), teardownStepTimeout)
		defer cancel()
		_ = otelShutdown(shCtx)
	})

	metricsShutdown, err := telemetry.SetupMetrics(s.ctx, s.cfg.OTEL.Metrics, telemetry.MetricsOptions{
		PrometheusAddr: s.opts.metrics.Addr,
		ServiceName:    "core-agent",
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "core-agent: metrics setup: %v\n", err)
		return runner.ExitConfigError
	}
	s.addCleanup(func() {
		shCtx, cancel := context.WithTimeout(context.Background(), teardownStepTimeout)
		defer cancel()
		_ = metricsShutdown(shCtx)
	})
	s.telemetryReady = true
	return runner.ExitOK
}

// stepResolveModelAndProvider resolves the model provider, wires Vertex context
// caching (#221) and Anthropic prompt caching (#714) ahead of Model(), and
// constructs the parent LLM (plus optional JSONL recorder).
func stepResolveModelAndProvider(s *runState) int {
	if s.cfg == nil || !s.taskProfileResolved {
		return runner.ExitConfigError
	}
	provider, err := models.Resolve(s.cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "core-agent: %v\n", err)
		return runner.ExitConfigError
	}
	s.provider = provider
	usage.SetBackend(models.BackendOf(provider))

	contextCacheManager := compose.MaybeWireContextCache(s.ctx, provider, s.cfg, s.opts.model.noContextCache, s.send)
	if contextCacheManager != nil {
		s.addCleanup(func() {
			shCtx, cancel := context.WithTimeout(context.Background(), teardownStepTimeout)
			defer cancel()
			contextCacheManager.Delete(shCtx)
		})
	}
	if status, _ := compose.MaybeWirePromptCacheTTL(provider, s.opts.model.noPromptCache, s.opts.model.promptCacheTTL); status != "" {
		s.send(status)
	}

	m, err := provider.Model(s.ctx, s.cfg.Model.Name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "core-agent: %v\n", err)
		return runner.ExitConfigError
	}
	if s.cfg.Mock.Record != "" {
		f, err := os.Create(s.cfg.Mock.Record)
		if err != nil {
			fmt.Fprintf(os.Stderr, "core-agent: --record-to: %v\n", err)
			return runner.ExitConfigError
		}
		s.addCleanup(func() { _ = f.Close() })
		m = recording.NewRecorder(m, f)
	}
	s.model = m
	s.modelResolved = true
	return runner.ExitOK
}

// stepBuildPermissionGate resolves permission/sandbox flags, guardrails,
// approval timeouts, and constructs the daemon template gate and per-session
// derived gate.
func stepBuildPermissionGate(s *runState) int {
	if s.cfg == nil || !s.modelResolved || !s.planModeApplied {
		return runner.ExitConfigError
	}
	userHome, _ := os.UserHomeDir()
	if userHome != "" {
		s.coreHome = filepath.Join(userHome, ".core-agent")
		s.homeAgDir = filepath.Join(userHome, ".agents")
	}

	if err := applyPermissionAndSandboxFlags(s.cfg, &s.opts.driver.yolo, s.opts.guardrails, s.send); err != nil {
		fmt.Fprintf(os.Stderr, "core-agent: %v\n", err)
		return runner.ExitConfigError
	}
	if len(s.opts.workspace.allowPathEntries) > 0 {
		s.cfg.PathScope.AllowPaths = append(s.cfg.PathScope.AllowPaths, s.opts.workspace.allowPathEntries...)
	}
	if s.opts.guardrails.bashSearchGate != "" {
		switch s.opts.guardrails.bashSearchGate {
		case config.BashSearchGateEnforce, config.BashSearchGateWarn, config.BashSearchGateAllow:
			s.cfg.Safety.BashSearchGate = s.opts.guardrails.bashSearchGate
		default:
			fmt.Fprintf(os.Stderr, "core-agent: --bash-search-gate: unknown value %q (want one of %q, %q, %q)\n",
				s.opts.guardrails.bashSearchGate, config.BashSearchGateEnforce, config.BashSearchGateWarn, config.BashSearchGateAllow)
			return runner.ExitConfigError
		}
	}
	prompter := resolveGatePrompter(s.cfg.Permissions.Mode == string(permissions.ModeYolo), os.Stdin, os.Stderr)
	hasPromptSurface := prompter != nil || s.opts.attach.Listen != "" || s.opts.attach.UnixSocket != ""
	guard, err := resolveGuardrails(guardrailInputs{
		WatchdogFlag:          s.opts.guardrails.watchdogMode,
		WatchdogConfig:        s.cfg.Safety.Watchdog,
		SessionCostFlag:       s.opts.guardrails.maxSessionCostUSD,
		SessionCostFlagSet:    s.opts.guardrails.maxSessionCostSet,
		SessionCostConfig:     s.cfg.Agent.MaxSessionCostUSD,
		Unattended:            s.unattended,
		ApprovalTimeoutFlag:   s.opts.guardrails.approvalTimeout,
		ApprovalTimeoutConfig: s.cfg.Permissions.ApprovalTimeout,
		PermMode:              s.cfg.Permissions.Mode,
		HasPromptSurface:      hasPromptSurface,
	})
	if err != nil {
		log.Printf("%v", err)
		return runner.ExitConfigError
	}
	s.guard = guard
	if guard.ApprovalTimeout > 0 {
		s.cfg.Permissions.ApprovalTimeout = guard.ApprovalTimeout.String()
	} else if guard.ApprovalTimeoutSource != sourceApprovalTimeoutUnset {
		s.cfg.Permissions.ApprovalTimeout = ""
	}
	template, err := permissions.FromConfig(s.cfg, s.cwd, s.coreHome, prompter)
	if err != nil {
		fmt.Fprintf(os.Stderr, "core-agent: %v\n", err)
		return runner.ExitConfigError
	}
	template.SetGrantStore(&permissions.ConfigGrantStore{AgentsDir: s.agentsDir})
	if err := wireApprover(s.ctx, s.provider, s.cfg, s.agentsDir, template, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "core-agent: %v\n", err)
		return runner.ExitConfigError
	}
	s.template = template
	s.gate = template.DeriveForSession("", prompter)
	s.gateBuilt = true
	return runner.ExitOK
}

// stepLoadInstructionsAndCheckSmallTier resolves content roots, loads
// AGENTS.md / CLAUDE.md / GEMINI.md instructions, and runs the small-tier
// parent model check (#121).
func stepLoadInstructionsAndCheckSmallTier(s *runState) int {
	if s.cfg == nil || !s.modelResolved || !s.gateBuilt || !s.credentialsWithheld {
		return runner.ExitConfigError
	}
	s.projectRoot = s.cwd
	if s.agentsDir != "" {
		s.projectRoot = filepath.Dir(s.agentsDir)
	}
	s.contentRootBase = s.agentsDir
	if s.contentRootBase == "" {
		s.contentRootBase = s.cwd
	}
	s.contentRoots = resolveContentRoots(s.cfg.ContentRoots, s.opts.workspace.contentDirEntries, s.contentRootBase)
	loaded, err := instruction.LoadForSession(s.projectRoot, s.coreHome, "", s.cfg.Attach.MultiSession.UsersDir,
		instruction.WithHomeAgentsRoot(s.homeAgDir),
		instruction.WithContentRoots(s.contentRoots),
		instruction.WithInterpolator(s.envInterp))
	if err != nil {
		fmt.Fprintf(os.Stderr, "core-agent: instruction load: %v\n", err)
		return runner.ExitConfigError
	}
	s.loadedInstruction = loaded

	if checkSmallTierParent(s.cfg, s.provider, s.opts.driver.prompt, s.opts.driver.yolo, s.opts.model.smallTierParent, s.send) != nil {
		return runner.ExitConfigError
	}
	s.instructionsLoaded = true
	return runner.ExitOK
}

// stepRegisterCoreMetricsAndBuildMCP registers usage/digest/agent metrics and
// starts configured MCP servers (#338, #84, #223).
func stepRegisterCoreMetricsAndBuildMCP(s *runState) int {
	if s.cfg == nil || !s.telemetryReady || !s.credentialsWithheld || !s.modelResolved || !s.gateBuilt || !s.instructionsLoaded {
		return runner.ExitConfigError
	}
	s.tracker = usage.NewTracker()
	s.primaryTracker = &primaryTrackerProvider{tracker: s.tracker}
	var usageMetricOpts []usage.RegisterOption
	if !s.cfg.OTEL.Metrics.SessionLabelsEnabled() {
		usageMetricOpts = append(usageMetricOpts, usage.WithoutSessionLabels())
	}
	if _, err := usage.RegisterMetrics(otel.GetMeterProvider(), s.primaryTracker, usageMetricOpts...); err != nil {
		fmt.Fprintf(os.Stderr, "core-agent: metrics: register usage observer: %v\n", err)
		return runner.ExitConfigError
	}
	if _, err := digest.RegisterMetrics(otel.GetMeterProvider()); err != nil {
		fmt.Fprintf(os.Stderr, "core-agent: metrics: register digest observer: %v\n", err)
		return runner.ExitConfigError
	}
	if _, err := agent.RegisterMetrics(otel.GetMeterProvider(), s.primaryTracker); err != nil {
		fmt.Fprintf(os.Stderr, "core-agent: metrics: register agent observer: %v\n", err)
		return runner.ExitConfigError
	}
	if !s.opts.toolCatalog.noMCPDigest {
		s.digestStore = &digest.LazyStore{}
		s.digestOpts = &mcp.DigestOptions{Store: s.digestStore}
		mcpCfg, _ := mcp.LoadAll(s.agentsDir, s.homeAgDir)
		llmEnabled := s.opts.toolCatalog.mcpAgenticWrapLLM || (!s.cfg.WorkspaceUntrusted() && mcpCfg.AgenticWrapLLMEnabled())
		llmModelOverride := s.opts.toolCatalog.mcpAgenticWrapModel
		if llmModelOverride == "" && !s.cfg.WorkspaceUntrusted() {
			llmModelOverride = mcpCfg.AgenticWrapModel
		}
		if llmEnabled {
			resolvedMCPModel := models.ResolveMCPSmallModel(s.provider, llmModelOverride, s.opts.toolCatalog.agenticSmallModel)
			s.digestOpts.LLMFallback = compose.BuildMCPDigestLLMFallback(&s.agentRef, s.provider, resolvedMCPModel)
			switch {
			case resolvedMCPModel == "":
				s.send(fmt.Sprintf("mcp agentic wrap: LLM subagent on, inherits parent (%s — no cheap-tier default for provider %q)", s.cfg.Model.Name, s.provider.Name()))
			case llmModelOverride != "":
				s.send(fmt.Sprintf("mcp agentic wrap: LLM subagent on, %s (mcp-specific override)", resolvedMCPModel))
			case s.opts.toolCatalog.agenticSmallModel != "":
				s.send(fmt.Sprintf("mcp agentic wrap: LLM subagent on, %s (via --agentic-small-model)", resolvedMCPModel))
			default:
				s.send(fmt.Sprintf("mcp agentic wrap: LLM subagent on, %s (provider default)", resolvedMCPModel))
			}
		}
	}
	s.mcpElicitor = makeMCPElicitor()
	mcpServers, mcpToolsets, mcpErr := mcp.Build(s.ctx, s.agentsDir, s.homeAgDir, s.send, s.gate, s.mcpElicitor, s.digestOpts)
	if mcpErr != nil {
		fmt.Fprintf(os.Stderr, "core-agent: mcp: %v\n", mcpErr)
	}
	for _, w := range mcp.Warnings(mcpServers) {
		fmt.Fprintf(os.Stderr, "core-agent: mcp: %s\n", w)
	}
	s.mcpServers = mcpServers
	s.mcpToolsets = mcpToolsets
	s.addCleanup(func() { mcp.CloseAll(s.mcpServers) })
	s.mcpBuilt = true
	return runner.ExitOK
}

// stepLoadSkillsAndStartupSummary resolves the built-in tool selection, guards
// credential files (#1201), loads SKILL.md bundles, reports env drift (#322),
// and emits the startup summary (#212).
func stepLoadSkillsAndStartupSummary(s *runState) int {
	if s.cfg == nil || !s.instructionsLoaded || !s.mcpBuilt || s.template == nil || s.gate == nil {
		return runner.ExitConfigError
	}
	var b tools.BuiltinTools
	if !s.opts.toolCatalog.noBuiltinTools {
		b = tools.Default()
		for _, name := range s.cfg.Tools.Disable {
			if err := b.Disable(name); err != nil {
				fmt.Fprintf(os.Stderr, "core-agent: config tools.disable: %v\n", err)
				return runner.ExitConfigError
			}
		}
		for _, name := range strings.Split(s.opts.toolCatalog.disableTools, ",") {
			if name = strings.TrimSpace(name); name == "" {
				continue
			}
			if err := b.Disable(name); err != nil {
				fmt.Fprintf(os.Stderr, "core-agent: --disable-tools: %v\n", err)
				return runner.ExitConfigError
			}
		}
		for _, name := range s.profileDisables {
			if err := b.Disable(name); err != nil {
				fmt.Fprintf(os.Stderr, "core-agent: task class %q profile: %v\n", s.cfg.Session.TaskClass, err)
				return runner.ExitConfigError
			}
		}
	}
	s.builtinSelection = b
	guardCredentialFiles(s.template, s.opts.attach.TokenFile, tools.BashRegistered(b), os.Stderr)

	loadedSkills, skillsErr := skills.LoadAll(s.ctx, s.agentsDir, s.coreHome, s.gate,
		skills.WithHomeAgentsSkillsDir(s.homeAgDir),
		skills.WithContentRoots(s.contentRoots),
		skills.WithShellTool(tools.BashRegistered(b)),
		skills.WithInterpolator(s.envInterp))
	if skillsErr != nil {
		fmt.Fprintf(os.Stderr, "core-agent: skills: %v\n", skillsErr)
	}
	for _, d := range loadedSkills.Dropped {
		fmt.Fprintf(os.Stderr, "core-agent: skills: %s — NOT loaded\n", d)
	}
	s.loadedSkills = loadedSkills

	if s.envResolver != nil {
		s.envResolver.NoteConfigRefs(s.cfg.EnvRefs())
		for _, w := range s.envResolver.ReportDrift() {
			s.send(w)
		}
	}
	for _, line := range compose.FormatStartupSummary(compose.StartupSummaryInputs{
		CfgPath:             s.opts.workspace.cfgPath,
		Cfg:                 s.cfg,
		AgentsDir:           s.agentsDir,
		AgentsDirOrigin:     s.agentsDirFrom.String(),
		DiscoveredConfigDir: s.discoveredAgentsDir,
		ProviderName:        s.provider.Name(),
		BuiltinTools:        compose.BuiltinToolsSummary(s.provider),
		MCPServers:          s.mcpServers,
		LoadedSkills:        s.loadedSkills,
		Instruction:         &s.loadedInstruction,
	}) {
		s.send(line)
	}
	if vcsLine := compose.FormatVCSStartupLine(s.cfg); vcsLine != "" {
		s.send(vcsLine)
	}
	s.allToolsets = append([]adktool.Toolset{}, excludeSelfRunMCPToolsets(s.cfg, s.mcpToolsets)...)
	if !s.loadedSkills.Empty() {
		s.allToolsets = append(s.allToolsets, s.loadedSkills.Toolset)
	}
	s.skillsLoaded = true
	return runner.ExitOK
}

// stepBuildToolCatalog constructs the built-in tool suite, VCS tools (#1490),
// ask_user, and call_peer (#595), then forwards the registered catalogs onto
// the permission template gate.
func stepBuildToolCatalog(s *runState) int {
	if s.cfg == nil || !s.skillsLoaded || s.template == nil || s.gate == nil {
		return runner.ExitConfigError
	}
	if !s.opts.toolCatalog.noBuiltinTools {
		if s.builtinSelection.Alert {
			if _, dead := alert.PartitionTargets(s.cfg); len(dead) > 0 {
				for _, d := range dead {
					fmt.Fprintf(os.Stderr, "core-agent: alerts: target %q is not deliverable (%s); dropped from the alert tool\n", d.Name, d.Reason)
				}
				if !alert.HasLiveTarget(s.cfg) {
					fmt.Fprintf(os.Stderr, "core-agent: alerts: no deliverable targets; the alert tool is NOT registered — the agent has no escalation path\n")
				}
			}
		}
		reg, err := tools.Build(s.cfg, s.gate, s.agentsDir, s.builtinSelection)
		if err != nil {
			fmt.Fprintf(os.Stderr, "core-agent: built-in tools: %v\n", err)
			return runner.ExitConfigError
		}
		s.builtinTools = reg.Tools
		if s.cfg.VCS.Enabled() {
			extraDisables := append(splitList(s.opts.toolCatalog.disableTools), s.profileDisables...)
			vcsTools, vcsErr := compose.BuildVCSTools(
				s.cfg,
				s.gate,
				s.projectRoot,
				extraDisables,
				vcs.WithEventSink(compose.VCSEventSink(func() *eventlog.Handle { return s.eventlogHandle })),
			)
			if vcsErr != nil {
				fmt.Fprintf(os.Stderr, "core-agent: vcs tools: %v\n", vcsErr)
				return runner.ExitConfigError
			}
			s.builtinTools = append(s.builtinTools, vcsTools...)
		}
		s.template.SetNativeSearchTools(s.gate.NativeSearchTools())
		s.template.SetRegisteredTools(s.gate.RegisteredTools())
	}

	askTool, err := resolveAskUserTool(s.opts.driver.ask, os.Stdin, os.Stderr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "core-agent: %v\n", err)
		return runner.ExitConfigError
	}
	if askTool != nil {
		s.builtinTools = append(s.builtinTools, askTool)
	}

	if s.cfg.Tools.CallPeer.Enabled {
		switch {
		case s.opts.attach.Listen == "" && s.opts.attach.UnixSocket == "":
			fmt.Fprintln(os.Stderr, "core-agent: tools.call_peer.enabled requires attach mode (--attach-listen or --attach-unix-socket)")
			return runner.ExitConfigError
		case !s.opts.attach.PeerHub:
			fmt.Fprintln(os.Stderr, "core-agent: tools.call_peer.enabled requires --attach-peer-hub (the peer registry is the only place call_peer will accept a destination from)")
			return runner.ExitConfigError
		}
		cpTool, err := compose.BuildCallPeerTool(s.gate, s.cfg, func() *attach.PeerRegistry { return s.peerReg })
		if err != nil {
			fmt.Fprintf(os.Stderr, "core-agent: call_peer: %v\n", err)
			return runner.ExitConfigError
		}
		s.builtinTools = append(s.builtinTools, cpTool)
	}
	if names, known := s.gate.PlanGatedTools(); known {
		s.template.RegisterPlanGatedTools(names...)
	}
	s.toolCatalogBuilt = true
	return runner.ExitOK
}

// stepSetupPricingAndSubagentTools refreshes/installs the pricing catalog,
// wires background subagent spawning (#626/#637), registers retrieve_raw and
// agentic_* subtask wrappers, and wraps survey built-in tools in pkg/digest (#706).
func stepSetupPricingAndSubagentTools(s *runState) int {
	if s.cfg == nil || s.provider == nil || !s.toolCatalogBuilt {
		return runner.ExitConfigError
	}
	refreshPricing := !s.opts.model.noPricingRefresh && s.coreHome != ""
	if s.cfg.Pricing.Refresh != nil && !*s.cfg.Pricing.Refresh {
		refreshPricing = false
	}
	if refreshPricing {
		outcome, perr := pricing.Refresh(s.ctx, s.coreHome, pricing.RefreshOptions{
			Source: s.cfg.Pricing.Source,
		})
		if perr != nil {
			fmt.Fprintf(os.Stderr, "core-agent: pricing refresh: %v\n", perr)
		} else if !s.opts.prompt.quiet {
			compose.DescribeRefresh(os.Stderr, outcome)
		}
	}
	if catalog, perr := pricing.NewCatalog(pricing.Options{
		CfgOverride: compose.CfgToCatalogOverride(s.cfg.Model.Pricing),
		AgentsDir:   s.agentsDir,
		UserHome:    s.coreHome,
		Declared:    usage.DeclaredRates(s.cfg),
		Profiles:    s.cfg.Providers,
	}); perr != nil {
		fmt.Fprintf(os.Stderr, "core-agent: pricing: %v\n", perr)
	} else {
		usage.SetCatalog(catalog)
	}
	usage.SetDeclaredContextWindows(s.cfg.ProfileContextWindows())
	s.pricingRate = usage.PriceFor(s.cfg.Model.Name, s.cfg)
	s.pricingCatalogInstalled = true

	if code := s.wireBackgroundSubagentTools(); code != runner.ExitOK {
		return code
	}
	if s.digestStore != nil {
		rtTool, err := tools.NewRetrieveRawTool(tools.RetrieveRawOptions{Store: s.digestStore})
		if err != nil {
			fmt.Fprintf(os.Stderr, "core-agent: retrieve_raw: %v\n", err)
			return runner.ExitConfigError
		}
		s.builtinTools = append(s.builtinTools, rtTool)
	}
	if s.opts.toolCatalog.agenticTools {
		agTools, bootLine, err := resolveAgenticTools(s.builtinTools, func() *agent.Agent { return s.agentRef }, s.provider, s.opts.toolCatalog.agenticSmallModel)
		if err != nil {
			fmt.Fprintf(os.Stderr, "core-agent: agentic tools: %v\n", err)
			return runner.ExitConfigError
		}
		if bootLine != "" {
			s.send(bootLine)
		}
		s.builtinTools = append(s.builtinTools, agTools...)
	}
	// Applied AFTER BuildAgenticTools so subtasks capture unwrapped inner tools (#706).
	if s.digestStore != nil {
		s.builtinTools = tools.NewDigester(&tools.DigestOptions{Store: s.digestStore}).Wrap(s.builtinTools)
	}
	return runner.ExitOK
}

func (s *runState) wireBackgroundSubagentTools() int {
	if s.opts.toolCatalog.noBackgroundAgents {
		return runner.ExitOK
	}
	spawnPricingCheck := func(sp background.SpawnPricing) error {
		p := s.ceilingPolicy.Load()
		if p == nil {
			return nil
		}
		warns, err := p.spawnDoor(sp, &s.spawnBudgetWarned)
		for _, w := range warns {
			fmt.Fprintf(os.Stderr, "core-agent: %s\n", w)
		}
		return err
	}
	allowAdhoc := !s.opts.driver.noREPL
	bgSmallModel := models.ResolveSmallModel(s.provider, s.opts.toolCatalog.agenticSmallModel)
	syncWait, swErr := resolveSyncWaitTimeout(s.opts.subagent.syncWait, s.cfg)
	if swErr != nil {
		fmt.Fprintf(os.Stderr, "core-agent: %v\n", swErr)
		return runner.ExitConfigError
	}
	bgMgr, err := background.NewManager(
		background.WithProvider(s.provider, s.cfg.Model.Name),
		background.WithGate(s.gate),
		background.WithCatalog(s.builtinTools),
		background.WithAllowAdhoc(allowAdhoc),
		background.WithSmallModelID(bgSmallModel),
		background.WithSpawnPricingCheck(spawnPricingCheck),
		background.WithSyncWaitTimeout(syncWait),
		background.WithParallelWritePolicy(s.cfg.Safety.ParallelSubagentWrites),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "core-agent: background agents: %v\n", err)
		return runner.ExitConfigError
	}
	s.bgMgr = bgMgr
	s.addCleanup(func() {
		if err := bgMgr.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "core-agent: shutdown: %v\n", err)
		}
	})
	spawnTools := background.NewSpawnTools(bgMgr)
	s.builtinTools = append(s.builtinTools, spawnTools...)
	s.bgRecipe = sessionBackgroundRecipe{
		provider:       s.provider,
		smallModelID:   bgSmallModel,
		spawnPricing:   spawnPricingCheck,
		allowAdhoc:     allowAdhoc,
		syncWait:       syncWait,
		parallelWrites: s.cfg.Safety.ParallelSubagentWrites,
		spawnToolNames: make(map[string]struct{}, len(spawnTools)),
		live:           newSessionManagerSet(),
	}
	s.addCleanup(func() {
		s.bgRecipe.live.closeAll()
	})
	for _, t := range spawnTools {
		s.bgRecipe.spawnToolNames[t.Name()] = struct{}{}
	}
	return runner.ExitOK
}

// stepBuildDeclarativeSubagentsAndPromptLayers resolves system-prompt layers
// (#459), builds declarative subagents, emits residual env warnings (#712),
// registers async subagent templates, and registers the combined MCP metrics
// gauge (#338).
func stepBuildDeclarativeSubagentsAndPromptLayers(s *runState) int {
	if s.cfg == nil || s.provider == nil || !s.pricingCatalogInstalled {
		return runner.ExitConfigError
	}
	appendPrompt := s.cfg.Agent.AppendSystemPrompt
	if s.opts.prompt.appendSystemPrompt != "" {
		appendPrompt = s.opts.prompt.appendSystemPrompt
	}
	if strings.HasPrefix(appendPrompt, "@") {
		raw, err := os.ReadFile(strings.TrimPrefix(appendPrompt, "@")) //nolint:gosec // operator-supplied CLI flag naming a file on the operator's own machine
		if err != nil {
			fmt.Fprintf(os.Stderr, "core-agent: --append-system-prompt: %v\n", err)
			return runner.ExitConfigError
		}
		appendPrompt = string(raw)
	}
	s.appendPrompt = appendPrompt

	replaceFile := s.cfg.Agent.SystemPromptFile
	if s.opts.prompt.systemPromptFile != "" {
		replaceFile = s.opts.prompt.systemPromptFile
	}
	if replaceFile != "" {
		raw, err := os.ReadFile(replaceFile) //nolint:gosec // operator-supplied CLI flag or config path
		if err != nil {
			fmt.Fprintf(os.Stderr, "core-agent: --system-prompt-file: %v\n", err)
			return runner.ExitConfigError
		}
		s.replacePrompt = string(raw)
		fmt.Fprintln(os.Stderr, "core-agent: system prompt replaced from file — the built-in harness contract (compaction framing, tool dispatch rules) is NOT included; tool-use degradation is on you")
	}

	mcpNamed := make([]namedToolset, 0, len(s.mcpServers))
	for _, srv := range s.mcpServers {
		if srv == nil {
			continue
		}
		mcpNamed = append(mcpNamed, namedToolset{name: srv.Name, toolset: srv.Toolset(), infos: srv.ToolInfos})
	}
	declaredSubagents, subagentTemplates, subagentServers, err := buildDeclaredSubagents(s.ctx, s.cfg, s.provider, s.projectRoot, parentSurface{
		builtinTools: s.builtinTools,
		mcpToolsets:  mcpNamed,
		skills:       s.loadedSkills,
	}, subagentDeps{
		gate:           s.gate,
		elicitor:       s.mcpElicitor,
		digestOpts:     s.digestOpts,
		interp:         s.envInterp,
		send:           s.send,
		rootBase:       s.contentRootBase,
		noPromptCache:  s.opts.model.noPromptCache,
		promptCacheTTL: s.opts.model.promptCacheTTL,
	})
	s.subagentServers = subagentServers
	s.addCleanup(func() { mcp.CloseAll(s.subagentServers) })
	if err != nil {
		fmt.Fprintf(os.Stderr, "core-agent: subagents: %v\n", err)
		return runner.ExitConfigError
	}
	s.declaredSubagents = declaredSubagents
	s.subagentTemplates = subagentTemplates

	if s.envResidual != nil {
		if w := s.envResidual.Warning(s.agentsDir); w != "" {
			s.send(w)
		}
	}
	if s.bgMgr != nil && len(subagentTemplates) > 0 {
		if err := s.bgMgr.SetSubagentTemplates(subagentTemplates); err != nil {
			fmt.Fprintf(os.Stderr, "core-agent: subagents: register async templates: %v\n", err)
			return runner.ExitConfigError
		}
		s.bgRecipe.templates = subagentTemplates
	}
	if allServers := append(append([]*mcp.Server{}, s.mcpServers...), subagentServers...); len(allServers) > 0 {
		if _, err := mcp.RegisterMetrics(otel.GetMeterProvider(), allServers); err != nil {
			fmt.Fprintf(os.Stderr, "core-agent: metrics: register mcp observer: %v\n", err)
			return runner.ExitConfigError
		}
	}
	s.subagentsBuilt = true
	return runner.ExitOK
}

// stepAssembleAgentAndEventlog builds the agent.Option slice, attachadapter
// options, cost ceiling + watchdog + checkpoint + hook options, opens the
// session eventlog store, and resolves auto-continue (#539, #558).
func stepAssembleAgentAndEventlog(s *runState) int {
	if s.cfg == nil || s.provider == nil || s.tracker == nil || !s.subagentsBuilt {
		return runner.ExitConfigError
	}
	opts := []agent.Option{
		agent.WithTools(s.builtinTools),
		agent.WithToolsets(s.allToolsets),
		agent.WithUserInstruction(s.loadedInstruction.Instruction),
		agent.WithGate(s.gate),
		agent.WithDescription(s.cfg.Agent.Description),
		agent.WithUsageTracker(s.tracker),
	}
	if len(s.declaredSubagents) > 0 {
		opts = append(opts, agent.WithSubagents(s.declaredSubagents))
	}
	switch {
	case s.cfg.Agent.SessionTitle != nil && !*s.cfg.Agent.SessionTitle:
		opts = append(opts, agent.WithoutSessionTitle())
	default:
		if titleModelID := models.ResolveSmallModel(s.provider, s.opts.toolCatalog.agenticSmallModel); titleModelID != "" {
			if built, err := s.provider.Model(s.ctx, titleModelID); err != nil {
				fmt.Fprintf(os.Stderr, "core-agent: session titles: %v (falling back to the prompt head)\n", err)
			} else {
				s.titleLLM = built
				opts = append(opts, agent.WithTitleModel(s.titleLLM))
			}
		}
	}
	if s.replacePrompt != "" {
		opts = append(opts, agent.WithInstruction(s.replacePrompt))
	}
	if s.appendPrompt != "" {
		opts = append(opts, agent.WithExtraInstruction(s.appendPrompt))
	}
	s.adapterOpts = buildAttachAdapterOpts(s)
	if s.bgMgr != nil {
		opts = append(opts, agent.WithBackgroundManager(s.bgMgr))
	}
	if !s.opts.toolCatalog.noCompact {
		compactor := compose.BuildCompactor(s.cfg.Compaction)
		if dc, ok := compactor.(*agent.DefaultCompactor); ok && !s.opts.prompt.quiet {
			fmt.Fprintln(os.Stderr, compactionBootLine(dc, s.cfg.Model.Name))
		}
		opts = append(opts, agent.WithCompactor(compactor))
	}
	var code int
	opts, code = s.appendGuardrailCheckpointerAndHookOpts(opts)
	if code != runner.ExitOK {
		return code
	}
	return s.openSessionEventlogAndAutoContinue(opts)
}

func (s *runState) appendGuardrailCheckpointerAndHookOpts(opts []agent.Option) ([]agent.Option, int) {
	if s.opts.guardrails.maxTurnCostUSD > 0 {
		s.cfg.Agent.MaxTurnCostUSD = &s.opts.guardrails.maxTurnCostUSD
	}
	s.cfg.Agent.MaxSessionCostUSD = &s.guard.SessionCostUSD
	if s.guard.ApprovalTimeout > 0 {
		s.send(fmt.Sprintf("approval timeout: %s [%s] (unanswered permission prompts are denied after this bound)", s.guard.ApprovalTimeout, s.guard.ApprovalTimeoutSource))
	} else if s.guard.ApprovalTimeoutSource != sourceApprovalTimeoutUnset {
		s.send(fmt.Sprintf("approval timeout: disabled [%s] (permission prompts wait indefinitely)", s.guard.ApprovalTimeoutSource))
	}
	ceiling := agent.CostCeiling{MaxSessionUSD: s.guard.SessionCostUSD}
	if s.cfg.Agent.MaxTurnCostUSD != nil {
		ceiling.MaxTurnUSD = *s.cfg.Agent.MaxTurnCostUSD
	}
	s.pricingPolicy = ceilingPricing{ceiling: ceiling, sessionSource: s.guard.SessionCostSource}
	warns, err := s.pricingPolicy.checkStartup(startupCeilingModels(s.cfg, s.subagentTemplates, s.bgRecipe.smallModelID))
	if err != nil {
		fmt.Fprintf(os.Stderr, "core-agent: %v\n", err)
		return nil, runner.ExitConfigError
	}
	for _, w := range warns {
		s.send(w)
	}
	s.ceilingPolicy.Store(&s.pricingPolicy)
	opts = compose.AppendCostAndWallclockOpts(opts, s.cfg, ceiling, s.guard.SessionCostSource, s.send)
	opts = compose.AppendWatchdogOpts(opts, s.guard, s.send)
	s.emitBashSearchGateBootLine()

	if s.opts.checkpoint.noCheckpointSet && s.opts.checkpoint.noCheckpoint {
		s.send(checkpointDeprecationNotice)
	}
	ckpt, err := resolveCheckpoint(checkpointInputs{
		ModeFlag:            s.opts.checkpoint.mode,
		NoCheckpointFlag:    s.opts.checkpoint.noCheckpoint,
		NoCheckpointFlagSet: s.opts.checkpoint.noCheckpointSet,
		ModeConfig:          s.cfg.Checkpoint.Mode,
	})
	if err != nil {
		log.Printf("core-agent: %v", err)
		return nil, runner.ExitConfigError
	}
	s.ckpt = ckpt
	if ckpt.Mode != config.CheckpointModeOff {
		opts = append(opts, agent.WithCheckpointer(agent.NewDefaultCheckpointer()))
		if ckpt.Mode == config.CheckpointModeOperator {
			opts = append(opts, agent.WithoutMarkTaskDoneTool())
		}
	}
	s.send(checkpointBootLine(ckpt))
	if hookDispatcher := hooks.New(s.cfg.Hooks, "", os.Stderr, s.gate); !hookDispatcher.Empty() {
		opts = append(opts, agent.WithEventHook(hookDispatcher.OnEvent, hookDispatcher.OnTurnEnd))
		s.send(fmt.Sprintf("hooks: %d event(s) wired", len(s.cfg.Hooks)))
	}
	opts = append(opts, agent.WithPostConstruct(s.onPrimaryAgentPostConstruct))
	return opts, runner.ExitOK
}

func (s *runState) emitBashSearchGateBootLine() {
	if !hasToolNamed(s.builtinTools, "bash") {
		return
	}
	active := s.gate.ActiveSearchBinaries()
	gated := strings.Join(active, "/")
	nativeList := s.gate.ActiveNativeSearchTools()
	natives := strings.Join(nativeList, "/") + " tool"
	if len(nativeList) > 1 {
		natives += "s"
	}
	switch {
	case s.gate.BashSearchGate() == config.BashSearchGateAllow:
		s.send("bash search gate: allow (search-shaped bash commands are not gated)")
	case len(active) == 0:
		s.send(fmt.Sprintf("bash search gate: %s but INERT (no native grep/glob tool is registered, so there is nothing to redirect a search-shaped bash command to)", s.gate.BashSearchGate()))
	case s.gate.BashSearchGate() == config.BashSearchGateWarn:
		s.send(fmt.Sprintf("bash search gate: warn (bash %s still run, but the result carries a pointer to the native %s)", gated, natives))
	default:
		s.send(fmt.Sprintf("bash search gate: enforce (bash %s refused; use the native %s. --bash-search-gate=allow to disable)", gated, natives))
	}
}

func (s *runState) onPrimaryAgentPostConstruct(a *agent.Agent) {
	s.agentRef = a
	acquirePrimaryResidency(s.eventlogHandle, a, s.stop)
	s.primaryTracker.SetIdentity(a)
	if s.digestStore != nil && s.eventlogHandle != nil {
		es, err := digest.NewEventlogStore(s.eventlogHandle, a.AppName(), a.UserID(), a.SessionID())
		if err != nil {
			fmt.Fprintf(os.Stderr, "core-agent: mcp digest store: %v (retrieve_raw disabled)\n", err)
			return
		}
		s.digestStore.Set(es)
		s.send(fmt.Sprintf("mcp digest store: bound EventlogStore for session %s (retrieve_raw enabled)", a.SessionID()))
	}
}

func (s *runState) openSessionEventlogAndAutoContinue(opts []agent.Option) int {
	handle, _, _, updatedOpts, err := openCLISessionAndEventlog(
		s.ctx, s.cfg, s.projectRoot, s.opts.session, s.opts.attach, s.opts.driver.prompt, s.opts.model.scriptPath,
		s.tracker, s.cfg.Model.Name, s.pricingRate, opts, s.stop, s.send,
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "core-agent: %v\n", err)
		return runner.ExitConfigError
	}
	s.agentOpts = updatedOpts
	if handle != nil {
		s.addCleanup(func() { _ = handle.Close() })
		s.eventlogHandle = handle
	}

	acRes, acErr := resolveAutoContinue(s.cfg.Agent.AutoContinue, s.cfg.Attach.MultiSession.Enabled, s.opts.driver.noREPL, s.eventlogHandle != nil, os.Stderr)
	if acErr != nil {
		fmt.Fprintf(os.Stderr, "core-agent: %v\n", acErr)
		return runner.ExitConfigError
	}
	s.autoContinueEnabled = acRes.enabled
	s.autoContinueFreshness = acRes.freshness
	s.autoContinueRetry = acRes.retry
	s.autoContinueRetryInterval = acRes.retryInterval
	s.loopHealth = &runner.LoopHealthSet{}

	autoContinueDriverCtx, stopAutoContinueDrivers := context.WithCancel(s.ctx)
	s.autoContinueDriverCtx = autoContinueDriverCtx
	s.addCleanup(func() {
		stopAutoContinueDrivers()
		s.autoContinueWG.Wait()
	})
	s.sessionEventlogResolved = true
	return runner.ExitOK
}

func buildAttachAdapterOpts(s *runState) []attachadapter.Option {
	return []attachadapter.Option{
		attachadapter.WithMemoryProvider(s.snapshotMemorySources),
		attachadapter.WithSkillsProvider(s.snapshotSkillInfos),
		attachadapter.WithSkillToolsProvider(s.snapshotSkillToolInfos),
		attachadapter.WithPricingProvider(s.snapshotPricingInfo),
		attachadapter.WithRefreshPricer(s.refreshPricingSnapshot),
		attachadapter.WithPricingSetter(s.setPricingOverride),
		attachadapter.WithReloader(s.reloadBundleSnapshot),
		attachadapter.WithReplanner(attachadapter.ReplanHandler(s.gate, s.agentsDir, func() tools.PlanOwner {
			if s.agentRef == nil {
				return tools.PlanOwner{}
			}
			return tools.PlanOwner{Agent: s.agentRef.AgentName(), Session: s.agentRef.SessionID()}
		})),
		attachadapter.WithMCPProvider(s.snapshotMCPInfo),
	}
}

func (s *runState) snapshotMemorySources() []attach.MemorySource {
	fresh, _ := instruction.Load(s.projectRoot, s.coreHome, instruction.WithHomeAgentsRoot(s.homeAgDir), instruction.WithContentRoots(s.contentRoots))
	out := make([]attach.MemorySource, 0, len(fresh.Sources)+1)
	out = append(out, attach.MemorySource{Scope: "system-prompt", Path: describePromptLayers(s.cfg.Model.Name, s.replacePrompt != "", s.appendPrompt != "", len(fresh.Sources))})
	for _, src := range fresh.Sources {
		out = append(out, attach.MemorySource{Scope: src.Scope, Path: src.Path, Size: src.Bytes})
	}
	return out
}

func (s *runState) snapshotSkillInfos() []attach.SkillInfo {
	fresh, err := skills.LoadAll(s.ctx, s.agentsDir, s.coreHome, s.gate, skills.WithHomeAgentsSkillsDir(s.homeAgDir), skills.WithContentRoots(s.contentRoots))
	if err != nil {
		return nil
	}
	out := make([]attach.SkillInfo, 0, len(fresh.Infos))
	for _, sk := range fresh.Infos {
		out = append(out, attach.SkillInfo{Name: sk.Name, Description: sk.Description})
	}
	return out
}

func (s *runState) snapshotSkillToolInfos() []attach.ToolInfo {
	fresh, err := skills.LoadAll(s.ctx, s.agentsDir, s.coreHome, s.gate, skills.WithHomeAgentsSkillsDir(s.homeAgDir), skills.WithContentRoots(s.contentRoots))
	if err != nil {
		return nil
	}
	infos := fresh.ToolInfos()
	out := make([]attach.ToolInfo, 0, len(infos))
	for _, t := range infos {
		out = append(out, attach.ToolInfo{Name: t.Name, Description: t.Description})
	}
	return out
}

func (s *runState) snapshotPricingInfo() attach.PricingInfo {
	currentRate, source := usage.PriceForWithSource(s.cfg.Model.Name, s.cfg)
	info := attach.PricingInfo{
		CurrentModel: s.cfg.Model.Name,
		KnownModels:  usage.KnownModelsCount(),
		Source:       source,
	}
	if !currentRate.IsZero() {
		info.Current = &attach.ModelPricing{
			InputUSDPerMTok:        currentRate.InputPerMTok,
			OutputUSDPerMTok:       currentRate.OutputPerMTok,
			CachedUSDPerMTok:       currentRate.CachedInputPerMTok,
			CacheWriteUSDPerMTok:   currentRate.CacheCreationInputPerMTok,
			CacheWrite1hUSDPerMTok: currentRate.CacheCreation1hInputPerMTok,
			UpdatedAt:              currentRate.UpdatedAt,
		}
	}
	return info
}

func (s *runState) refreshPricingSnapshot(ctx context.Context) (attach.PricingRefreshResponse, error) {
	if s.coreHome == "" {
		return attach.PricingRefreshResponse{}, fmt.Errorf("pricing refresh: $HOME unavailable, no user file to write")
	}
	summary, err := compose.RefreshPricing(ctx, s.cfg, s.agentsDir, s.coreHome)
	if err != nil {
		return attach.PricingRefreshResponse{}, err
	}
	return attach.PricingRefreshResponse{
		Updated:     true,
		LastRefresh: time.Now(),
		Detail:      summary,
	}, nil
}

func (s *runState) setPricingOverride(req attach.PricingSetRequest) error {
	if s.coreHome == "" {
		return fmt.Errorf("pricing set: $HOME unavailable, no user file to write")
	}
	_, err := compose.SetPricing(s.cfg, s.agentsDir, s.coreHome, req.Model, req.InputUSDPerMTok, req.OutputUSDPerMTok)
	return err
}

func (s *runState) reloadBundleSnapshot(_ context.Context) attach.ReloadResponse {
	out := attach.ReloadResponse{}
	if _, err := instruction.Load(s.projectRoot, s.coreHome, instruction.WithHomeAgentsRoot(s.homeAgDir), instruction.WithContentRoots(s.contentRoots)); err != nil {
		out.Errors = append(out.Errors, fmt.Sprintf("memory: %v", err))
	} else {
		out.Memory = true
	}
	if _, err := skills.LoadAll(s.ctx, s.agentsDir, s.coreHome, s.gate, skills.WithHomeAgentsSkillsDir(s.homeAgDir), skills.WithContentRoots(s.contentRoots)); err != nil {
		out.Errors = append(out.Errors, fmt.Sprintf("skills: %v", err))
	} else {
		out.Skills = true
	}
	if _, err := mcp.LoadAll(s.agentsDir, s.homeAgDir); err != nil {
		out.Errors = append(out.Errors, fmt.Sprintf("mcp config: %v", err))
	}
	out.MCP = false
	out.Errors = append(out.Errors, "mcp: live server restart requires daemon restart (tracked for v2.3)")
	return out
}

func (s *runState) snapshotMCPInfo() attach.MCPInfo {
	servers := make([]attach.MCPServerInfo, 0, len(s.mcpServers))
	for _, srv := range s.mcpServers {
		tools := make([]attach.MCPToolInfo, 0, len(srv.ToolInfos))
		for _, t := range srv.ToolInfos {
			tools = append(tools, attach.MCPToolInfo{Name: t.Name, Description: t.Description})
		}
		status := "running"
		if srv.Status == mcp.StatusError {
			status = "failed"
		}
		servers = append(servers, attach.MCPServerInfo{
			Name:      srv.Name,
			Status:    status,
			Transport: "",
			Tools:     tools,
		})
	}
	return attach.MCPInfo{Servers: servers}
}

// stepStartAttachAndPeerHub wires the HTTP/Unix attach listener, multi-session
// factory, and remote peer-hub registration (#162, #595).
func stepStartAttachAndPeerHub(s *runState) int {
	if s.cfg == nil || !s.sessionEventlogResolved {
		return runner.ExitConfigError
	}
	if s.opts.attach.Listen != "" || s.opts.attach.UnixSocket != "" {
		if code := s.startAttachServer(); code != runner.ExitOK {
			return code
		}
	}
	if s.opts.attach.RegisterTo != "" {
		stopPeer, regErr := registerWithPeerHub(s.ctx, s.opts.attach, s.attachToken, s.send)
		if regErr != nil {
			fmt.Fprintf(os.Stderr, "core-agent: %v\n", regErr)
			return runner.ExitConfigError
		}
		s.addCleanup(stopPeer)
	}
	return runner.ExitOK
}

func (s *runState) startAttachServer() int {
	var aclStore attach.SessionACLStore
	if s.eventlogHandle != nil && s.eventlogHandle.DB != nil {
		st, err := attach.NewSessionACLStore(s.ctx, s.eventlogHandle.DB)
		if err != nil {
			fmt.Fprintf(os.Stderr, "core-agent: session ACL store: %v\n", err)
			return runner.ExitConfigError
		}
		aclStore = st
	}
	attachReg := attach.NewSessionRegistryWithStore(aclStore)
	s.attachRegistry = attachReg
	s.primaryTracker.SetRegistry(compose.RegistryTrackerProvider(attachReg))
	s.primaryTracker.SetAgentRegistry(func() []*agent.Agent { return compose.RegistryAgents(attachReg) })

	promptBroker := attach.NewPromptBroker()
	s.addCleanup(promptBroker.Close)
	s.adapterOpts = append(s.adapterOpts, attachadapter.WithPromptBroker(promptBroker))
	s.gate.SetPrompter(promptBroker)

	approvalNotifier, err := approvalnotify.New(s.cfg, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "core-agent: %v\n", err)
		return runner.ExitConfigError
	}
	approvalNotifier.Attach(promptBroker, primarySessionID(&s.agentRef))
	if t := approvalNotifier.Target(); t != "" {
		fmt.Fprintf(os.Stderr, "core-agent: unanswered permission prompts will be announced on alert target %q\n", t)
		if a := s.cfg.Permissions.ApprovalNotifyAfter; a != "" {
			fmt.Fprintf(os.Stderr, "core-agent: ...including prompts an attached client leaves unanswered for %s\n", a)
		}
	}

	token := s.attachToken
	if s.opts.attach.TokenEnv != "" && token == "" {
		fmt.Fprintf(os.Stderr, "core-agent: --attach-token=%s is empty in the environment\n", s.opts.attach.TokenEnv)
		return runner.ExitConfigError
	}
	if s.opts.attach.PeerStateFile != "" && !s.opts.attach.PeerHub {
		fmt.Fprintln(os.Stderr, "core-agent: --attach-peer-state-file requires --attach-peer-hub (nothing else writes peer state)")
		return runner.ExitConfigError
	}
	if s.opts.attach.PeerHub {
		if s.opts.attach.PeerStateFile != "" {
			s.peerReg, err = attach.NewPeerRegistryWithState(s.opts.attach.PeerStateFile)
			if err != nil {
				fmt.Fprintf(os.Stderr, "core-agent: peer state: %v\n", err)
				return runner.ExitConfigError
			}
		} else {
			s.peerReg = attach.NewPeerRegistry()
		}
		s.addCleanup(func() { _ = s.peerReg.Close() })
	}
	return s.bindAndServeAttachServer(attachReg, aclStore, approvalNotifier, token)
}

func (s *runState) bindAndServeAttachServer(attachReg *attach.SessionRegistry, aclStore attach.SessionACLStore, approvalNotifier *approvalnotify.Notifier, token string) int {
	cardConfig, err := resolveAgentCardConfig(s.agentsDir, s.opts.agentCard, s.cfg.Agent.Description)
	if err != nil {
		fmt.Fprintf(os.Stderr, "core-agent: agent card: %v\n", err)
		return runner.ExitConfigError
	}
	authn, defaultCaller, authErr := compose.BuildMultiSessionAuthnWithToken(s.cfg.Attach.MultiSession, token)
	if authErr != nil {
		fmt.Fprintf(os.Stderr, "core-agent: multi-session auth: %v\n", authErr)
		return runner.ExitConfigError
	}
	monitorAckHandler, monErr := buildMonitorAckHandler(s.cfg, s.eventlogHandle, daemonToolsets(s.mcpToolsets, s.subagentServers), len(s.mcpServers) > 0)
	if monErr != nil {
		fmt.Fprintf(os.Stderr, "core-agent: %v\n", monErr)
		return runner.ExitConfigError
	}
	sessionFactory, sessionResumer, a2aOptions := s.wireMultiSessionFactory(attachReg, aclStore, approvalNotifier, token)
	uiAssets, code := resolveAttachUIAssets(s.opts.attach)
	if code != runner.ExitOK {
		return code
	}
	sessionIdleTimeout, costLimit, attachShutdownTimeout, tuneErr := resolveAttachServerTuning(s.cfg)
	if tuneErr != nil {
		fmt.Fprintf(os.Stderr, "core-agent: %v\n", tuneErr)
		return runner.ExitConfigError
	}
	attachOptions := attach.Options{
		Registry:        attachReg,
		PeerRegistry:    s.peerReg,
		DaemonCtx:       s.ctx,
		Addr:            s.opts.attach.Listen,
		AllowedHosts:    s.cfg.Attach.AllowedHosts,
		UnixSocket:      s.opts.attach.UnixSocket,
		CostRateLimit:   costLimit,
		ShutdownTimeout: attachShutdownTimeout,
		Auth: attach.AuthConfig{
			TLSCertFile:  s.opts.attach.TLSCert,
			TLSKeyFile:   s.opts.attach.TLSKey,
			ClientCAFile: s.opts.attach.ClientCA,
			BearerToken:  token,
			ReadOnly:     s.opts.attach.ReadOnly,
		},
		AgentCard:           cardConfig,
		A2A:                 a2aOptions,
		Authenticator:       authn,
		DefaultCaller:       defaultCaller,
		MultiSessionEnabled: s.cfg.Attach.MultiSession.Enabled,
		AllowAnonymous:      s.cfg.Attach.MultiSession.AllowAnonymous,
		AllowNonAdminYolo:   s.cfg.Attach.MultiSession.AllowNonAdminYolo,
		AllowNonAdminBash:   s.cfg.Attach.MultiSession.AllowNonAdminBash,
		ProxyHeader:         s.cfg.Attach.MultiSession.AssertedCallerHeader,
		UI:                  uiAssets,
		SessionFactory:      sessionFactory,
		Resumer:             sessionResumer,
		SessionIdleTimeout:  sessionIdleTimeout,
		HealthChecks:        daemonHealthChecks(s.eventlogHandle, s.loopHealth),
		MonitorAck:          monitorAckHandler,
	}
	attachSrv, err := attach.NewServer(attachOptions)
	if err != nil {
		fmt.Fprintf(os.Stderr, "core-agent: attach server: %v\n", err)
		return runner.ExitConfigError
	}
	if err := checkLocalListenerAuth(attachOptions, hasToolNamed(s.builtinTools, "bash"), s.opts.attach.AllowUnauthenticatedLocal, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "core-agent: %v\n", err)
		return runner.ExitConfigError
	}
	if err := attachSrv.Bind(); err != nil {
		fmt.Fprintf(os.Stderr, "core-agent: attach listener: %v\n", err)
		return runner.ExitConfigError
	}
	if _, err := attach.RegisterMetrics(otel.GetMeterProvider(), attachSrv); err != nil {
		fmt.Fprintf(os.Stderr, "core-agent: metrics: register attach observer: %v\n", err)
		return runner.ExitConfigError
	}
	endpoint := s.opts.attach.Listen
	if endpoint == "" {
		endpoint = "unix://" + s.opts.attach.UnixSocket
	}
	extras := ""
	if s.peerReg != nil {
		extras = " (peer-hub enabled)"
	}
	fmt.Fprintf(os.Stderr, "core-agent: attach listener on %s%s\n", endpoint, extras)
	go func() {
		if err := attachSrv.Serve(); err != nil {
			fmt.Fprintf(os.Stderr, "core-agent: attach server: %v\n", err)
		}
	}()
	s.addCleanup(func() { _ = attachSrv.Close() })
	if !s.opts.driver.noREPL {
		startMultiSessionAutoContinue(s.autoContinueDriverCtx, &s.autoContinueWG, s.cfg, s.multiSessionFactoryDeps, s.autoContinueEnabled, s.autoContinueRetry, s.autoContinueRetryInterval)
	}
	return runner.ExitOK
}

func (s *runState) wireMultiSessionFactory(attachReg *attach.SessionRegistry, aclStore attach.SessionACLStore, approvalNotifier *approvalnotify.Notifier, token string) (attach.SessionFactory, attach.SessionResumer, attach.A2AOptions) {
	if !servesSessionFactory(s.cfg, token) {
		return nil, nil, attach.A2AOptions{}
	}
	wakeLoops := &compose.WakeLoopGroup{}
	wakeCtx, cancelWakeLoops := context.WithCancel(s.ctx)
	s.addCleanup(func() {
		cancelWakeLoops()
		if !wakeLoops.WaitFor(wakeLoopDrainTimeout) {
			fmt.Fprintf(os.Stderr, "core-agent: shutdown: session wake loops still running after %s; closing the session db anyway\n", wakeLoopDrainTimeout)
		}
	})
	isReadOnly, subAgentNames := buildDaemonEffectControls(s.cfg, daemonToolsets(s.mcpToolsets, s.subagentServers)...)
	factoryDeps := compose.SessionFactoryDeps{
		DaemonCtx:              wakeCtx,
		WakeLoops:              wakeLoops,
		LoopHealth:             s.loopHealth,
		Model:                  s.model,
		TitleModel:             s.titleLLM,
		Template:               s.template,
		PricingRate:            s.pricingRate,
		AgentsDir:              s.agentsDir,
		Cfg:                    s.cfg,
		MCPServers:             s.mcpServers,
		BuiltinTools:           s.builtinTools,
		Toolsets:               s.allToolsets,
		Subagents:              s.declaredSubagents,
		EventlogHandle:         s.eventlogHandle,
		ProjectRoot:            s.projectRoot,
		UserRoot:               s.coreHome,
		HomeAgentsDir:          s.homeAgDir,
		ContentRoots:           s.contentRoots,
		UsersDir:               s.cfg.Attach.MultiSession.UsersDir,
		EnvInterp:              s.envInterp,
		Registry:               attachReg,
		ACLStore:               aclStore,
		NoCompact:              s.opts.toolCatalog.noCompact,
		CheckpointMode:         s.ckpt.Mode,
		WatchdogMode:           s.guard.Watchdog,
		AutoContinueEnabled:    s.autoContinueEnabled,
		AutoContinueFreshness:  s.autoContinueFreshness,
		IsReadOnly:             isReadOnly,
		SubAgentNames:          subAgentNames,
		SessionBackground:      s.bgRecipe.factory(),
		AttachApprovalNotifier: approvalNotifier.AttachSession,
	}
	a2aOptions := buildDaemonA2AOptions(wakeCtx, s.cfg, attachReg, &factoryDeps)
	s.multiSessionFactoryDeps = &factoryDeps
	var sessionFactory attach.SessionFactory
	var sessionResumer attach.SessionResumer
	if servesSessionCreation(s.cfg, token) {
		sessionFactory = compose.BuildSessionFactory(factoryDeps)
		sessionResumer = compose.BuildSessionResumer(factoryDeps)
	}
	return sessionFactory, sessionResumer, a2aOptions
}

func resolveAttachUIAssets(attachCfg attachOpts) (fs.FS, int) {
	if attachCfg.UIDir != "" {
		fmt.Fprintf(os.Stderr, "core-agent: --ui-dir: serving %s at /ui/\n", attachCfg.UIDir)
		return os.DirFS(attachCfg.UIDir), runner.ExitOK
	}
	if attachCfg.UI {
		if !webui.HasAssets() {
			fmt.Fprintln(os.Stderr, "core-agent: --ui requested but the embedded mast-web bundle is empty.")
			fmt.Fprintln(os.Stderr, "  Run `dev/tools/fetch-mast-web` before `go build` to populate internal/webui/dist/,")
			fmt.Fprintln(os.Stderr, "  or pass --ui-dir <path> to serve from a local mast-web checkout instead.")
			return nil, runner.ExitConfigError
		}
		f, ferr := webui.FS()
		if ferr != nil {
			fmt.Fprintf(os.Stderr, "core-agent: --ui: %v\n", ferr)
			return nil, runner.ExitConfigError
		}
		fmt.Fprintln(os.Stderr, "core-agent: --ui: serving embedded mast-web bundle at /ui/")
		return f, runner.ExitOK
	}
	return nil, runner.ExitOK
}

// executeDriver runs the selected terminal/headless/daemon driver after all
// startupSteps have completed (#685).
func (s *runState) executeDriver() int {
	colorOn, err := resolveColor(s.opts.driver.color, os.Stdout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "core-agent: %v\n", err)
		return runner.ExitConfigError
	}
	eventsOpts := []runner.EventsOption{
		runner.WithColor(colorOn),
		runner.WithOutputFormat(s.outFormat),
	}

	if s.opts.driver.prompt != "" {
		code, err := runner.Headless(s.ctx, s.model, s.opts.driver.prompt, os.Stdout, os.Stderr, s.tracker, s.pricingRate, s.agentOpts, eventsOpts...)
		if err != nil {
			fmt.Fprintf(os.Stderr, "core-agent: %v\n", err)
		}
		if code == runner.ExitOK {
			if !s.opts.prompt.quiet {
				runner.WriteSummary(os.Stderr, s.tracker, s.model.Name())
			}
			persistTranscript(s.agentsDir, s.model.Name(), s.opts.driver.prompt, s.tracker)
		}
		return code
	}

	if s.opts.driver.noREPL {
		return s.runNoREPLDaemon()
	}

	if !s.opts.driver.noTUI && term.IsTerminal(int(os.Stdin.Fd())) {
		didRun, code, err := launchTUIv2(s.ctx, tuiDeps{
			Cfg:         s.cfg,
			Model:       s.model,
			AgentOpts:   s.agentOpts,
			AdapterOpts: s.adapterOpts,
			AttachReg:   s.attachRegistry,
			Provider:    s.provider,
			CheckModelPricing: func(modelID, backend string) error {
				return s.pricingPolicy.door("model", modelID, backend)
			},
			Gate:          s.gate,
			Tracker:       s.tracker,
			Memory:        s.loadedInstruction,
			MCPServers:    s.mcpServers,
			LoadedSkills:  s.loadedSkills,
			AgentsDir:     s.agentsDir,
			CoreHome:      s.coreHome,
			HomeAgentsDir: s.homeAgDir,
			ProjectRoot:   s.projectRoot,
			ContentRoots:  s.contentRoots,
			EnvInterp:     s.envInterp,
			InitialPrompt: s.opts.driver.initialPrompt,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "core-agent: %v\n", err)
			if !didRun {
				return runner.ExitAgentError
			}
		}
		if didRun {
			if code == runner.ExitOK && !s.opts.prompt.quiet {
				runner.WriteSummary(os.Stderr, s.tracker, s.model.Name())
			}
			return code
		}
	}

	replAgent, _, err := buildAttachedAgent(s.model, s.agentOpts, s.adapterOpts, s.attachRegistry)
	if err != nil {
		fmt.Fprintf(os.Stderr, "core-agent: %v\n", err)
		return runner.ExitAgentError
	}
	code, err := runner.Run(s.ctx, runner.RunOptions{
		Agent:         replAgent,
		InitialPrompt: s.opts.driver.initialPrompt,
		Tracker:       s.tracker,
		Pricing:       s.pricingRate,
		EventsOptions: eventsOpts,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "core-agent: %v\n", err)
	}
	if code == runner.ExitOK && !s.opts.prompt.quiet {
		runner.WriteSummary(os.Stderr, s.tracker, s.model.Name())
	}
	return code
}

func (s *runState) runNoREPLDaemon() int {
	if s.opts.attach.Listen == "" && s.opts.attach.UnixSocket == "" {
		fmt.Fprintln(os.Stderr, "core-agent: --no-repl requires --attach-listen or --attach-unix-socket")
		return runner.ExitConfigError
	}
	a, _, err := buildAttachedAgent(s.model, s.agentOpts, s.adapterOpts, s.attachRegistry)
	if err != nil {
		fmt.Fprintf(os.Stderr, "core-agent: %v\n", err)
		return runner.ExitAgentError
	}
	s.send(fmt.Sprintf("--no-repl: attach-only mode, session %s (Ctrl-C or SIGTERM to exit)", a.SessionID()))
	schedHandle, schedErr := startSingleSessionScheduleAndAutoContinue(
		s.ctx, s.autoContinueDriverCtx, &s.autoContinueWG, s.cfg, s.eventlogHandle, a,
		daemonToolsets(s.mcpToolsets, s.subagentServers), s.multiSessionFactoryDeps,
		s.autoContinueEnabled, s.autoContinueRetry,
		s.autoContinueFreshness, s.autoContinueRetryInterval,
	)
	if schedErr != nil {
		fmt.Fprintf(os.Stderr, "core-agent: %v\n", schedErr)
		return runner.ExitConfigError
	}
	defer schedHandle.Stop()

	singleLoopHealth, unregisterLoopHealth := s.loopHealth.Register(a.SessionID())
	defer unregisterLoopHealth()
	runner.WakeLoop(s.ctx, a, runner.WakeLoopOptions{
		Tracker:    s.tracker,
		Model:      s.model.Name(),
		Pricing:    s.pricingRate,
		Health:     singleLoopHealth,
		OnTurnDone: schedHandle.AfterTurn,
		OnTurnError: func(err error) {
			fmt.Fprintf(os.Stderr, "core-agent: turn: %v\n", err)
		},
		Debugf: func(format string, args ...any) {
			debugf("--no-repl: "+format, args...)
		},
	})
	return runner.ExitOK
}
