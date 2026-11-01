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

package judge

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/glebarez/sqlite"
	"google.golang.org/genai"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	adkagent "google.golang.org/adk/v2/agent"
	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/plugin"
	"google.golang.org/adk/v2/runner"
	adksession "google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/session/database"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"github.com/go-steer/core-agent/v3/internal/effects"
	"github.com/go-steer/core-agent/v3/internal/evals"
	"github.com/go-steer/core-agent/v3/internal/evals/evalcompose"
	"github.com/go-steer/core-agent/v3/pkg/agent"
	"github.com/go-steer/core-agent/v3/pkg/approval"
)

const (
	appName = "mast"
	userID  = "mast-evals-judge"

	// agentName is the single SRE agent the corpus runs against.
	//
	// Not the shipped gke-triage roster, and the reason is the corpus
	// rather than convenience: those 13 specialists cover GKE triage,
	// while the corpus spans DNS, Ingress, TLS, NetworkPolicy, service
	// mesh, HPA, quota and storage across 31 categories. Roughly 25 of
	// them would route to _fallback, so the board would measure the
	// fallback specialist and call it a roster. Matching the corpus by
	// inventing 18 more specialists is teaching to the test.
	//
	// A single agent is also the like-for-like comparison: upstream is
	// one ReAct agent with a tool belt. Delegation, routing and roster
	// behaviour are measured by the W0.3 differentiators, which is
	// where they belong.
	//
	// It is built by internal/agent.NewCoordinator — mast's Chat-mode
	// constructor — holding the read toolset with no sub-agents, rather
	// than through compose.BuildRoot. Two things rule the composed path
	// out for a roster of one, and both are shape facts worth knowing:
	// a Task-mode specialist cannot be a runner root ("root agent must
	// be a chat LlmAgent"), and routing one through a coordinator
	// applies router.defaultCoordinatorInstruction, which rewrites the
	// specialist's answer into an "INCIDENT SUMMARY" block. That would
	// move severity_accuracy and the judge's grade for a formatting
	// reason rather than a capability one.
	agentName = "sre"
)

// systemInstruction is mast's side of the comparison.
//
// It deliberately does not copy upstream's SYSTEM_PROMPT verbatim.
// Theirs names eight subagents and eight of their own tools, and it
// asks for a "[CRITICAL] / • item" section layout — but every
// expected_response in the corpus is prose of the form "CRITICAL:
// <diagnosis>. Recommended action: <remedy>". Reproducing their prompt
// would produce a shape their own ground truth does not have, and both
// severity_accuracy and the judge would mark the difference. So the
// severity vocabulary and definitions are theirs verbatim, and the
// output shape follows the corpus.
const systemInstruction = `You are an autonomous SRE bot specializing in Kubernetes.

You are given one alert. Diagnose it using the cluster read tools, then
report.

## Reading the cluster

Call the tools you need. Each returns what it can see; "no abnormal
findings in this scope" means that reading was clean, not that the tool
failed. Several tools answer overlapping questions — k8s_triage_workload
returns a correlated snapshot of one workload, and k8s_cluster_health a
whole-cluster scorecard — so prefer the tool that answers your question
in one call.

You have read tools only. Do not claim to have changed anything.

## Reporting

Answer with a single short report, beginning with a severity token on
its own line or as a "SEVERITY: " prefix:

    CRITICAL: <what is wrong and why>. Recommended action: <what to do>.

Severity definitions:
  - CRITICAL: must fix immediately (service down, crash loops, OOM
    kills, 0 ready endpoints)
  - WARNING: should fix soon (no PDB, missing probes, :latest images,
    wildcard RBAC)
  - INFO: optimization opportunities (right-sizing, orphaned PVs,
    suspended CronJobs)
  - OK: no problem found

Name the specific resources, values and messages you read. A report that
does not say which object is affected is not actionable. If the evidence
does not support a diagnosis, say what you checked and what you would
need — do not guess.`

