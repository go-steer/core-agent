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

package alert

import (
	"strings"
	"testing"

	"github.com/go-steer/core-agent/v2/internal/testutil"
	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/permissions"
)

// The rationale for the ban list lives with the list itself, in
// internal/testutil.ModelFacingBans (#909).
//
// alert believed itself swept by the built-in catalog's sweep and was
// not: that helper builds a DefaultConfig registry, and alert is gated
// on HasLiveTarget rather than a toggle, so it was never in the list it
// was assumed to be in (#919). Swept locally now, which is where it
// cannot fall out of a config again.
//
// Local also happens to be the only place the interesting half is
// reachable. buildDescription COMPOSES the description from the
// operator's target names and their descriptions, so what the model reads
// is half in-tree prose and half deployment data — and the in-tree half
// is the only half this can hold to a standard.
func TestAlertToolTextIsDeploymentNeutral(t *testing.T) {
	t.Parallel()
	cfg := config.DefaultConfig()
	cfg.Alerts.Targets = []config.AlertTarget{
		{Name: "oncall", URL: "https://example.com/hook", Description: "paging a human when nothing else can proceed"},
	}
	gate := permissions.New(permissions.Options{Mode: permissions.ModeYolo})
	tl, err := New(gate, cfg)
	if err != nil {
		t.Fatalf("alert.New: %v", err)
	}
	if !strings.Contains(tl.Description(), "oncall") {
		t.Fatalf("the registered target did not reach the description; the sweep is not seeing the composed text:\n  %s", tl.Description())
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
