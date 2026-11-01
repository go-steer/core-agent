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

// Command outcome runs the O tier: the outcome evals (#297,
// docs/outcome-evals-design.md).
//
// It is the only tier that needs all three of a real model, a real
// cluster and a merge-blocking verdict. Everything the S/U/E tiers gate
// on is a property of how mast is built; scoring a trajectory needs a
// model that chooses, and a scripted provider does not choose.
//
// Normally invoked through scripts/outcome.sh, which is what the
// `outcome` job in .github/workflows/outcome.yml runs. Direct use:
//
//	go run ./internal/evals/cmd/outcome
//	go run ./internal/evals/cmd/outcome --model=claude-opus-5
//	go run ./internal/evals/cmd/outcome --keep    # leave the cluster up
//
// Prerequisites: `kind`, `kubectl`, a container runtime, a pinned
// `lookout` on PATH (see outcome.PinnedLookout), and provider
// credentials. IT CREATES A KUBERNETES CLUSTER and deletes it on the way
// out, including on every failure path.
//
// Exit codes, the same three-valued shape the evals command uses:
//
//	0  the board is green
//	1  the board is red — a case failed every repetition, a required
//	   check measured nothing, a catastrophic safeguard tripped, or the
//	   pass did not finish its roster
//	2  the tier could not run — no cluster, no lookout, no credentials
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/go-steer/core-agent/v3/internal/evals"
	"github.com/go-steer/core-agent/v3/internal/evals/evalcompose"
	"github.com/go-steer/core-agent/v3/internal/evals/outcome"
	"github.com/go-steer/core-agent/v3/internal/modelretry"
)

// defaultModel is what the gate measures unless told otherwise.
//
// The mid tier rather than mast's frontier default, and the reason is
// what this tier is for. It gates a *substrate* claim — that mast's read
// path, tool surface, trace adapter and cluster safeguards hold up end
// to end with a model that chooses — not a frontier-capability claim.
// All three admitted cases are one OOM diagnosis from a named namespace,
// comfortably inside this tier's range, so the capability headroom a
// frontier model buys is headroom the corpus does not spend, at roughly
// five times the bill on every pull request.
//
// The same discipline as §7's wall-clock ceiling, applied to money:
// decide the number first and let it constrain what runs. --model asks a
// capability question of anything else without changing what gates.
const defaultModel = "claude-sonnet-5"

func main() {
	var (
		model    = flag.String("model", defaultModel, "the model under test")
		provider = flag.String("provider", "", "gemini, vertex, anthropic, or anthropic-vertex (default: whichever the environment provides)")
		root     = flag.String("root", "", "repository root; defaults to the nearest ancestor with a go.mod")
		lookout  = flag.String("lookout", "", "path to the k8s-lookout binary (default: `lookout` on PATH)")
		ceiling  = flag.Duration("ceiling", outcome.DefaultCeiling, "wall clock the whole pass is allowed")
		keep     = flag.Bool("keep", false, "leave the cluster up afterwards, for reading a red cell by hand")
		printPin = flag.Bool("print-lookout-pin", false, "print the pinned k8s-lookout version and exit")
		printImg = flag.Bool("print-images", false, "print the fixture images that must be pulled locally, and exit")
	)
	flag.Parse()

	// The workflow installs lookout at this pin and this command refuses
	// a build that does not match what the intent table needs, so the
	// two have to be the same string. Read out of Go rather than
	// duplicated into YAML, which is where it would drift.
	if *printPin {
		fmt.Println(outcome.PinnedLookout)
		return
	}

	// Same single-source-of-truth argument as the pin. The cluster is
	// never allowed to reach a registry, so the fixture images have to be
	// on the host before a pass starts — and the list is collected from
	// the manifests, so a caller that needs it should ask rather than
	// keep a second copy that can disagree with what the fixtures run.
	if *printImg {
		imgs, err := images(*root)
		if err != nil {
			fmt.Fprintf(os.Stderr, "outcome: %v\n", err)
			os.Exit(2)
		}
		for _, img := range imgs {
			fmt.Println(img)
		}
		return
	}

	err := run(context.Background(), options{
		model:    *model,
		provider: *provider,
		root:     *root,
		lookout:  *lookout,
		ceiling:  *ceiling,
		keep:     *keep,
	})
	switch {
	case err == nil:
	case errors.Is(err, errRed):
		// The board already said what is red and why.
		os.Exit(1)
	default:
		fmt.Fprintf(os.Stderr, "outcome: %v\n", err)
		os.Exit(2)
	}
}

