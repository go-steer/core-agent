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
	"errors"
	"fmt"
	"strings"
	"sync"

	adkmodel "google.golang.org/adk/model"
	adktool "google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"

	"github.com/go-steer/core-agent/v2/pkg/agent"
	"github.com/go-steer/core-agent/v2/pkg/eventlog"
)

// The checker is bouncer's independent CI verifier: it submits the
// candidate manifest, watches it run, and returns a structured
// pass/fail. Upstream it is an ADK Agent with
// `output_schema=CheckerResult`, invoked by the generator as
// `subprocess.Popen(["adk", "run", "checker", ...])` whose stdout the
// generator greps for the literal string "success: True".
//
// core-agent has no output_schema, so the contract inverts: instead
// of constraining the final message, the terminal ACTION is a tool.
// `report_verdict(success, details)` is an ordinary Go function whose
// signature is the schema — the model cannot report an outcome in a
// shape the compiler didn't approve, and the generator receives a
// typed `verdict` rather than parsing a subprocess's stdout.
//
// The fail-closed behaviour is preserved: a checker that stops
// without calling report_verdict is a FAILURE, exactly as a missing
// "success: True" in the upstream stdout grep is.

// verdict is the checker's structured answer — the Go equivalent of
// upstream's `class CheckerResult(BaseModel)`.
type verdict struct {
	Success bool   `json:"success"`
	Details string `json:"details"`
}

// verdictSink captures the single verdict a checker run produces.
// The tool handler and the run loop are on the same goroutine today,
// but agent.Run may dispatch tools in parallel, so the mutex is not
// decoration.
type verdictSink struct {
	mu  sync.Mutex
	set bool
	v   verdict
}

func (s *verdictSink) record(v verdict) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// First verdict wins: the prompt says report once, and a second
	// call must not be able to flip a FAILURE into a SUCCESS.
	if s.set {
		return
	}
	s.set = true
	s.v = v
}

func (s *verdictSink) result() (verdict, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.v, s.set
}

type reportVerdictArgs struct {
	Success bool   `json:"success" jsonschema_description:"true only if the preflight genuinely ran on TPU hardware and exited cleanly"`
	Details string `json:"details" jsonschema_description:"evidence for the verdict: the log lines, events or errors that decided it"`
}

type reportVerdictResult struct {
	Recorded bool `json:"recorded"`
}

// reportVerdictTool is the structured-output replacement. Its
// argument struct is the schema the model must satisfy.
func reportVerdictTool(sink *verdictSink) (adktool.Tool, error) {
	return functiontool.New(
		functiontool.Config{
			Name: "report_verdict",
			Description: "Report the final verdict on the candidate preflight and end your run. " +
				"Call this exactly once, after cleaning up the workload. Set success=true only " +
				"if the logs prove the framework initialized the TPU and completed its miniature " +
				"workload; put the deciding evidence in details so the generator can act on it.",
		},
		func(_ adktool.Context, in reportVerdictArgs) (reportVerdictResult, error) {
			sink.record(verdict(in))
			return reportVerdictResult{Recorded: true}, nil
		},
	)
}

type saveLibraryArgs struct {
	Name        string   `json:"name" jsonschema_description:"short descriptive name for the preflight, e.g. maxtext-v5e-256-4x8x8"`
	Features    []string `json:"features,omitempty" jsonschema_description:"hardware/framework features this preflight exercises"`
	TargetLabel string   `json:"target_label,omitempty" jsonschema_description:"the accelerator/topology label this preflight targets"`
	Metadata    string   `json:"metadata,omitempty" jsonschema_description:"free-form notes worth keeping with the entry"`
}

type saveLibraryResult struct {
	Path string `json:"path"`
}

