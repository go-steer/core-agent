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
// (docs/scheduled-monitoring-design.md). Stands up the full
// supervisor topology — BackgroundAgentManager +
// WithBackgroundDefaultScheduler(SleepScheduler()) + the spawn-tool
// family + DefaultSchedulingInstruction + bash gated to read-only
// kubectl — so the operator can walk the design doc's UAT scenarios
// against a real K8s cluster + real LLM.
//
// In-memory only. No persistence; chat output is the live observation
// surface (same renderer the bundled CLI uses). Crash-resume (U5/U6)
// is intentionally deferred — add eventlog wiring back when needed.
//
// Usage:
//
//	go run ./dev/uat/scheduled-monitor --provider=vertex
//	go run ./dev/uat/scheduled-monitor --provider=vertex --goal="<custom prompt>"
//
// Env: see the bundled CLI's provider docs — Vertex+Gemini needs
// GOOGLE_GENAI_USE_VERTEXAI=true + GOOGLE_CLOUD_PROJECT +
// GOOGLE_CLOUD_LOCATION + ADC; Anthropic/Vertex needs
// ANTHROPIC_VERTEX_PROJECT_ID + CLOUD_ML_REGION + ADC. KUBECONFIG (or
// ~/.kube/config) must point at the cluster the bash tool will run
// kubectl against.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/glebarez/sqlite"
	"google.golang.org/adk/session"
	adktool "google.golang.org/adk/tool"

	"github.com/go-steer/core-agent/agent"
	"github.com/go-steer/core-agent/config"
	"github.com/go-steer/core-agent/eventlog"
	"github.com/go-steer/core-agent/models"
	_ "github.com/go-steer/core-agent/models/anthropic"
	_ "github.com/go-steer/core-agent/models/gemini"
	"github.com/go-steer/core-agent/permissions"
	"github.com/go-steer/core-agent/runner"
	coretools "github.com/go-steer/core-agent/tools"
)

const defaultGoal = `Watch this Kubernetes project. Spawn one background monitor per target the operator specifies (cluster names or namespaces — discover them via kubectl get if not told explicitly), and have each monitor scan periodically for anything odd: error-rate spikes in logs, new deployments, deployments that disappeared, pods in CrashLoopBackOff, image-pull failures.

Each monitor should:
- Use the bash tool with kubectl to inspect state on every wake.
- Write its prior-scan baseline to an ABSOLUTE PATH under /tmp/ (e.g. /tmp/scheduled-monitor-uat/<monitor-name>-baseline.json — NOT just "baseline.json" in cwd) so it can diff on the next wake. State does NOT survive schedule_next_turn — write what you need to remember to disk. NEVER write baselines into the working directory; cwd is the operator's repo and we don't want to pollute it.
- Call schedule_next_turn with wake_in_sec set per the cadence ladder in your system prompt (default to 600 seconds = 10 minutes for cluster scans, 30 seconds for hot anomaly investigation).
- Call report_alert when it finds something genuinely odd. Avoid noise.

After spawning your monitors, stay alive by calling schedule_next_turn yourself (wake_in_sec=3600 — one hour is a fine default for supervisor check-ins; child alerts will arrive in your inbox on your next wake regardless of when you scheduled it). On each supervisor wake, drain any alerts that arrived, call list_agents to confirm everyone's still running, and either react (spawn triage, adjust cadence, stop decommissioned monitors) or schedule the next check-in. Do NOT call report_done — that exits the run permanently and kills all your children. The operator's wallclock budget (or Ctrl+C) ends the run when the test session is over.`

