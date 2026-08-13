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
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	adkmodel "google.golang.org/adk/model"
	adktool "google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"

	"github.com/go-steer/core-agent/v2/pkg/agent"
	"github.com/go-steer/core-agent/v2/pkg/eventlog"
)

// The generator is bouncer's parent agent: it reads a completed
// production workload, derives a single-slice smoke test, debugs it
// against the test namespace itself, and only then hands the
// candidate to the checker.
//
// The one structural change from upstream is save_if_validated. There
// it shells out — `subprocess.Popen(["adk", "run", "checker", ...])`
// — and greps the child's stdout for "success: True". Here it calls
// runChecker in-process and gets a typed verdict back. Same
// two-agent contract, minus the subprocess, the stdout parsing, and
// the second interpreter.

// generatorConfig is everything the parent agent needs.
type generatorConfig struct {
	model       adkmodel.LLM
	instruction string
	store       *store
	sandbox     *sandbox
	// objective is the original derivation request, returned verbatim
	// by get_original_objective when the model loses the thread.
	objective string
	// check runs the checker. Injected rather than constructed here
	// so the hand-off can be exercised without a second model.
	check func(context.Context, checkRequest) (verdict, error)
	// notify prints one-line progress to the operator; may be nil.
	notify func(string)
	// log and sessionID wire the durable event log; both optional.
	log       *eventlog.Handle
	sessionID string
	// history backs get_conversation_history; buildGenerator creates
	// one when it is nil.
	history *handoffLog
	// gate serializes hand-offs. The candidate manifest is a single
	// file on disk, so two save_if_validated calls dispatched in the
	// same turn would race: the second write can land while the first
	// checker is mid-verification, and the checker would then submit —
	// and save to the library — a manifest nobody asked it to check.
	// buildGenerator creates one when it is nil.
	gate *sync.Mutex
}

func (c generatorConfig) say(format string, args ...any) {
	if c.notify != nil {
		c.notify(fmt.Sprintf(format, args...))
	}
}

// handoffEntry is one generator→checker round trip.
type handoffEntry struct {
	Attempt       int    `json:"attempt"`
	Name          string `json:"name"`
	ManifestBytes int    `json:"manifest_bytes"`
	Success       bool   `json:"success"`
	Details       string `json:"details"`
}

// handoffLog is the generator's memory of what it has already tried.
//
// Upstream's `get_conversation_history` re-reads the ADK session so
// the model can recover "what I generated and what the Checker said"
// after the CLI truncates its context. core-agent keeps the live
// transcript for the model, but an autonomous run can span many turns
// and be compacted, so the same recovery hatch is worth having — and
// this version is strictly the part the prompt actually asks for,
// rather than a replay of every token.
type handoffLog struct {
	mu      sync.Mutex
	entries []handoffEntry
}

func (l *handoffLog) record(e handoffEntry) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e.Attempt = len(l.entries) + 1
	e.Details = truncate(e.Details, 1000)
	l.entries = append(l.entries, e)
}

func (l *handoffLog) snapshot() []handoffEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]handoffEntry, len(l.entries))
	copy(out, l.entries)
	return out
}

type historyResult struct {
	Handoffs []handoffEntry `json:"handoffs"`
}

func historyTool(l *handoffLog) (adktool.Tool, error) {
	return functiontool.New(
		functiontool.Config{
			Name: "get_conversation_history",
			Description: "Recall every candidate you have already handed to the Checker and the verdict " +
				"it returned, oldest first. Use this if you have lost track of what you have tried.",
		},
		func(_ adktool.Context, _ struct{}) (historyResult, error) {
			return historyResult{Handoffs: l.snapshot()}, nil
		},
	)
}

type saveIfValidatedArgs struct {
	Name        string   `json:"name" jsonschema_description:"short descriptive name for the preflight, e.g. maxtext-v5e-256-4x8x8"`
	Manifest    string   `json:"manifest" jsonschema_description:"the complete candidate preflight YAML you have already tested yourself"`
	Features    []string `json:"features,omitempty" jsonschema_description:"hardware/framework features this preflight exercises"`
	TargetLabel string   `json:"target_label,omitempty" jsonschema_description:"the accelerator/topology label this preflight targets"`
	Metadata    string   `json:"metadata,omitempty" jsonschema_description:"free-form notes worth keeping with the entry"`
}

type saveIfValidatedResult struct {
	Success bool   `json:"success"`
	Details string `json:"details"`
}

// saveIfValidatedTool is the hand-off to the checker.
func saveIfValidatedTool(cfg generatorConfig) (adktool.Tool, error) {
	return functiontool.New(
		functiontool.Config{
			Name: "save_if_validated",
			Description: "Hand your finished candidate manifest to the independent Checker agent. " +
				"The Checker submits it, watches it run and decides. You have NOT succeeded until " +
				"this returns success=true; on failure, read details, fix the manifest and call again.",
		},
		func(ctx adktool.Context, in saveIfValidatedArgs) (saveIfValidatedResult, error) {
			return saveIfValidatedFunc(cfg)(stdContext(ctx), in)
		},
	)
}

