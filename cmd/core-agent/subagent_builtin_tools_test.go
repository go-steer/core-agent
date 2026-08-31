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
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/config"
)

// A subagent that declares its own model block replaces the parent's
// ModelConfig wholesale, which would silently drop model.builtin_tools
// with it — the same trap #714's prompt_cache block already had to be
// dug out of. An operator who turned web search off for the project
// meant it for the whole process.
func TestInheritBuiltinTools(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name           string
		parent, sub    *config.BuiltinToolsConfig
		wantNil        bool
		wantWebSearch  *bool
		wantURLContext *bool
		wantCodeExec   *bool
	}{
		{
			name:    "nothing anywhere",
			wantNil: true,
		},
		{
			name:          "sub-only survives untouched",
			sub:           &config.BuiltinToolsConfig{WebSearch: boolPtr(true)},
			wantWebSearch: boolPtr(true),
		},
		{
			name:          "parent-only is inherited whole",
			parent:        &config.BuiltinToolsConfig{WebSearch: boolPtr(false)},
			wantWebSearch: boolPtr(false),
		},
		{
			// The load-bearing case. Merged per field, not per block:
			// the fields are independent tri-states, so a subagent that
			// names code_execution has said nothing about web_search and
			// must not lose the project-wide disable by mentioning a
			// neighbour.
			name:          "sub naming one field still inherits the others",
			parent:        &config.BuiltinToolsConfig{WebSearch: boolPtr(false), URLContext: boolPtr(false)},
			sub:           &config.BuiltinToolsConfig{CodeExecution: boolPtr(true)},
			wantWebSearch: boolPtr(false), wantURLContext: boolPtr(false), wantCodeExec: boolPtr(true),
		},
		{
			// An explicit subagent value is a deliberate override and
			// beats the parent, including in the risk-increasing
			// direction — declaring it is the point.
			name:          "explicit sub value wins over the parent",
			parent:        &config.BuiltinToolsConfig{WebSearch: boolPtr(false)},
			sub:           &config.BuiltinToolsConfig{WebSearch: boolPtr(true)},
			wantWebSearch: boolPtr(true),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// Snapshot the parent so the merge can be checked for
			// mutation: subCfg is a shallow copy of the real config, so
			// writing through the parent's pointer would corrupt every
			// later resolution.
			var parentBefore config.BuiltinToolsConfig
			if tc.parent != nil {
				parentBefore = *tc.parent
			}

			got := inheritBuiltinTools(tc.parent, tc.sub)
			if tc.wantNil {
				if got != nil {
					t.Fatalf("got %+v, want nil", got)
				}
				return
			}
			if got == nil {
				t.Fatal("got nil, want a merged block")
			}
			checkTriState(t, "web_search", got.WebSearch, tc.wantWebSearch)
			checkTriState(t, "url_context", got.URLContext, tc.wantURLContext)
			checkTriState(t, "code_execution", got.CodeExecution, tc.wantCodeExec)

			if tc.parent != nil && *tc.parent != parentBefore {
				t.Errorf("parent block was mutated: %+v", *tc.parent)
			}
		})
	}
}

func checkTriState(t *testing.T, field string, got, want *bool) {
	t.Helper()
	switch {
	case got == nil && want == nil:
	case got == nil || want == nil:
		t.Errorf("%s: got %v, want %v", field, fmtTriState(got), fmtTriState(want))
	case *got != *want:
		t.Errorf("%s: got %v, want %v", field, *got, *want)
	}
}

func fmtTriState(b *bool) string {
	if b == nil {
		return "unset"
	}
	if *b {
		return "true"
	}
	return "false"
}
