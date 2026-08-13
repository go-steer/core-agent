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
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	adkmodel "google.golang.org/adk/model"

	"github.com/go-steer/core-agent/v2/pkg/agent"
	"github.com/go-steer/core-agent/v2/pkg/models/mock"
)

// TestDerivationEndToEnd runs the whole port hermetically: two
// scripted transcripts stand in for the models, a fake kubectl stands
// in for the cluster, and the assertions are on the artifacts a real
// derivation would leave behind.
//
// This is the test that proves the port's central claim — that
// generator → save_if_validated → checker → report_verdict →
// library works in-process, with no subprocess and no stdout
// grepping.
func TestDerivationEndToEnd(t *testing.T) {
	state := t.TempDir()
	withFakeKubectl(t)

	err := run([]string{
		"--source", filepath.Join("testdata", "prod-jobset.yaml"),
		"--generator-script", filepath.Join("testdata", "generator.jsonl"),
		"--checker-script", filepath.Join("testdata", "checker.jsonl"),
		"--sandbox", sandboxModeNone,
		"--state-dir", state,
		"--session-id", "e2e",
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	manifestPath := filepath.Join(state, "library", "maxtext-v5e-256-16x16.yaml")
	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("the verified preflight was not saved to the library: %v", err)
	}
	for _, want := range []string{
		"kind: Job", // JobSet collapsed to a Job
		"cloud.google.com/gke-tpu-topology: 16x16", // topology preserved
		"parallelism: 64",                          // per-slice gang preserved
		"namespace: test-preflight",                // policy namespace applied
		"steps=1",                                  // miniaturised
		"emptyDir",                                 // GCS corpus replaced, not deleted
	} {
		if !strings.Contains(string(manifest), want) {
			t.Errorf("saved preflight is missing %q", want)
		}
	}
	// The generator's private note to the checker must never reach a
	// stored (or submittable) manifest.
	if strings.Contains(string(manifest), "checker-instruction") {
		t.Error("checker-instruction line survived into the library entry")
	}

	metaBody, err := os.ReadFile(filepath.Join(state, "library", "maxtext-v5e-256-16x16.json"))
	if err != nil {
		t.Fatalf("read library metadata: %v", err)
	}
	var meta libraryEntry
	if err := json.Unmarshal(metaBody, &meta); err != nil {
		t.Fatalf("parse library metadata: %v", err)
	}
	if meta.SourceJob != "prod-jobset.yaml" {
		t.Errorf("metadata source_job = %q, want the production manifest it came from", meta.SourceJob)
	}
	if len(meta.Features) == 0 {
		t.Error("metadata carries no features")
	}

	// Scratch state: the source and the candidate the checker ran.
	for _, name := range []string{sourceFile, candidateFile, experienceFile} {
		if _, err := os.Stat(filepath.Join(state, "sessions", "e2e", name)); err != nil {
			t.Errorf("session scratch is missing %s: %v", name, err)
		}
	}
	// Both agents' events land in one durable log.
	if info, err := os.Stat(filepath.Join(state, "eventlog.db")); err != nil || info.Size() == 0 {
		t.Errorf("event log was not written: %v", err)
	}
}

// TestCheckerFailsClosedWithoutVerdict pins the fail-closed contract
// that replaces upstream's `"success: True" in stdout` grep: a
// checker that wanders off without calling report_verdict has NOT
// verified anything.
func TestCheckerFailsClosedWithoutVerdict(t *testing.T) {
	cfg := scriptedCheckerConfig(t,
		`{"responses":[{"Content":{"parts":[{"text":"Looks fine to me, shipping it."}],"role":"model"},"TurnComplete":true,"FinishReason":"STOP"}]}`)

	v, err := runChecker(context.Background(), cfg, checkRequest{Name: "p"})
	if err != nil {
		t.Fatalf("runChecker: %v", err)
	}
	if v.Success {
		t.Fatal("a checker that never called report_verdict must not report success")
	}
	if !strings.Contains(v.Details, "report_verdict") {
		t.Errorf("details should explain the missing verdict, got %q", v.Details)
	}
	if !strings.Contains(v.Details, "shipping it") {
		t.Errorf("details should quote the checker's last message, got %q", v.Details)
	}
}

// TestCheckerReturnsTypedVerdict is the structured-output
// replacement working: the tool call, not the final message, carries
// the outcome.
func TestCheckerReturnsTypedVerdict(t *testing.T) {
	cfg := scriptedCheckerConfig(t,
		`{"responses":[{"Content":{"parts":[{"functionCall":{"name":"report_verdict","args":{"success":false,"details":"pod Pending: didn't match Pod's node affinity/selector"}}}],"role":"model"},"TurnComplete":true,"FinishReason":"STOP"}]}`,
		`{"responses":[{"Content":{"parts":[{"text":"Reported."}],"role":"model"},"TurnComplete":true,"FinishReason":"STOP"}]}`)

	v, err := runChecker(context.Background(), cfg, checkRequest{
		Name: "p", Features: []string{"jax"}, TargetLabel: "v5e", Metadata: "note",
	})
	if err != nil {
		t.Fatalf("runChecker: %v", err)
	}
	if v.Success {
		t.Error("verdict.Success should be false")
	}
	if !strings.Contains(v.Details, "node affinity") {
		t.Errorf("details were not carried back verbatim: %q", v.Details)
	}
}

// TestSubmitCandidateAppliesWhatWasSaved pins the checker's one
// privileged action: it submits the manifest on disk, into the
// configured test namespace, and nothing else. The model supplies no
// arguments, so it cannot smuggle a different manifest or namespace
// past the operator's policy.
func TestSubmitCandidateAppliesWhatWasSaved(t *testing.T) {
	withFakeKubectl(t)
	st := newTestStore(t)
	box := newSandbox(sandboxModeNone, st.sessionDir)
	submit := submitCandidateFunc(st, box, "test-preflight")

	if _, err := submit(context.Background()); err == nil {
		t.Fatal("submitting with no candidate on disk must be an error")
	}
	if err := st.writeCandidate("   \n"); err != nil {
		t.Fatal(err)
	}
	if _, err := submit(context.Background()); err == nil {
		t.Fatal("submitting an empty candidate must be an error")
	}

	if err := st.writeCandidate("kind: Job\n"); err != nil {
		t.Fatal(err)
	}
	got, err := submit(context.Background())
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if !got.Applied || got.ExitCode != 0 {
		t.Errorf("submit = %+v, want a clean apply", got)
	}
	if got.Namespace != "test-preflight" || !strings.Contains(got.Command, "-n 'test-preflight'") {
		t.Errorf("submit did not pin the test namespace: %+v", got)
	}
	if !strings.Contains(got.Command, candidateFile) {
		t.Errorf("submit did not apply the saved candidate: %q", got.Command)
	}

	// No namespace configured is a refusal, not an apply into default.
	if _, err := submitCandidateFunc(st, box, "  ")(context.Background()); err == nil {
		t.Error("an unset namespace must be refused")
	}
}

// TestSaveIfValidatedSerializesHandoffs is the concurrency test: the
// candidate manifest is a single file, so two hand-offs dispatched in
// one turn must not interleave. Without the gate the second write
// lands while the first checker is mid-verification, and the checker
// verifies — and saves to the library — a manifest nobody asked it to
// check. Run with -race.
func TestSaveIfValidatedSerializesHandoffs(t *testing.T) {
	st := newTestStore(t)
	var mismatches int64
	cfg := generatorConfig{
		store:   st,
		gate:    &sync.Mutex{},
		history: &handoffLog{},
		check: func(_ context.Context, req checkRequest) (verdict, error) {
			// Read the candidate the way the real checker does, with a
			// gap either side to widen the race window.
			time.Sleep(2 * time.Millisecond)
			got, err := st.readCandidate()
			if err != nil {
				return verdict{}, err
			}
			time.Sleep(2 * time.Millisecond)
			if !strings.Contains(got, req.Name) {
				atomic.AddInt64(&mismatches, 1)
			}
			return verdict{Success: true, Details: "ok"}, nil
		},
	}
	save := saveIfValidatedFunc(cfg)

	var wg sync.WaitGroup
	for i := range 8 {
		name := fmt.Sprintf("preflight-%d", i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := save(context.Background(), saveIfValidatedArgs{
				Name:     name,
				Manifest: "kind: Job\nmetadata:\n  name: " + name + "\n",
			}); err != nil {
				t.Errorf("save_if_validated(%s): %v", name, err)
			}
		}()
	}
	wg.Wait()

	if n := atomic.LoadInt64(&mismatches); n != 0 {
		t.Errorf("%d checker runs saw a manifest from a different hand-off", n)
	}
	if got := len(cfg.history.snapshot()); got != 8 {
		t.Errorf("history recorded %d hand-offs, want 8", got)
	}
}

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"test-preflight":  "'test-preflight'",
		"/workspace/a.ya": "'/workspace/a.ya'",
		"it's":            `'it'\''s'`,
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestHandoffLogRecordsEveryAttempt covers get_conversation_history:
// the generator's recall of what it already tried must survive
// context compaction across an autonomous run.
func TestHandoffLogRecordsEveryAttempt(t *testing.T) {
	l := &handoffLog{}
	if got := l.snapshot(); len(got) != 0 {
		t.Errorf("a fresh log should be empty, got %v", got)
	}
	l.record(handoffEntry{Name: "a", Success: false, Details: strings.Repeat("x", 5000)})
	l.record(handoffEntry{Name: "b", Success: true, Details: "verified"})

	got := l.snapshot()
	if len(got) != 2 {
		t.Fatalf("snapshot has %d entries, want 2", len(got))
	}
	if got[0].Attempt != 1 || got[1].Attempt != 2 {
		t.Errorf("attempts are not numbered in order: %+v", got)
	}
	if got[0].Name != "a" || !got[1].Success {
		t.Errorf("entries were not recorded verbatim: %+v", got)
	}
	if len(got[0].Details) > 1100 {
		t.Errorf("a 5000-byte failure detail was not truncated (%d bytes)", len(got[0].Details))
	}
	// The snapshot must be a copy: handing the model a live slice
	// would race the next hand-off.
	got[0].Name = "mutated"
	if l.snapshot()[0].Name != "a" {
		t.Error("snapshot returned the live backing array")
	}
}

func TestVerdictSinkFirstWins(t *testing.T) {
	s := &verdictSink{}
	if _, ok := s.result(); ok {
		t.Error("an untouched sink must report no verdict")
	}
	s.record(verdict{Success: false, Details: "failed"})
	s.record(verdict{Success: true, Details: "actually fine"})
	got, ok := s.result()
	if !ok || got.Success || got.Details != "failed" {
		t.Errorf("a second report_verdict must not overwrite the first: %+v", got)
	}
}

func TestCheckRequestPrompt(t *testing.T) {
	p := checkRequest{Name: "n", Features: []string{"a", "b"}, TargetLabel: "t", Metadata: "m"}.prompt()
	for _, want := range []string{"name: n", "features: a, b", "target_label: t", "metadata: m", "report_verdict"} {
		if !strings.Contains(p, want) {
			t.Errorf("hand-off prompt is missing %q:\n%s", want, p)
		}
	}
	bare := checkRequest{Name: "n"}.prompt()
	if strings.Contains(bare, "features:") || strings.Contains(bare, "metadata:") {
		t.Errorf("empty fields should be omitted:\n%s", bare)
	}
}

func TestRenderPromptSubstitutesPlaceholders(t *testing.T) {
	got, err := renderPrompt("prompts/generator_prompt.md", "test-preflight", "preflight-sa", "        - No hostNetwork.")
	if err != nil {
		t.Fatalf("renderPrompt: %v", err)
	}
	if strings.Contains(got, "{{") {
		t.Error("an unsubstituted placeholder survived into the system instruction")
	}
	for _, want := range []string{"test-preflight", "preflight-sa", "No hostNetwork.", "save_if_validated"} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered prompt is missing %q", want)
		}
	}
}

