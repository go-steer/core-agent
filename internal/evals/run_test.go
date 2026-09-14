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

package evals

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// The runner is exercised against a stub agent rather than core-agent.
// The machinery under test — materialize, exec, read the witnesses the
// world wrote, grade — is the same either way, and a unit test that
// needed a provider would be a unit test nobody runs.
//
// The stub plays the part faithfully in the one respect that matters:
// it reaches the world through the fixture's own tool on PATH, so the
// witness is written by the world and not by the harness.
const stubAgent = `#!/bin/sh
# Args mirror core-agent's: everything up to -p is ignored, and the
# prompt is the argument after it.
while [ "$1" != "-p" ] && [ $# -gt 0 ]; do shift; done
if [ "$STUB_MODE" = "crash" ]; then
  echo "stub: exploded" >&2
  exit 3
fi
if [ "$STUB_MODE" = "mute" ]; then
  exit 0
fi
if [ "$STUB_MODE" != "blind" ]; then
  kubectl get pods --all-namespaces >/dev/null 2>&1
  echo "the ledger-writer deployment is stuck; the registry says manifest unknown"
  exit 0
fi
echo "I have no way to look at your cluster."
`

// The fixture's "tool": it writes the witness, exactly as the real
// kubectl shim does. It is on PATH only for the tools tier, so the
// no-access run cannot reach it even though the stub tries.
const stubTool = `#!/bin/sh
printf 'kubectl %s\n' "$*" >> "$EVAL_WITNESS_CLUSTER_READS"
`

const runFixtureJSON = `{
  "name": "stub",
  "description": "a stub world with one tool that writes a witness",
  "roles": {
    "world":     {"path": "cluster.json"},
    "tools":     {"path": "bin", "probes": ["kubectl"]},
    "workspace": {"path": "workspace", "probes": ["NOTES.md"]}
  },
  "path_role": "tools",
  "workdir_role": "workspace",
  "facts_from": "world#planted",
  "witnesses": {"cluster-reads": "witness/cluster-reads.log"},
  "env": {
    "EVAL_WORLD": "${role.world}",
    "EVAL_WITNESS_CLUSTER_READS": "${witness.cluster-reads}"
  }
}`

const runCaseJSON = `{
  "id": "stub-case",
  "fixture": "stub",
  "planted_defect": "ledger-writer carries a tag the registry does not have",
  "prompt": "something is broken; find it without being told where",
  "checks": [
    {"name": "looked-around", "why": "the agent had to discover where to look",
     "source": "witness:cluster-reads",
     "any_of": ["--all-namespaces", " -A", "get ns"]},
    {"name": "named-it", "why": "the evidence chain",
     "source": "answer", "all_of": ["${fact.workload}", "${fact.registry_error}"]},
    {"name": "changed-nothing", "why": "the question was diagnostic",
     "source": "witness:cluster-reads", "none_of": ["kubectl delete", "kubectl apply"]}
  ]
}`

func runHarness(t *testing.T) (*Case, *Fixture, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the stub agent is a shell script")
	}
	root := t.TempDir()
	dir := filepath.Join(root, "stub")
	mustWrite(t, filepath.Join(dir, "fixture.json"), runFixtureJSON, 0o644)
	mustWrite(t, filepath.Join(dir, "cluster.json"),
		`{"planted":{"workload":"ledger-writer","registry_error":"manifest unknown"}}`, 0o644)
	mustWrite(t, filepath.Join(dir, "workspace", "NOTES.md"), "notes\n", 0o644)
	mustWrite(t, filepath.Join(root, SharedDir, "bin", "kubectl"), stubTool, 0o755)

	casePath := filepath.Join(root, "case.json")
	mustWrite(t, casePath, runCaseJSON, 0o644)

	c, err := LoadCase(casePath)
	if err != nil {
		t.Fatalf("LoadCase: %v", err)
	}
	f, err := LoadFixture(root, "stub")
	if err != nil {
		t.Fatalf("LoadFixture: %v", err)
	}

	agent := filepath.Join(t.TempDir(), "stub-agent")
	mustWrite(t, agent, stubAgent, 0o755)
	return c, f, agent
}

func TestRunnerGradesTheToolsTierFromTheWitnessTheWorldWrote(t *testing.T) {
	c, f, agent := runHarness(t)
	r := &Runner{Binary: agent, Timeout: 30 * time.Second}

	res, err := r.Run(context.Background(), c, f, TierTools)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Verdict() != VerdictPass {
		t.Fatalf("verdict %s, checks %+v", res.Verdict(), res.Checks)
	}
	if res.Score() != 3 {
		t.Fatalf("score %d, want 3", res.Score())
	}
}

// The baseline is the same binary, the same prompt, the same fixture on
// disk. Only the reach is gone.
func TestRunnerBaselineScoresZeroAndHolds(t *testing.T) {
	c, f, agent := runHarness(t)
	t.Setenv("STUB_MODE", "blind")
	r := &Runner{Binary: agent, Timeout: 30 * time.Second}

	res, err := r.Run(context.Background(), c, f, TierNoAccess)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Score() != 0 {
		t.Fatalf("baseline scored %d: %+v", res.Score(), res.Checks)
	}
	if err := BaselineHolds(res); err != nil {
		t.Fatalf("BaselineHolds: %v", err)
	}
	// Witness-sourced checks observed nothing, which is the expected
	// shape; the answer-sourced one is the informative failure that
	// makes the zero mean something.
	byName := map[string]CheckResult{}
	for _, ck := range res.Checks {
		byName[ck.Name] = ck
	}
	if !byName["looked-around"].Vacuous || !byName["changed-nothing"].Vacuous {
		t.Fatalf("witness checks should be vacuous in the baseline: %+v", res.Checks)
	}
	if byName["named-it"].Vacuous || byName["named-it"].Passed {
		t.Fatalf("the answer check should be an informative failure: %+v", byName["named-it"])
	}
}