// Outcome is one scenario's run.
type Outcome struct {
	ID       string   `json:"id"`
	Category string   `json:"category"`
	Response string   `json:"response"`
	Tools    []string `json:"tools_called"`
	// Answering is the set of tools that had data for this scenario, so
	// a reader can tell a miss from an unlucky guess.
	Answering []string `json:"answering_tools"`
	// Results are the deterministic metrics, scored over the recorded
	// event log by the same code the free tier uses.
	Results []evals.MetricResult `json:"results"`
	// Ceiling is the highest intent_coverage this scenario can reach
	// against a read-only surface. Below 1.0 means the corpus expects a
	// write tool: LC-13 expects kubectl_rollback_deployment, which
	// lookout excludes by design because remediation is mast's
	// effect-outbox path. Reported rather than folded into the score.
	Ceiling float64 `json:"intent_coverage_ceiling"`
	// Authored reports that this scenario's cluster is a hand-written
	// override rather than one the quoting rule derived. Carried onto the
	// board because the share of the corpus resting on authored fixtures
	// is the honest reader's first question about a judge score.
	Authored bool `json:"authored_fixture,omitempty"`
	// Calls are the recorded calls with their arguments and a digest of
	// what each returned. Tools above says which tools were reached;
	// this says how, which is the difference between an intent the run
	// never pursued and one it pursued against a scope that held
	// nothing (#169).
	Calls []evals.CallRecord `json:"calls,omitempty"`
	// Violations are the things wrong with those calls, enumerated
	// rather than averaged — see the note at the top of
	// internal/evals/validity.go.
	Violations []evals.Violation `json:"violations,omitempty"`
	// Misses explains this row's intent_coverage: which unanswered
	// questions the catalog would have answered (tool selection) and
	// which it could not (the ceiling). See internal/evals/misses.go for
	// why the count of uncalled tools is not the thing to measure.
	Misses evals.MissReport `json:"misses"`
	// Quality is the judge's grade. Zero value when grading was not
	// requested.
	Quality *Grade `json:"quality,omitempty"`
	// LoopStop is why the in-turn loop guard ended this run, empty when
	// it did not. The row is still scored: a daemon would have stopped
	// the model at the same call.
	LoopStop string `json:"loop_stop,omitempty"`
}

// Rig runs corpus scenarios against a real model over the fixture
// cluster.
type Rig struct {
	tbl      evals.IntentTable
	fixtures map[string]Observations
	model    adkmodel.LLM
	scratch  string
	// pred classifies tool calls for the trace adapter. Built once: it
	// depends only on the intent table, and effects.Overrides logs a
	// line per policy, so rebuilding it per scenario would print 341
	// lines of the same eleven facts over a 31-row run.
	pred effects.Predicate
}

// NewRig prepares the judge tier. The model is the caller's — the
// harness builds it from compose.BuildModel so the tier exercises
// mast's real provider path rather than a bespoke client.
func NewRig(tbl evals.IntentTable, fixtures map[string]Observations, m adkmodel.LLM, scratch string) (*Rig, error) {
	if m == nil {
		return nil, fmt.Errorf("judge: no model")
	}
	if scratch == "" {
		return nil, fmt.Errorf("judge: no scratch dir")
	}
	// Every fixture tool is read-only, and the agent has no sub-agents.
	// Stating that explicitly rather than defaulting keeps the trace
	// adapter honest: effects.NewPredicate treats unknown tools as
	// mutating, which would make effect_ordering complain about reads.
	//
	// The quiet logger is the tier's, not a global: the override lines
	// are correct and wanted in a daemon, and only noise in a report.
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	pred := effects.NewPredicate(effects.Overrides(quiet, readOnlyPolicies(tbl)))
	return &Rig{tbl: tbl, fixtures: fixtures, model: m, scratch: scratch, pred: pred}, nil
}

