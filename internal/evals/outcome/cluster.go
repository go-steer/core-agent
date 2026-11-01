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
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/go-steer/core-agent/v3/pkg/childenv"
)

// The cluster half of the provisioner. Substrate is kind, settled in
// docs/outcome-evals-design.md §4 by measurement rather than by cost:
// testdata/evals/intents.yaml has no rows for the hosted GKE MCP surface,
// so on GKE every intent_satisfied check in the corpus is vacuous and the
// aggregate would red on a fact about the table.
//
// # Isolation
//
// §4 says tier C's discipline "carries over verbatim", and this is where.
// scripts/live-kind-v0.4.sh enforces it four ways and each one is here:
//
//  1. the cluster is named mast-outcome-<pid>, created by us, deleted on
//     the way out, and an existing cluster of that name is refused rather
//     than adopted — it is by definition not one we made;
//  2. the kubeconfig lives under ${TMPDIR} and we refuse to start if it
//     already exists, because kind MERGES into an existing file;
//  3. after create, the kubeconfig is verified to describe exactly one
//     context, which is ours;
//  4. every kubectl runs with --kubeconfig and --context, from an
//     environment with KUBECONFIG unset.
//
// (3) is what makes the rest redundant: if the only file these processes
// read describes one cluster, a dropped flag downstream cannot reach
// another one. (4) is the rule stated as a rule — **the ambient
// current-context is never resolved, on any path** — and it is enforced
// mechanically rather than by review: [Cluster.command] is the only way
// this package builds a kubectl, it always sets both flags, and
// TestCommandNeverResolvesAmbientContext asserts that over every helper
// in the file.
//
// The one deliberate difference from tier C is (3)'s implementation. The
// shell verifies the context count by grepping for `- context:`; here the
// kubeconfig is parsed. A grep counts a string that can appear in a
// comment or a value, and this check is the one the other three lean on.

// clusterPrefix is the name every cluster this package creates starts
// with, and the guard every deletion checks. Distinct from tier C's
// `mast-live-` so a stray from one tier can never be deleted by the
// other — a cluster this package did not make is refused, not adopted.
const clusterPrefix = "mast-outcome-"

// kubectlTimeout bounds a single read. Generous for a kind control plane
// on a loaded CI box, and short enough that a hung apiserver fails the
// run rather than the job's wall clock.
const kubectlTimeout = 60 * time.Second

// Cluster is a throwaway kind cluster and the only kubeconfig anything
// here is allowed to read.
type Cluster struct {
	// Name is the kind cluster name; Context is kind's name for it.
	Name    string
	Context string
	// Kubeconfig is the single-context file under ${TMPDIR}.
	Kubeconfig string
}

// ClusterOptions configures [CreateCluster]. The zero value is what the
// runner uses.
type ClusterOptions struct {
	// Dir holds the kubeconfig. Defaults to ${TMPDIR}/mast-outcome-<pid>,
	// per house rule #5: scratch state never lands in $HOME.
	Dir string
	// Name overrides the cluster name. It must still carry the prefix, so
	// a test can name its own cluster without escaping the delete guard.
	Name string
	// Wait bounds kind's own readiness wait. Defaults to 3 minutes.
	Wait time.Duration
	// Images are side-loaded into the node after create, so nothing the
	// fixtures need is fetched from a registry at run time.
	Images []string
}

