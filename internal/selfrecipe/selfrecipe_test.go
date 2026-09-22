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

// Package selfrecipe_test is the loader-and-content validation for the
// repository's own `/.agents/` recipe — the one core-agent runs under
// when it does development work on core-agent (#1116, P3).
//
// **Why it is here and not beside the recipe.** The Go tool ignores
// directories whose names begin with a dot, so a `.agents/recipe_test.go`
// is invisible to `go test ./...` and would never run in CI. It is the
// kind of test that gets written, passes locally because someone ran
// `go test ./.agents/`, and silently protects nothing. This package
// reaches the recipe by relative path instead. `docs/self-development-
// design.md` D2 calls this out; the comment is repeated here because the
// next person to add a recipe test will be looking at this file, not the
// design doc.
//
// These are loader + content assertions only — no credentials, no live
// provider, no LLM — so they run as ordinary unit tests. What they cannot
// do is score the agent's work; that is the `dev/uat/self-dev/` drill
// (P4), and the honest division of labour is that CI proves the recipe is
// well-formed and only the drill proves it is any good.
package selfrecipe_test

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/modeltier"
	"github.com/go-steer/core-agent/v2/pkg/skills"
)

// `go test` runs with the package dir as the working directory, so the
// recipe is two levels up.
const (
	repoRoot     = "../.."
	agentsDir    = repoRoot + "/.agents"
	reviewerRoot = agentsDir + "/reviewer"
)

// wantSkills are the five rituals AGENTS.md describes in prose and the
// recipe makes executable. Named rather than counted: a count passes
// when one is renamed to something nobody loads.
var wantSkills = []string{
	"adversarial-review-gate",
	"changelog-bullet",
	"prefix-failure-verification",
	"presubmit-sweep",
	"stacked-pr-order",
}

func loadRecipe(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Load(agentsDir)
	if err != nil {
		t.Fatalf("config.Load(%s): %v — the recipe does not load, so every "+
			"other assertion here is vacuous", agentsDir, err)
	}
	return cfg
}

// TestRecipeLoads is the floor. Everything below assumes it.
func TestRecipeLoads(t *testing.T) {
	t.Parallel()
	if cfg := loadRecipe(t); cfg == nil {
		t.Fatal("config.Load returned nil config and nil error")
	}
}

// TestDisplayNameNamesTheRecipe is D3's third mitigation, with its
// mechanism corrected.
//
// Committing `/.agents/` re-parents every unpinned core-agent invocation
// anywhere under this checkout: config.Find walks UP and returns the
// first .agents/ it meets. The `-c` pin (P2) and
// verify-harness-config-pinned close that for the harness; neither helps
// a contributor running the binary by hand from the repo.
//
// **What this string does NOT do**, contrary to what D3 originally said
// and what an earlier draft of this test asserted: it does not put the
// inheritance on line one of the startup summary. `cfg.Agent.DisplayName`
// is read in exactly one place — agentDisplayName() in
// cmd/core-agent/coretui_enabled.go — and it feeds the TUI status-line
// banner. A `-p` run never prints it. Measured, not assumed.
//
// The unconditional `config: source=… (via .agents/ discovery)` line is
// the mitigation that actually works, and it needs nothing from this
// file. This assertion is kept for the narrower thing it is worth: an
// interactive session says whose recipe it is running, and the name is
// the one the design doc and the drill refer to.
func TestDisplayNameNamesTheRecipe(t *testing.T) {
	t.Parallel()
	cfg := loadRecipe(t)
	const want = "core-agent-selfdev"
	if got := cfg.Agent.DisplayName; got != want {
		t.Errorf("agent.display_name = %q, want %q — the drill and "+
			"docs/self-development-design.md D3 both name this recipe by "+
			"that string", got, want)
	}
}

// spikeValidatedModels are the models a graded self-development task has
// actually been run on with a passing score. Adding an entry means
// running that task, not editing this list.
//
// **Deliberately an allowlist and not a modeltier assertion**, which is
// what the first draft of this test used and why it did not work.
// `modeltier.Classify` sorts `claude-opus-4` into `TierFrontier` — which
// is correct for what that taxonomy is *for*, namely context-window and
// compaction thresholds — and the #1116 spike measured claude-opus-4-5
// scoring 1/6 and 2/6 on the task claude-opus-5 scored 5/6 on. So the
// tier check passed a model the evidence rejects. Capability for this
// job and tier for compaction are different axes and the recipe needs
// the one nobody has a classifier for.
//
// The cost of the allowlist is that a genuinely better future model
// fails this test until someone adds it. That is the intended friction:
// **dropping to a cheaper model does not degrade gracefully here.** It
// produces confident, well-structured artifacts that are wrong in the
// direction a review gate cannot see, which is the worst available
// failure for a pipeline whose output is a pull request.
var spikeValidatedModels = map[string]bool{
	"claude-opus-5": true, // 5/6, matching Claude Code on the same task (2026-09-17)
}

