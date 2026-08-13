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

// Example: the generator + checker half of gke-demos/bouncer, ported
// onto core-agent as a Go library.
//
// bouncer mines completed GKE TPU/JAX production workloads and
// derives verified single-slice "preflight" smoke tests from them.
// Upstream that is python google-adk: two agents in two directories,
// launched as two `adk run` subprocesses, agreeing on state through
// environment variables and on the verdict through a stdout grep.
//
// Here both agents are ordinary core-agent Agents in one process. The
// upstream prompts are copied verbatim under prompts/; what changes
// is the plumbing:
//
//   - the checker's `output_schema=CheckerResult` becomes a
//     report_verdict tool whose Go signature is the schema (checker.go)
//   - `subprocess.Popen(["adk","run","checker"])` + `grep "success:
//     True"` becomes an in-process call returning a typed verdict
//   - the BaseApiClient.async_request monkeypatch becomes an
//     adkmodel.LLM decorator (retry.go)
//   - bwrap/sudo containment stays exactly as upstream wrote it,
//     registered as the only shell the model can reach (exec.go) —
//     core-agent's built-in bash tool is never wired in
//   - the one-shot `adk run` becomes autonomous.Run with turn,
//     wallclock and cost budgets
//
// Hermetic run from the repo root — no cluster, no credentials, no
// cost (testdata/bin holds a fake kubectl):
//
//	PATH="$PWD/examples/bouncer-preflight/testdata/bin:$PATH" \
//	go run ./examples/bouncer-preflight \
//	  --source examples/bouncer-preflight/testdata/prod-jobset.yaml \
//	  --generator-script examples/bouncer-preflight/testdata/generator.jsonl \
//	  --checker-script examples/bouncer-preflight/testdata/checker.jsonl \
//	  --sandbox none --state-dir /tmp/bouncer-preflight
//
// Live run against a real test cluster (needs bwrap, sudo, kubectl
// and an agent-runner uid on the host — see README.md):
//
//	GOOGLE_API_KEY=... go run ./examples/bouncer-preflight \
//	  --source /path/to/completed-jobset.yaml \
//	  --namespace test-preflight --service-account preflight-sa
package main

import (
	"context"
	"embed"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/glebarez/sqlite"
	adkmodel "google.golang.org/adk/model"
	adktool "google.golang.org/adk/tool"

	"github.com/go-steer/core-agent/v2/pkg/agent"
	"github.com/go-steer/core-agent/v2/pkg/agent/autonomous"
	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/eventlog"
	"github.com/go-steer/core-agent/v2/pkg/models"
	_ "github.com/go-steer/core-agent/v2/pkg/models/gemini"
	"github.com/go-steer/core-agent/v2/pkg/models/mock"
)

// prompts are embedded so the binary carries the agents' behaviour
// with it — the upstream files, unedited, placeholders and all.
//
//go:embed prompts/generator_prompt.md prompts/checker_prompt.md
var prompts embed.FS

// defaultModel is bouncer's DEFAULT_MODEL (shared_tools/config.py).
const defaultModel = "gemini-3.1-pro-preview"

type options struct {
	source         string
	stateDir       string
	libraryDir     string
	sessionID      string
	namespace      string
	serviceAccount string
	policyFile     string
	sandboxMode    string
	provider       string
	model          string
	generatorTape  string
	checkerTape    string
	maxTurns       int
	maxWallclock   time.Duration
	maxCostUSD     float64
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		log.Fatal(err)
	}
}

