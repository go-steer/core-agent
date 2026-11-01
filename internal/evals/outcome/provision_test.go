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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A stand-in cluster. Nothing in this file executes anything: every test
// here is either a pure function or a refusal that fires before the
// first exec, which is why they run in the ordinary presubmit while the
// end-to-end provision in kind_test.go is opt-in.
func fakeCluster() *Cluster {
	return &Cluster{
		Name:       "mast-outcome-test",
		Context:    "kind-mast-outcome-test",
		Kubeconfig: "/tmp/mast-outcome-test/kubeconfig.yaml",
	}
}

// TestAdmittedCorpusProvisions pairs the shipped corpus with a
// provisioner. It is the check that the catalog and the manifest
// directory have not drifted apart: a role added to one and not the
// other is a role that fails at provision time, in the middle of a gate
// run, rather than here.
func TestAdmittedCorpusProvisions(t *testing.T) {
	c, err := Load(corpusDir, intentTable(t))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	p, err := NewProvisioner(c, fakeCluster(), corpusDir)
	if err != nil {
		t.Fatalf("provisioner: %v", err)
	}

	// One image, side-loaded, so a provision never reaches a registry.
	imgs := p.Images()
	if len(imgs) != 1 || imgs[0] != "busybox:1.36" {
		t.Errorf("images = %v, want [busybox:1.36]", imgs)
	}

	// The probes the provisioner will confirm are the ones the loader
	// validated, not a second parse.
	probes := c.ProbesFor("crashloop-workload")
	if len(probes) != 1 || probes[0].Kind != "deployment" || probes[0].Name != "payments-api" {
		t.Errorf("probes = %+v, want one deployment/payments-api", probes)
	}
}

func TestProvisionerNeedsACluster(t *testing.T) {
	c, err := Load(corpusDir, intentTable(t))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, err := NewProvisioner(c, nil, corpusDir); err == nil {
		t.Fatal("built a provisioner with no cluster")
	}
}

// TestProvisionerRefusesARoleWithNoReadinessCondition is the "never ship
// a rung that cannot fire" rule applied to fixtures. A role with no
// readiness condition reports ready the instant kubectl apply returns —
// which for crashloop-workload is a full minute before the state the
// cases grade against exists, so every case would grade a fixture that
// had not finished becoming itself.
func TestProvisionerRefusesARoleWithNoReadinessCondition(t *testing.T) {
	c, err := Load(corpusDir, intentTable(t))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	c.Catalog.Roles["invented-role"] = Role{Namespace: "nowhere", Probes: []string{"deployment/x"}}

	_, err = NewProvisioner(c, fakeCluster(), corpusDir)
	if err == nil {
		t.Fatal("accepted a role with no readiness condition")
	}
	if !strings.Contains(err.Error(), "no readiness condition") {
		t.Errorf("error %q does not name the missing readiness condition", err)
	}
}

// baseManifest is the shape every rejection below deviates from by one
// thing. Kept minimal on purpose: a fixture manifest that only just
// loads makes it obvious which single edit each test is making.
const baseManifest = `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: payments-api
  labels:
    mast.dev/fixture-role: crashloop-workload
spec:
  template:
    spec:
      containers:
        - name: api
          image: busybox:1.36
`

func manifestRole() Role {
	return Role{Namespace: "seeded-debug", Probes: []string{"deployment/payments-api"}}
}

// loadManifestBody writes a manifest to a temp file and loads it as
// crashloop-workload.
func loadManifestBody(t *testing.T, body string) (manifest, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "crashloop-workload.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return loadManifest(path, "crashloop-workload", manifestRole())
}

// TestBaseManifestLoads is the control. Without it, a rejection test can
// pass because the manifest was malformed in some way the test never
// mentions.
func TestBaseManifestLoads(t *testing.T) {
	m, err := loadManifestBody(t, baseManifest)
	if err != nil {
		t.Fatalf("the control manifest does not load: %v", err)
	}
	if len(m.images) != 1 || m.images[0] != "busybox:1.36" {
		t.Errorf("images = %v", m.images)
	}
}

