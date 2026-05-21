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

// UAT driver for the scheduled-monitoring feature
// (docs/scheduled-monitoring-design.md). Drives a real LLM against a
// real K8s cluster so the operator can walk the UAT scenarios
// (U1–U10) before sign-off.
//
// What this binary wires that the bundled CLI doesn't:
//
//   - BackgroundAgentManager with WithBackgroundDefaultScheduler(SleepScheduler()).
//   - The parent agent's system prompt composes agent.DefaultInstruction +
//     agent.DefaultSchedulingInstruction + a GKE-supervisor brief.
//   - The full spawn-tool family (spawn_agent / list_agents /
//     check_agent / stop_agent) so the parent's model can manage the
//     child monitors at runtime.
//   - bash with a read-only kubectl allowlist by default so the
//     children can poll cluster state.
//   - --session-db enabled by default (required for U5/U6 resume).
//
// Usage (from the repo root):
//
//	# Fresh run against Vertex+Claude, 2-hour budget, default goal:
//	go run ./dev/uat/scheduled-monitor \
//	    --provider=anthropic-vertex \
//	    --max-wallclock=2h
//
//	# Override the goal:
//	go run ./dev/uat/scheduled-monitor \
//	    --goal="Watch namespace ingress-system. Alert on pod restarts."
//
//	# Resume after kill -9 (U5/U6):
//	go run ./dev/uat/scheduled-monitor --resume --session-id=run-2026-05-20
//
// Env required (depending on provider): GOOGLE_GENAI_USE_VERTEXAI=true
// + GOOGLE_CLOUD_PROJECT + GOOGLE_CLOUD_LOCATION for Vertex/Gemini;
// ANTHROPIC_VERTEX_PROJECT_ID + CLOUD_ML_REGION for anthropic-vertex;
// ANTHROPIC_API_KEY for anthropic; GEMINI_API_KEY/GOOGLE_API_KEY for
// gemini API. KUBECONFIG (or default ~/.kube/config) must point at the
// cluster the bash tool will run kubectl against.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/glebarez/sqlite"
	adktool "google.golang.org/adk/tool"

	"github.com/go-steer/core-agent/agent"
	"github.com/go-steer/core-agent/config"
	"github.com/go-steer/core-agent/eventlog"
	"github.com/go-steer/core-agent/models"
	_ "github.com/go-steer/core-agent/models/anthropic"
	_ "github.com/go-steer/core-agent/models/gemini"
	"github.com/go-steer/core-agent/permissions"
	coretools "github.com/go-steer/core-agent/tools"
)

const defaultGoal = `Watch this Kubernetes project. Your job is to spawn one background monitor per target the operator specifies (cluster names or namespaces — discover them via kubectl get if not told explicitly), and have each monitor scan periodically for anything odd: error-rate spikes in logs, new deployments, deployments that disappeared, pods in CrashLoopBackOff, image-pull failures.

Each monitor should:
- Use the bash tool with kubectl to inspect state on every wake.
- Write its prior-scan baseline to a small file in /tmp so it can diff on the next wake (state does NOT survive schedule_next_turn — write what you need to remember to disk).
- Call schedule_next_turn with wake_in_sec set per the cadence ladder in your system prompt (default to 600 seconds = 10 minutes for cluster scans, 30 seconds for hot anomaly investigation).
- Call report_alert when it finds something genuinely odd. Avoid noise: a single error spike isn't worth alerting; a sustained pattern is.

You (the supervisor) only spawn and supervise. Use list_agents to see what's running, check_agent to inspect a specific monitor's status, and stop_agent if a target is decommissioned.

Once your monitors are launched and reporting in a steady state, call report_done with state="done" and a one-sentence summary of what you set up.`

const supervisorBrief = `You are the supervisor of a fleet of Kubernetes cluster/namespace monitors. The user gave you a Kubernetes monitoring goal; your job is to translate that into spawn_agent calls that launch one focused background subagent per monitoring target, then stand by.

When children alert you via the [Background reports] inbox prepended to your turns, decide:
- Investigate further: spawn a one-shot triage subagent with scheduler="none" to dig into a specific incident.
- Adjust cadence: stop a monitor and respawn it with a different prompt if its cadence is wrong for what you're seeing.
- Decommission: stop_agent when a target is no longer in scope.

Do not poll yourself. Do not call schedule_next_turn at the supervisor level — that's for the children. You're reactive: you act when children alert.`

