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

package agentenv

import (
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/childenv"
)

// #1157: `${env:NAME}` splices a value into text the model reads —
// AGENTS.md, skills and skill resources, which are re-read from disk on
// every load and whose loads never prompt. A daemon credential must
// never come back that way, whether the manifest declares it (the usual
// K8s shape, so the drift report stays quiet) or the resolver reaches it
// through the os.Getenv fallback.
//
// Not parallel: t.Setenv.
func TestInterpolateNeverSplicesAWithheldCredential(t *testing.T) {
	const declared = "CORE_AGENT_1157_DECLARED_TOKEN"
	const undeclared = "CORE_AGENT_1157_UNDECLARED_TOKEN"
	t.Setenv(undeclared, "s3cret-undeclared")
	childenv.Withhold(declared, undeclared)

	m := &Manifest{Env: []Entry{
		{Name: declared, Required: true},
		{Name: "CORE_AGENT_1157_ORDINARY", Default: "fine"},
	}}
	r := NewResolver(m, mkLookup(map[string]string{declared: "s3cret-declared"}))

	for _, name := range []string{declared, undeclared} {
		if got := r.Interpolate("token=${env:" + name + "}"); got != "token=" {
			t.Errorf("${env:%s} interpolated to %q: a withheld daemon credential reached text the model reads", name, got)
		}
	}
	// Control: the guard is the withheld set, not interpolation broken
	// wholesale.
	if got := r.Interpolate("${env:CORE_AGENT_1157_ORDINARY}"); got != "fine" {
		t.Errorf("an ordinary declared var interpolated to %q, want %q", got, "fine")
	}
}