func saveIfValidatedFunc(cfg generatorConfig) func(context.Context, saveIfValidatedArgs) (saveIfValidatedResult, error) {
	return func(ctx context.Context, in saveIfValidatedArgs) (saveIfValidatedResult, error) {
		if strings.TrimSpace(in.Manifest) == "" {
			return saveIfValidatedResult{}, fmt.Errorf("save_if_validated: manifest is required")
		}
		if strings.TrimSpace(in.Name) == "" {
			return saveIfValidatedResult{}, fmt.Errorf("save_if_validated: name is required")
		}
		// One candidate file, one checker at a time: hold the gate for
		// the whole write-then-verify, so a second hand-off dispatched
		// in the same turn cannot swap the manifest under a running
		// checker.
		if cfg.gate != nil {
			cfg.gate.Lock()
			defer cfg.gate.Unlock()
		}
		// Strip the generator's private notes to the checker before
		// anything can submit the YAML to an API server.
		manifest := stripCheckerInstructions(in.Manifest)
		if err := cfg.store.writeCandidate(manifest); err != nil {
			return saveIfValidatedResult{}, err
		}
		cfg.say("generator handed %q to the checker", in.Name)

		v, err := cfg.check(stdContext(ctx), checkRequest{
			Name:        in.Name,
			Features:    in.Features,
			TargetLabel: in.TargetLabel,
			Metadata:    in.Metadata,
		})
		if err != nil {
			// A broken checker run is a tool error the model should
			// see, not a verdict about the manifest.
			return saveIfValidatedResult{}, err
		}
		if cfg.history != nil {
			cfg.history.record(handoffEntry{
				Name:          in.Name,
				ManifestBytes: len(manifest),
				Success:       v.Success,
				Details:       v.Details,
			})
		}
		if v.Success {
			cfg.say("checker verdict: SUCCESS — %s", oneLine(v.Details))
			return saveIfValidatedResult{
				Success: true,
				Details: "SUCCESS: the Checker verified the preflight and saved it to the library. " + v.Details,
			}, nil
		}
		cfg.say("checker verdict: FAILURE — %s", oneLine(v.Details))
		return saveIfValidatedResult{
			Success: false,
			Details: "FAILURE: the Checker rejected the preflight. " + v.Details,
		}, nil
	}
}

type readSourceResult struct {
	Manifest string `json:"manifest"`
}

func readSourceTool(st *store) (adktool.Tool, error) {
	return functiontool.New(
		functiontool.Config{
			Name:        "read_source_manifest",
			Description: "Read the full production manifest you are deriving a preflight from.",
		},
		func(_ adktool.Context, _ struct{}) (readSourceResult, error) {
			manifest, err := st.readSource()
			if err != nil {
				return readSourceResult{}, err
			}
			return readSourceResult{Manifest: manifest}, nil
		},
	)
}

type objectiveResult struct {
	Objective string `json:"objective"`
}

func objectiveTool(objective string) (adktool.Tool, error) {
	return functiontool.New(
		functiontool.Config{
			Name:        "get_original_objective",
			Description: "Re-read the original task you were given, verbatim, if you have lost the thread.",
		},
		func(_ adktool.Context, _ struct{}) (objectiveResult, error) {
			return objectiveResult{Objective: objective}, nil
		},
	)
}

type experienceArgs struct {
	Topic string `json:"topic" jsonschema_description:"grouping key, e.g. jax-oom or gke-admission-webhook"`
	Note  string `json:"note" jsonschema_description:"the durable lesson: a constraint, a quirk, or how an error was resolved"`
}

type experienceResult struct {
	Recorded bool `json:"recorded"`
}

func experienceTool(st *store) (adktool.Tool, error) {
	return functiontool.New(
		functiontool.Config{
			Name: "append_experience_log",
			Description: "Record something worth remembering — a framework quirk, a topology constraint, " +
				"or how you resolved a specific error — so future runs can retrieve it.",
		},
		func(_ adktool.Context, in experienceArgs) (experienceResult, error) {
			if strings.TrimSpace(in.Note) == "" {
				return experienceResult{}, fmt.Errorf("append_experience_log: note is required")
			}
			if err := st.appendExperience(in.Topic, in.Note); err != nil {
				return experienceResult{}, err
			}
			return experienceResult{Recorded: true}, nil
		},
	)
}

type retrieverArgs struct {
	Query string `json:"query" jsonschema_description:"keywords to search for, e.g. \"v5e 4x8x8 maxtext\""`
}

type retrieverResult struct {
	Matches []string `json:"matches"`
}