// TestPromptsNameOnlyToolsWeRegister guards the port's honesty: the
// upstream prompts instruct the model to call specific tools by name,
// so every tool they name must actually exist here. A prompt that
// tells the model to call a tool the port never wired is how a live
// run dead-ends.
func TestPromptsNameOnlyToolsWeRegister(t *testing.T) {
	registered := map[string]bool{
		// Injected by autonomous.Run rather than by buildGenerator, so
		// it is not in either agent's static tool set.
		"report_done": true,
	}
	for _, a := range buildBothAgents(t) {
		for _, tl := range a.Tools() {
			registered[tl.Name()] = true
		}
	}

	// Every backticked snake_case token in a prompt is a tool name the
	// model will be told to call. Deriving the expectation from the
	// prompt text rather than a hand-kept list is the point: copying a
	// newer upstream prompt in must fail here, not in production.
	toolLike := regexp.MustCompile("`([a-z][a-z0-9]*(?:_[a-z0-9]+)+)`")
	for _, name := range []string{"prompts/generator_prompt.md", "prompts/checker_prompt.md"} {
		body, err := prompts.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		named := map[string]bool{}
		for _, m := range toolLike.FindAllStringSubmatch(string(body), -1) {
			named[m[1]] = true
		}
		if len(named) == 0 {
			t.Errorf("%s names no tools at all; the extraction regexp has stopped working", name)
		}
		for tool := range named {
			if !registered[tool] {
				t.Errorf("%s tells the model to call %q, which this port does not register", name, tool)
			}
		}
	}
	// The two load-bearing hand-off tools must be named by the
	// prompts, or the agents never talk to each other.
	gen, _ := prompts.ReadFile("prompts/generator_prompt.md")
	if !strings.Contains(string(gen), "save_if_validated") {
		t.Error("generator prompt no longer names save_if_validated")
	}
	chk, _ := prompts.ReadFile("prompts/checker_prompt.md")
	if !strings.Contains(string(chk), "save_derived_preflight_to_library") {
		t.Error("checker prompt no longer names save_derived_preflight_to_library")
	}
}