func main() {
	providerFlag := flag.String("provider", "anthropic-vertex", "model provider: anthropic-vertex | vertex | anthropic | gemini")
	modelFlag := flag.String("model", "", "model name (default chosen per provider — claude-opus-4-7 for anthropic-vertex)")
	goalFlag := flag.String("goal", defaultGoal, "the operator's prompt — what the supervisor should accomplish")
	maxWallclock := flag.Duration("max-wallclock", 2*time.Hour, "hard cap on the supervisor's total wallclock")
	maxTurns := flag.Int("max-turns", 200, "hard cap on the supervisor's turn count")
	maxDefer := flag.Duration("max-defer", 1*time.Hour, "driver-level ceiling on child schedule_next_turn delays")
	sessionDB := flag.String("session-db", defaultSessionDBPath(), "SQLite path for the durable event log (required for resume)")
	sessionID := flag.String("session-id", fmt.Sprintf("uat-%s", time.Now().UTC().Format("2006-01-02-150405")), "session ID — set explicitly to enable --resume against the same session")
	resume := flag.Bool("resume", false, "resume from a deferred or interrupted run on this session-id instead of starting fresh")
	kubectlAllowAll := flag.Bool("kubectl-allow-all", false, "DANGER: allow bash to run any kubectl command (default: only get/logs/describe/version/cluster-info)")
	maxConcurrent := flag.Int("max-concurrent", 8, "max concurrent background subagents")
	flag.Parse()

	if err := run(*providerFlag, *modelFlag, *goalFlag, *maxWallclock, *maxTurns, *maxDefer,
		*sessionDB, *sessionID, *resume, *kubectlAllowAll, *maxConcurrent); err != nil {
		log.Fatal(err)
	}
}

