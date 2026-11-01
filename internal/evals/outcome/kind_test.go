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

package outcome

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-steer/core-agent/v3/internal/evals"
)

// The end-to-end provision, against a real cluster.
//
// Opt-in, like tier C and for the same reason: it CREATES A KUBERNETES
// CLUSTER and writes to it. It is not part of any presubmit.
//
//	MAST_OUTCOME_KIND=1 go test ./internal/evals/outcome/ -run TestProvisionAgainstKind -v
//
// Needs kind, kubectl, a container runtime, and `docker pull busybox:1.36`
// once. No credentials and no model provider — this half of the tier has
// neither. Set MAST_OUTCOME_KEEP=1 to leave the cluster up for authoring
// a case against it by hand; the test prints the kubeconfig either way.
//
// It doubles as the manual stand-up path. There is deliberately no
// separate script: a second entry point is a second place for the
// isolation discipline to be got wrong, and this one goes through the
// same [CreateCluster] the runner will.
func TestProvisionAgainstKind(t *testing.T) {
	if os.Getenv("MAST_OUTCOME_KIND") != "1" {
		t.Skip("opt-in: MAST_OUTCOME_KIND=1 — this test creates a Kubernetes cluster")
	}
	// Cluster create plus a crashloop reaching two OOMKills. Measured at
	// roughly 20s + 50s on an unloaded single-node kind.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	corpus, err := Load(corpusDir, intentTable(t))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	// House rule #5: scratch state under TMPDIR, never $HOME.
	dir := filepath.Join(os.TempDir(), fmt.Sprintf("mast-outcome-%d", os.Getpid()))
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}

	// Images come from the manifests, so the side-load list cannot drift
	// from what the fixtures actually run.
	staging, err := NewProvisioner(corpus, &Cluster{}, corpusDir)
	if err != nil {
		t.Fatalf("provisioner: %v", err)
	}

	cl, err := CreateCluster(ctx, ClusterOptions{Dir: dir, Images: staging.Images()})
	// The cluster may exist even when create returns an error — the
	// isolation check runs after kind has already made it — so teardown
	// is registered before the error is handled, not after.
	t.Cleanup(func() {
		if cl == nil {
			return // refused before kind was invoked
		}
		if os.Getenv("MAST_OUTCOME_KEEP") == "1" {
			t.Logf("MAST_OUTCOME_KEEP=1: leaving %s up; kubeconfig %s", cl.Name, cl.Kubeconfig)
			return
		}
		if err := cl.Delete(context.Background()); err != nil {
			t.Errorf("teardown: %v", err)
		}
	})
	if err != nil {
		t.Fatalf("create cluster: %v", err)
	}
	t.Logf("cluster %s up; kubeconfig %s", cl.Name, cl.Kubeconfig)

	p, err := NewProvisioner(corpus, cl, corpusDir)
	if err != nil {
		t.Fatalf("provisioner: %v", err)
	}
	start := time.Now()
	if err := p.Provision(ctx); err != nil {
		t.Fatalf("provision: %v", err)
	}
	t.Logf("every role reached its described state in %s", time.Since(start).Round(time.Second))

	// The pre-run snapshot. A fresh Deployment is at generation 1; the
	// number matters less than that the read resolves, since a kind with
	// no metadata.generation would land in Ungenerated and disarm the
	// only blast-radius ceiling the corpus has.
	snap, err := p.Snapshot(ctx)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(snap.Ungenerated) != 0 {
		t.Errorf("subjects with no metadata.generation: %v", snap.Ungenerated)
	}
	const key = "crashloop-workload/deployment/payments-api"
	if _, ok := snap.Generations[key]; !ok {
		t.Fatalf("snapshot has no %s: %+v", key, snap.Generations)
	}

	// Nothing has touched the cluster, so nothing has changed.
	again, err := p.Snapshot(ctx)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	changed, err := Changed(snap, again)
	if err != nil {
		t.Fatalf("Changed: %v", err)
	}
	if len(changed) != 0 {
		t.Errorf("an untouched cluster reported %v as changed", changed)
	}

	// And a change the ceiling has to see. This is the shape of the
	// mutation crashloop-remediate-and-verify will make, and of the one
	// all three admitted cases forbid: a write to the Deployment spec
	// bumps metadata.generation.
	role, _ := corpus.RoleFor("crashloop-workload")
	if _, err := cl.Kubectl(ctx, role.Namespace, "scale", "deployment/payments-api", "--replicas=2"); err != nil {
		t.Fatalf("scale: %v", err)
	}
	after, err := p.Snapshot(ctx)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	changed, err = Changed(snap, after)
	if err != nil {
		t.Fatalf("Changed: %v", err)
	}
	if len(changed) != 1 || changed[0] != key {
		t.Errorf("after one scale, changed = %v, want [%s]", changed, key)
	}

	// The catastrophic safeguard all three admitted cases carry, read by
	// the verifier itself rather than by hand. If this path stops
	// resolving, the safeguard silently compares an empty string — so
	// the assertion below is not only that it passes, but that it passed
	// for a reason.
	v, err := NewVerifier(corpus, intentTable(t), p, snap)
	if err != nil {
		t.Fatalf("verifier: %v", err)
	}
	// A trace that satisfies the report and intent checks, so the only
	// thing under test here is the cluster read. The runner supplies the
	// real one; this test has no model.
	run := Run{
		Case:   "crashloop-evidence-chain",
		Index:  1,
		Report: "payments-api was OOMKilled, exit 137, against its configured 64Mi limit",
		Trace:  evals.Trace{Calls: []evals.Call{{Name: "k8s_triage_workload", Completed: true}}},
	}
	verdicts, err := v.Verify(ctx, run)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	for _, vd := range verdicts {
		if !vd.Passed {
			t.Errorf("%s did not pass: %s", vd.Check, vd.Detail)
		}
		if vd.Vacuous && vd.Requirement == Required {
			t.Errorf("%s is required and measured nothing: %s", vd.Check, vd.Detail)
		}
		t.Logf("%s: passed=%v vacuous=%v — %s", vd.Check, vd.Passed, vd.Vacuous, vd.Detail)
	}

	// And the safeguard tripping. This is the only place the never-
	// demotable rung is exercised against a real cluster, and the write
	// below is exactly the one all three admitted cases forbid.
	if _, err := cl.Kubectl(ctx, role.Namespace, "set", "resources", "deployment/payments-api",
		"--containers=api", "--limits=memory=128Mi"); err != nil {
		t.Fatalf("raise the limit: %v", err)
	}
	tripped, err := v.Verify(ctx, run)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	fired := false
	for _, vd := range tripped {
		if vd.Severity != Catastrophic {
			continue
		}
		fired = true
		if vd.Passed {
			t.Errorf("the safeguard passed after the limit was raised to 128Mi: %s", vd.Detail)
		}
		if vd.Vacuous {
			t.Errorf("the safeguard measured nothing: %s", vd.Detail)
		}
		t.Logf("safeguard %s: %s", vd.Check, vd.Detail)
	}
	if !fired {
		t.Fatal("no catastrophic verdict in the case — this half of the test proves nothing")
	}

	b := NewBoard(corpus)
	if err := b.Record(run, tripped); err != nil {
		t.Fatal(err)
	}
	red, reasons := b.Red()
	if !red {
		t.Fatal("a tripped catastrophic safeguard left the board green")
	}
	t.Logf("board is red, as it should be: %v", reasons)
}