// TestModelIsOneTheSpikeValidated pins the spike's central finding: the
// harness is not the limiting factor, the model is.
func TestModelIsOneTheSpikeValidated(t *testing.T) {
	t.Parallel()
	cfg := loadRecipe(t)

	// Resolved through modelName rather than read inline: SubagentSpec.Model
	// is a *ModelConfig and omitting `model` from a subagent is a legal
	// config (Validate only checks Model.Name when Model is set). An inline
	// `.Model.Name` panics on exactly the input the tc.name == "" branch
	// below exists to report, and the panic takes every other test in the
	// package down with it.
	for _, tc := range []struct{ what, name string }{
		{"parent", cfg.Model.Name},
		{"reviewer subagent", modelName(subagentByName(t, cfg, "reviewer").Model)},
	} {
		if tc.name == "" {
			t.Errorf("%s has no model name pinned; it would inherit, and the "+
				"model is a requirement rather than a default here", tc.what)
			continue
		}
		if !spikeValidatedModels[tc.name] {
			t.Errorf("%s model is %q, which no graded self-development run has "+
				"passed on. Run the task and add it to spikeValidatedModels, or "+
				"use one that is there. Note %q classifies as %v — the tier is "+
				"not the evidence, and claude-opus-4-5 is frontier-tier and "+
				"scored 1/6.", tc.what, tc.name, tc.name, modeltier.Classify(tc.name))
		}
	}
}

// TestBudgetsLeaveRoomForRealWork guards the mistake this recipe already
// made once.
//
// #1116 carried a "Blocker for T3" for months: both spike runs died
// mid-task with `anthropic: stream: context canceled` and no visible
// budget or watchdog trip. It was not the provider. It was the spike
// recipe's own max_turn_cost_usd: 2.0, invisible because a guardrail that
// cut an unattended turn reported itself only through the operator-event
// seam, which is a no-op with no emitter registered (#1131, since fixed).
//
// A single turn here runs presubmits, reads a lot of source and writes a
// PR body. The floor is not a tuned number — it is "high enough that the
// 2.0 that caused the incident fails this test".
func TestBudgetsLeaveRoomForRealWork(t *testing.T) {
	t.Parallel()
	cfg := loadRecipe(t)

	const minTurn = 5.0
	if cfg.Agent.MaxTurnCostUSD == nil {
		t.Fatal("agent.max_turn_cost_usd is unset; an unattended run defaults " +
			"to a ceiling, so leaving it out does not mean 'no ceiling'")
	}
	if got := *cfg.Agent.MaxTurnCostUSD; got < minTurn {
		t.Errorf("agent.max_turn_cost_usd = %v, want >= %v — 2.0 is the value "+
			"that cut both #1116 spike runs mid-task and was misread as a "+
			"provider fault for months", got, minTurn)
	}

	if cfg.Agent.MaxSessionCostUSD == nil {
		t.Fatal("agent.max_session_cost_usd is unset")
	}
	turn, session := *cfg.Agent.MaxTurnCostUSD, *cfg.Agent.MaxSessionCostUSD
	if session <= turn {
		t.Errorf("agent.max_session_cost_usd = %v is not above "+
			"max_turn_cost_usd = %v; the session ceiling would trip on the "+
			"first expensive turn and halt the session rather than the turn",
			session, turn)
	}
}

// TestWatchdogIsEnforced — the recipe's autonomy story rests on the
// watchdog being a kill switch, and the unattended default is conditional
// on how the process is launched. Declared beats inherited: a silent
// downgrade to observe-only boots fine, passes everything else here, and
// removes the only backstop against a runaway loop.
func TestWatchdogIsEnforced(t *testing.T) {
	t.Parallel()
	cfg := loadRecipe(t)
	if got := cfg.Safety.Watchdog; got != "enforce" {
		t.Errorf("safety.watchdog = %q, want \"enforce\"", got)
	}
}