// saveLibraryTool persists the verified candidate. It deliberately
// takes no manifest argument: the manifest is read from the store, so
// the artifact the checker actually ran is the artifact the library
// keeps. Letting the model re-send the YAML here is how a library
// entry silently diverges from what was tested.
func saveLibraryTool(st *store, sourceJob string) (adktool.Tool, error) {
	return functiontool.New(
		functiontool.Config{
			Name: "save_derived_preflight_to_library",
			Description: "Save the candidate manifest you just verified into the durable preflight " +
				"library. Call this only after the run genuinely passed, and before report_verdict.",
		},
		func(_ adktool.Context, in saveLibraryArgs) (saveLibraryResult, error) {
			manifest, err := st.readCandidate()
			if err != nil {
				return saveLibraryResult{}, err
			}
			path, err := st.saveLibraryEntry(libraryEntry{
				Name:        in.Name,
				Features:    in.Features,
				TargetLabel: in.TargetLabel,
				Metadata:    in.Metadata,
				SourceJob:   sourceJob,
			}, manifest)
			if err != nil {
				return saveLibraryResult{}, err
			}
			return saveLibraryResult{Path: path}, nil
		},
	)
}

type submitResult struct {
	Applied   bool   `json:"applied"`
	Command   string `json:"command"`
	ExitCode  int    `json:"exit_code"`
	Stdout    string `json:"stdout,omitempty"`
	Stderr    string `json:"stderr,omitempty"`
	Namespace string `json:"namespace"`
}

// submitCandidateTool is bouncer's `submit_candidate_preflight`: the
// one privileged action the checker takes on the cluster.
//
// It is a distinct tool rather than "just run kubectl apply yourself"
// for the same reason upstream keeps it separate — the checker must
// submit exactly the manifest the generator saved, into exactly the
// test namespace. Leaving the apply to a free-form shell command
// invites the model to inline a manifest of its own, and the library
// would then hold something that was never run.
func submitCandidateTool(st *store, s *sandbox, namespace string) (adktool.Tool, error) {
	return functiontool.New(
		functiontool.Config{
			Name: "submit_candidate_preflight",
			Description: "Apply the saved candidate manifest to the test cluster. Takes no arguments: " +
				"it always submits the manifest the generator saved, into the test namespace. " +
				"A non-zero exit code means the cluster rejected it — do not fix it, report it.",
		},
		func(ctx adktool.Context, _ struct{}) (submitResult, error) {
			return submitCandidateFunc(st, s, namespace)(stdContext(ctx))
		},
	)
}

func submitCandidateFunc(st *store, s *sandbox, namespace string) func(context.Context) (submitResult, error) {
	return func(ctx context.Context) (submitResult, error) {
		if strings.TrimSpace(namespace) == "" {
			return submitResult{}, errors.New("submit_candidate_preflight: no test namespace configured")
		}
		// Read it first so a missing or empty candidate is a clear tool
		// error rather than an opaque kubectl message.
		manifest, err := st.readCandidate()
		if err != nil {
			return submitResult{}, fmt.Errorf("submit_candidate_preflight: no candidate manifest to submit: %w", err)
		}
		if strings.TrimSpace(manifest) == "" {
			return submitResult{}, errors.New("submit_candidate_preflight: the candidate manifest is empty")
		}
		cmd := fmt.Sprintf("kubectl apply -n %s -f %s",
			shellQuote(namespace), shellQuote(s.workspacePath(candidateFile)))
		out := s.run(ctx, cmd)
		return submitResult{
			Applied:   out.ExitCode == 0,
			Command:   cmd,
			ExitCode:  out.ExitCode,
			Stdout:    out.Stdout,
			Stderr:    out.Stderr,
			Namespace: namespace,
		}, nil
	}
}

// shellQuote makes a value safe to interpolate into the `bash -c`
// string the sandbox runs. Namespace and path are ours, not the
// model's, but this command is assembled by string concatenation and
// should not become the exception that proves the rule.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

type readCandidateResult struct {
	Manifest string `json:"manifest"`
}

func readCandidateTool(st *store) (adktool.Tool, error) {
	return functiontool.New(
		functiontool.Config{
			Name:        "read_candidate_manifest",
			Description: "Read the candidate preflight manifest the generator saved for you.",
		},
		func(_ adktool.Context, _ struct{}) (readCandidateResult, error) {
			manifest, err := st.readCandidate()
			if err != nil {
				return readCandidateResult{}, err
			}
			return readCandidateResult{Manifest: manifest}, nil
		},
	)
}