// Run executes one scenario end to end: build its fixture cluster, give
// the agent the alert, and score the recorded log.
func (r *Rig) Run(ctx context.Context, sc evals.Scenario) (Outcome, error) {
	obs, ok := r.fixtures[sc.ID]
	if !ok {
		return Outcome{}, fmt.Errorf("judge: %s has no fixture", sc.ID)
	}
	cluster, err := NewCluster(r.tbl, sc, obs)
	if err != nil {
		return Outcome{}, err
	}

	svc, err := database.NewSessionService(
		sqlite.Open(filepath.Join(r.scratch, sc.ID+".db")),
		&gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)},
	)
	if err != nil {
		return Outcome{}, fmt.Errorf("judge: %s: open session store: %w", sc.ID, err)
	}
	if err := database.AutoMigrate(svc); err != nil {
		return Outcome{}, fmt.Errorf("judge: %s: migrate session store: %w", sc.ID, err)
	}

	calls := &callLog{}
	tools, err := buildTools(cluster, calls)
	if err != nil {
		return Outcome{}, err
	}
	// Read the schemas off the built tools before the run, so a rig that
	// cannot describe its own tools fails here rather than reporting
	// every call the model makes as unknown.
	schemas, err := toolSchemas(tools)
	if err != nil {
		return Outcome{}, fmt.Errorf("judge: %s: %w", sc.ID, err)
	}

	ag, err := agent.NewCoordinator(agent.CoordinatorConfig{
		Name:        agentName,
		Description: "diagnoses Kubernetes incidents from cluster reads",
		Instruction: systemInstruction,
		Model:       r.model,
		Toolsets:    []tool.Toolset{&staticToolset{name: "lookout-fixture", tools: tools}},
	})
	if err != nil {
		return Outcome{}, fmt.Errorf("judge: %s: build agent: %w", sc.ID, err)
	}

	// The in-turn loop guard, at mast's default posture, so a model that
	// loops is scored the way a daemon would run it: told inside the
	// turn, and stopped if it keeps going (#514). Without it one looping
	// row runs until the server refuses the context, and the board
	// prices that as the model's cost rather than mast's.
	guard, err := evalcompose.LoopGuard(evalcompose.LoopGuardConfig{
		MaxModelCallsPerTurn: evalcompose.DefaultMaxModelCallsPerTurn,
	})
	if err != nil {
		return Outcome{}, fmt.Errorf("judge: %s: build loop guard: %w", sc.ID, err)
	}
	var plugins []*plugin.Plugin
	if guard != nil {
		plugins = append(plugins, guard)
	}
	run, err := runner.New(runner.Config{
		AppName:           appName,
		Agent:             ag,
		SessionService:    svc,
		AutoCreateSession: true,
		PluginConfig:      runner.PluginConfig{Plugins: plugins},
	})
	if err != nil {
		return Outcome{}, fmt.Errorf("judge: %s: construct runner: %w", sc.ID, err)
	}

	sessionID := sc.ID
	msg := genai.NewContentFromText(sc.Inputs.Scenario, genai.RoleUser)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := &turnStop{cancel: cancel}
	runCtx = approval.WithTurnStop(runCtx, stop)
	for _, rerr := range run.Run(runCtx, userID, sessionID, msg, adkagent.RunConfig{
		StreamingMode: adkagent.StreamingModeNone,
	}) {
		if rerr != nil {
			if stop.why() != nil {
				// The guard ended the turn. That is a completed row,
				// scored on what the model did before it was stopped.
				break
			}
			return Outcome{}, fmt.Errorf("judge: %s: run: %w", sc.ID, rerr)
		}
	}
	var loopStop string
	if why := stop.why(); why != nil {
		loopStop = why.Error()
	}

	resp, err := svc.Get(ctx, &adksession.GetRequest{
		AppName: appName, UserID: userID, SessionID: sessionID,
	})
	if err != nil {
		return Outcome{}, fmt.Errorf("judge: %s: read session: %w", sc.ID, err)
	}

	trace := evals.TraceFromEvents(resp.Session.Events(), r.pred, nil)

	return Outcome{
		ID:         sc.ID,
		Category:   sc.Category,
		Response:   trace.FinalText,
		Tools:      calls.names(),
		Answering:  cluster.AnsweringTools(),
		Calls:      evals.RecordCalls(trace.Calls),
		Violations: evals.ValidateCalls(schemas, trace.Calls, calls.emptyReads()),
		Misses:     evals.ClassifyMisses(r.tbl, sc, trace),
		Results:    evals.EvaluateAll(r.tbl, sc, trace),
		Ceiling:    r.ceiling(sc),
		Authored:   !obs.Derived,
		LoopStop:   loopStop,
	}, nil
}

// turnStop is approval.TurnStop for one rig run: it records why the
// turn ended and cancels it, the way the daemon's refusalStop does.
type turnStop struct {
	mu     sync.Mutex
	reason error
	cancel context.CancelFunc
}

