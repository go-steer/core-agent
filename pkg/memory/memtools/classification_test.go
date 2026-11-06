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

package memtools

import (
	"testing"

	"github.com/go-steer/core-agent/v3/pkg/permissions"
	"github.com/go-steer/core-agent/v3/pkg/tools"
)

// The memory tools are classified in several hand-kept name tables: the
// gate's ToolClass (durable write gate, docs/durable-write-gate-design.md),
// the dispatch serializer, and the plan-first exemption. A tool missing
// from one fails closed as mutating, which is safe but wrong for
// recall_memory: it would serialize behind writes, be blocked before
// record_plan, and be journaled as an effect. This pins all of them.
// internal/effects keeps a parallel table; it is asserted in that
// package's own test (TestMemoryToolClasses), beside the table.
func TestMemoryToolsAreClassifiedConsistently(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		readOnly bool
	}{
		{RecallToolName, true},
		{PersistToolName, false},
		{ForgetToolName, false},
	}
	for _, c := range cases {
		wantClass := permissions.ClassMutating
		if c.readOnly {
			wantClass = permissions.ClassReadOnly
		}
		if got := permissions.DefaultToolClass(c.name); got != wantClass {
			t.Errorf("permissions.DefaultToolClass(%q) = %v, want %v", c.name, got, wantClass)
		}
		if got := tools.IsReadOnlyToolName(c.name); got != c.readOnly {
			t.Errorf("tools.IsReadOnlyToolName(%q) = %v, want %v", c.name, got, c.readOnly)
		}
	}
}
