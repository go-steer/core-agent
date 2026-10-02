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

package hooks

import (
	"context"
	"strings"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/childenv"
)

// #1157. A hook command is operator-authored, but it runs on events the
// model drives and commonly calls scripts in the repository the agent is
// editing — so it is one more door to the daemon's credentials.
//
// The hook fails if it can see the variable, and execCommand returns that
// failure with the command's output attached, so a leak is an error here
// rather than something to infer.
//
// Not parallel: t.Setenv.
func TestHookCommandsNeverInheritAWithheldCredential(t *testing.T) {
	const name = "CORE_AGENT_1157_HOOK_TOKEN"
	t.Setenv(name, "s3cret")
	childenv.Withhold(name)

	d := &Dispatcher{}
	err := d.execCommand(context.Background(), `if [ -n "$`+name+`" ]; then echo "LEAKED=$`+name+`"; exit 1; fi`, nil)
	if err != nil {
		t.Fatalf("a hook command could read the withheld %s: %v", name, err)
	}

	// Control: the same probe on a variable that is NOT withheld must
	// fail, or the assertion above would pass on a hook that sees no
	// environment at all.
	t.Setenv("CORE_AGENT_1157_HOOK_VISIBLE", "1")
	err = d.execCommand(context.Background(), `[ -z "$CORE_AGENT_1157_HOOK_VISIBLE" ] || exit 1`, nil)
	if err == nil || !strings.Contains(err.Error(), "exit status 1") {
		t.Fatalf("control probe: a non-withheld variable should reach the hook, got err=%v", err)
	}
}