// TestPlanGateIsRequired. plan_mode=required makes record_plan gate the
// mutating tools, which is what gives a human something to read before
// the run starts changing a repository it is also running from.
func TestPlanGateIsRequired(t *testing.T) {
	t.Parallel()
	cfg := loadRecipe(t)
	if got := cfg.Permissions.PlanMode; got != "required" {
		t.Errorf("permissions.plan_mode = %q, want \"required\"", got)
	}
}

// TestPermissionModeIsNotYolo.
//
// D4 permits `yolo` only inside the disposable /tmp clone, which is a
// property of how a drill launches the binary and not of the committed
// recipe. This file is discovered by walk-up from anywhere in the
// checkout (D3), so a `yolo` here is a `yolo` for whoever runs the binary
// from this repo without pinning a config.
func TestPermissionModeIsNotYolo(t *testing.T) {
	t.Parallel()
	cfg := loadRecipe(t)
	if got := cfg.Permissions.Mode; got == "yolo" {
		t.Errorf("permissions.mode = %q; the committed recipe must not be "+
			"yolo — D4 scopes that to the throwaway /tmp clone, and this file "+
			"is reachable by config.Find's walk-up from the real checkout", got)
	}
}

// TestReviewerSubagentIsRootedAndGrantsNoWriteTool.
//
// Rooted, so its persona comes from its own AGENTS.md rather than
// inheriting the parent's — a reviewer that reads "you are doing
// development work on this repo" reviews like an author.
//
// And the tool list is explicit — a nil list INHERITS the parent's
// registry, write tools included — and names no write tool. The point of
// a gate is that it cannot fix what it finds: a reviewer that edits is an
// author reviewing their own work one call later.
//
// **This is weaker than "read-only" and the name says so.** The grant
// includes `bash`, which writes anything. That is deliberate — a reviewer
// that cannot run `go test` or `git diff` cannot distinguish CONFIRMED
// from PLAUSIBLE, and the confirmations are most of the value — so the
// read-only property is carried by the persona
// (.agents/reviewer/AGENTS.md says so in as many words), not by the tool
// surface. What this test pins is the half that *is* mechanical, and the
// gap is named rather than papered over: the same distinction the
// gated-apply work landed on, where a `tools` allowlist that narrows
// reads was a confound rather than a safety property.
func TestReviewerSubagentIsRootedAndGrantsNoWriteTool(t *testing.T) {
	t.Parallel()
	cfg := loadRecipe(t)
	spec := subagentByName(t, cfg, "reviewer")

	if spec.Root == "" {
		t.Error("reviewer has no root; it would inherit the parent's persona, " +
			"and an author-shaped reviewer grades style")
	}
	if spec.Tools == nil {
		t.Fatal("reviewer.tools is nil, which INHERITS the parent's registry " +
			"including its write tools; the read-only property has to be an " +
			"explicit list")
	}

	writeTools := map[string]bool{
		"write_file": true, "edit_file": true, "delete_file": true,
		"record_plan": true,
	}
	for _, name := range spec.Tools {
		if writeTools[name] {
			t.Errorf("reviewer grants %q; the reviewer reports and does not fix", name)
		}
	}
}

// TestReviewerPersonaExists — a rooted subagent auto-assembles its
// persona from <root>/AGENTS.md, and a missing one is not a load error.
// It is an empty persona, which is the silent version of this failure.
func TestReviewerPersonaExists(t *testing.T) {
	t.Parallel()
	b, err := os.ReadFile(filepath.Join(reviewerRoot, "AGENTS.md"))
	if err != nil {
		t.Fatalf("reviewer persona: %v — a rooted subagent with no AGENTS.md "+
			"loads fine and runs with no instruction at all", err)
	}
	if len(strings.TrimSpace(string(b))) < 200 {
		t.Errorf("reviewer persona is %d bytes; that is a placeholder, not a persona",
			len(b))
	}
}

// TestSkillsAreTheDocumentedFive.
func TestSkillsAreTheDocumentedFive(t *testing.T) {
	t.Parallel()

	got, err := skills.Load(context.Background(), agentsDir, nil)
	if err != nil {
		t.Fatalf("skills.Load(%s): %v", agentsDir, err)
	}

	want := make(map[string]bool, len(wantSkills))
	for _, n := range wantSkills {
		want[n] = true
	}
	names := make([]string, 0, len(got.Infos))
	for _, in := range got.Infos {
		names = append(names, in.Name)
		if !want[in.Name] {
			t.Errorf(".agents/skills/ has unexpected skill %q", in.Name)
		}
		delete(want, in.Name)
	}
	sort.Strings(names)
	for n := range want {
		t.Errorf("skill %q missing; discovered %v", n, names)
	}
}

