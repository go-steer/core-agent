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

package runner

import (
	"regexp"
	"strings"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/usage"
)

// The pattern docs/acceptance-m1.md documents for this line. It is the
// contract a shell pipeline would have been written against, so the
// thinking segment has to leave it matching.
var documentedSummary = regexp.MustCompile(`^core-agent: \d+ turn\(s\) · ↑\d+ ↓\d+ tokens · \$\d+\.\d+ \(.+?\)`)

func summaryFor(t *testing.T, turns ...usage.TurnUsage) string {
	t.Helper()
	tr := usage.NewTracker()
	for _, u := range turns {
		tr.AppendUsage("m", u, usage.Pricing{InputPerMTok: 1, OutputPerMTok: 10})
	}
	var sb strings.Builder
	WriteSummary(&sb, tr, "m")
	return sb.String()
}

// Gemini reports reasoning separately from output and it is billed at
// the output rate, so the dollar figure already includes it. Before
// this, the line showed that cost next to arrows that did not, and a
// thinking-heavy run looked overpriced for its token count.
func TestSummaryNamesThinkingTokensTheCostAlreadyIncludes(t *testing.T) {
	t.Parallel()
	got := summaryFor(t,
		usage.TurnUsage{InputTokens: 1000, OutputTokens: 85, ThoughtsTokens: 570},
		usage.TurnUsage{InputTokens: 1000, OutputTokens: 15, ThoughtsTokens: 30},
	)
	if want := " · +600 thinking tokens (billed as output)"; !strings.Contains(got, want) {
		t.Errorf("summary does not report the run's thinking tokens summed across turns\nwant substring: %q\ngot:            %q", want, got)
	}
	// ↓ stays the provider's output count; thinking is added beside it,
	// not folded in, or a reader could not tell the two apart.
	if !strings.Contains(got, "↓100 tokens") {
		t.Errorf("↓ should remain the output count alone (100)\ngot: %q", got)
	}
	if !documentedSummary.MatchString(got) {
		t.Errorf("the line no longer matches the documented summary pattern %s\ngot: %q", documentedSummary, got)
	}
}

// Anthropic bills thinking inside output_tokens and reports no separate
// count, so every Anthropic turn arrives with zero here. Those runs must
// print exactly the line they always have.
func TestSummaryIsUnchangedWhenNoThinkingWasReported(t *testing.T) {
	t.Parallel()
	got := summaryFor(t, usage.TurnUsage{InputTokens: 1000, OutputTokens: 100})
	if want := "core-agent: 1 turn(s) · ↑1000 ↓100 tokens · $0.0020 (m)\n"; got != want {
		t.Errorf("summary changed for a run with no thinking tokens\nwant: %q\ngot:  %q", want, got)
	}
}
