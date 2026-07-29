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
	"strings"
)

// describePromptLayers renders the active system-prompt layer stack
// (#459) for the /memory endpoint's first row — the operator-visible
// answer to "what shape is the assembled prompt?" without reading
// code. Mirrors agent.New's assembly rules: a full replace skips
// layers 1–3; quirks key off the model identifier.
func describePromptLayers(modelName string, replaced, appended bool, memorySources int) string {
	if replaced {
		parts := []string{"REPLACED(system_prompt_file)", fmt.Sprintf("memory(%d sources)", memorySources)}
		if appended {
			parts = append(parts, "append")
		}
		return strings.Join(parts, " + ")
	}
	parts := []string{"core"}
	if strings.Contains(strings.ToLower(modelName), "gemini") {
		parts = append(parts, "quirks(gemini-parallelism)")
	}
	parts = append(parts, "interactive-overlay", fmt.Sprintf("memory(%d sources)", memorySources))
	if appended {
		parts = append(parts, "append")
	}
	return strings.Join(parts, " + ")
}

// promptOpts carries the #459 system-prompt flags from main's flag
// block into run (same pattern as attachOpts).
type promptOpts struct {
	appendSystemPrompt string
	systemPromptFile   string
}