const supervisorBrief = `You are the supervisor of a fleet of Kubernetes cluster/namespace monitors. The user gave you a Kubernetes monitoring goal; your job is to:

1. On your first turn, translate the goal into spawn_agent calls that launch one focused background subagent per monitoring target.
2. After spawning, call schedule_next_turn(wake_in_sec=3600) so you stay alive without burning tokens. You will wake earlier if either (a) a child alert arrives or (b) the operator types a command into the inbox — both signals interrupt your scheduled sleep automatically. So 1-hour is fine as a baseline.
3. On each wake (scheduled OR signalled):
   - Drain alerts: child anomalies arrive in the [Background reports] block prepended to your turn. Operator commands arrive in the [Inbox] block. Read both blocks before deciding what to do.
   - Decide: spawn a one-shot triage subagent (use scheduler="none" for triage — it shouldn't outlive its investigation), adjust a misconfigured monitor's cadence by stopping + respawning, stop_agent a decommissioned target, or just acknowledge in your reply if there's nothing to act on.
   - Sanity check: call list_agents to confirm all expected monitors are still running.
   - Schedule the next check-in: schedule_next_turn(wake_in_sec=3600).

DO NOT call report_done at any point. report_done exits the autonomous loop and kills all your background children (their HTTP requests get context-cancelled mid-flight). The operator's wallclock budget (or Ctrl+C) is what ends the run; you just supervise until then.

DO NOT poll children yourself between scheduled wakes — that's what schedule_next_turn is for. You're reactive at supervisor wakes, not pollee.`

func main() {
	provider := flag.String("provider", "vertex", "model provider: vertex | anthropic-vertex | anthropic | gemini")
	model := flag.String("model", "", "model name (default chosen per provider — gemini-3.1-pro-preview-customtools for vertex, claude-opus-4-7 for anthropic-vertex)")
	goal := flag.String("goal", defaultGoal, "the operator's prompt — what the supervisor should accomplish")
	maxWallclock := flag.Duration("max-wallclock", 2*time.Hour, "hard cap on the supervisor's total wallclock")
	maxTurns := flag.Int("max-turns", 200, "hard cap on the supervisor's turn count")
	maxDefer := flag.Duration("max-defer", 1*time.Hour, "driver-level ceiling on child schedule_next_turn delays")
	maxConcurrent := flag.Int("max-concurrent", 8, "max concurrent background subagents")
	kubectlAllowAll := flag.Bool("kubectl-allow-all", false, "DANGER: allow bash to run any kubectl command (default: only get/logs/describe/version/cluster-info)")
	sessionDB := flag.String("session-db", "", "SQLite path for the durable event log. Empty (default) = in-memory only — fast and simple. Set to e.g. /tmp/scheduled-monitor-uat/sessions.db to enable --resume across runs (writes are serialized through the eventlog service so concurrent subagents don't race).")
	sessionID := flag.String("session-id", "", "session ID. Empty defaults to a timestamp. Set explicitly when using --session-db so --resume can find the prior run.")
	resumeFlag := flag.Bool("resume", false, "resume from the latest checkpoint on --session-id instead of starting fresh. Requires --session-db.")
	flag.Parse()

	if err := run(*provider, *model, *goal, *maxWallclock, *maxTurns, *maxDefer, *maxConcurrent, *kubectlAllowAll,
		*sessionDB, *sessionID, *resumeFlag); err != nil {
		log.Fatal(err)
	}
}