// checkerConfig is everything a checker run needs. It is built once
// in main.go and reused for every save_if_validated call.
type checkerConfig struct {
	model       adkmodel.LLM
	instruction string
	store       *store
	sandbox     *sandbox
	// log and sessionID are optional; when set, the checker's events
	// land in the same durable event log as the generator's, under a
	// derived session id, so one audit query returns both halves of
	// the derivation.
	log       *eventlog.Handle
	sessionID string
	// namespace is the test namespace submit_candidate_preflight
	// applies into; the prompt's "run all tests strictly within the
	// required test namespace" constraint is enforced here, not asked
	// for politely.
	namespace string
	// sourceJob is recorded on library entries for provenance.
	sourceJob string
}

// checkRequest is the generator's hand-off: the trigger text the
// checker prompt expects.
type checkRequest struct {
	Name        string
	Features    []string
	TargetLabel string
	Metadata    string
}

func (r checkRequest) prompt() string {
	var b strings.Builder
	b.WriteString("The generator has saved a candidate manifest for you to verify.\n")
	fmt.Fprintf(&b, "name: %s\n", r.Name)
	if len(r.Features) > 0 {
		fmt.Fprintf(&b, "features: %s\n", strings.Join(r.Features, ", "))
	}
	if r.TargetLabel != "" {
		fmt.Fprintf(&b, "target_label: %s\n", r.TargetLabel)
	}
	if r.Metadata != "" {
		fmt.Fprintf(&b, "metadata: %s\n", r.Metadata)
	}
	b.WriteString("Submit it with submit_candidate_preflight, monitor it, clean up, " +
		"then report_verdict.\n")
	return b.String()
}

func buildChecker(cfg checkerConfig, sink *verdictSink) (*agent.Agent, error) {
	shell, err := sandboxTool(cfg.sandbox)
	if err != nil {
		return nil, err
	}
	read, err := readCandidateTool(cfg.store)
	if err != nil {
		return nil, err
	}
	submit, err := submitCandidateTool(cfg.store, cfg.sandbox, cfg.namespace)
	if err != nil {
		return nil, err
	}
	wait, err := waitTool()
	if err != nil {
		return nil, err
	}
	save, err := saveLibraryTool(cfg.store, cfg.sourceJob)
	if err != nil {
		return nil, err
	}
	report, err := reportVerdictTool(sink)
	if err != nil {
		return nil, err
	}

	opts := []agent.Option{
		agent.WithName("checker"),
		agent.WithDescription("independently verifies a candidate preflight against the test cluster"),
		agent.WithMode(agent.ModeAutonomous),
		agent.WithInstruction(cfg.instruction),
		agent.WithTools([]adktool.Tool{shell, read, submit, wait, save, report}),
	}
	if cfg.log != nil {
		opts = append(opts,
			agent.WithEventLog(cfg.log),
			agent.WithSession("bouncer", cfg.sessionID+":checker"))
	}
	return agent.New(cfg.model, opts...)
}

// runChecker drives one checker run to completion and returns its
// verdict. This is the in-process replacement for
// `subprocess.Popen(["adk", "run", "checker", ...])` plus the
// "success: True" stdout grep.
func runChecker(ctx context.Context, cfg checkerConfig, req checkRequest) (verdict, error) {
	sink := &verdictSink{}
	a, err := buildChecker(cfg, sink)
	if err != nil {
		return verdict{}, fmt.Errorf("checker: build: %w", err)
	}

	var finalText strings.Builder
	for ev, err := range a.Run(ctx, req.prompt()) {
		if err != nil {
			// A transport failure mid-verification is a genuine
			// error, not a FAILURE verdict: the generator should not
			// be told its manifest is broken because the API 500'd.
			return verdict{}, fmt.Errorf("checker: run: %w", err)
		}
		if ev == nil || ev.Content == nil || ev.Partial {
			continue
		}
		for _, p := range ev.Content.Parts {
			if p != nil && p.Text != "" {
				finalText.WriteString(p.Text)
			}
		}
	}

	if v, ok := sink.result(); ok {
		return v, nil
	}
	// Fail closed, exactly like the upstream stdout grep: no verdict
	// reported means not verified.
	return verdict{
		Success: false,
		Details: "the checker ended its run without calling report_verdict, so the preflight is NOT verified. " +
			"Last message: " + strings.TrimSpace(truncate(finalText.String(), 2000)),
	}, nil
}