// retrieverTool is bouncer's bouncer_docs_retriever: a plain
// substring grep over the saved preflight library and the experience
// log. Upstream is exactly this — no embeddings, no vector store —
// and the honest port keeps it that way rather than inventing a
// retrieval story the original never had.
func retrieverTool(st *store) (adktool.Tool, error) {
	return functiontool.New(
		functiontool.Config{
			Name: "bouncer_docs_retriever",
			Description: "Search the saved preflight library and your experience log for prior work. " +
				"Do this FIRST: if a saved preflight already matches this workload's topology, " +
				"hardware and framework, reuse it instead of deriving a new one.",
		},
		func(_ adktool.Context, in retrieverArgs) (retrieverResult, error) {
			return retrieverResult{Matches: st.grep(in.Query, 40)}, nil
		},
	)
}

type reuseArgs struct {
	Name   string `json:"name" jsonschema_description:"the library entry to reuse, as reported by bouncer_docs_retriever"`
	Reason string `json:"reason" jsonschema_description:"why the existing preflight matches this workload exactly"`
}

type reuseResult struct {
	Path string `json:"path"`
}

func reuseTool(st *store) (adktool.Tool, error) {
	return functiontool.New(
		functiontool.Config{
			Name: "reuse_existing_preflight",
			Description: "Declare that an existing library preflight already covers this workload. " +
				"Only valid when the topology, hardware and framework match exactly.",
		},
		func(_ adktool.Context, in reuseArgs) (reuseResult, error) {
			slug := slugify(in.Name)
			path := filepath.Join(st.libraryDir, slug+".yaml")
			if _, err := os.Stat(path); err != nil {
				return reuseResult{}, fmt.Errorf("reuse_existing_preflight: no library entry named %q", in.Name)
			}
			if err := st.appendExperience("reuse", in.Name+": "+in.Reason); err != nil {
				return reuseResult{}, err
			}
			return reuseResult{Path: path}, nil
		},
	)
}

// grep is the retriever's implementation: case-insensitive substring
// matching over the library metadata and the experience log, newest
// entries first is not attempted — upstream doesn't either.
func (s *store) grep(query string, limit int) []string {
	terms := strings.Fields(strings.ToLower(query))
	if len(terms) == 0 {
		return nil
	}
	var matches []string
	scan := func(label, body string) {
		for _, line := range strings.Split(body, "\n") {
			if len(matches) >= limit {
				return
			}
			low := strings.ToLower(line)
			for _, t := range terms {
				if strings.Contains(low, t) {
					matches = append(matches, label+": "+strings.TrimSpace(line))
					break
				}
			}
		}
	}

	entries, err := os.ReadDir(s.libraryDir)
	if err == nil {
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			body, err := os.ReadFile(filepath.Join(s.libraryDir, e.Name())) // #nosec G304 -- library dir listing
			if err != nil {
				continue
			}
			scan("library/"+e.Name(), string(body))
		}
	}
	if body, err := os.ReadFile(filepath.Join(s.sessionDir, experienceFile)); err == nil { // #nosec G304 -- fixed name
		scan("experience", string(body))
	}
	return matches
}

// buildGenerator constructs the parent agent. extras carries the
// tools autonomous.Run injects (report_done, and schedule_next_turn
// when a Scheduler is installed).
func buildGenerator(cfg generatorConfig, extras []adktool.Tool) (*agent.Agent, error) {
	if cfg.history == nil {
		cfg.history = &handoffLog{}
	}
	if cfg.gate == nil {
		cfg.gate = &sync.Mutex{}
	}
	tools := []adktool.Tool{}
	for _, mk := range []func() (adktool.Tool, error){
		func() (adktool.Tool, error) { return sandboxTool(cfg.sandbox) },
		waitTool,
		func() (adktool.Tool, error) { return readSourceTool(cfg.store) },
		func() (adktool.Tool, error) { return objectiveTool(cfg.objective) },
		func() (adktool.Tool, error) { return experienceTool(cfg.store) },
		func() (adktool.Tool, error) { return retrieverTool(cfg.store) },
		func() (adktool.Tool, error) { return reuseTool(cfg.store) },
		func() (adktool.Tool, error) { return historyTool(cfg.history) },
		func() (adktool.Tool, error) { return saveIfValidatedTool(cfg) },
	} {
		t, err := mk()
		if err != nil {
			return nil, err
		}
		tools = append(tools, t)
	}
	tools = append(tools, extras...)

	opts := []agent.Option{
		agent.WithName("generator"),
		agent.WithDescription("derives single-slice TPU preflights from completed production workloads"),
		agent.WithMode(agent.ModeAutonomous),
		agent.WithInstruction(cfg.instruction),
		agent.WithTools(tools),
	}
	if cfg.log != nil {
		opts = append(opts,
			agent.WithEventLog(cfg.log),
			agent.WithSession("bouncer", cfg.sessionID))
	}
	return agent.New(cfg.model, opts...)
}

func oneLine(s string) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
	return truncate(s, 160)
}