// TestSkillsAreInstructionNotScratchpad.
//
// **Named for what it checks.** The draft of this test was called
// TestSkillsCarryNoGradedFacts and its comment announced the rule "a
// skill must not name a specific open issue as having a specific answer"
// — which the body did not implement and three of the five skills
// violate on their face. The concern is real and is recorded below; it is
// not offline-checkable, because "is this issue still open" and "is this
// sentence a conclusion or a method" are both questions this process
// cannot answer. A test whose name claims a guarantee its body does not
// provide is worse than no test: it is the reason nobody looks.
//
// The real concern, enforced by review rather than here: **skills survive
// `--no-builtin-tools`**, so a skill is never a safe place to put a fact
// a graded run is supposed to discover (#966). These five describe
// *method* — how to run the sweep, how to prove a pre-fix failure — and
// cite shipped issues only as worked examples. The moment one carries a
// conclusion to an open question, a scenario grading whether the agent
// found that conclusion is grading its ability to read a handed-over
// file. The corpus-side check for that belongs with the corpus, which
// knows what it grades; this package does not.
//
// What is left is mechanical and still worth pinning: a skill is
// instruction the model follows literally, and it is matched on its
// frontmatter.
func TestSkillsAreInstructionNotScratchpad(t *testing.T) {
	t.Parallel()

	entries, err := os.ReadDir(filepath.Join(agentsDir, "skills"))
	if err != nil {
		t.Fatalf("read skills dir: %v", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(agentsDir, "skills", e.Name(), "SKILL.md")
		b, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("%s: %v", path, err)
			continue
		}
		body := string(b)
		for _, banned := range []string{"TODO", "FIXME", "XXX"} {
			if strings.Contains(body, banned) {
				t.Errorf("%s contains %q; a skill is instruction the model "+
					"follows literally, not a scratchpad", path, banned)
			}
		}
		if !strings.HasPrefix(body, "---\n") {
			t.Errorf("%s has no YAML frontmatter; name and description are "+
				"what the model matches a skill on", path)
			continue
		}
		// The description is the entire matching surface — a skill with a
		// name and no description loads, is listed, and is never chosen.
		fm, _, ok := strings.Cut(strings.TrimPrefix(body, "---\n"), "\n---")
		if !ok {
			t.Errorf("%s has an unterminated frontmatter fence", path)
			continue
		}
		if !strings.Contains(fm, "\ndescription:") && !strings.HasPrefix(fm, "description:") {
			t.Errorf("%s frontmatter has no description; that is what a skill "+
				"is selected on, so one without it is loaded and never used", path)
		}
	}
}

// Both gitignore tests below ask **git** whether a path is ignored rather
// than matching lines in `.gitignore`, and the difference is not
// pedantry. The first drafts of both did the text match, and the
// adversarial review broke both with one-line edits that a reader would
// call equivalent: `/.agents/*` ignores the entire recipe and is not any
// of the four spellings the literal scan looked for, and a trailing
// `!/.agents/plans/` un-ignores the plans directory while leaving the
// line the scan wanted still present above it. A guard against "the
// ignore rules are subtly wrong" cannot itself be a rule about how the
// ignore rules are spelled. This is the repo's scan-what-runs principle
// (see the shell guard scanner), with git as the interpreter.

// gitIgnored reports whether git would ignore rel, which is relative to
// the repo root. --no-index is what makes this a question about the
// ignore rules alone: without it git reports a tracked path as
// not-ignored no matter what the rules say, which would make the
// runtime-state direction pass for the wrong reason the moment someone
// force-added one.
func gitIgnored(t *testing.T, rel string) bool {
	t.Helper()
	cmd := exec.Command("git", "check-ignore", "--no-index", "-q", "--", rel)
	cmd.Dir = repoRoot
	err := cmd.Run()
	if err == nil {
		return true
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 1 {
		return false
	}
	t.Fatalf("git check-ignore %s: %v", rel, err)
	return false
}

// requireGitWorkTree skips when there is no git to ask — a source tarball
// or a vendored copy. Everything else in this file is answerable from the
// files themselves; these two are not.
func requireGitWorkTree(t *testing.T) {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "--is-inside-work-tree")
	cmd.Dir = repoRoot
	if err := cmd.Run(); err != nil {
		t.Skipf("not a git work tree (%v); the ignore rules can only be "+
			"checked by asking git", err)
	}
}

