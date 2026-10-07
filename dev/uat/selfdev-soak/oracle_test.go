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

package selfdevsoak

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestA7OracleCompiles keeps the A7 task oracle building against
// pkg/agent's exported API. The oracle carries the a7oracle build tag,
// so nothing else in the tree compiles it, and an API rename would
// otherwise surface only when grade_a7.py runs it against a real run:
// as a build failure at the base, which the grader refuses as evidence.
// This test vets the oracle in place and never runs it. Running it here
// would fail, by design, until #1234's recovery half is fixed.
func TestA7OracleCompiles(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles pkg/agent")
	}
	cmd := exec.Command("go", "vet", "-tags", "a7oracle", "./oracle/")
	cmd.Env = append(os.Environ(), "GOWORK=off")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go vet -tags a7oracle ./oracle/: %v\n%s", err, out)
	}
	if strings.TrimSpace(string(out)) != "" {
		t.Logf("go vet output:\n%s", out)
	}
}
