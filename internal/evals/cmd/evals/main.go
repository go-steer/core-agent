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

// Command evals runs the v0.3 parity eval suite (docs/v0.3-plan.md W0.4).
//
// Normally invoked through scripts/evals.sh, which is what
// dev/ci/presubmits/evals.sh and the ci.yml evals job run. Direct use:
//
//	go run ./internal/evals/cmd/evals              # deterministic tier
//	go run ./internal/evals/cmd/evals --format=json
//	go run ./internal/evals/cmd/evals --tier=judge # metered; needs credentials
//	go run ./internal/evals/cmd/evals --tier=judge --baseline=last.json --out=board.json
//
// Exit codes mirror the harness's own three-valued outcome, because the
// distinction matters to whoever reads the CI log:
//
//	0  the suite gates green
//	1  a scenario missed its declared outcome, or a metric is a constant
//	2  the harness could not run — bad fixture, unknown tier, no credentials
//
// The judge tier reports rather than gates, so a low score exits 0. It
// exits 1 only when the board is incomplete: a row that did not run, or
// a metric that scores nothing anywhere.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-steer/core-agent/v3/internal/evals/evalcompose"
	"github.com/go-steer/core-agent/v3/internal/evals/harness"
)

func main() {
	var (
		tier     = flag.String("tier", harness.TierDeterministic, "which tier to run: deterministic (free, gating) or judge (metered, live credentials)")
		root     = flag.String("root", "", "repository root; defaults to the nearest ancestor with a go.mod")
		format   = flag.String("format", "text", "output format: text or json")
		model    = flag.String("model", "", "judge tier: the model under test (default: the Anthropic default)")
		grader   = flag.String("grader", "", "judge tier: the model that scores response_quality (default: the small Anthropic model)")
		provider = flag.String("provider", "", "judge tier: gemini, vertex, anthropic, anthropic-vertex, or a provider profile such as vertex-maas (default: whichever the environment provides)")
		rows     = flag.String("rows", "", "judge tier: comma-separated scenario ids to run instead of the whole corpus (a cost probe, or re-running one row)")
		baseline = flag.String("baseline", "", "a previous board (--format=json output) to report this run's delta against")
		out      = flag.String("out", "", "also write the JSON board to this path, whatever --format prints")
		cache    = flag.String("prompt-cache", "", "judge tier: Claude prompt caching for the model under test, as a bundle's prompt_cache block would set it: off, 5m or 1h (default: the product default, 5m)")
	)
	flag.Parse()

	// Declared provider profiles (.agents/providers/*.yaml), so a judge
	// run can name one — a profile with tiers is what J-cost-tier needs
	// to price a tiered roster on a provider beyond Gemini and Claude.
	declared, err := evalcompose.DiscoverProviders()
	if err != nil {
		fmt.Fprintln(os.Stderr, "evals:", err)
		os.Exit(2)
	}
	evalcompose.RegisterProfiles(declared)

	pc, err := promptCache(*cache)
	if err != nil {
		fmt.Fprintln(os.Stderr, "evals:", err)
		os.Exit(2)
	}

	err = run(harness.Config{
		Tier:     *tier,
		Root:     *root,
		Model:    *model,
		Grader:   *grader,
		Provider: *provider,
		Rows:     splitRows(*rows),
		// The bundle block an eval run does not have.
		PromptCache: pc,
		// stderr, so a run piped through tee still writes a clean board.
		Progress: os.Stderr,
	}, *format, *baseline, *out)
	switch {
	case err == nil:
	case errors.Is(err, errFailed):
		// The report already said what is red and why; repeating it on
		// stderr would only bury it.
		os.Exit(1)
	default:
		fmt.Fprintf(os.Stderr, "evals: %v\n", err)
		os.Exit(2)
	}
}

// errFailed marks the "suite ran and something is red" case, so main can
// tell it apart from "the harness could not run".
var errFailed = errors.New("suite failed")

func run(cfg harness.Config, format, baseline, out string) error {
	if cfg.Root == "" {
		r, err := findRoot()
		if err != nil {
			return err
		}
		cfg.Root = r
	}
	if format != "text" && format != "json" {
		return fmt.Errorf("unknown format %q (want text or json)", format)
	}

	// Read the baseline before spending a metered run on a board that
	// then turns out to have nothing to compare against.
	var prev harness.Summary
	if baseline != "" {
		p, err := harness.LoadSummary(baseline)
		if err != nil {
			return err
		}
		prev = p
	}

	sum, err := harness.Run(context.Background(), cfg)
	if err != nil {
		return err
	}

	// A judge run costs money and minutes; losing it because the
	// delta or the terminal failed afterwards would be the expensive
	// kind of avoidable.
	if out != "" {
		f, err := os.Create(out)
		if err != nil {
			return err
		}
		werr := sum.WriteJSON(f)
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			return werr
		}
	}

	switch format {
	case "json":
		if err := sum.WriteJSON(os.Stdout); err != nil {
			return err
		}
	default:
		sum.WriteText(os.Stdout)
		if baseline != "" {
			sum.WriteDelta(os.Stdout, prev)
		}
	}
	if !sum.OK() {
		return errFailed
	}
	return nil
}

// findRoot walks up from the working directory for the module root, so
// the command works from anywhere in the tree rather than only from the
// directory the fixtures happen to be relative to.
func findRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.mod in any parent of the working directory; pass --root")
		}
		dir = parent
	}
}

// promptCache maps --prompt-cache onto the bundle block it stands in
// for. Unset is the block left out: the product default.
func promptCache(s string) (evalcompose.PromptCache, error) {
	on, off := true, false
	switch s {
	case "":
		return evalcompose.PromptCache{}, nil
	case "off":
		return evalcompose.PromptCache{Enabled: &off}, nil
	case evalcompose.PromptCacheTTL5m, evalcompose.PromptCacheTTL1h:
		return evalcompose.PromptCache{Enabled: &on, TTL: s}, nil
	}
	return evalcompose.PromptCache{}, fmt.Errorf("--prompt-cache=%q: want off, %s or %s", s, evalcompose.PromptCacheTTL5m, evalcompose.PromptCacheTTL1h)
}

func splitRows(s string) []string {
	var out []string
	for _, r := range strings.Split(s, ",") {
		if r = strings.TrimSpace(r); r != "" {
			out = append(out, r)
		}
	}
	return out
}
