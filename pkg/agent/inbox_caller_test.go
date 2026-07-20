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

package agent

import (
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/auth"
)

func TestInbox_PushPreservesCaller(t *testing.T) {
	t.Parallel()
	q := newInbox()
	want := auth.Caller{Identity: "alice@example.com"}
	if _, err := q.push("hello", want); err != nil {
		t.Fatalf("push: %v", err)
	}
	msgs := q.drain()
	if len(msgs) != 1 {
		t.Fatalf("drain count = %d, want 1", len(msgs))
	}
	if msgs[0].caller.Identity != want.Identity {
		t.Errorf("caller.Identity: got %q, want %q", msgs[0].caller.Identity, want.Identity)
	}
}

func TestDrainInboxFull_LastNonEmptyCallerWins(t *testing.T) {
	t.Parallel()
	// Per docs/multi-session-design.md "the turn answers the most
	// recent ask" — when a batch arrives with mixed callers, the
	// last non-empty caller becomes the turn originator.
	a := &Agent{inbox: newInbox()}
	_, _ = a.inbox.push("first", auth.Caller{Identity: "alice@example.com"})
	_, _ = a.inbox.push("second", auth.Caller{}) // empty — should not overwrite
	_, _ = a.inbox.push("third", auth.Caller{Identity: "bob@example.com"})
	_, _ = a.inbox.push("fourth", auth.Caller{}) // empty trailing — should not clobber bob

	texts, originator := a.drainInboxFull()
	if len(texts) != 4 {
		t.Fatalf("texts count = %d, want 4", len(texts))
	}
	if originator.Identity != "bob@example.com" {
		t.Errorf("originator: got %q, want %q (last non-empty caller wins, empty trailing must not clobber)",
			originator.Identity, "bob@example.com")
	}
}

func TestDrainInboxFull_EmptyInbox(t *testing.T) {
	t.Parallel()
	a := &Agent{inbox: newInbox()}
	texts, originator := a.drainInboxFull()
	if texts != nil {
		t.Errorf("texts: got %v, want nil", texts)
	}
	if originator.Identity != "" {
		t.Errorf("originator: got %q, want empty", originator.Identity)
	}
}

func TestDrainInboxFull_AllEmptyCallersYieldsZeroOriginator(t *testing.T) {
	t.Parallel()
	a := &Agent{inbox: newInbox()}
	_, _ = a.inbox.push("x", auth.Caller{})
	_, _ = a.inbox.push("y", auth.Caller{})
	texts, originator := a.drainInboxFull()
	if len(texts) != 2 {
		t.Fatalf("texts count = %d, want 2", len(texts))
	}
	if originator.Identity != "" {
		t.Errorf("originator: got %q, want empty (no message carried an identity)", originator.Identity)
	}
}
