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

package config

import (
	"path/filepath"
	"testing"
)

// A subagent whose model block sets only a name and a thinking level must
// keep the level when written to markdown and read back (#1605): the
// "bare name" shortcut used to write `model: <name>` and drop it.
func TestSubagentMarkdownKeepsThinkingLevel(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "triage.md")
	spec := SubagentSpec{
		Name:         "triage",
		Description:  "triages",
		Mode:         SubagentModeTask,
		Instructions: "Look at the cluster.",
		Model:        &ModelConfig{Name: "claude-sonnet-5-5", ThinkingLevel: "low"},
	}
	if err := writeSubagentMarkdownFile(path, spec); err != nil {
		t.Fatal(err)
	}
	got, err := LoadSubagentMarkdownFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Model == nil || got.Model.Name != "claude-sonnet-5-5" || got.Model.ThinkingLevel != "low" {
		t.Fatalf("model after round trip = %+v, want name claude-sonnet-5-5 and thinking_level low", got.Model)
	}

	// A name alone still takes the bare-name form and reads back the same.
	spec.Model = &ModelConfig{Name: "claude-sonnet-5-5"}
	if err := writeSubagentMarkdownFile(path, spec); err != nil {
		t.Fatal(err)
	}
	if got, err = LoadSubagentMarkdownFile(path); err != nil || got.Model == nil || got.Model.Name != "claude-sonnet-5-5" || got.Model.ThinkingLevel != "" {
		t.Fatalf("bare name after round trip = %+v, %v", got.Model, err)
	}
}
