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

func TestParseSlash_DoneAndAlias(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in       string
		wantAct  SlashAction
		wantArgs string
	}{
		{"/done", SlashDone, ""},
		{"/done finished the auth migration", SlashDone, "finished the auth migration"},
		{"/checkpoint", SlashDone, ""},
		{"/checkpoint shipped feature X", SlashDone, "shipped feature X"},
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

func TestHandleDoneCommand_ShowsRunningNotice(t *testing.T) {
	t.Parallel()
	m := newOperatorInputTestModel(t)
	m.handleDoneCommand("")
	last := lastSystemMessage(t, m)
	if !strings.Contains(last, "Writing checkpoint") {
		t.Errorf("expected 'Writing checkpoint…' notice, got %q", last)
	}
}

func TestHandleDoneCommand_NoteInNotice(t *testing.T) {
	t.Parallel()
	m := newOperatorInputTestModel(t)
	m.handleDoneCommand("finished the gRPC server")
	last := lastSystemMessage(t, m)
	if !strings.Contains(last, "note: finished the gRPC server") {
		t.Errorf("expected operator note in notice, got %q", last)
	}
}

func TestHandleDoneResult_NoCheckpointerSurfacesGuidance(t *testing.T) {
	t.Parallel()
	m := newOperatorInputTestModel(t)
	m.handleDoneResult(doneResultMsg{err: agent.ErrNoCheckpointer})
	last := lastSystemMessage(t, m)
	if !strings.Contains(last, "WithCheckpointer") {
		t.Errorf("expected wiring hint when no checkpointer; got %q", last)
	}
}

func TestHandleDoneResult_SkippedIsFriendly(t *testing.T) {
	t.Parallel()
	m := newOperatorInputTestModel(t)
	m.handleDoneResult(doneResultMsg{res: agent.CheckpointResult{Skipped: true}})
	last := lastSystemMessage(t, m)
	if !strings.Contains(last, "nothing to checkpoint") {
		t.Errorf("expected friendly skipped message, got %q", last)
	}
}

func TestHandleDoneResult_SuccessShowsNoteAndDuration(t *testing.T) {
	t.Parallel()
	m := newOperatorInputTestModel(t)
	m.handleDoneResult(doneResultMsg{res: agent.CheckpointResult{
		CheckpointEventID: "checkpoint-1234",
		SummaryText:       strings.Repeat("x", 3300),
		TaskNote:          "shipped /done slash",
		Duration:          2 * time.Second,
	}})
	last := lastSystemMessage(t, m)
	if !strings.Contains(last, "shipped /done slash") {
		t.Errorf("expected task note in success message, got %q", last)
	}
	if !strings.Contains(last, "3300 chars") {
		t.Errorf("expected summary length, got %q", last)
	}
	if !strings.Contains(last, "2s") {
		t.Errorf("expected duration, got %q", last)
	}
	if !strings.Contains(last, "audit log is preserved") {
		t.Errorf("expected operator-reassurance about audit log, got %q", last)
	}
}

func TestHandleDoneResult_GenericErrorSurfacesText(t *testing.T) {
	t.Parallel()
	m := newOperatorInputTestModel(t)
	m.handleDoneResult(doneResultMsg{err: errors.New("model upstream 503")})
	last := lastSystemMessage(t, m)
	if !strings.Contains(last, "model upstream 503") {
		t.Errorf("expected error text propagated, got %q", last)
	}
}