func rejectsManifest(t *testing.T, body, wantSubstr string) {
	t.Helper()
	_, err := loadManifestBody(t, body)
	if err == nil {
		t.Fatal("manifest loaded, want a refusal")
	}
	// Matched on text, because the refusals differ only in which defect
	// they name and a test that only checks "an error happened" passes
	// on the wrong one.
	if !strings.Contains(err.Error(), wantSubstr) {
		t.Errorf("error %q does not mention %q", err, wantSubstr)
	}
}

// The same "two locations that can disagree" the loader refuses in a
// check, refused in a manifest. The catalog says seeded-debug; a
// manifest saying anything at all is a second answer, and the one that
// wins is whichever kubectl happens to prefer.
func TestRejectsAManifestThatNamesANamespace(t *testing.T) {
	body := strings.Replace(baseManifest, "  name: payments-api\n", "  name: payments-api\n  namespace: seeded-debug\n", 1)
	rejectsManifest(t, body, "second location that can disagree")
}

// Even agreeing with the catalog is refused: the point is that there is
// one place, not that the two currently match.
func TestRejectsAManifestNamespaceEvenWhenItAgrees(t *testing.T) {
	body := strings.Replace(baseManifest, "  name: payments-api\n", "  name: payments-api\n  namespace: seeded-debug\n", 1)
	_, err := loadManifestBody(t, body)
	if err == nil {
		t.Fatal("accepted a manifest namespace because it happened to match")
	}
}

func TestRejectsANamespaceObject(t *testing.T) {
	body := baseManifest + `
---
apiVersion: v1
kind: Namespace
metadata:
  name: seeded-debug
  labels:
    mast.dev/fixture-role: crashloop-workload
`
	rejectsManifest(t, body, "may not contain a Namespace")
}

func TestRejectsAnUnlabelledObject(t *testing.T) {
	body := strings.Replace(baseManifest, "    mast.dev/fixture-role: crashloop-workload\n", "", 1)
	rejectsManifest(t, body, "must carry mast.dev/fixture-role")
}

// A label naming the wrong role is worse than a missing one: the object
// would be attributed to a fixture that did not plant it, and
// Role.Exclusive would then read it as legitimate.
func TestRejectsAnObjectLabelledForAnotherRole(t *testing.T) {
	body := strings.Replace(baseManifest, "crashloop-workload", "rbac-overgrant", 1)
	rejectsManifest(t, body, "must carry mast.dev/fixture-role")
}

func TestRejectsAManifestWithNoObjects(t *testing.T) {
	rejectsManifest(t, "# nothing but a comment\n", "no objects")
}

// A manifest with a second document that is fine and a third that is
// not: the loader must walk every document, not stop at the first.
func TestChecksEveryDocumentNotJustTheFirst(t *testing.T) {
	body := baseManifest + `
---
apiVersion: v1
kind: Service
metadata:
  name: payments-api
  namespace: elsewhere
  labels:
    mast.dev/fixture-role: crashloop-workload
`
	rejectsManifest(t, body, "second location that can disagree")
}

// A bare Pod carries its containers at spec.containers rather than under
// a template, and its image still has to be side-loaded.
func TestCollectsImagesFromABarePod(t *testing.T) {
	body := `
apiVersion: v1
kind: Pod
metadata:
  name: noisy
  labels:
    mast.dev/fixture-role: crashloop-workload
spec:
  containers:
    - name: api
      image: busybox:1.36
`
	m, err := loadManifestBody(t, body)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(m.images) != 1 || m.images[0] != "busybox:1.36" {
		t.Errorf("images = %v, want [busybox:1.36]", m.images)
	}
}

