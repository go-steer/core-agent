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

package gkeplatformagent_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// #1249: `LEG=readonly ./scripts/set-up-demo.sh` after a LEG=d1/d2 deploy
// restored the read-only daemon config and left the gated-apply Role and
// RoleBinding standing in TARGET_NS. `kubectl apply -k` does not prune, so
// the daemon kept a patch grant on Deployments that the read-only posture
// says it lacks, and nothing in the output mentioned it. Only teardown.sh
// removed it.
//
// These tests EXECUTE set-up-demo.sh and teardown.sh, rather than scanning
// their text, against a copy of the recipe tree and a fakebin that answers
// for kubectl, kustomize and gcloud and records every call. Text scanning
// cannot tell a call from a mention — see AGENTS.md, "Testing that a shell
// script CALLS something" — and the property here is about ORDER and about
// which leg makes the call, which only a run can show.
//
// WHICH DIRECTION THIS MAY ERR, per check:
//
//   - "the read-only leg deletes the grant" and "the read-only leg runs
//     verify-gated-apply.sh denied" are must-find checks. The fakes are
//     stricter than the real tools — an invocation they do not recognise
//     exits 97, which fails the run wherever the script does not swallow
//     the exit (it does, deliberately, for the scale-to-0, the PVC delete,
//     the foreign-watcher scan and the boot-log grep) — so a script change
//     can make these fail when nothing is wrong. That is the allowed
//     direction: loud. They cannot pass on a mention, because a mention
//     never reaches the fake.
//
//   - "the d1/d2 legs do NOT delete the grant" is a must-not-find check,
//     where the directions invert: a run that aborts before the point where
//     the delete would happen also finds no delete, and would pass silently.
//     So each gated run must exit 0 AND print "✓ deployed" AND log its
//     `apply -k` of the gated overlay before the absence counts.
//
// The copy is what makes running set-up-demo.sh safe here: it rewrites
// tracked files with `sed -i`, and the tree under test must stay clean.
// Nothing can reach a real cluster: PATH puts the fakes first, KUBECONFIG
// and CLOUDSDK_CONFIG point at nothing, and HOME is a temp dir.

const (
	fakeContext  = "fake-context"
	fakeTargetNS = "fake-target-ns" // deliberately not the default, so a hardcoded namespace shows
)

// The fakes. Each logs its argv to ${FAKE_DIR}/calls.log, one line per call.
const fakeKubectl = `#!/usr/bin/env bash
set -uo pipefail
printf 'kubectl %s\n' "$*" >> "${FAKE_DIR}/calls.log"
a=" $* "
case "${a}" in
    *" version -o json "*) echo '{"serverVersion":{"minor":"35"}}' ;;
    *" get deploy -A "*) : ;;
    *" -n kube-system delete role,rolebinding "*) : ;;
    *" delete role,rolebinding "*) exit "${FAKE_REVOKE_RC:-0}" ;;
    *" scale deploy/core-agent "*|*" delete pvc "*|*" apply -k "*|*" rollout status "*) : ;;
    *" logs deploy/core-agent "*|*" delete namespace "*|*" delete clusterrole,clusterrolebinding "*) : ;;
    *) echo "fake kubectl: unexpected invocation: $*" >&2; exit 97 ;;
esac
`

// kustomize build renders just enough for set-up-demo.sh's assertions: the
// daemon's ServiceAccount (namespace check), the core-agent Deployment with
// the -c value READ from the patch file the script just wrote (so the
// rendered-config assertion still checks the script's write), an image line,
// and — for the gated overlay only — a gated-apply Role, which is what
// renders_gated_apply looks for.
const fakeKustomize = `#!/usr/bin/env bash
set -euo pipefail
printf 'kustomize %s\n' "$*" >> "${FAKE_DIR}/calls.log"
case "$1" in
    edit) exit 0 ;;
    build) dir="$2" ;;
    *) echo "fake kustomize: unexpected invocation: $*" >&2; exit 97 ;;
esac
deploy="$(cd "${dir}/../.." && pwd)"
case "${dir##*/}" in
    example)     patch="${dir}/patch-agent-config.yaml"; gated=0 ;;
    gated-apply) patch="${deploy}/components/gated-apply/patch-agent-config.yaml"; gated=1 ;;
    *) echo "fake kustomize: no render for ${dir}" >&2; exit 97 ;;
esac
cfg=$(sed -nE 's|^  value: (/.*/\.agents/config\..*)$|\1|p' "${patch}")
cat <<EOF
apiVersion: v1
kind: ServiceAccount
metadata:
  name: core-agent-daemon
  namespace: ${FAKE_DAEMON_NS}
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: core-agent
  namespace: ${FAKE_DAEMON_NS}
spec:
  template:
    spec:
      containers:
      - name: core-agent
        image: ghcr.io/go-steer/core-agent:fake
        args: ["daemon", "-c", "${cfg}"]
EOF
if (( gated )); then
    printf -- '---\napiVersion: rbac.authorization.k8s.io/v1\nkind: Role\nmetadata:\n  name: gated-apply-fake\n  namespace: x\n'
fi
`