func run(providerName, modelName, goal string, maxWC time.Duration, maxT int, maxDef time.Duration, maxConc int, kubectlAllowAll bool,
	dbPath, sessID string, resumeFlag bool) error {
	if resumeFlag && dbPath == "" {
		return errors.New("--resume requires --session-db")
	}
	if sessID == "" {
		sessID = "uat-" + time.Now().UTC().Format("2006-01-02-150405")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// === Provider + model ===
	cfg := config.DefaultConfig()
	cfg.Model.Provider = providerName
	if modelName == "" && (providerName == "anthropic-vertex" || providerName == "anthropic") {
		modelName = "claude-opus-4-7"
	}
	if modelName != "" {
		cfg.Model.Name = modelName
	}
	cfg.Permissions.Mode = config.PermissionModeAllow

	// Pattern grammar (permissions/policy.go): "<tool>:<glob>".
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

	prov, err := models.Resolve(cfg)
	if err != nil {
		return fmt.Errorf("resolve provider: %w", err)
	}
	parentModel, err := prov.Model(ctx, cfg.Model.Name)
	if err != nil {
		return fmt.Errorf("build model: %w", err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	gate, err := permissions.FromConfig(cfg, cwd, "", nil)
	if err != nil {
		return fmt.Errorf("permissions: %w", err)
	}
	reg, err := coretools.Build(cfg, gate, coretools.Default())
	if err != nil {
		return fmt.Errorf("tools.Build: %w", err)
	}

	// === Optional eventlog (enables --resume) ===
	const (
		appName = "scheduled-monitor-uat"
		userID  = "uat"
	)
	var handle *eventlog.Handle
	if dbPath != "" {
		if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
			return fmt.Errorf("mkdir session-db dir: %w", err)
		}
		handle, err = eventlog.Open(ctx, sqlite.Open(dbPath))
		if err != nil {
			return fmt.Errorf("eventlog.Open: %w", err)
		}
		defer func() { _ = handle.Close() }()
	}

	// === BackgroundAgentManager with the new default scheduler ===
	mgr, err := agent.NewBackgroundAgentManager(
		agent.WithBackgroundProvider(prov, cfg.Model.Name),
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
	// onAlertWake fires after each alert; rebound below once the
	// AutonomousHandle exists so we can call handle.RequestWake() to
	// pierce the supervisor's active sleep.
	var onAlertWake func(agent.Alert)
	mgr.OnAlert(func(a agent.Alert) {
		fmt.Printf("[alert] %s %s: %s\n", a.From, a.Kind, a.Text)
		if onAlertWake != nil {
			onAlertWake(a)
		}
	})

	// === Parent agent build ===
	instruction := agent.DefaultInstruction + "\n\n" +
		agent.DefaultSchedulingInstruction + "\n\n" +
		supervisorBrief
	spawnTools := agent.NewBackgroundSpawnTools(mgr)
	mkAgent := func(extras []adktool.Tool, sid string) (*agent.Agent, error) {
		all := make([]adktool.Tool, 0, len(reg.Tools)+len(spawnTools)+len(extras))
		all = append(all, reg.Tools...)
		all = append(all, spawnTools...)
		all = append(all, extras...)
		opts := []agent.Option{
			agent.WithAppName(appName),
			agent.WithName("scheduled-monitor-supervisor"),
			agent.WithSession(userID, sid),
			agent.WithInstruction(instruction),
			agent.WithTools(all),
			agent.WithBackgroundManager(mgr),
		}
		if handle != nil {
			opts = append(opts, agent.WithEventLog(handle))
		}
		return agent.New(parentModel, opts...)
	}
	build := func(extras []adktool.Tool) (*agent.Agent, error) {
		return mkAgent(extras, sessID)
	}
	resumeBuild := func(extras []adktool.Tool, sid string) (*agent.Agent, error) {
		return mkAgent(extras, sid)
	}

	// === Chat-style streaming via the bundled CLI's renderer ===
	eventCh := make(chan *session.Event, 1024)
	var chatWG sync.WaitGroup
	chatWG.Go(func() {
		seq := func(yield func(*session.Event, error) bool) {
			for ev := range eventCh {
				if !yield(ev, nil) {
					return
				}
			}
		}
		_ = runner.WriteEvents(seq, os.Stdout, os.Stderr)
	})
	defer func() { close(eventCh); chatWG.Wait() }()

	// === Banner + go ===
	fmt.Printf("== scheduled-monitor UAT driver ==\n")
	fmt.Printf("provider:       %s\n", providerName)
	fmt.Printf("model:          %s\n", cfg.Model.Name)
	fmt.Printf("max-wallclock:  %s\n", maxWC)
	fmt.Printf("max-turns:      %d\n", maxT)
	fmt.Printf("max-defer:      %s\n", maxDef)
	fmt.Printf("max-concurrent: %d\n", maxConc)
	fmt.Printf("bash allowlist: %s\n", strings.Join(bashAllow, ", "))
	if dbPath != "" {
		fmt.Printf("session-db:     %s\n", dbPath)
		fmt.Printf("session-id:     %s\n", sessID)
		fmt.Printf("mode:           %s\n", modeLabel(resumeFlag))
	} else {
		fmt.Printf("session-db:     (none — in-memory; --session-db enables resume)\n")
	}
	fmt.Println()

	go logHeartbeat(ctx, mgr)

	autoOpts := []agent.AutonomousOption{
		agent.WithMaxWallclock(maxWC),
		agent.WithMaxTurns(maxT),
		agent.WithMaxDefer(maxDef),
		agent.WithScheduler(coretools.SleepScheduler()),
		agent.WithPermissionsGate(gate),
		agent.WithProgress(func(_ int, ev *session.Event) {
			if ev == nil {
				return
			}
			select {
			case eventCh <- ev:
			default:
			}
		}),
	}

	// Resume currently uses the blocking ResumeAutonomous path —
	// AutonomousHandle covers the fresh-run case where we need
	// out-of-band Inject + RequestWake from the alert-watcher and
	// stdin-reader goroutines below.
	var (
		result agent.RunResult
		runErr error
	)
	if resumeFlag {
		result, runErr = agent.ResumeAutonomous(ctx, resumeBuild,
			agent.SessionRef{Handle: handle, AppName: appName, UserID: userID, SessionID: sessID},
			autoOpts...)
	} else {
		handle, err := agent.StartAutonomous(ctx, build, goal, autoOpts...)
		if err != nil {
			return fmt.Errorf("StartAutonomous: %w", err)
		}

		// Wake the supervisor on every alert. mgr.OnAlert runs
		// synchronously in the alert-pushing goroutine; we only need
		// to call handle.RequestWake (non-blocking, coalesced).
		onAlertWake = func(_ agent.Alert) { handle.RequestWake() }

		// Operator stdin reader. Each non-empty line is injected as
		// a message into the supervisor's inbox; Inject internally
		// fires the wake so the supervisor reacts immediately
		// rather than waiting for its next scheduled wake. The
		// goroutine exits on EOF; we don't block on it at shutdown
		// (handle.Wait below is the canonical run-end signal).
		//
		// Waits on handle.Ready() before printing the prompt so any
		// line the operator types lands in an existing agent's
		// inbox instead of failing with "agent not yet constructed".
		go func() {
			select {
			case <-handle.Ready():
			case <-ctx.Done():
				return
			}
			scanner := bufio.NewScanner(os.Stdin)
			scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
			fmt.Println("[input] type a message + Enter to inject into the supervisor's inbox; Ctrl+D to stop reading stdin (the run continues either way).")
			for scanner.Scan() {
				line := strings.TrimSpace(scanner.Text())
				if line == "" {
					continue
				}
				if err := handle.Inject(line); err != nil {
					fmt.Fprintf(os.Stderr, "[input] inject failed: %v\n", err)
					continue
				}
				fmt.Printf("[input] queued: %s\n", line)
			}
			if err := scanner.Err(); err != nil {
				fmt.Fprintf(os.Stderr, "[input] stdin scanner: %v\n", err)
			}
		}()

		result, runErr = handle.Wait()
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
		fmt.Printf("next-wake-at:  %s\n", result.NextWakeAt.Format(time.RFC3339))
	}
	if result.DoneDetail != "" {
		fmt.Printf("done detail:   %s\n", result.DoneDetail)
	}
	fmt.Println()
	fmt.Println("background subagents (terminal status):")
	for _, h := range mgr.List() {
		fmt.Printf("  %s -> %s\n", h.Name, h.Status())
	}
	return runErr
}

func modeLabel(resumeFlag bool) string {
	if resumeFlag {
		return "RESUME"
	}
	return "FRESH"
}

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