func parseFlags(argv []string) (options, error) {
	var o options
	fs := flag.NewFlagSet("bouncer-preflight", flag.ContinueOnError)
	fs.StringVar(&o.source, "source", "", "path to the completed production manifest to derive from (required)")
	fs.StringVar(&o.stateDir, "state-dir", filepath.Join(os.TempDir(), "bouncer-preflight"),
		"scratch + library root; never the operator's $HOME")
	fs.StringVar(&o.libraryDir, "library-dir", "", "durable preflight library (default <state-dir>/library)")
	fs.StringVar(&o.sessionID, "session-id", "", "session id for the event log and scratch dir (default derived from the source file)")
	fs.StringVar(&o.namespace, "namespace", "test-preflight", "test namespace the preflight must target")
	fs.StringVar(&o.serviceAccount, "service-account", "default", "service account the preflight must run as")
	fs.StringVar(&o.policyFile, "policy-file", "", "file of extra cluster deployment policy bullets, spliced into the generator prompt")
	fs.StringVar(&o.sandboxMode, "sandbox", sandboxModeBwrap,
		"shell containment: \"bwrap\" (bubblewrap + uid drop, as upstream) or \"none\" (NO containment; demos and tests only)")
	fs.StringVar(&o.provider, "provider", config.ProviderGemini, "model provider: gemini or vertex")
	fs.StringVar(&o.model, "model", defaultModel, "model name")
	fs.StringVar(&o.generatorTape, "generator-script", "", "JSONL transcript to replay for the generator instead of calling a model")
	fs.StringVar(&o.checkerTape, "checker-script", "", "JSONL transcript to replay for the checker instead of calling a model")
	fs.IntVar(&o.maxTurns, "max-turns", 40, "turn budget for the generator loop")
	fs.DurationVar(&o.maxWallclock, "max-wallclock", 2*time.Hour, "wallclock budget for the generator loop")
	fs.Float64Var(&o.maxCostUSD, "max-cost", 0, "cost budget in USD (0 = unlimited)")
	if err := fs.Parse(argv); err != nil {
		return o, err
	}
	if o.source == "" {
		return o, errors.New("--source is required")
	}
	if o.libraryDir == "" {
		o.libraryDir = filepath.Join(o.stateDir, "library")
	}
	if o.sessionID == "" {
		o.sessionID = slugify(strings.TrimSuffix(filepath.Base(o.source), filepath.Ext(o.source)))
		if o.sessionID == "" {
			o.sessionID = "session"
		}
	}
	// Scripted mode is all-or-nothing: half a tape means the other
	// agent silently reaches for real credentials mid-run.
	if (o.generatorTape == "") != (o.checkerTape == "") {
		return o, errors.New("--generator-script and --checker-script must be given together")
	}
	switch o.sandboxMode {
	case sandboxModeBwrap, sandboxModeNone:
	default:
		return o, fmt.Errorf("--sandbox must be %q or %q, got %q", sandboxModeBwrap, sandboxModeNone, o.sandboxMode)
	}
	return o, nil
}

func run(argv []string) error {
	o, err := parseFlags(argv)
	if err != nil {
		return err
	}

	// Ctrl-C / SIGTERM cancels the loop; autonomous.Run unwinds and
	// reports the turns it did complete.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if o.sandboxMode == sandboxModeBwrap {
		if _, err := exec.LookPath("bwrap"); err != nil {
			return fmt.Errorf("--sandbox=bwrap needs bubblewrap on PATH (%w); install it, or pass --sandbox=none to run WITHOUT containment", err)
		}
	} else {
		fmt.Fprintln(os.Stderr,
			"bouncer-preflight: WARNING --sandbox=none — model-authored shell commands run uncontained, with this process's own privileges")
	}

	sourceBody, err := os.ReadFile(o.source) // #nosec G304 -- operator-supplied path
	if err != nil {
		return fmt.Errorf("read --source: %w", err)
	}

	st, err := newStore(o.stateDir, o.libraryDir, o.sessionID)
	if err != nil {
		return err
	}
	if err := st.writeSource(string(sourceBody)); err != nil {
		return err
	}
	workspace, err := resolveWorkspace(st.sessionDir)
	if err != nil {
		return fmt.Errorf("prepare workspace: %w", err)
	}
	box := newSandbox(o.sandboxMode, workspace)

	generatorLLM, checkerLLM, err := resolveModels(ctx, o)
	if err != nil {
		return err
	}

	logHandle, err := eventlog.Open(ctx, sqlite.Open(filepath.Join(o.stateDir, "eventlog.db")))
	if err != nil {
		return fmt.Errorf("open event log: %w", err)
	}
	defer func() { _ = logHandle.Close() }()

	policy, err := readPolicy(o.policyFile)
	if err != nil {
		return err
	}
	generatorPrompt, err := renderPrompt("prompts/generator_prompt.md", o.namespace, o.serviceAccount, policy)
	if err != nil {
		return err
	}
	checkerPrompt, err := renderPrompt("prompts/checker_prompt.md", o.namespace, o.serviceAccount, policy)
	if err != nil {
		return err
	}
	// The upstream checker prompt names its namespace only as an
	// example ("e.g. 'test-preflight'"); pin the real one so the
	// verifier can't wander into another namespace.
	checkerPrompt += fmt.Sprintf("\n### Environment\n- Test namespace: %s\n- Service account: %s\n- Workspace (the only writable path in the sandbox): /workspace\n",
		o.namespace, o.serviceAccount)

	say := func(msg string) { fmt.Fprintln(os.Stderr, "bouncer-preflight: "+msg) }
	checkCfg := checkerConfig{
		model:       checkerLLM,
		instruction: checkerPrompt,
		store:       st,
		sandbox:     box,
		log:         logHandle,
		sessionID:   o.sessionID,
		namespace:   o.namespace,
		sourceJob:   filepath.Base(o.source),
	}
	genCfg := generatorConfig{
		model:       generatorLLM,
		instruction: generatorPrompt,
		store:       st,
		sandbox:     box,
		objective:   objective(o),
		notify:      say,
		log:         logHandle,
		sessionID:   o.sessionID,
		check: func(ctx context.Context, req checkRequest) (verdict, error) {
			return runChecker(ctx, checkCfg, req)
		},
	}

	say("session " + o.sessionID + " — scratch " + st.sessionDir)
	say("library " + st.libraryDir)

	opts := []autonomous.Option{
		autonomous.WithMaxTurns(o.maxTurns),
		autonomous.WithMaxWallclock(o.maxWallclock),
	}
	if o.maxCostUSD > 0 {
		opts = append(opts, autonomous.WithMaxCost(o.maxCostUSD))
	}
	res, err := autonomous.Run(ctx,
		func(extras []adktool.Tool) (*agent.Agent, error) { return buildGenerator(genCfg, extras) },
		genCfg.objective, opts...)
	if err != nil {
		return fmt.Errorf("generator loop: %w", err)
	}

	fmt.Printf("stop reason: %s\n", res.Reason)
	fmt.Printf("turns:       %d\n", res.Turns)
	fmt.Printf("tokens:      %d in / %d out\n", res.InputTokens, res.OutputTokens)
	if res.CostUSD > 0 {
		fmt.Printf("cost:        $%.4f\n", res.CostUSD)
	}
	fmt.Printf("duration:    %s\n", res.Duration.Round(time.Millisecond))
	if res.DoneDetail != "" {
		fmt.Printf("done detail: %s\n", res.DoneDetail)
	}
	saved, err := st.listLibrary()
	if err != nil {
		return err
	}
	if len(saved) == 0 {
		fmt.Println("library:     (empty — no preflight was verified)")
		return nil
	}
	fmt.Printf("library:     %s\n", strings.Join(saved, ", "))
	return nil
}