const fakeGcloud = `#!/usr/bin/env bash
printf 'gcloud %s\n' "$*" >> "${FAKE_DIR}/calls.log"
case "$1 $2" in
    "projects describe"|"config get-value") exit 0 ;;
    *) echo "fake gcloud: unexpected invocation: $*" >&2; exit 97 ;;
esac
`

// Sibling scripts set-up-demo.sh calls by path. Replaced in the COPY with
// stubs that log their argv; verify-gated-apply.sh's exit code is settable
// so the failure path can be driven.
const stubSibling = `#!/usr/bin/env bash
printf '%s %s\n' "${0##*/}" "$*" >> "${FAKE_DIR}/calls.log"
if [[ "${0##*/}" == verify-gated-apply.sh ]]; then exit "${FAKE_VERIFY_RC:-0}"; fi
exit 0
`

type rigRun struct {
	out   string
	calls []string
	err   error
}

// index returns the first call line containing sub, or -1.
func (r rigRun) index(sub string) int {
	for i, c := range r.calls {
		if strings.Contains(c, sub) {
			return i
		}
	}
	return -1
}

func (r rigRun) dump() string {
	return fmt.Sprintf("--- output ---\n%s\n--- calls ---\n%s", r.out, strings.Join(r.calls, "\n"))
}

