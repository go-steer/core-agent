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
	"strings"
	"testing"
)

func TestDefaultSchedulingInstruction_NonEmpty(t *testing.T) {
	t.Parallel()
	if DefaultSchedulingInstruction == "" {
		t.Fatal("DefaultSchedulingInstruction is empty")
	}
	// Guard the load-bearing pieces of the instruction.
	for _, want := range []string{
		"schedule_next_turn",
		"slow cadences",
		"State does not survive",
		"report_done wins",
	} {
		if !strings.Contains(DefaultSchedulingInstruction, want) {
			t.Errorf("DefaultSchedulingInstruction missing %q", want)
		}
	}
}