// objective is the derivation request handed to the generator — the
// Go counterpart of the prompt bouncer.py builds per completed job.
func objective(o options) string {
	return fmt.Sprintf(
		"A production workload has completed and its manifest is saved for you; read it with "+
			"read_source_manifest. Derive a single-slice TPU preflight smoke test from it, verify the "+
			"preflight yourself in namespace %q as service account %q, then hand it to the Checker with "+
			"save_if_validated. You are done only once save_if_validated reports success.",
		o.namespace, o.serviceAccount)
}

// resolveModels returns the generator and checker models. Scripted
// tapes replay a recorded transcript with no credentials; otherwise
// both agents share one credentialled provider, each wrapped in the
// retry decorator.
func resolveModels(ctx context.Context, o options) (adkmodel.LLM, adkmodel.LLM, error) {
	warn := func(who string) func(int, error) {
		return func(attempt int, err error) {
			fmt.Fprintf(os.Stderr, "bouncer-preflight: %s model attempt %d failed (%v); backing off\n", who, attempt, err)
		}
	}
	if o.generatorTape != "" {
		gen, err := scriptedModel(ctx, o.generatorTape)
		if err != nil {
			return nil, nil, err
		}
		chk, err := scriptedModel(ctx, o.checkerTape)
		if err != nil {
			return nil, nil, err
		}
		// No retry wrapper in scripted mode: a tape that runs out is
		// a test bug, and retrying would just replay the same end.
		return gen, chk, nil
	}

	cfg := config.DefaultConfig()
	cfg.Model.Provider = o.provider
	cfg.Model.Name = o.model
	if err := cfg.Validate(); err != nil {
		return nil, nil, err
	}
	provider, err := models.Resolve(cfg)
	if err != nil {
		return nil, nil, err
	}
	m, err := provider.Model(ctx, cfg.Model.Name)
	if err != nil {
		return nil, nil, err
	}
	return withRetry(m, withRetryNotify(warn("generator"))),
		withRetry(m, withRetryNotify(warn("checker"))), nil
}

func scriptedModel(ctx context.Context, path string) (adkmodel.LLM, error) {
	p, err := mock.NewScripted(path, false)
	if err != nil {
		return nil, fmt.Errorf("load transcript %s: %w", path, err)
	}
	return p.Model(ctx, "")
}

// renderPrompt fills the upstream placeholders. bouncer does the same
// substitution at agent-construction time; keeping the markdown
// unedited is what makes "the prompts are the port" true.
func renderPrompt(name, namespace, serviceAccount, policy string) (string, error) {
	body, err := prompts.ReadFile(name)
	if err != nil {
		return "", fmt.Errorf("read embedded prompt %s: %w", name, err)
	}
	return strings.NewReplacer(
		"{{NAMESPACE}}", namespace,
		"{{SERVICE_ACCOUNT}}", serviceAccount,
		"{{CLUSTER_POLICY_RULES}}", policy,
	).Replace(string(body)), nil
}

func readPolicy(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	body, err := os.ReadFile(path) // #nosec G304 -- operator-supplied path
	if err != nil {
		return "", fmt.Errorf("read --policy-file: %w", err)
	}
	return strings.TrimRight(string(body), "\n"), nil
}
