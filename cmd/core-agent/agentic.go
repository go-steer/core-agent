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
	"fmt"

	adktool "google.golang.org/adk/tool"

	"github.com/go-steer/core-agent/agent"
	"github.com/go-steer/core-agent/models"
	"github.com/go-steer/core-agent/tools/agentic"
)

// buildAgenticTools constructs the agentic_* tool wrappers from
// docs/context-management-design.md Mechanism B. The wrappers
// call Agent.RunSubtask so raw tool output stays in the subtask
// and only the digest reaches the parent's context.
//
// builtinTools is the already-constructed list (from tools.Build)
// — we pick out the canonical inner tools (read_file, fetch_url,
// grep, list_dir, glob) by name so the subtask shares the parent
// agent's permission gate and per-tool output caps. Wrappers
// whose required inner tool isn't in builtinTools are silently
// skipped (e.g. --no-builtin-tools, --disable-tools=read_file,
// or fetch_url disabled because url_scope.allow is empty).
//
// agentGetter is the late-binding closure that resolves *Agent
// once agent.New finishes. provider+smallModelID is the cost-
// efficiency lever: when smallModelID is non-empty, the wrappers
// route subtasks through that cheaper model. Empty smallModelID
// means subtasks inherit the parent's model — the wrappers still
// work, just without the cost win.
func buildAgenticTools(
	builtinTools []adktool.Tool,
	agentGetter func() *agent.Agent,
	provider models.Provider,
	smallModelID string,
) ([]adktool.Tool, error) {
	byName := make(map[string]adktool.Tool, len(builtinTools))
	for _, t := range builtinTools {
		byName[t.Name()] = t
	}

	base := agentic.AgenticToolOpts{
		AgentGetter:  agentGetter,
		Provider:     provider,
		SmallModelID: smallModelID,
	}

	var out []adktool.Tool

	if readFile, ok := byName["read_file"]; ok {
		opts := base
		opts.InnerTools = []adktool.Tool{readFile}
		out = append(out, agentic.AgenticReadFile(opts))
	}

	if fetchURL, ok := byName["fetch_url"]; ok {
		opts := base
		opts.InnerTools = []adktool.Tool{fetchURL}
		out = append(out, agentic.AgenticFetchURL(opts))
	}

	if grep, ok := byName["grep"]; ok {
		inner := []adktool.Tool{grep}
		if rf, hasRF := byName["read_file"]; hasRF {
			inner = append(inner, rf)
		}
		opts := base
		opts.InnerTools = inner
		out = append(out, agentic.AgenticGrep(opts))
	}

	// Research wrapper needs the full read-only investigation
	// kit — skip it if any required tool is missing rather than
	// register a wrapper that calls a subtask with a degraded
	// toolset.
	if _, hasRead := byName["read_file"]; hasRead {
		if _, hasGrep := byName["grep"]; hasGrep {
			if _, hasList := byName["list_dir"]; hasList {
				if _, hasGlob := byName["glob"]; hasGlob {
					opts := base
					opts.InnerTools = []adktool.Tool{
						byName["read_file"],
						byName["grep"],
						byName["list_dir"],
						byName["glob"],
					}
					out = append(out, agentic.AgenticResearch(opts))
				}
			}
		}
	}

	if len(out) == 0 {
		return nil, fmt.Errorf("--agentic-tools requires at least one of read_file / fetch_url / grep to be enabled, but builtin tools are %v", toolNames(builtinTools))
	}
	return out, nil
}

func toolNames(ts []adktool.Tool) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.Name()
	}
	return out
}