// CreateCluster stands up a throwaway cluster. The caller must call
// [Cluster.Delete], including on every failure path: a cluster that
// outlives a failed run is both a leak and the thing the no-adopt rule
// trips over next time.
func CreateCluster(ctx context.Context, opts ClusterOptions) (*Cluster, error) {
	name := opts.Name
	if name == "" {
		name = fmt.Sprintf("%s%d", clusterPrefix, os.Getpid())
	}
	if !strings.HasPrefix(name, clusterPrefix) {
		return nil, fmt.Errorf("outcome: cluster name %q must start with %q, so the teardown guard can tell a cluster we made from one we did not", name, clusterPrefix)
	}
	dir := opts.Dir
	if dir == "" {
		dir = filepath.Join(os.TempDir(), name)
	}
	wait := opts.Wait
	if wait == 0 {
		wait = 3 * time.Minute
	}

	cl := &Cluster{
		Name:       name,
		Context:    "kind-" + name,
		Kubeconfig: filepath.Join(dir, "kubeconfig.yaml"),
	}
	// The local refusals run before anything is executed, so a machine
	// with no kind installed still gets the refusal it earned rather
	// than a message about a missing binary.
	//
	// kind merges into an existing kubeconfig rather than replacing it,
	// which is how a second cluster gets into the file that (3) is
	// supposed to guarantee has one.
	if _, err := os.Stat(cl.Kubeconfig); err == nil {
		return nil, fmt.Errorf("outcome: %s already exists — kind merges into an existing kubeconfig", cl.Kubeconfig)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("outcome: %s: %w", cl.Kubeconfig, err)
	}

	existing, err := kindClusters(ctx)
	if err != nil {
		return nil, err
	}
	// Captured first, then matched. Piped into grep, this refusal failed
	// open in tier C once already: the match closes the pipe, kind takes
	// EPIPE, pipefail promotes its status, and the script goes on to
	// adopt the very cluster the message says it refuses to adopt.
	for _, c := range existing {
		if c == name {
			return nil, fmt.Errorf("outcome: cluster %q already exists — refusing to adopt a cluster this package did not create", name)
		}
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("outcome: cluster workdir: %w", err)
	}

	create := cl.command(ctx, "kind", "create", "cluster",
		"--name", cl.Name,
		"--kubeconfig", cl.Kubeconfig,
		"--wait", wait.String())
	if out, err := run(create); err != nil {
		return nil, fmt.Errorf("outcome: kind create cluster: %w\n%s", err, out)
	}

	if err := cl.verifyIsolation(); err != nil {
		// The cluster exists at this point, so the caller still has to
		// tear it down; hand it back alongside the refusal.
		return cl, err
	}
	for _, img := range opts.Images {
		if err := cl.LoadImage(ctx, img); err != nil {
			return cl, err
		}
	}
	return cl, nil
}

// Delete removes the cluster and its kubeconfig. Safe to call twice and
// on a half-built cluster, so it can go in a defer immediately.
func (c *Cluster) Delete(ctx context.Context) error {
	if c == nil || c.Name == "" {
		return nil
	}
	if !strings.HasPrefix(c.Name, clusterPrefix) {
		return fmt.Errorf("outcome: refusing to delete cluster %q: not one this package names", c.Name)
	}
	out, err := run(c.command(ctx, "kind", "delete", "cluster", "--name", c.Name))
	_ = os.Remove(c.Kubeconfig)
	if err != nil {
		return fmt.Errorf("outcome: kind delete cluster: %w\n%s", err, out)
	}
	return nil
}

// LoadImage side-loads a local image into the node. The fixtures run
// with imagePullPolicy: IfNotPresent, so this is what keeps a provision
// from depending on a registry.
func (c *Cluster) LoadImage(ctx context.Context, image string) error {
	// Refuse before kind does, with the fix in the message: kind's own
	// error for a missing image is about docker save, which does not
	// read as "run docker pull".
	if out, err := run(c.command(ctx, "docker", "image", "inspect", image)); err != nil {
		return fmt.Errorf("outcome: image %s is not present locally — `docker pull %s` first; the cluster is deliberately not allowed to reach a registry: %w\n%s", image, image, err, out)
	}
	if out, err := run(c.command(ctx, "kind", "load", "docker-image", image, "--name", c.Name)); err != nil {
		return fmt.Errorf("outcome: kind load docker-image %s: %w\n%s", image, err, out)
	}
	return nil
}

// Kubectl runs one kubectl against this cluster and only this cluster.
// namespace may be empty for a cluster-scoped call.
func (c *Cluster) Kubectl(ctx context.Context, namespace string, args ...string) (string, error) {
	full := kubectlArgs(c.Kubeconfig, c.Context, namespace, args)
	ctx, cancel := context.WithTimeout(ctx, kubectlTimeout)
	defer cancel()
	out, err := run(c.command(ctx, "kubectl", full...))
	if err != nil {
		return out, fmt.Errorf("kubectl %s: %w\n%s", strings.Join(args, " "), err, out)
	}
	return out, nil
}

