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
	"strings"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/agent"
	"github.com/go-steer/core-agent/v2/pkg/compose"
	"github.com/go-steer/core-agent/v2/pkg/config"
)

func ptr(v float64) *float64 { return &v }

// The token figure is computed from the model's REAL window, which is the
// mistake the line exists to prevent: 0.021 of gemini-3.7-flash's
// 1,048,576-token window is 22,020, not the 21,000 a reader assumes.
func TestTheCompactionBootLineStatesTokensFromTheRealWindow(t *testing.T) {
	t.Parallel()
	dc := compose.BuildCompactor(config.CompactionConfig{Threshold: ptr(0.021)}).(*agent.DefaultCompactor)
	got := compactionBootLine(dc, "gemini-3.7-flash")
	if want := "fires at 0.021 of gemini-3.7-flash's 1048576-token window = 22020 tokens"; !strings.Contains(got, want) {
		t.Fatalf("boot line = %q\nwant it to contain %q", got, want)
	}
}

// The line reports the threshold the daemon will actually use. With
// nothing configured that is the frontier tier default, not the 0.85
// fallback by coincidence of value — so check a mid-tier model, whose
// default (0.65) differs from the fallback.
func TestTheCompactionBootLineReportsTheEffectiveTierThreshold(t *testing.T) {
	t.Parallel()
	dc := compose.BuildCompactor(config.CompactionConfig{}).(*agent.DefaultCompactor)
	got := compactionBootLine(dc, "gemini-3.5-flash")
	if !strings.Contains(got, "fires at 0.65 of gemini-3.5-flash's") {
		t.Fatalf("boot line = %q; want the mid-tier default 0.65, the threshold ShouldCompact will use", got)
	}
}

// An uncatalogued model does not stop compaction: the runtime assumes a
// 128,000-token window (agent.AssumedContextWindowSize). The line states
// that figure — the one the daemon acts on — not "unknown", which the
// first draft printed and which the review caught as the one case where
// line and decision disagreed.
func TestTheCompactionBootLineStatesTheAssumedWindowForAnUnknownModel(t *testing.T) {
	t.Parallel()
	dc := compose.BuildCompactor(config.CompactionConfig{}).(*agent.DefaultCompactor)
	got := compactionBootLine(dc, "some-unlisted-model")
	if want := "assumed 128000-token window = 108800 tokens"; !strings.Contains(got, want) {
		t.Fatalf("boot line = %q\nwant it to contain %q (0.85 of the assumed window)", got, want)
	}
}
