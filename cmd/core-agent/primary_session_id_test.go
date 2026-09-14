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

	"github.com/go-steer/core-agent/v2/pkg/agent"
)

// The approval-notify wiring (#647) is constructed alongside the prompt
// broker, which is several hundred lines before runner.Run calls
// agent.New — and on a multi-session daemon no primary agent is ever
// built at all, because POST /sessions constructs them on demand. The
// first cut read agentRef.SessionID() at the wiring site and segfaulted
// every daemon at startup, with approval_notify unset and nothing to
// notify: a nil dereference does not care that the feature is off.
//
// So the contract under test is timing, not formatting: resolve late,
// and tolerate there being nothing to resolve.
func TestPrimarySessionIDResolvesLateAndToleratesNoPrimaryAgent(t *testing.T) {
	t.Parallel()

	var ref *agent.Agent
	get := primarySessionID(&ref)

	// The wiring site. This is the call that used to panic.
	if got := get(); got != "" {
		t.Fatalf("before the agent exists, session id = %q, want empty", got)
	}

	a, err := agent.New(mustEchoModel(t),
		agent.WithName("primary"),
		agent.WithSession("operator", "sess-late-bound"))
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	ref = a

	// The notification site, some time later.
	if got := get(); got != "sess-late-bound" {
		t.Errorf("after the agent exists, session id = %q, want %q", got, "sess-late-bound")
	}

	// A multi-session daemon never assigns ref; the getter must keep
	// answering rather than being a landmine somebody steps on when the
	// first prompt of an unattended run goes unanswered.
	ref = nil
	if got := get(); got != "" {
		t.Errorf("with no primary agent, session id = %q, want empty", got)
	}
}