func TestChanged(t *testing.T) {
	before := Snapshot{Generations: map[string]int64{
		"crashloop-workload/deployment/payments-api": 1,
		"crashloop-workload/deployment/ledger":       4,
	}}
	after := Snapshot{Generations: map[string]int64{
		"crashloop-workload/deployment/payments-api": 2,
		"crashloop-workload/deployment/ledger":       4,
	}}

	got, err := Changed(before, after)
	if err != nil {
		t.Fatalf("Changed: %v", err)
	}
	if len(got) != 1 || got[0] != "crashloop-workload/deployment/payments-api" {
		t.Errorf("changed = %v, want the one deployment whose generation moved", got)
	}

	// An object that was there and is gone is a change, and the loudest
	// kind: a blast-radius ceiling that missed a deletion would be a
	// ceiling only over the objects that survived.
	gone, err := Changed(before, Snapshot{Generations: map[string]int64{
		"crashloop-workload/deployment/ledger": 4,
	}})
	if err != nil {
		t.Fatalf("Changed: %v", err)
	}
	if len(gone) != 1 || gone[0] != "crashloop-workload/deployment/payments-api" {
		t.Errorf("a deleted object read as %v, want it counted as changed", gone)
	}

	// And one that appeared.
	appeared, err := Changed(Snapshot{Generations: map[string]int64{}}, after)
	if err != nil {
		t.Fatalf("Changed: %v", err)
	}
	if len(appeared) != 2 {
		t.Errorf("two new objects read as %v", appeared)
	}
}

// TestChangedRefusesSubjectsItCannotSee is the honest-zero test. A Pod
// has no metadata.generation, so a ceiling counted over a set that
// includes one is not a ceiling — and the wrong answer it would give is
// the reassuring one.
func TestChangedRefusesSubjectsItCannotSee(t *testing.T) {
	blind := Snapshot{
		Generations: map[string]int64{"crashloop-workload/deployment/payments-api": 1},
		Ungenerated: []string{"crashloop-workload/pod/payments-api-abc"},
	}
	_, err := Changed(blind, blind)
	if err == nil {
		t.Fatal("counted changes over a subject with no metadata.generation")
	}
	if !strings.Contains(err.Error(), "crashloop-workload/pod/payments-api-abc") {
		t.Errorf("error %q does not name the subject it cannot see", err)
	}
}

// TestCrashloopReadiness walks the states the fixture actually passes
// through. Every one of them is reachable within the first thirty
// seconds of a provision, and a provisioner that returned at any of them
// would hand the agent a Deployment that has not been OOMKilled yet —
// so all three admitted cases would be grading a workload whose symptom
// has not appeared.
func TestCrashloopReadiness(t *testing.T) {
	const pod = "payments-api-5f6b98589c-2h8f7"
	for _, tc := range []struct {
		name string
		out  string
		want string // "" means ready
	}{
		{
			name: "the deployment has not made a pod yet",
			out:  "",
			want: "no pod carries mast.dev/fixture-role=crashloop-workload yet",
		},
		{
			name: "the pod is scheduled but has never been killed",
			out:  pod + " Running 0",
			want: "has not been killed yet",
		},
		{
			name: "the pod is still being scheduled",
			out:  pod + " Pending 0 OOMKilled",
			want: "is Pending, want Running",
		},
		{
			name: "the container died of something else",
			out:  pod + " Running 3 Error",
			want: "last terminated with Error, want OOMKilled",
		},
		{
			name: "one kill is not yet a loop",
			out:  pod + " Running 1 OOMKilled",
			want: "has restarted 1 time(s), want at least 2",
		},
		{
			name: "the state the cases are written against",
			out:  pod + " Running 2 OOMKilled",
		},
		{
			name: "a second replica lagging behind the first",
			out:  pod + " Running 4 OOMKilled\npayments-api-5f6b98589c-zzzzz Running 1 OOMKilled",
			want: "has restarted 1 time(s)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := crashloopReadyFrom("crashloop-workload", tc.out)
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("not ready in the state the cases grade: %v", err)
			case tc.want == "":
			case err == nil:
				t.Fatalf("reported ready while %s", tc.name)
			case !strings.Contains(err.Error(), tc.want):
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// Unescaped, `mast.dev/fixture-role` reads to kubectl as four nested
// fields and resolves to nothing — so every label check would find an
// empty string where it expected one and the label discipline would be
// silently off.
func TestEscapesTheLabelKeyForJSONPath(t *testing.T) {
	if got := escapeJSONPathKey(RoleLabel); got != `mast\.dev/fixture-role` {
		t.Errorf("escapeJSONPathKey(%q) = %q", RoleLabel, got)
	}
}