// errRed marks "the tier ran and the board is red", so main can tell it
// from "the tier could not run". The distinction is the whole point of
// the exit codes: one of them is a finding about mast and the other is a
// finding about the machine.
var errRed = errors.New("the board is red")

type options struct {
	model, provider, root, lookout string
	ceiling                        time.Duration
	keep                           bool
}

// loadCorpus resolves the root if it was not given and loads the intent
// table and the corpus from it. Both entry points go through here: the
// image list is only correct if it comes from the same manifests the
// pass will side-load.
func loadCorpus(root string) (string, string, evals.IntentTable, outcome.Corpus, error) {
	if root == "" {
		r, err := findRoot()
		if err != nil {
			return "", "", evals.IntentTable{}, outcome.Corpus{}, err
		}
		root = r
	}
	dir := filepath.Join(root, "testdata", "outcome")
	tbl, err := evals.LoadIntentTable(filepath.Join(root, "testdata", "evals", "intents.yaml"))
	if err != nil {
		return "", "", evals.IntentTable{}, outcome.Corpus{}, err
	}
	corpus, err := outcome.Load(dir, tbl)
	if err != nil {
		return "", "", evals.IntentTable{}, outcome.Corpus{}, err
	}
	return root, dir, tbl, corpus, nil
}

// images is the fixture image list, read out of the manifests through
// the same staging provisioner the pass builds.
func images(root string) ([]string, error) {
	_, dir, _, corpus, err := loadCorpus(root)
	if err != nil {
		return nil, err
	}
	staging, err := outcome.NewProvisioner(corpus, &outcome.Cluster{}, dir)
	if err != nil {
		return nil, err
	}
	return staging.Images(), nil
}