// newRig copies the recipe's scripts/ and deploy/ into a temp dir, stubs the
// sibling scripts, writes the fakebin, and returns the copy's recipe root.
func newRig(t *testing.T) (root string, run func(script string, env ...string) rigRun) {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skipf("bash not on PATH: %v", err)
	}
	for _, tool := range []string{"jq", "sed", "awk", "grep"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("set-up-demo.sh needs %s, which is not on PATH: %v", tool, err)
		}
	}
	// The rig runs under a bare PATH and a temp HOME, which can hide the
	// python3 — or the PyYAML, often a user-site install — that this process
	// sees. So resolve both HERE and hand the script a wrapper that pins
	// them, rather than skipping on a machine that has everything.
	//
	// In CI a missing tool is a failure, not a skip: a skip there would turn
	// every check in this file into a silent pass on the one run that gates
	// the merge.
	skip := t.Skipf
	if os.Getenv("CI") != "" {
		skip = t.Fatalf
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		skip("set-up-demo.sh's rendered-config assertion needs python3: %v", err)
	}
	site, err := exec.Command(python, "-c",
		"import os, yaml; print(os.path.dirname(os.path.dirname(yaml.__file__)))").Output()
	if err != nil {
		skip("set-up-demo.sh's rendered-config assertion needs python3 with PyYAML: %v", err)
	}
	fakePython := fmt.Sprintf("#!/usr/bin/env bash\nexport PYTHONPATH=%q\nexec %q \"$@\"\n",
		strings.TrimSpace(string(site)), python)

	tmp := t.TempDir()
	root = filepath.Join(tmp, "recipe")
	for _, d := range []string{"scripts", "deploy"} {
		if err := os.CopyFS(filepath.Join(root, d), os.DirFS(d)); err != nil {
			t.Fatalf("copy %s: %v", d, err)
		}
	}
	for _, s := range []string{"verify-gated-apply.sh", "debug-pod.sh", "grant-iam.sh"} {
		if err := os.WriteFile(filepath.Join(root, "scripts", s), []byte(stubSibling), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	fakebin := filepath.Join(tmp, "fakebin")
	if err := os.MkdirAll(fakebin, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"kubectl": fakeKubectl, "kustomize": fakeKustomize, "gcloud": fakeGcloud,
		"python3": fakePython,
	} {
		if err := os.WriteFile(filepath.Join(fakebin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	daemonNS := ""
	for _, o := range gatedApplyDecode(t, filepath.Join("deploy", "base", "00-namespace.yaml")) {
		if o.Kind == "Namespace" {
			daemonNS = o.Metadata.Name
		}
	}
	if daemonNS == "" {
		t.Fatal("deploy/base/00-namespace.yaml carries no Namespace")
	}

	n := 0
	run = func(script string, env ...string) rigRun {
		t.Helper()
		n++
		fakeDir := filepath.Join(tmp, fmt.Sprintf("run%d", n))
		if err := os.MkdirAll(fakeDir, 0o755); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(bash, filepath.Join(root, "scripts", script))
		cmd.Dir = root
		cmd.Env = append([]string{
			"PATH=" + fakebin + ":/usr/bin:/bin",
			"HOME=" + tmp,
			"TMPDIR=" + tmp,
			"KUBECONFIG=" + filepath.Join(tmp, "no-kubeconfig"),
			"CLOUDSDK_CONFIG=" + filepath.Join(tmp, "no-gcloud"),
			"FAKE_DIR=" + fakeDir,
			"FAKE_DAEMON_NS=" + daemonNS,
			"PROJECT_ID=fake-project",
			"CLUSTER_NAME=fake-cluster",
			"KUBE_CONTEXT=" + fakeContext,
			"REGION=us-central1",
			"TARGET_NS=" + fakeTargetNS,
			"OTEL=0",
			"RIG_STATE_DIR=" + filepath.Join(tmp, "state"),
		}, env...)
		out, err := cmd.CombinedOutput()
		r := rigRun{out: string(out), err: err}
		if b, rerr := os.ReadFile(filepath.Join(fakeDir, "calls.log")); rerr == nil {
			r.calls = strings.Split(strings.TrimRight(string(b), "\n"), "\n")
		}
		return r
	}
	return root, run
}

// grantName reads the Role's name out of role.yaml with a real YAML parser,
// independently of the sed both scripts use.
func grantName(t *testing.T, recipeRoot string) string {
	t.Helper()
	for _, o := range gatedApplyDecode(t, filepath.Join(recipeRoot, "deploy", "components", "gated-apply", "role.yaml")) {
		if o.Kind == "Role" {
			return o.Metadata.Name
		}
	}
	t.Fatal("role.yaml carries no Role")
	return ""
}

func revokeCall(name string) string {
	return fmt.Sprintf("kubectl --context %s -n %s delete role,rolebinding %s --ignore-not-found",
		fakeContext, fakeTargetNS, name)
}

// TestReadOnlyLegRevokesTheGatedApplyGrant pins (1): the read-only leg
// deletes the grant, by the same name lookup teardown.sh uses, after the
// read-only config is applied and before the rollout waits.
//
// "Same lookup" is shown by behaviour: both scripts run against the
// committed role.yaml and against a copy whose Role has been renamed, and in
// both trees they must delete exactly the name role.yaml carries. A
// hardcoded or DEMO_NS-reconstructed name passes the first tree and fails
// the second.
func TestReadOnlyLegRevokesTheGatedApplyGrant(t *testing.T) {
	for _, tc := range []struct {
		name   string
		rename string // "" keeps the committed role.yaml
	}{
		{name: "committed role.yaml"},
		{name: "role renamed in role.yaml", rename: "gated-apply-renamed-by-test"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, run := newRig(t)
			if tc.rename != "" {
				p := filepath.Join(root, "deploy", "components", "gated-apply", "role.yaml")
				b, err := os.ReadFile(p)
				if err != nil {
					t.Fatal(err)
				}
				old := "  name: " + grantName(t, root) + "\n"
				if strings.Count(string(b), old) != 1 {
					t.Fatalf("role.yaml does not carry %q exactly once", old)
				}
				if err := os.WriteFile(p, []byte(strings.Replace(string(b), old, "  name: "+tc.rename+"\n", 1)), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want := revokeCall(grantName(t, root))
			if tc.rename != "" && !strings.Contains(want, tc.rename) {
				t.Fatalf("rename did not take: %q", want)
			}

			setup := run("set-up-demo.sh", "LEG=readonly")
			if setup.err != nil {
				t.Fatalf("LEG=readonly set-up-demo.sh failed: %v\n%s", setup.err, setup.dump())
			}
			apply := setup.index("apply -k ")
			revoke := setup.index(want)
			rollout := setup.index("rollout status deploy/core-agent")
			switch {
			case revoke < 0:
				t.Fatalf("LEG=readonly never issued\n  %s\nso a grant an earlier d1/d2 deploy left in "+
					"TARGET_NS survives the switch back (#1249).\n%s", want, setup.dump())
			case apply < 0 || revoke < apply:
				t.Errorf("the grant was revoked (call %d) before the read-only config was applied "+
					"(call %d); a failed revoke would then leave the apply-capable config live.\n%s",
					revoke, apply, setup.dump())
			case rollout < 0 || revoke > rollout:
				t.Errorf("the grant was revoked (call %d) after the rollout wait (call %d); a rollout "+
					"timeout under set -e would leave it behind.\n%s", revoke, rollout, setup.dump())
			}
			if !strings.Contains(setup.out, "removing any gated-apply grant left in "+fakeTargetNS) {
				t.Errorf("the read-only deploy revoked the grant without saying so in its output.\n%s", setup.dump())
			}

			td := run("teardown.sh")
			if td.err != nil {
				t.Fatalf("teardown.sh failed: %v\n%s", td.err, td.dump())
			}
			if td.index(want) < 0 {
				t.Errorf("teardown.sh did not issue\n  %s\nThe two scripts must delete the same name, "+
					"read the same way.\n%s", want, td.dump())
			}
		})
	}
}

// TestReadOnlyLegProvesItIsReadOnly pins (2): the read-only leg runs
// `verify-gated-apply.sh denied` after revoking, and a failing probe fails
// the deploy rather than being reported as advice.
func TestReadOnlyLegProvesItIsReadOnly(t *testing.T) {
	t.Run("probe runs after the revoke", func(t *testing.T) {
		root, run := newRig(t)
		r := run("set-up-demo.sh", "LEG=readonly")
		if r.err != nil {
			t.Fatalf("LEG=readonly set-up-demo.sh failed: %v\n%s", r.err, r.dump())
		}
		verify := r.index("verify-gated-apply.sh denied")
		revoke := r.index(revokeCall(grantName(t, root)))
		if verify < 0 {
			t.Fatalf("LEG=readonly never ran `verify-gated-apply.sh denied`, so nothing proves the "+
				"deploy is read-only.\n%s", r.dump())
		}
		if revoke < 0 || verify < revoke {
			t.Errorf("the denied probe (call %d) must run after the revoke (call %d).\n%s", verify, revoke, r.dump())
		}
		if !strings.Contains(r.out, "✓ deployed") {
			t.Errorf("a passing probe should leave the deploy successful.\n%s", r.dump())
		}
	})

	t.Run("a daemon that can still patch fails the deploy", func(t *testing.T) {
		_, run := newRig(t)
		r := run("set-up-demo.sh", "LEG=readonly", "FAKE_VERIFY_RC=1")
		if r.err == nil {
			t.Fatalf("set-up-demo.sh exited 0 although verify-gated-apply.sh denied failed.\n%s", r.dump())
		}
		if !strings.Contains(r.out, "read-only boundary check FAILED") {
			t.Errorf("failed, but without naming the read-only boundary check.\n%s", r.dump())
		}
		if strings.Contains(r.out, "✓ deployed") {
			t.Errorf("printed ✓ deployed after the denied probe failed.\n%s", r.dump())
		}
	})

	t.Run("a revoke that fails stops before the probe", func(t *testing.T) {
		_, run := newRig(t)
		r := run("set-up-demo.sh", "LEG=readonly", "FAKE_REVOKE_RC=1")
		if r.err == nil {
			t.Fatalf("set-up-demo.sh exited 0 although the grant could not be deleted.\n%s", r.dump())
		}
		if !strings.Contains(r.out, "could not remove the gated-apply grant from "+fakeTargetNS) {
			t.Errorf("failed, but without naming the grant it could not remove.\n%s", r.dump())
		}
		if r.index("verify-gated-apply.sh") >= 0 {
			t.Errorf("ran the probe after a failed revoke.\n%s", r.dump())
		}
	})
}

// TestGatedLegsKeepTheGrant pins (3): d1 and d2 never delete the grant they
// just applied, and do not run the denied probe — which would fail on them.
//
// A must-not-find check, so the run has to be shown COMPLETE before the
// absence means anything: exit 0, "✓ deployed", and the gated overlay's
// `apply -k` in the log.
func TestGatedLegsKeepTheGrant(t *testing.T) {
	for _, leg := range []string{"d1", "d2"} {
		t.Run(leg, func(t *testing.T) {
			_, run := newRig(t)
			r := run("set-up-demo.sh", "LEG="+leg)
			if r.err != nil {
				t.Fatalf("LEG=%s set-up-demo.sh failed: %v\n%s", leg, r.err, r.dump())
			}
			if !strings.Contains(r.out, "✓ deployed") {
				t.Fatalf("LEG=%s did not reach ✓ deployed, so the absence checks below prove nothing.\n%s", leg, r.dump())
			}
			if i := r.index("apply -k "); i < 0 || !strings.HasSuffix(r.calls[i], "/deploy/overlays/gated-apply") {
				t.Fatalf("LEG=%s did not apply the gated overlay.\n%s", leg, r.dump())
			}
			if i := r.index("-n " + fakeTargetNS + " delete role,rolebinding"); i >= 0 {
				t.Errorf("LEG=%s deleted the gated-apply grant it just applied: %q\n%s", leg, r.calls[i], r.dump())
			}
			if i := r.index("verify-gated-apply.sh denied"); i >= 0 {
				t.Errorf("LEG=%s ran the denied probe, which fails by design on a granted leg.\n%s", leg, r.dump())
			}
		})
	}
}