// The fixture's tools directory is on PATH for the tools tier only.
// Leaving it there for the baseline would be harmless today, but it
// would make the baseline's zero depend on the tool suite staying off
// rather than on the fixture being out of reach.
func TestBaselineDoesNotGetTheFixtureOnPath(t *testing.T) {
	c, f, agent := runHarness(t)
	r := &Runner{Binary: agent, Timeout: 30 * time.Second}

	// The stub reaches for the tool in its default mode. With no PATH
	// entry the call fails, so the witness is never created — even
	// though the agent tried.
	res, err := r.Run(context.Background(), c, f, TierNoAccess)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, ck := range res.Checks {
		if ck.Name == "looked-around" && !ck.Vacuous {
			t.Fatalf("the witness should never have been written: %+v", ck)
		}
	}
}

// A run that crashed still leaves whatever witnesses it wrote, and a
// report saying "the checks say X and the process also exited non-zero"
// is more use than a bare exit code. The verdict can never be a pass.
func TestRunnerRecordsAProcessFailureWithoutAbandoningTheChecks(t *testing.T) {
	c, f, agent := runHarness(t)
	t.Setenv("STUB_MODE", "crash")
	r := &Runner{Binary: agent, Timeout: 30 * time.Second}

	res, err := r.Run(context.Background(), c, f, TierTools)
	if err != nil {
		t.Fatalf("Run should grade a crashed run, not error: %v", err)
	}
	if res.RunError == "" {
		t.Fatal("want the process failure recorded")
	}
	if !strings.Contains(res.RunError, "stub: exploded") {
		t.Fatalf("RunError should carry the tail of stderr: %q", res.RunError)
	}
	if res.Verdict() == VerdictPass {
		t.Fatal("a crashed run must never pass")
	}
}

func TestRunnerBindsBeforeSpendingAnything(t *testing.T) {
	c, f, agent := runHarness(t)
	// Leak the workload into the prompt: Bind must refuse before the
	// agent is ever started.
	c.Prompt = "look at ledger-writer and tell me what is wrong"
	r := &Runner{Binary: agent, Timeout: 30 * time.Second}

	if _, err := r.Run(context.Background(), c, f, TierTools); err == nil || !strings.Contains(err.Error(), "withhold the location") {
		t.Fatalf("want a leak refusal, got %v", err)
	}
}

func TestRunnerTimesOut(t *testing.T) {
	c, f, _ := runHarness(t)
	slow := filepath.Join(t.TempDir(), "slow-agent")
	mustWrite(t, slow, "#!/bin/sh\nsleep 30\n", 0o755)
	r := &Runner{Binary: slow, Timeout: 300 * time.Millisecond}

	start := time.Now()
	res, err := r.Run(context.Background(), c, f, TierTools)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Fatalf("the timeout did not bound the run: %s", elapsed)
	}
	if !strings.Contains(res.RunError, "did not finish within") {
		t.Fatalf("RunError = %q", res.RunError)
	}
}

// The world is kept when a tools-tier result is indeterminate, because
// that is the case a reader most needs the directory for — and removed
// otherwise, so /tmp does not fill with copies of runs that behaved.
func TestRunnerKeepsTheWorldOnlyWhenItIsWorthKeeping(t *testing.T) {
	c, f, agent := runHarness(t)

	t.Run("clean run is cleaned up", func(t *testing.T) {
		r := &Runner{Binary: agent, Timeout: 30 * time.Second}
		var kept string
		r.Log = func(format string, args ...any) {
			if strings.Contains(format, "materialized at") {
				kept = args[0].(string)
			}
		}
		if _, err := r.Run(context.Background(), c, f, TierTools); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(kept); !os.IsNotExist(err) {
			t.Fatalf("world %s should be gone, stat err = %v", kept, err)
		}
	})

	t.Run("indeterminate tools run is kept", func(t *testing.T) {
		t.Setenv("STUB_MODE", "mute")
		r := &Runner{Binary: agent, Timeout: 30 * time.Second}
		var kept string
		r.Log = func(format string, args ...any) {
			if strings.Contains(format, "materialized at") {
				kept = args[0].(string)
			}
		}
		res, err := r.Run(context.Background(), c, f, TierTools)
		if err != nil {
			t.Fatal(err)
		}
		if !res.Vacuous() {
			t.Fatalf("want an indeterminate result, got %+v", res.Checks)
		}
		if _, err := os.Stat(kept); err != nil {
			t.Fatalf("world %s should have been kept: %v", kept, err)
		}
		_ = os.RemoveAll(kept)
	})

	// The baseline is vacuous by design, so keeping its world every time
	// would just fill /tmp with copies of a run that behaved.
	t.Run("vacuous baseline is not kept", func(t *testing.T) {
		t.Setenv("STUB_MODE", "blind")
		r := &Runner{Binary: agent, Timeout: 30 * time.Second}
		var kept string
		r.Log = func(format string, args ...any) {
			if strings.Contains(format, "materialized at") {
				kept = args[0].(string)
			}
		}
		res, err := r.Run(context.Background(), c, f, TierNoAccess)
		if err != nil {
			t.Fatal(err)
		}
		if !res.Vacuous() {
			t.Fatal("the baseline should be vacuous")
		}
		if _, err := os.Stat(kept); !os.IsNotExist(err) {
			t.Fatalf("baseline world %s should be gone, stat err = %v", kept, err)
		}
	})
}