// JSONPath is the read the verifiers are built on: one field of one
// object, as a string. An empty result and an absent object are the same
// string, so the second return distinguishes them.
func (c *Cluster) JSONPath(ctx context.Context, namespace, kind, name, path string) (value string, found bool, err error) {
	out, err := c.Kubectl(ctx, namespace, "get", kind, name, "-o", "jsonpath="+path)
	if err != nil {
		if isNotFound(out) {
			return "", false, nil
		}
		return "", false, err
	}
	return out, true, nil
}

// kubectlArgs prefixes both flags, always. Split out from [Cluster.Kubectl]
// so the rule is a pure function a test can assert over rather than
// something a reader has to take on trust.
func kubectlArgs(kubeconfig, kubecontext, namespace string, args []string) []string {
	full := []string{"--kubeconfig", kubeconfig, "--context", kubecontext}
	if namespace != "" {
		full = append(full, "--namespace", namespace)
	}
	return append(full, args...)
}

// command is the only place in this package that builds an external
// command, which is what makes the isolation rule checkable rather than
// a convention. KUBECONFIG is dropped from the environment on every
// path, including the ones that pass no kubeconfig flag at all
// (`kind get clusters`, `docker image inspect`) — so there is no helper
// here through which an ambient current-context could be resolved.
func (c *Cluster) command(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = envWithoutKubeconfig(childenv.Environ())
	// Deliberately no cmd.Dir: the working directory stays the caller's,
	// so a relative manifest path resolves against the corpus the way the
	// caller wrote it rather than against the cluster's scratch dir.
	return cmd
}

// envWithoutKubeconfig strips KUBECONFIG. Stripped rather than set to
// empty: client-go treats an empty KUBECONFIG as unset, but kubectl's
// own diagnostics echo it back, and a tool that is not kubectl may not
// make the same choice.
func envWithoutKubeconfig(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if strings.HasPrefix(kv, "KUBECONFIG=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func run(cmd *exec.Cmd) (string, error) {
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return strings.TrimRight(buf.String(), "\n"), err
}

func kindClusters(ctx context.Context) ([]string, error) {
	out, err := run((*Cluster)(nil).command(ctx, "kind", "get", "clusters"))
	if err != nil {
		// kind prints this to stderr and exits 0 on some versions and
		// non-zero on others; either way it means none.
		if strings.Contains(out, "No kind clusters found") {
			return nil, nil
		}
		return nil, fmt.Errorf("outcome: kind get clusters: %w\n%s", err, out)
	}
	var names []string
	for _, line := range strings.Split(out, "\n") {
		if s := strings.TrimSpace(line); s != "" && !strings.Contains(s, " ") {
			names = append(names, s)
		}
	}
	return names, nil
}

// kubeconfigShape is the part of a kubeconfig the isolation check reads.
type kubeconfigShape struct {
	CurrentContext string `yaml:"current-context"`
	Contexts       []struct {
		Name string `yaml:"name"`
	} `yaml:"contexts"`
}

// verifyIsolation is the check that makes the others redundant. Parsed
// rather than grepped: `- context:` is a string that can appear in a
// comment or inside a value, and this is the check the other three lean
// on.
func (c *Cluster) verifyIsolation() error {
	data, err := os.ReadFile(c.Kubeconfig)
	if err != nil {
		return fmt.Errorf("outcome: read kubeconfig: %w", err)
	}
	return verifyKubeconfig(data, c.Context)
}

func verifyKubeconfig(data []byte, want string) error {
	var kc kubeconfigShape
	if err := yaml.Unmarshal(data, &kc); err != nil {
		return fmt.Errorf("outcome: parse kubeconfig: %w", err)
	}
	if n := len(kc.Contexts); n != 1 {
		var names []string
		for _, ctx := range kc.Contexts {
			names = append(names, ctx.Name)
		}
		return fmt.Errorf("outcome: kubeconfig describes %d contexts (%s), want exactly 1 — refusing to run against a merged kubeconfig", n, strings.Join(names, ", "))
	}
	if got := kc.Contexts[0].Name; got != want {
		return fmt.Errorf("outcome: kubeconfig's only context is %q, want %q", got, want)
	}
	if kc.CurrentContext != want {
		return fmt.Errorf("outcome: kubeconfig current-context is %q, want %q", kc.CurrentContext, want)
	}
	return nil
}

func isNotFound(out string) bool {
	return strings.Contains(out, "NotFound") || strings.Contains(out, "not found")
}
