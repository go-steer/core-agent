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

// evalrun runs one eval case at both tiers and reports (#652).
//
// It is the thin part. Everything worth arguing with lives in
// internal/evals; this binary parses flags, decides an exit code, and
// prints something a person can read at 2am.
//
// Exit codes:
//
//	0  the tools tier passed and the no-access baseline held
//	1  a check failed, or the baseline scored, or the harness errored
//	2  the report is indeterminate — some check observed nothing
//
// 2 is separate from 1 on purpose. A failing check says the agent did
// the wrong thing; an indeterminate one says the harness did not find
// out what the agent did, and the two send an operator to different
// places. Both are non-zero: an unanswered question is not a pass.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/go-steer/core-agent/v2/internal/evals"
)

func main() {
	casePath := flag.String("case", "", "path to the case JSON (required)")
	fixtures := flag.String("fixtures", "dev/evals/fixtures", "fixtures root")
	binary := flag.String("binary", "", "core-agent binary under test (required)")
	agentArgs := flag.String("agent-args", "", "extra arguments passed to the agent before -p, space-separated (e.g. \"--provider=vertex\")")
	timeout := flag.Duration("timeout", evals.DefaultTimeout, "per-tier wall-clock bound")
	tiers := flag.String("tiers", "tools,no-access", "comma-separated tiers to run")
	jsonOut := flag.String("json", "", "also write the full report to this path")
	keep := flag.Bool("keep-world", false, "keep the materialized fixture after the run")
	flag.Parse()

	if *casePath == "" || *binary == "" {
		fmt.Fprintln(os.Stderr, "evalrun: --case and --binary are required")
		flag.Usage()
		os.Exit(1)
	}

	if err := run(*casePath, *fixtures, *binary, *agentArgs, *tiers, *jsonOut, *timeout, *keep); err != nil {
		var ind indeterminate
		if asIndeterminate(err, &ind) {
			fmt.Fprintf(os.Stderr, "evalrun: INDETERMINATE — %s\n", ind.why)
			os.Exit(2)
		}
		fmt.Fprintf(os.Stderr, "evalrun: %v\n", err)
		os.Exit(1)
	}
}

type indeterminate struct{ why string }

func (i indeterminate) Error() string { return "indeterminate: " + i.why }

func asIndeterminate(err error, out *indeterminate) bool {
	if i, ok := err.(indeterminate); ok {
		*out = i
		return true
	}
	return false
}

func run(casePath, fixturesRoot, binary, agentArgs, tierList, jsonOut string, timeout time.Duration, keep bool) error {
	c, err := evals.LoadCase(casePath)
	if err != nil {
		return err
	}
	f, err := evals.LoadFixture(fixturesRoot, c.Fixture)
	if err != nil {
		return err
	}
	// Bind here as well as inside the runner. It costs nothing and it
	// moves the two failures Bind owns — an unknown fact key and a
	// prompt that leaks the location — to before the first provider
	// call, where they cost no money and no wall clock.
	if _, err := c.Bind(f); err != nil {
		return err
	}

	abs, err := filepath.Abs(binary)
	if err != nil {
		return err
	}
	runner := &evals.Runner{
		Binary:    abs,
		Args:      splitArgs(agentArgs),
		Timeout:   timeout,
		KeepWorld: keep,
		Log:       func(format string, args ...any) { fmt.Printf("   · "+format+"\n", args...) },
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Printf("case %s — %s\n", c.ID, f.Description)
	fmt.Printf("planted defect: %s\n\n", c.PlantedDefect)

	var report evals.Report
	var baselineErr error
	baselineRan := false
	for _, t := range strings.Split(tierList, ",") {
		tier := evals.Tier(strings.TrimSpace(t))
		if tier == "" {
			continue
		}
		fmt.Printf("── tier %s ──────────────────────────────────────\n", tier)
		res, err := runner.Run(ctx, c, f, tier)
		if err != nil {
			return err
		}
		printResult(res)
		report.Results = append(report.Results, res)
		if tier == evals.TierNoAccess {
			baselineRan = true
			baselineErr = evals.BaselineHolds(res)
		}
	}

	if jsonOut != "" {
		blob, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(jsonOut, append(blob, '\n'), 0o644); err != nil {
			return err
		}
		fmt.Printf("report written to %s\n", jsonOut)
	}

	// The baseline is reported before the verdict, and it wins. A
	// tools-tier pass on a case whose checks a naked model can also
	// satisfy is not evidence of anything, so there is no order of
	// results in which it should be printed as the headline.
	if baselineErr != nil {
		return baselineErr
	}

	switch v := report.Verdict(); v {
	case evals.VerdictPass:
		// A tools-tier pass with no baseline behind it is the exact
		// shape of green this harness exists to refuse, so --tiers can
		// narrow the run for debugging but cannot buy a zero exit with
		// it. The per-check lines above are still the useful output.
		if !baselineRan {
			return fmt.Errorf("the tools tier passed but %q was not in --tiers, so nothing checked whether a model with no access passes the same checks — this is not a result", evals.TierNoAccess)
		}
		fmt.Println("\nevalrun: PASS")
		return nil
	case evals.VerdictIndeterminate:
		return indeterminate{why: indeterminateWhy(report)}
	default:
		return fmt.Errorf("FAIL — see the per-check reasons above")
	}
}

// indeterminateWhy names the checks that observed nothing, because
// "indeterminate" with no referent is the least actionable word a
// harness can print.
func indeterminateWhy(rep evals.Report) string {
	var parts []string
	for _, r := range rep.Results {
		if r.Tier == evals.TierNoAccess {
			// Vacuity in the baseline is the expected shape, not a
			// finding: no witness is written because nothing reached
			// the world. Naming those here would bury the one line the
			// reader needs under the two that are working as designed.
			continue
		}
		for _, c := range r.Checks {
			if c.Vacuous {
				parts = append(parts, fmt.Sprintf("%s/%s: %s", r.Tier, c.Name, c.Reason))
			}
		}
	}
	if len(parts) == 0 {
		return "no results"
	}
	return strings.Join(parts, "; ")
}

func printResult(r evals.Result) {
	for _, c := range r.Checks {
		mark := "FAIL"
		switch {
		case c.Vacuous:
			mark = "????"
		case c.Passed:
			mark = "PASS"
		}
		fmt.Printf("  [%s] %s — %s\n", mark, c.Name, c.Reason)
	}
	if r.RunError != "" {
		fmt.Printf("  agent process: %s\n", r.RunError)
	}
	fmt.Printf("  score %d/%d, verdict %s\n\n", r.Score(), len(r.Checks), r.Verdict())
}

func splitArgs(s string) []string {
	return strings.Fields(s)
}
