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
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The isolation rule — "the ambient current-context is never resolved,
// on any path" — is the one thing in this package that has to hold
// without exception, because the failure it prevents is writing to
// somebody's real cluster. Three tests below hold it up between them:
// the flags are always both there, the environment never carries
// KUBECONFIG, and there is exactly one place that can exec anything.

func TestKubectlArgsAlwaysCarryBothFlags(t *testing.T) {
	for _, tc := range []struct {
		name      string
		namespace string
		args      []string
		wantNS    bool
	}{
		{name: "namespaced", namespace: "seeded-debug", args: []string{"get", "pods"}, wantNS: true},
		{name: "cluster scoped", namespace: "", args: []string{"get", "nodes"}},
		{name: "no args at all", namespace: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := kubectlArgs("/tmp/kc.yaml", "kind-mast-outcome-1", tc.namespace, tc.args)
			if i := slices.Index(got, "--kubeconfig"); i < 0 || got[i+1] != "/tmp/kc.yaml" {
				t.Errorf("no --kubeconfig in %v", got)
			}
			if i := slices.Index(got, "--context"); i < 0 || got[i+1] != "kind-mast-outcome-1" {
				t.Errorf("no --context in %v", got)
			}
			if hasNS := slices.Contains(got, "--namespace"); hasNS != tc.wantNS {
				t.Errorf("--namespace present = %v, want %v (%v)", hasNS, tc.wantNS, got)
			}
			// The flags come first: a subcommand's own positional args
			// must not be able to sit between kubectl and them.
			if len(tc.args) > 0 && slices.Index(got, tc.args[0]) < slices.Index(got, "--context") {
				t.Errorf("subcommand precedes the flags: %v", got)
			}
		})
	}
}

func TestCommandDropsKubeconfigFromTheEnvironment(t *testing.T) {
	t.Setenv("KUBECONFIG", "/home/someone/.kube/config")

	cl := &Cluster{Name: "mast-outcome-test", Context: "kind-mast-outcome-test"}
	cmd := cl.command(context.Background(), "kubectl", "version")
	for _, kv := range cmd.Env {
		if strings.HasPrefix(kv, "KUBECONFIG=") {
			t.Fatalf("KUBECONFIG survived into the command environment: %q", kv)
		}
	}
	// Stripped, not blanked. kubectl treats an empty value as unset, but
	// a tool that is not kubectl need not, and `kind` is one of the tools
	// this builds.
	if slices.Contains(cmd.Env, "KUBECONFIG=") {
		t.Error("KUBECONFIG was blanked rather than removed")
	}
	if len(cmd.Env) == 0 {
		t.Error("the environment was emptied rather than filtered")
	}
}

// TestOneExecPath is the test that makes the other two general. They
// prove the one construction path is safe; this proves there is only
// one, so a helper added later cannot quietly shell out without both
// flags and without the environment filtered.
func TestOneExecPath(t *testing.T) {
	// Every non-test file in the package, discovered rather than listed:
	// a hard-coded list is a guard that stops covering the package the
	// first time somebody adds a file to it.
	sources, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var offenders []string
	total := 0
	for _, name := range sources {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			if !strings.Contains(line, "exec.Command") {
				continue
			}
			total++
			// The single sanctioned site, inside (*Cluster).command.
			if name == "cluster.go" && strings.Contains(line, "cmd := exec.CommandContext(ctx, name, args...)") {
				continue
			}
			offenders = append(offenders, fileLine(name, i+1, line))
		}
	}
	if total == 0 {
		t.Fatal("found no exec.Command at all — this test is matching on a string that has moved, and is passing for the wrong reason")
	}
	if len(offenders) > 0 {
		t.Errorf("exec.Command outside (*Cluster).command, which is the only path that drops KUBECONFIG:\n  %s", strings.Join(offenders, "\n  "))
	}
}

func fileLine(name string, n int, line string) string {
	return name + ":" + strconv.Itoa(n) + ": " + strings.TrimSpace(line)
}

func TestVerifyKubeconfig(t *testing.T) {
	const want = "kind-mast-outcome-9"
	single := `
apiVersion: v1
current-context: kind-mast-outcome-9
contexts:
  - name: kind-mast-outcome-9
    context: {cluster: kind-mast-outcome-9, user: kind-mast-outcome-9}
`
	merged := `
apiVersion: v1
current-context: kind-mast-outcome-9
contexts:
  - name: kind-mast-outcome-9
    context: {cluster: kind-mast-outcome-9, user: kind-mast-outcome-9}
  - name: gke_prod_us-central1_payments
    context: {cluster: gke_prod, user: gke_prod}
`
	for _, tc := range []struct {
		name string
		yaml string
		want string
	}{
		{name: "the only context is ours", yaml: single},
		{name: "a merged kubeconfig", yaml: merged, want: "describes 2 contexts"},
		{
			name: "somebody else's cluster, alone in the file",
			yaml: strings.ReplaceAll(single, "kind-mast-outcome-9", "gke_prod"),
			want: `only context is "gke_prod"`,
		},
		{
			name: "our context, but current-context points elsewhere",
			yaml: strings.Replace(single, "current-context: kind-mast-outcome-9", "current-context: gke_prod", 1),
			want: "current-context is \"gke_prod\"",
		},
		{name: "no contexts at all", yaml: "apiVersion: v1\n", want: "describes 0 contexts"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := verifyKubeconfig([]byte(tc.yaml), want)
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("rejected a single-context kubeconfig: %v", err)
			case tc.want == "":
			case err == nil:
				t.Fatalf("accepted %s", tc.name)
			case !strings.Contains(err.Error(), tc.want):
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// The merged-kubeconfig case is the one worth naming twice: it is not
// hypothetical. kind MERGES into an existing kubeconfig rather than
// replacing it, so the refusal to start when the file already exists and
// this check are the same defence read from two ends.
func TestCreateClusterRefusesAnExistingKubeconfig(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "kubeconfig.yaml"), []byte("apiVersion: v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := CreateCluster(context.Background(), ClusterOptions{Dir: dir, Name: "mast-outcome-existing-kc"})
	if err == nil {
		t.Fatal("created a cluster over an existing kubeconfig")
	}
	if !strings.Contains(err.Error(), "kind merges") {
		t.Errorf("error %q does not say why an existing kubeconfig is fatal", err)
	}
}

func TestCreateClusterRefusesAnUnprefixedName(t *testing.T) {
	_, err := CreateCluster(context.Background(), ClusterOptions{Dir: t.TempDir(), Name: "prod"})
	if err == nil || !strings.Contains(err.Error(), "must start with") {
		t.Fatalf("accepted an unprefixed cluster name: %v", err)
	}
}

// Delete is the other side of the same guard. A Cluster value can be
// built by hand — the runner passes one around — so the name is checked
// again at the point of deletion rather than only at creation.
func TestDeleteRefusesAClusterItDidNotName(t *testing.T) {
	cl := &Cluster{Name: "gke-prod-payments", Context: "gke-prod"}
	err := cl.Delete(context.Background())
	if err == nil || !strings.Contains(err.Error(), "not one this package names") {
		t.Fatalf("Delete on a foreign cluster returned %v", err)
	}
}

func TestDeleteOnAnEmptyClusterIsANoop(t *testing.T) {
	if err := (*Cluster)(nil).Delete(context.Background()); err != nil {
		t.Errorf("Delete on a nil cluster: %v", err)
	}
	if err := (&Cluster{}).Delete(context.Background()); err != nil {
		t.Errorf("Delete on a zero cluster: %v", err)
	}
}
