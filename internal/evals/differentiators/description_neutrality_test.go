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

package differentiators

import (
	"testing"

	"github.com/go-steer/core-agent/v3/internal/testutil"
)

func TestDifferentiatorToolTextIsDeploymentNeutral(t *testing.T) {
	t.Parallel()
	r := &rig{}
	tools, err := r.buildTools()
	if err != nil {
		t.Fatalf("buildTools: %v", err)
	}
	if len(tools) != 3 {
		t.Fatalf("swept %d tools, want the 3 this package registers", len(tools))
	}
	for _, tl := range tools {
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
			t.Errorf("tool %q description tells the model to set %q, which it does not declare:\n  %s", tl.Name(), ref, tl.Description())
		}
	}
}