func run(ctx context.Context, opt options) error {
	note := func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, format+"\n", args...)
	}

	// Load and validate before anything is created or spent. A corpus
	// that does not load is not worth a cluster, and a cluster is not
	// worth a model.
	root, corpusDir, tbl, corpus, err := loadCorpus(opt.root)
	if err != nil {
		return err
	}
	opt.root = root
	note("[corpus] %d case(s), %d run(s), ceiling %s", len(corpus.Cases), corpus.Runs(), opt.ceiling)

	// The model next, because "no credentials" is the most common reason
	// this cannot run and the cheapest one to discover.
	raw, err := evalcompose.BuildModel(ctx, opt.provider, opt.model, evalcompose.ModelOptions{})
	if err != nil {
		return fmt.Errorf("build model: %w", err)
	}
	if evalcompose.IsOfflineFake(opt.model) {
		return fmt.Errorf("--model=%s is an offline fake — this tier exists because a scripted provider cannot be wrong about a diagnosis, so grading one would only assert the script equals itself", opt.model)
	}
	// A 429 is the provider asking us to wait, not a finding about mast
	// (#239). Without this a quota blip presents as a red merge gate.
	//
	// The judge tier's schedule, not the production one, for the same
	// reason the judge tier uses it: this pass is metered, sequential and
	// measured in minutes, so half a minute spent keeping a row is a
	// bargain, and an unattended turn's arithmetic is the opposite.
	retryCfg := modelretry.JudgeConfig()
	retryCfg.OnWait = func(_ string, attempt int, wait time.Duration, err error) {
		note("[retry] %v — waiting %s before attempt %d", err, wait, attempt+1)
	}
	retries := modelretry.New(retryCfg)
	under := retries.Wrap(raw)

	// Scratch under TMPDIR, never $HOME (house rule #5).
	scratch := filepath.Join(os.TempDir(), fmt.Sprintf("mast-outcome-%d", os.Getpid()))
	if err := os.RemoveAll(scratch); err != nil {
		return err
	}
	if err := os.MkdirAll(scratch, 0o750); err != nil {
		return err
	}

	// Images come from the manifests, so the side-load list cannot drift
	// from what the fixtures run. This provisioner is staging only — it
	// has no cluster yet.
	staging, err := outcome.NewProvisioner(corpus, &outcome.Cluster{}, corpusDir)
	if err != nil {
		return err
	}

	note("[cluster] creating")
	cl, err := outcome.CreateCluster(ctx, outcome.ClusterOptions{Dir: scratch, Images: staging.Images()})
	// The cluster can exist even when create returns an error — the
	// isolation check runs after kind has already made it — so teardown
	// is arranged before the error is handled.
	defer func() {
		if cl == nil {
			return // refused before kind was invoked
		}
		if opt.keep {
			note("[cluster] --keep: leaving %s up; kubeconfig %s", cl.Name, cl.Kubeconfig)
			return
		}
		// context.Background(): the pass's deadline may already have
		// expired, and a cluster that outlives a failed run is both a
		// leak and the thing the no-adopt rule trips over next time.
		if err := cl.Delete(context.Background()); err != nil {
			note("[cluster] teardown: %v", err)
		}
	}()
	if err != nil {
		return err
	}
	note("[cluster] %s up; kubeconfig %s", cl.Name, cl.Kubeconfig)

	surface, err := outcome.NewSurface(ctx, outcome.SurfaceConfig{
		Binary:  opt.lookout,
		Cluster: cl,
		Table:   tbl,
	})
	if err != nil {
		return err
	}
	note("[surface] %s — %d tool(s) advertised", surface.Version, len(surface.Tools))

	prov, err := outcome.NewProvisioner(corpus, cl, corpusDir)
	if err != nil {
		return err
	}
	runner, err := outcome.NewRunner(outcome.RunnerConfig{
		Corpus:      corpus,
		Table:       tbl,
		Cluster:     cl,
		Provisioner: prov,
		Surface:     surface,
		Model:       under,
		Scratch:     scratch,
		Ceiling:     opt.ceiling,
		Progress:    note,
	})
	if err != nil {
		return err
	}

	pass, err := runner.Run(ctx)
	if err != nil {
		return err
	}
	report(pass, retries)
	if red, _ := pass.Board.Red(); red {
		return errRed
	}
	return nil
}

// report prints the board and the line that says what produced it.
func report(pass outcome.Pass, retries *modelretry.Policy) {
	fmt.Print(pass.Board.Summary())
	fmt.Printf("\nmodel %s, surface %s, %s of a %s ceiling\n",
		pass.Model, pass.Surface, pass.Elapsed.Round(time.Second), pass.Ceiling)
	// This tier wrapped its model in a retry from the day it was written
	// and then never read the counter, so a pass that only completed
	// because it waited out two quota blips was indistinguishable from
	// one that never met a provider under pressure — and the elapsed
	// figure above, which the ceiling is judged against, silently
	// included the waiting. Printed only when non-zero: a healthy pass
	// should not have to say that nothing went wrong.
	if n, _, waited := retries.Stats(); n > 0 {
		fmt.Printf("%d model call(s) only completed after waiting out a transient provider error, costing %s of the elapsed figure above\n",
			n, waited.Round(time.Second))
	}
	if pass.TimedOut() {
		fmt.Println("the pass hit its ceiling: the roster no longer fits the budget, and the board is short rather than green")
	}
	red, reasons := pass.Board.Red()
	if !red {
		fmt.Println("\nthe board is green")
		return
	}
	fmt.Println("\nthe board is RED:")
	for _, r := range reasons {
		fmt.Printf("  - %s\n", r)
	}
}

// findRoot walks up for the nearest ancestor with a go.mod.
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
			return "", fmt.Errorf("no go.mod in any ancestor of the working directory; pass --root")
		}
		dir = parent
	}
}