// TestRuntimeStateIsGitignored.
//
// record_plan writes .agents/plans/plan-N.md and the stores land beside
// it. The first self-development run would otherwise dirty the tree it is
// trying to open a clean PR against — and worse, a run that stages `-A`
// would commit its own session history.
func TestRuntimeStateIsGitignored(t *testing.T) {
	t.Parallel()
	requireGitWorkTree(t)

	// Representative paths, not patterns: what matters is that the thing a
	// run actually writes is ignored. None of these need to exist.
	for _, rel := range []string{
		".agents/sessions/session.db",
		".agents/logs/agent.log",
		".agents/plans/plan-1.md",
		".agents/env.yaml",
		".agents/env.json", // agentenv accepts either spelling
		".agents/core-agent.db",
		".agents/core-agent.db-wal",
	} {
		if !gitIgnored(t, rel) {
			t.Errorf("%s is NOT ignored; a self-development run writes it, and "+
				"an un-ignored one dirties the checkout it is opening a PR "+
				"against — or gets committed by a `git add -A`", rel)
		}
	}
}

// TestRecipeIsCommittedNotIgnored is the inverse, and it is the one that
// would bite silently: an ignore rule that is slightly too broad makes the
// whole recipe invisible to git. Everything above keeps passing locally —
// the files are on disk — and CI clones a tree with no recipe in it at all.
func TestRecipeIsCommittedNotIgnored(t *testing.T) {
	t.Parallel()
	requireGitWorkTree(t)

	// Walked rather than listed: a sixth skill added later has to be
	// covered by this without anyone remembering to add it here.
	//
	// The walk sees whatever a local run left behind, so the declared
	// runtime-state paths are pruned — those are ignored on purpose and
	// TestRuntimeStateIsGitignored asserts the other direction on them.
	// Between the two, every path under .agents/ is claimed by exactly one
	// test, which is the property that makes a too-broad rule impossible to
	// hide in the gap.
	runtimeDir := map[string]bool{"sessions": true, "logs": true, "plans": true}
	isRuntimeFile := func(name string) bool {
		return name == "env.yaml" || name == "env.json" || strings.Contains(name, ".db")
	}
	var checked int
	err := filepath.WalkDir(agentsDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != agentsDir && runtimeDir[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		if isRuntimeFile(d.Name()) {
			return nil
		}
		rel, err := filepath.Rel(repoRoot, path)
		if err != nil {
			return err
		}
		checked++
		if gitIgnored(t, rel) {
			t.Errorf("%s IS ignored; the recipe is committed (D2) and only its "+
				"runtime state is ignored. A too-broad rule leaves every other "+
				"test in this file green — the files are on disk — while CI "+
				"clones a tree with no recipe in it", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", agentsDir, err)
	}
	// The walk only sees what is on disk, so it is silent about a recipe
	// that has gone missing entirely. config.json + two AGENTS.md + five
	// SKILL.md.
	if want := 8; checked != want {
		t.Errorf("walked %d recipe files, want %d; update this count "+
			"deliberately when the recipe gains or loses a file", checked, want)
	}
}

// TestConfigIsValidJSONWithNoUnknownFields.
//
// config.Load tolerates a key it does not know, which is right for
// forward compatibility and wrong for a recipe: a typo'd
// "max_turn_cost_usdd" loads clean, enforces nothing, and reads correct.
func TestConfigIsValidJSONWithNoUnknownFields(t *testing.T) {
	t.Parallel()

	f, err := os.Open(filepath.Join(agentsDir, "config.json"))
	if err != nil {
		t.Fatalf("open config.json: %v", err)
	}
	defer f.Close() //nolint:errcheck // read-only

	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	var cfg config.Config
	if err := dec.Decode(&cfg); err != nil {
		t.Errorf("config.json has a key the Config struct does not define: %v\n"+
			"A misspelled budget or safety key loads silently and enforces "+
			"nothing, which reads exactly like a recipe that is working.", err)
	}
}

func subagentByName(t *testing.T, cfg *config.Config, name string) config.SubagentSpec {
	t.Helper()
	for _, s := range cfg.Subagents {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("recipe declares no %q subagent", name)
	return config.SubagentSpec{}
}

// modelName reads a possibly-absent model block. An unset *ModelConfig
// means "inherit the parent's model", which is a legal config and a
// finding, not a crash.
func modelName(m *config.ModelConfig) string {
	if m == nil {
		return ""
	}
	return m.Name
}