func run(providerName, modelName, goal string, maxWC time.Duration, maxT int, maxDef time.Duration,
	dbPath, sessID string, resumeFlag, kubectlAllowAll bool, maxConc int,
) error {
	// Graceful shutdown on SIGINT/SIGTERM so background goroutines
	// drain cleanly and the final checkpoint lands.
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// === Provider + model ===
	cfg := config.DefaultConfig()
	cfg.Model.Provider = providerName
	if modelName == "" {
		switch providerName {
		case "anthropic-vertex", "anthropic":
			modelName = "claude-opus-4-7"
		}
	}
	if modelName != "" {
		cfg.Model.Name = modelName
	}
	cfg.Permissions.Mode = config.PermissionModeAllow
	provider, err := models.Resolve(cfg)
	if err != nil {
		return fmt.Errorf("resolve provider: %w", err)
	}
	parentModel, err := provider.Model(ctx, cfg.Model.Name)
	if err != nil {
		return fmt.Errorf("build model: %w", err)
	}

	// === Permission gate ===
	// Default: read-only kubectl. Use --kubectl-allow-all only if you
	// trust the model to mutate state in your test cluster.
	// Pattern grammar (see permissions/policy.go): "<tool>:<glob>".
	bashAllow := []string{
		"bash:kubectl get *",
		"bash:kubectl logs *",
		"bash:kubectl describe *",
		"bash:kubectl version *",
		"bash:kubectl cluster-info*",
		"bash:echo *",
		"bash:cat *",
		"bash:date",
	}
	if kubectlAllowAll {
		bashAllow = append(bashAllow, "bash:kubectl *")
	}
	cfg.Permissions.Allow = append(cfg.Permissions.Allow, bashAllow...)
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	gate, err := permissions.FromConfig(cfg, cwd, "", nil)
	if err != nil {
		return fmt.Errorf("permissions: %w", err)
	}

	// === Built-in tools ===
	reg, err := coretools.Build(cfg, gate, coretools.Default())
	if err != nil {
		return fmt.Errorf("tools.Build: %w", err)
	}

	// === Eventlog (required for U5/U6 resume) ===
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return fmt.Errorf("mkdir session-db dir: %w", err)
	}
	handle, err := eventlog.Open(ctx, sqlite.Open(dbPath))
	if err != nil {
		return fmt.Errorf("eventlog.Open: %w", err)
	}
	defer func() { _ = handle.Close() }()

	const (
		appName = "scheduled-monitor-uat"
		userID  = "uat"
	)

	// === BackgroundAgentManager with the new default scheduler ===
	mgr, err := agent.NewBackgroundAgentManager(
		agent.WithBackgroundProvider(provider, cfg.Model.Name),
		agent.WithBackgroundGate(gate),
		agent.WithBackgroundCatalog(reg.Tools),
		agent.WithBackgroundMaxConcurrent(maxConc),
		agent.WithBackgroundDefaultBudgets(agent.BackgroundBudgets{
			MaxTurns:     500,
			MaxWallclock: 24 * time.Hour,
		}),
		agent.WithBackgroundDefaultScheduler(coretools.SleepScheduler()),
	)
	if err != nil {
		return fmt.Errorf("BackgroundAgentManager: %w", err)
	}
	defer func() { _ = mgr.Close() }()

	// Inline alert display so the operator sees children reporting
	// in real time even without attach mode.
	mgr.OnAlert(func(a agent.Alert) {
		fmt.Printf("[alert] %s %s: %s\n", a.From, a.Kind, a.Text)
	})

	// === Parent agent build function ===
	// Composes the three steering layers per the design doc.
	instruction := agent.DefaultInstruction + "\n\n" +
		agent.DefaultSchedulingInstruction + "\n\n" +
		supervisorBrief

	spawnTools := agent.NewBackgroundSpawnTools(mgr)

	build := func(extras []adktool.Tool) (*agent.Agent, error) {
		all := make([]adktool.Tool, 0, len(reg.Tools)+len(spawnTools)+len(extras))
		all = append(all, reg.Tools...)
		all = append(all, spawnTools...)
		all = append(all, extras...)
		return agent.New(parentModel,
			agent.WithAppName(appName),
			agent.WithName("scheduled-monitor-supervisor"),
			agent.WithSession(userID, sessID),
			agent.WithInstruction(instruction),
			agent.WithTools(all),
			agent.WithEventLog(handle),
			agent.WithBackgroundManager(mgr),
		)
	}
	resumeBuild := func(extras []adktool.Tool, sid string) (*agent.Agent, error) {
		all := make([]adktool.Tool, 0, len(reg.Tools)+len(spawnTools)+len(extras))
		all = append(all, reg.Tools...)
		all = append(all, spawnTools...)
		all = append(all, extras...)
		return agent.New(parentModel,
			agent.WithAppName(appName),
			agent.WithName("scheduled-monitor-supervisor"),
			agent.WithSession(userID, sid),
			agent.WithInstruction(instruction),
			agent.WithTools(all),
			agent.WithEventLog(handle),
			agent.WithBackgroundManager(mgr),
		)
	}

	// === Banner + go ===
	fmt.Printf("== scheduled-monitor UAT driver ==\n")
	fmt.Printf("provider:       %s\n", providerName)
	fmt.Printf("model:          %s\n", cfg.Model.Name)
	fmt.Printf("session-db:     %s\n", dbPath)
	fmt.Printf("session-id:     %s\n", sessID)
	fmt.Printf("max-wallclock:  %s\n", maxWC)
	fmt.Printf("max-turns:      %d\n", maxT)
	fmt.Printf("max-defer:      %s\n", maxDef)
	fmt.Printf("max-concurrent: %d\n", maxConc)
	fmt.Printf("bash allowlist: %s\n", strings.Join(bashAllow, ", "))
	fmt.Printf("mode:           %s\n", modeLabel(resumeFlag))
	fmt.Println()

	go logHeartbeat(ctx, mgr)

	opts := []agent.AutonomousOption{
		agent.WithMaxWallclock(maxWC),
		agent.WithMaxTurns(maxT),
		agent.WithMaxDefer(maxDef),
		agent.WithScheduler(coretools.SleepScheduler()),
		agent.WithPermissionsGate(gate),
	}

	var (
		result agent.RunResult
		runErr error
	)
	if resumeFlag {
		result, runErr = agent.ResumeAutonomous(ctx, resumeBuild,
			agent.SessionRef{
				Handle: handle, AppName: appName, UserID: userID, SessionID: sessID,
			}, opts...)
	} else {
		result, runErr = agent.RunAutonomous(ctx, build, goal, opts...)
	}

	fmt.Println()
	fmt.Println("== supervisor exit ==")
	fmt.Printf("reason:        %s\n", result.Reason)
	fmt.Printf("turns:         %d\n", result.Turns)
	fmt.Printf("duration:      %s\n", result.Duration.Round(time.Second))
	fmt.Printf("input tokens:  %d\n", result.InputTokens)
	fmt.Printf("output tokens: %d\n", result.OutputTokens)
	fmt.Printf("cost (USD):    %.4f\n", result.CostUSD)
	if !result.NextWakeAt.IsZero() {
		fmt.Printf("next-wake-at:  %s (re-run with --resume --session-id=%s after this time)\n",
			result.NextWakeAt.Format(time.RFC3339), sessID)
	}
	if result.DoneDetail != "" {
		fmt.Printf("done detail:   %s\n", result.DoneDetail)
	}
	if result.FinalText != "" {
		fmt.Println()
		fmt.Println("== final text ==")
		fmt.Println(result.FinalText)
	}
	fmt.Println()
	fmt.Printf("background subagents (terminal status):\n")
	for _, h := range mgr.List() {
		fmt.Printf("  %s -> %s\n", h.Name, h.Status())
	}
	return runErr
}

// logHeartbeat prints a periodic snapshot of running children +
// goroutine count, so the operator can spot leaks during a long run
// without running pprof.
func logHeartbeat(ctx context.Context, mgr *agent.BackgroundAgentManager) {
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			running := 0
			for _, h := range mgr.List() {
				if h.Status() == agent.StatusRunning {
					running++
				}
			}
			fmt.Printf("[heartbeat] running_children=%d goroutines=%d\n",
				running, runtime.NumGoroutine())
		}
	}
}

func defaultSessionDBPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "scheduled-monitor-uat.db"
	}
	return filepath.Join(home, ".scheduled-monitor-uat", "sessions.db")
}

func modeLabel(resumeFlag bool) string {
	if resumeFlag {
		return "RESUME"
	}
	return "FRESH"
}
