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

// Command trajectory prints what an agent did, in order, out of a
// recorded GKE-drill run.
//
//	go run ./dev/trajectory/cmd/trajectory ~/.gke-drill/runs/20260911T110503Z-a
//	go run ./dev/trajectory/cmd/trajectory --all ~/.gke-drill/runs
//	go run ./dev/trajectory/cmd/trajectory --json <run> | jq '.steps[]'
//
// This is an instrument, not a gate. It prints and exits 0 whatever it
// finds; a non-zero exit ALWAYS means the tool itself failed, never that
// a run looked bad. Same convention as dev/lookout-pin-check, and for
// the same reason: `go run` collapses every non-zero child status to 1,
// so a tool that exits non-zero to signal a finding is indistinguishable
// from a tool that failed to build.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/go-steer/core-agent/v2/dev/trajectory"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "trajectory: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	all := flag.Bool("all", false, "treat the argument as a directory OF run directories and read every one")
	asJSON := flag.Bool("json", false, "emit the loaded trajectories as JSON instead of a table")
	flag.Usage = usage
	flag.Parse()

	if flag.NArg() != 1 {
		flag.Usage()
		return fmt.Errorf("want exactly one run directory, got %d arguments", flag.NArg())
	}
	dir := flag.Arg(0)

	var runs []*trajectory.Trajectory
	var err error
	if *all {
		runs, err = trajectory.LoadRuns(dir)
	} else {
		var t *trajectory.Trajectory
		t, err = trajectory.LoadRun(dir)
		runs = []*trajectory.Trajectory{t}
	}
	if err != nil {
		return err
	}
	if len(runs) == 0 {
		return fmt.Errorf("no runs under %s (a run directory is one containing transcript.jsonl)", dir)
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(runs)
	}

	for i, t := range runs {
		if i > 0 {
			fmt.Println(strings.Repeat("─", 72))
		}
		if err := trajectory.WriteSteps(os.Stdout, t); err != nil {
			return err
		}
	}
	if len(runs) > 1 {
		fmt.Println(strings.Repeat("─", 72))
		writeCorpus(runs)
	}
	return nil
}

// writeCorpus prints one line per run so a backfill over the whole
// archive can be read at a glance.
func writeCorpus(runs []*trajectory.Trajectory) {
	fmt.Printf("\n%d runs\n\n", len(runs))
	fmt.Printf("  %-26s %-3s %6s %6s %6s %8s  %s\n",
		"run", "scn", "frames", "steps", "notok", "cost", "agents")
	for _, t := range runs {
		notOK := 0
		agents := map[string]int{}
		for _, s := range t.Steps {
			if s.Status != trajectory.StatusOK {
				notOK++
			}
			agents[s.Agent]++
		}
		names := make([]string, 0, len(agents))
		for a := range agents {
			names = append(names, a)
		}
		sort.Strings(names)
		for i, a := range names {
			names[i] = fmt.Sprintf("%s:%d", a, agents[a])
		}
		fmt.Printf("  %-26s %-3s %6d %6d %6d %8.4f  %s\n",
			shorten(t.Meta.RunID), t.Meta.ScenarioID, len(t.Frames), len(t.Steps),
			notOK, t.Usage.CostUSD, strings.Join(names, " "))
	}
}

func shorten(id string) string {
	id = filepath.Base(id)
	if len(id) > 26 {
		return id[:26]
	}
	return id
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: trajectory [--all] [--json] <dir>

Reads a GKE-drill run directory (meta.json + transcript.jsonl +
subagents.json) and prints the ordered tool calls with their results.

  --all    <dir> holds run directories; read all of them
  --json   emit the parsed trajectories instead of the table

Exits non-zero only if the tool itself failed. It renders no verdict.
`)
	flag.PrintDefaults()
}
