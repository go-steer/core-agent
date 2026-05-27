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

package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/core-agent/agent"
)

func TestParseSlash_CompactAndAlias(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in       string
		wantAct  SlashAction
		wantArgs string
	}{
		{"/compact", SlashCompact, ""},
		{"/compact focus on the auth thread", SlashCompact, "focus on the auth thread"},
		{"/summarize", SlashCompact, ""},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			act, _, args, ok := ParseSlash(tc.in)
			if !ok || act != tc.wantAct {
				t.Errorf("ParseSlash(%q) = (%v, ok=%v), want (%v, true)", tc.in, act, ok, tc.wantAct)
			}
			if args != tc.wantArgs {
				t.Errorf("ParseSlash(%q) args = %q, want %q", tc.in, args, tc.wantArgs)
			}
		})
	}
}

func TestHandleCompactCommand_ShowsRunningNotice(t *testing.T) {
	t.Parallel()
	m := newOperatorInputTestModel(t)
	// The handler spawns a goroutine that calls m.agent.Compact —
	// our test agent has no compactor wired, so the goroutine
	// returns ErrNoCompactor quickly. Verify the synchronous part
	// (the running notice) lands first.
	m.handleCompactCommand("")
	last := lastSystemMessage(t, m)
	if !strings.Contains(last, "Compacting") {
		t.Errorf("expected 'Compacting…' notice, got %q", last)
	}
}

func TestHandleCompactCommand_FocusInNotice(t *testing.T) {
	t.Parallel()
	m := newOperatorInputTestModel(t)
	m.handleCompactCommand("the auth-rewrite thread")
	last := lastSystemMessage(t, m)
	if !strings.Contains(last, "focus: the auth-rewrite thread") {
		t.Errorf("expected focus hint in notice, got %q", last)
	}
}

func TestHandleCompactResult_NoCompactorSurfacesGuidance(t *testing.T) {
	t.Parallel()
	m := newOperatorInputTestModel(t)
	m.handleCompactResult(compactResultMsg{err: agent.ErrNoCompactor})
	last := lastSystemMessage(t, m)
	if !strings.Contains(last, "WithCompactor") {
		t.Errorf("expected wiring hint when no compactor; got %q", last)
	}
}

func TestHandleCompactResult_SkippedIsFriendly(t *testing.T) {
	t.Parallel()
	m := newOperatorInputTestModel(t)
	m.handleCompactResult(compactResultMsg{res: agent.CompactionResult{Skipped: true}})
	last := lastSystemMessage(t, m)
	if !strings.Contains(last, "nothing to summarize") {
		t.Errorf("expected friendly skipped message, got %q", last)
	}
}

func TestHandleCompactResult_SuccessShowsDuration(t *testing.T) {
	t.Parallel()
	m := newOperatorInputTestModel(t)
	m.handleCompactResult(compactResultMsg{res: agent.CompactionResult{
		SummaryEventID: "compaction-1234",
		SummaryText:    strings.Repeat("x", 4200),
		Duration:       2 * time.Second,
	}})
	last := lastSystemMessage(t, m)
	if !strings.Contains(last, "4200 chars") {
		t.Errorf("expected summary length in success message, got %q", last)
	}
	if !strings.Contains(last, "2s") {
		t.Errorf("expected duration in success message, got %q", last)
	}
	if !strings.Contains(last, "audit log is preserved") {
		t.Errorf("expected operator-reassurance about audit log, got %q", last)
	}
}

func TestHandleCompactResult_GenericErrorSurfacesText(t *testing.T) {
	t.Parallel()
	m := newOperatorInputTestModel(t)
	m.handleCompactResult(compactResultMsg{err: errors.New("upstream 500")})
	last := lastSystemMessage(t, m)
	if !strings.Contains(last, "upstream 500") {
		t.Errorf("expected error text propagated, got %q", last)
	}
}