func (s *turnStop) StopTurn(reason error) {
	s.mu.Lock()
	if s.reason == nil {
		s.reason = reason
	}
	s.mu.Unlock()
	s.cancel()
}

func (s *turnStop) why() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reason
}

// ceiling is the highest intent_coverage a read-only surface can reach
// on this scenario: the fraction of its expected intents that some
// lookout tool satisfies.
func (r *Rig) ceiling(sc evals.Scenario) float64 {
	want, unknown := r.tbl.IntentsFor(sc.Outputs.ExpectedTools)
	denom := len(want) + len(unknown)
	if denom == 0 {
		return 1
	}
	reachable := make(map[string]bool)
	for _, lt := range r.tbl.LookoutTools {
		for _, id := range lt.Satisfies {
			reachable[id] = true
		}
	}
	hit := 0
	for _, id := range want {
		if reachable[id] {
			hit++
		}
	}
	return float64(hit) / float64(denom)
}

// readOnlyPolicies declares every lookout tool non-mutating. lookout's
// MCP surface is read-path only by design (testdata/evals/intents.yaml),
// so this is a statement of that fact rather than a fixture
// convenience.
func readOnlyPolicies(tbl evals.IntentTable) []effects.ToolPolicy {
	out := make([]effects.ToolPolicy, 0, len(tbl.LookoutTools))
	no := false
	for name := range tbl.LookoutTools {
		out = append(out, effects.ToolPolicy{Name: name, Mutating: &no})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// scopeArgs is the argument shape every fixture tool takes.
//
// Real lookout tools have varied signatures, and the fixture does not
// branch on the value. Giving each a bespoke argument struct would be
// precision the fixture does not have; one honest scope string keeps
// what is measured — which tool the agent reached for — undiluted.
type scopeArgs struct {
	Scope string `json:"scope"`
}

// toolDescription is what the model sees. The text is the tool's own
// purpose from the intent table's notes where one exists, so the agent
// chooses from lookout's real self-description rather than from
// something written to make this eval pass.
var toolDescription = map[string]string{
	"k8s_triage_workload":  "Correlated snapshot of one workload: sanitized spec, everything abnormal, broken dependency edges, blast radius, distilled logs.",
	"k8s_cluster_health":   "Ten-category cluster health scorecard in one call: nodes, pods, workloads, quota, storage.",
	"k8s_triage_delta":     "What changed in cluster health since the last snapshot.",
	"k8s_resource_spec":    "One resource's spec, status and conditions, by kind. Covers NetworkPolicies and CRDs.",
	"k8s_triage_logs":      "Distilled container logs for a workload.",
	"k8s_event_timeline":   "Recent events for an object and its owner tree, in order.",
	"k8s_resource_top":     "CPU and memory usage against allocatable or limits, for nodes or pods.",
	"k8s_state_edges":      "Dependency edges: Service selectors and endpoints, Ingress backends, ConfigMap and Secret references.",
	"k8s_volume_conflicts": "PVC and PV binding state, StorageClass provisioning, access-mode conflicts.",
	"k8s_recent_changes":   "Recent rollouts and revision history.",
	"k8s_cloud_quota":      "ResourceQuota, LimitRange and cloud-side quota headroom.",
}

func buildTools(c *Cluster, calls *callLog) ([]tool.Tool, error) {
	names := make([]string, 0, len(toolShape))
	for name := range toolShape {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]tool.Tool, 0, len(names))
	for _, name := range names {
		desc, ok := toolDescription[name]
		if !ok {
			return nil, fmt.Errorf("judge: tool %q has no description — the model would be choosing blind", name)
		}
		t, err := functiontool.New(functiontool.Config{
			Name:        name,
			Description: desc + " Scope is a namespace, a namespace/name, or empty for the whole cluster.",
		}, func(_ adkagent.Context, args scopeArgs) (map[string]any, error) {
			reading, found := c.ReadResult(name, args.Scope)
			calls.record(name, args.Scope, found)
			return map[string]any{"reading": reading}, nil
		})
		if err != nil {
			return nil, fmt.Errorf("judge: build tool %q: %w", name, err)
		}
		out = append(out, t)
	}
	return out, nil
}

// toolSchemas reads back the schema each fixture tool declares, for
// [evals.ValidateCalls]. Read from the built tools rather than written
// out beside them: an expectation maintained by hand next to the thing
// it describes is an expectation that goes stale, and here it would go
// stale silently — every recorded call would validate against a
// catalog that no longer matches what the model was shown.
func toolSchemas(tools []tool.Tool) (evals.ToolSchemas, error) {
	out := make(evals.ToolSchemas, len(tools))
	for _, t := range tools {
		d, ok := t.(interface {
			Declaration() *genai.FunctionDeclaration
		})
		if !ok {
			return nil, fmt.Errorf("judge: tool %T does not expose Declaration(); its calls could not be validated", t)
		}
		decl := d.Declaration()
		if decl == nil || decl.Name == "" {
			return nil, fmt.Errorf("judge: tool %T declared nothing", t)
		}
		var src any
		switch {
		case decl.Parameters != nil:
			src = decl.Parameters
		case decl.ParametersJsonSchema != nil:
			src = decl.ParametersJsonSchema
		default:
			out[decl.Name] = map[string]any{"type": "object"}
			continue
		}
		raw, err := json.Marshal(src)
		if err != nil {
			return nil, fmt.Errorf("judge: tool %q: marshal declaration: %w", decl.Name, err)
		}
		var schema map[string]any
		if err := json.Unmarshal(raw, &schema); err != nil {
			return nil, fmt.Errorf("judge: tool %q: unmarshal declaration: %w", decl.Name, err)
		}
		out[decl.Name] = schema
	}
	return out, nil
}

// callLog records tool calls in order. The trace adapter also sees them,
// but this is the fixture's own count, so a disagreement between the two
// is visible rather than assumed away.
//
// It also records what only the fixture knows: whether the read found
// anything. The trace can see the reading, but "no abnormal findings in
// this scope" is prose the agent is meant to read as a real cluster's
// silence, and pattern-matching it back out downstream would couple the
// scorer to the fixture's wording. The cluster says so directly instead.
type callLog struct {
	mu sync.Mutex
	in []fixtureCall
}

// fixtureCall is one call as the fixture served it.
type fixtureCall struct {
	name  string
	scope string
	found bool
}

func (l *callLog) record(name, scope string, found bool) {
	l.mu.Lock()
	l.in = append(l.in, fixtureCall{name: name, scope: scope, found: found})
	l.mu.Unlock()
}

func (l *callLog) names() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, 0, len(l.in))
	for _, c := range l.in {
		out = append(out, c.name)
	}
	return out
}

