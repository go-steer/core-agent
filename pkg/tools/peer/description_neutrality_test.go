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

package peer

import (
	"testing"

	"github.com/go-steer/core-agent/v2/internal/testutil"
	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/permissions"
)

// The rationale for the ban list lives with the list itself, in
// internal/testutil.ModelFacingBans (#909). call_peer was outside every
// sweep #909 shipped: it is registered from pkg/compose rather than
// tools.Build, so the built-in catalog's sweep never sees it, and this
// package had no sweep of its own (#919).
//
// Only the DEFAULT description is swept. An operator who sets
// tools.call_peer.description owns that string the way they own their
// AGENTS.md; the in-tree default is the one nobody can see or override.
func TestCallPeerToolTextIsDeploymentNeutral(t *testing.T) {
	t.Parallel()
	cfg := config.DefaultConfig()
	gate := permissions.New(permissions.Options{Mode: permissions.ModeYolo})
	tl, err := New(gate, cfg, DirectoryFunc(func() []Peer { return nil }))
	if err != nil {
		t.Fatalf("peer.New: %v", err)
	}
	if tl.Description() == "" {
		t.Fatal("call_peer has no description: the sweep would pass vacuously")
	}
	texts, scanned := testutil.ModelFacingText(tl)
	if !scanned {
		t.Errorf("tool %q exposes no arg schema to scan", tl.Name())
	}
	for _, text := range texts {
		for _, bad := range testutil.ModelFacingBanViolations(text) {
			t.Errorf("tool %q: %s\n  %s", tl.Name(), bad, text)
		}
	}
	refs, checked := testutil.UndeclaredArgRefs(tl)
	if !checked {
		t.Errorf("tool %q exposes no arg schema to cross-check its description against", tl.Name())
	}
	for _, ref := range refs {
		t.Errorf("tool %q description tells the model to set %q, which it does not declare — ADK validates with additionalProperties:false, so obeying is a hard error:\n  %s", tl.Name(), ref, tl.Description())
	}
}