func TestParseFlagsValidation(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		want string
	}{
		{"missing source", nil, "--source is required"},
		{"half a tape", []string{"--source", "s.yaml", "--generator-script", "g.jsonl"}, "must be given together"},
		{"bad sandbox", []string{"--source", "s.yaml", "--sandbox", "docker"}, "--sandbox must be"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseFlags(tc.argv)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("parseFlags(%v) error = %v, want one containing %q", tc.argv, err, tc.want)
			}
		})
	}
}

func TestParseFlagsDefaults(t *testing.T) {
	o, err := parseFlags([]string{"--source", "/tmp/some/prod-jobset.yaml"})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if o.sessionID != "prod-jobset" {
		t.Errorf("sessionID = %q, want it derived from the source file", o.sessionID)
	}
	if o.libraryDir != filepath.Join(o.stateDir, "library") {
		t.Errorf("libraryDir = %q, want it under the state dir", o.libraryDir)
	}
	if strings.HasPrefix(o.stateDir, os.Getenv("HOME")) && os.Getenv("HOME") != "" {
		t.Errorf("state dir %q defaults into $HOME; throwaway state belongs under the temp dir", o.stateDir)
	}
	if o.sandboxMode != sandboxModeBwrap {
		t.Errorf("sandbox default = %q, want the contained mode", o.sandboxMode)
	}
	if o.model != defaultModel {
		t.Errorf("model default = %q, want bouncer's %q", o.model, defaultModel)
	}
}