// emptyReads returns a predicate reporting whether a recorded call
// found nothing, matching a trace call to the fixture's own record by
// (tool, scope) in order.
//
// Matching in order rather than by identity is deliberate: the same
// tool called twice on the same scope is two entries here, and
// collapsing them would hide the second. A trace call with no fixture
// entry — an unknown tool the runtime rejected before it reached the
// fixture — is not claimed to be empty; ValidateCalls has already
// reported it as unknown, and adding a second line about the same call
// would inflate the count.
func (l *callLog) emptyReads() func(evals.Call) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	remaining := make([]fixtureCall, len(l.in))
	copy(remaining, l.in)

	return func(c evals.Call) bool {
		scope, _ := c.Args["scope"].(string)
		for i, f := range remaining {
			if f.name != c.Name || f.scope != scope {
				continue
			}
			remaining = append(remaining[:i], remaining[i+1:]...)
			return !f.found
		}
		return false
	}
}

// staticToolset offers a fixed tool list, the minimum tool.Toolset a
// fixture needs to reach specialists.Build's allowlist filter.
type staticToolset struct {
	name  string
	tools []tool.Tool
}

func (s *staticToolset) Name() string { return s.name }

func (s *staticToolset) Tools(adkagent.ReadonlyContext) ([]tool.Tool, error) { return s.tools, nil }

// summarize renders one outcome's metrics as a compact line.
func (o Outcome) summarize() string {
	parts := make([]string, 0, len(o.Results))
	for _, res := range o.Results {
		parts = append(parts, fmt.Sprintf("%s=%.2f", res.Metric, res.Score))
	}
	return strings.Join(parts, " ")
}