func TestObjectiveNamesThePolicy(t *testing.T) {
	got := objective(options{namespace: "ns-1", serviceAccount: "sa-1"})
	for _, want := range []string{"ns-1", "sa-1", "read_source_manifest", "save_if_validated"} {
		if !strings.Contains(got, want) {
			t.Errorf("objective is missing %q: %s", want, got)
		}
	}
}

func TestReadPolicy(t *testing.T) {
	if got, err := readPolicy(""); err != nil || got != "" {
		t.Errorf("no policy file should yield an empty string, got %q %v", got, err)
	}
	path := filepath.Join(t.TempDir(), "policy.md")
	if err := os.WriteFile(path, []byte("        - No hostNetwork.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readPolicy(path)
	if err != nil || got != "        - No hostNetwork." {
		t.Errorf("readPolicy = %q, %v", got, err)
	}
	if _, err := readPolicy(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("a missing policy file must be an error, not silence")
	}
}

// scriptedCheckerConfig builds a checker wired to an inline
// transcript and a throwaway store holding a candidate manifest.
func scriptedCheckerConfig(t *testing.T, lines ...string) checkerConfig {
	t.Helper()
	st := newTestStore(t)
	if err := st.writeCandidate("kind: Job\n"); err != nil {
		t.Fatalf("writeCandidate: %v", err)
	}
	path := filepath.Join(t.TempDir(), "checker.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	provider, err := mock.NewScripted(path, false)
	if err != nil {
		t.Fatalf("mock.NewScripted: %v", err)
	}
	var m adkmodel.LLM
	m, err = provider.Model(context.Background(), "")
	if err != nil {
		t.Fatalf("provider.Model: %v", err)
	}
	return checkerConfig{
		model:       m,
		instruction: "verify the candidate and report_verdict",
		store:       st,
		sandbox:     newSandbox(sandboxModeNone, st.sessionDir),
		namespace:   "test-preflight",
	}
}

// buildBothAgents constructs the real generator and checker with a
// throwaway model, so tests can inspect the tool set the port actually
// registers rather than a hand-kept list that drifts from it.
func buildBothAgents(t *testing.T) []*agent.Agent {
	t.Helper()
	st := newTestStore(t)
	box := newSandbox(sandboxModeNone, st.sessionDir)
	m := &fakeLLM{fn: func(int, context.Context, func(*adkmodel.LLMResponse, error) bool) {}}

	gen, err := buildGenerator(generatorConfig{
		model:       m,
		instruction: "derive a preflight",
		store:       st,
		sandbox:     box,
		objective:   "derive a preflight",
		check:       func(context.Context, checkRequest) (verdict, error) { return verdict{}, nil },
	}, nil)
	if err != nil {
		t.Fatalf("buildGenerator: %v", err)
	}
	chk, err := buildChecker(checkerConfig{
		model:       m,
		instruction: "verify a preflight",
		store:       st,
		sandbox:     box,
		namespace:   "test-preflight",
	}, &verdictSink{})
	if err != nil {
		t.Fatalf("buildChecker: %v", err)
	}
	return []*agent.Agent{gen, chk}
}

// withFakeKubectl puts testdata/bin first on PATH so the scripted
// kubectl calls resolve to the fake instead of a real cluster client.
func withFakeKubectl(t *testing.T) {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Setenv("PATH", filepath.Join(wd, "testdata", "bin")+string(os.PathListSeparator)+os.Getenv("PATH"))
}
