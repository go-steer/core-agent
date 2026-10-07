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

package selfdevsoak

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The deploy/ tests render the A7 rig with deploy/render.sh, the one
// path the runbook applies, and assert the properties the brief and the
// design settled: images by digest, placeholders that fail closed, the
// writer App's key mounted in the dispatcher's pod alone, a read-only
// hashed users table, the pod security context, and the network policy.
//
// render.sh shells out to kustomize (or `kubectl kustomize`). Where
// neither is on PATH these tests skip, except on GitHub Actions, whose
// runner image ships both: there a missing kustomize is a broken gate,
// not a reason to pass. The tests that need no kustomize (the replacement
// index, the image sentinels, no committed secrets) always run.

const (
	deployDir       = "deploy"
	soakNamespace   = "core-agent-selfdev"
	testImage       = "us-docker.pkg.dev/proj/repo/core-agent-selfdev-soak@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	testSlack       = "C0123ABCD"
	appKeySecret    = "selfdev-soak-writer-app"
	attachSecret    = "selfdev-soak-dispatcher-attach"
	usersSecret     = "core-agent-selfdev-users"
	switchSecret    = "core-agent-selfdev-switchboard"
	imageSentinel   = "REPLACED-BY-KUSTOMIZE-FROM-SOAK_IMAGE"
	slackTargetName = "core-agent-selfdev-config"
)

var digestImageRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._/:-]*@sha256:[0-9a-f]{64}$`)

// goodInputs is a complete, valid inputs.env.
func goodInputs() map[string]string {
	return map[string]string{
		"SOAK_IMAGE":          testImage,
		"SOAK_VERTEX_PROJECT": "proj-1",
		"SOAK_SLACK_CHANNEL":  testSlack,
		"SOAK_APP_ID":         "123456",
		"SOAK_COMMIT_NAME":    "Soak Worker",
		"SOAK_COMMIT_EMAIL":   "soak-worker@example.com",
	}
}

func writeInputs(t *testing.T, kv map[string]string) string {
	t.Helper()
	keys := make([]string, 0, len(kv))
	for k := range kv {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k + "=" + kv[k] + "\n")
	}
	p := filepath.Join(t.TempDir(), "inputs.env")
	if err := os.WriteFile(p, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// requireKustomize skips (or, on GitHub Actions, fails) when render.sh
// would find no kustomize.
func requireKustomize(t *testing.T) {
	t.Helper()
	for _, bin := range []string{"kustomize", "kubectl"} {
		if _, err := exec.LookPath(bin); err == nil {
			return
		}
	}
	if os.Getenv("GITHUB_ACTIONS") == "true" {
		t.Fatal("neither kustomize nor kubectl is on PATH; the GitHub runner image ships both, so the deploy/ render gate would be checking nothing")
	}
	t.Skip("neither kustomize nor kubectl is on PATH; install kustomize to run the deploy/ render tests")
}

// runRender runs deploy/render.sh and returns its exit code, stdout and
// stderr separately: a refusal must leave stdout empty, because the
// runbook pipes it into kubectl.
func runRender(t *testing.T, inputsPath string, extra ...string) (int, string, string) {
	t.Helper()
	args := append([]string{filepath.Join(deployDir, "render.sh"), "--inputs", inputsPath}, extra...)
	cmd := exec.Command("bash", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run render.sh: %v", err)
	}
	return code, stdout.String(), stderr.String()
}

// ---- a minimal typed view of the rendered objects ----

type objMeta struct {
	Name        string            `yaml:"name"`
	Namespace   string            `yaml:"namespace"`
	Labels      map[string]string `yaml:"labels"`
	Annotations map[string]string `yaml:"annotations"`
}

type k8sObject struct {
	APIVersion string            `yaml:"apiVersion"`
	Kind       string            `yaml:"kind"`
	Metadata   objMeta           `yaml:"metadata"`
	Data       map[string]string `yaml:"data"`
	Automount  *bool             `yaml:"automountServiceAccountToken"`
	Spec       yaml.Node         `yaml:"spec"`
}

type envVar struct {
	Name      string `yaml:"name"`
	Value     string `yaml:"value"`
	ValueFrom *struct {
		ConfigMapKeyRef *struct {
			Name string `yaml:"name"`
			Key  string `yaml:"key"`
		} `yaml:"configMapKeyRef"`
		SecretKeyRef *struct {
			Name string `yaml:"name"`
			Key  string `yaml:"key"`
		} `yaml:"secretKeyRef"`
	} `yaml:"valueFrom"`
}

type volumeMount struct {
	Name      string `yaml:"name"`
	MountPath string `yaml:"mountPath"`
	SubPath   string `yaml:"subPath"`
	ReadOnly  bool   `yaml:"readOnly"`
}

type container struct {
	Name            string        `yaml:"name"`
	Image           string        `yaml:"image"`
	Command         []string      `yaml:"command"`
	Args            []string      `yaml:"args"`
	Env             []envVar      `yaml:"env"`
	VolumeMounts    []volumeMount `yaml:"volumeMounts"`
	SecurityContext struct {
		AllowPrivilegeEscalation *bool `yaml:"allowPrivilegeEscalation"`
		ReadOnlyRootFilesystem   *bool `yaml:"readOnlyRootFilesystem"`
		Capabilities             struct {
			Drop []string `yaml:"drop"`
		} `yaml:"capabilities"`
	} `yaml:"securityContext"`
}

type volume struct {
	Name   string `yaml:"name"`
	Secret *struct {
		SecretName  string `yaml:"secretName"`
		DefaultMode *int   `yaml:"defaultMode"`
	} `yaml:"secret"`
	ConfigMap *struct {
		Name string `yaml:"name"`
	} `yaml:"configMap"`
	PersistentVolumeClaim *struct {
		ClaimName string `yaml:"claimName"`
	} `yaml:"persistentVolumeClaim"`
	EmptyDir *struct {
		SizeLimit string `yaml:"sizeLimit"`
	} `yaml:"emptyDir"`
}

type podSpec struct {
	ServiceAccountName string `yaml:"serviceAccountName"`
	Automount          *bool  `yaml:"automountServiceAccountToken"`
	RestartPolicy      string `yaml:"restartPolicy"`
	SecurityContext    struct {
		RunAsUser      *int64 `yaml:"runAsUser"`
		RunAsGroup     *int64 `yaml:"runAsGroup"`
		FSGroup        *int64 `yaml:"fsGroup"`
		RunAsNonRoot   *bool  `yaml:"runAsNonRoot"`
		SeccompProfile struct {
			Type string `yaml:"type"`
		} `yaml:"seccompProfile"`
	} `yaml:"securityContext"`
	Affinity struct {
		PodAffinity struct {
			Required []struct {
				LabelSelector struct {
					MatchLabels map[string]string `yaml:"matchLabels"`
				} `yaml:"labelSelector"`
				TopologyKey string `yaml:"topologyKey"`
			} `yaml:"requiredDuringSchedulingIgnoredDuringExecution"`
		} `yaml:"podAffinity"`
	} `yaml:"affinity"`
	InitContainers []container `yaml:"initContainers"`
	Containers     []container `yaml:"containers"`
	Volumes        []volume    `yaml:"volumes"`
}

type workloadSpec struct {
	Replicas     *int `yaml:"replicas"`
	BackoffLimit *int `yaml:"backoffLimit"`
	Strategy     struct {
		Type string `yaml:"type"`
	} `yaml:"strategy"`
	Template struct {
		Metadata objMeta   `yaml:"metadata"`
		Spec     yaml.Node `yaml:"spec"`
	} `yaml:"template"`
}

// pod is one rendered pod template with the workload that owns it.
type pod struct {
	owner     string // Kind/name
	component string
	workload  workloadSpec
	spec      podSpec
	raw       yaml.Node
}

type rendered struct {
	objects []k8sObject
}

func parseRendered(t *testing.T, out string) rendered {
	t.Helper()
	dec := yaml.NewDecoder(strings.NewReader(out))
	var r rendered
	for {
		var o k8sObject
		err := dec.Decode(&o)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("parse rendered manifests: %v", err)
		}
		if o.Kind == "" {
			continue
		}
		r.objects = append(r.objects, o)
	}
	if len(r.objects) == 0 {
		t.Fatal("render produced no objects")
	}
	return r
}

func (r rendered) byKind(kind string) []k8sObject {
	var out []k8sObject
	for _, o := range r.objects {
		if o.Kind == kind {
			out = append(out, o)
		}
	}
	return out
}

func (r rendered) get(t *testing.T, kind, name string) k8sObject {
	t.Helper()
	for _, o := range r.objects {
		if o.Kind == kind && o.Metadata.Name == name {
			return o
		}
	}
	t.Fatalf("rendered output has no %s/%s", kind, name)
	return k8sObject{}
}

// configMap finds a generated ConfigMap by its pre-hash name.
func (r rendered) configMap(t *testing.T, prefix string) k8sObject {
	t.Helper()
	for _, o := range r.byKind("ConfigMap") {
		if o.Metadata.Name == prefix || strings.HasPrefix(o.Metadata.Name, prefix+"-") {
			return o
		}
	}
	t.Fatalf("rendered output has no ConfigMap %s", prefix)
	return k8sObject{}
}

func (r rendered) pods(t *testing.T) []pod {
	t.Helper()
	var out []pod
	for _, o := range r.objects {
		switch o.Kind {
		case "Deployment", "Job", "StatefulSet", "DaemonSet", "CronJob", "Pod", "ReplicaSet":
		default:
			continue
		}
		if o.Kind != "Deployment" && o.Kind != "Job" {
			t.Fatalf("unexpected workload kind %s/%s; extend these tests before adding one", o.Kind, o.Metadata.Name)
		}
		var w workloadSpec
		if err := o.Spec.Decode(&w); err != nil {
			t.Fatalf("decode %s/%s: %v", o.Kind, o.Metadata.Name, err)
		}
		var ps podSpec
		if err := w.Template.Spec.Decode(&ps); err != nil {
			t.Fatalf("decode %s/%s pod spec: %v", o.Kind, o.Metadata.Name, err)
		}
		out = append(out, pod{
			owner:     o.Kind + "/" + o.Metadata.Name,
			component: w.Template.Metadata.Labels["app.kubernetes.io/component"],
			workload:  w,
			spec:      ps,
			raw:       w.Template.Spec,
		})
	}
	return out
}

func (r rendered) pod(t *testing.T, owner string) pod {
	t.Helper()
	for _, p := range r.pods(t) {
		if p.owner == owner {
			return p
		}
	}
	t.Fatalf("no pod template owned by %s", owner)
	return pod{}
}

func (p pod) container(t *testing.T, name string) container {
	t.Helper()
	for _, c := range p.spec.Containers {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("%s has no container %s", p.owner, name)
	return container{}
}

func (p pod) volume(name string) (volume, bool) {
	for _, v := range p.spec.Volumes {
		if v.Name == name {
			return v, true
		}
	}
	return volume{}, false
}

func (p pod) allContainers() []container {
	return append(slices.Clone(p.spec.InitContainers), p.spec.Containers...)
}

// renderGood renders a variant with goodInputs.
func renderGood(t *testing.T, extra ...string) rendered {
	t.Helper()
	requireKustomize(t)
	code, out, errOut := runRender(t, writeInputs(t, goodInputs()), extra...)
	if code != 0 {
		t.Fatalf("render.sh %v with valid inputs: exit %d\n%s", extra, code, errOut)
	}
	return parseRendered(t, out)
}

// renderAll is everything the runbook applies: the base, then the A7
// Job (rendered separately, so that re-applying the base never starts a
// run).
func renderAll(t *testing.T) rendered {
	t.Helper()
	base := renderGood(t)
	job := renderGood(t, "--a7-job")
	return rendered{objects: append(base.objects, job.objects...)}
}

// TestRenderA7JobIsSeparate: the base holds no Job, and the A7 render
// holds the Job alone. Re-applying the base (an image roll, the 2-week
// soak) must never recreate a --once run that claims the oldest queued
// issue, and applying the run must not touch the rig.
func TestRenderA7JobIsSeparate(t *testing.T) {
	base := renderGood(t)
	for _, v := range []rendered{base, renderGood(t, "--fqdn-egress")} {
		if jobs := v.byKind("Job"); len(jobs) != 0 {
			t.Errorf("the rig render contains %d Job(s); the A7 run belongs to --a7-job only", len(jobs))
		}
	}
	if code, out, _ := runRender(t, writeInputs(t, goodInputs()), "--fqdn-egress", "--a7-job"); code == 0 || out != "" {
		t.Errorf("--fqdn-egress with --a7-job: exit %d, stdout %d bytes; want a refusal, they are separate renders", code, len(out))
	}
	job := renderGood(t, "--a7-job")
	if len(job.objects) != 1 || job.objects[0].Kind != "Job" || job.objects[0].Metadata.Name != "selfdev-soak-dispatcher-a7" {
		var got []string
		for _, o := range job.objects {
			got = append(got, o.Kind+"/"+o.Metadata.Name)
		}
		t.Errorf("--a7-job renders %v, want exactly Job/selfdev-soak-dispatcher-a7", got)
	}
	// The Job reads the inputs ConfigMap the base applies, by its fixed
	// name: a hash suffix would make the names disagree.
	if cm := base.configMap(t, "selfdev-soak-inputs"); cm.Metadata.Name != "selfdev-soak-inputs" {
		t.Errorf("the inputs ConfigMap is %s; it must keep its fixed name for the separately rendered Job", cm.Metadata.Name)
	}
	for _, c := range job.pod(t, "Job/selfdev-soak-dispatcher-a7").spec.Containers {
		for _, e := range c.Env {
			if e.ValueFrom != nil && e.ValueFrom.ConfigMapKeyRef != nil && e.ValueFrom.ConfigMapKeyRef.Name != "selfdev-soak-inputs" {
				t.Errorf("Job env %s reads ConfigMap %s, which the base does not apply", e.Name, e.ValueFrom.ConfigMapKeyRef.Name)
			}
		}
	}
}

// ---- images ----

func TestRenderImagesByDigest(t *testing.T) {
	r := renderAll(t)
	n := 0
	for _, p := range r.pods(t) {
		for _, c := range p.allContainers() {
			n++
			if c.Image != testImage {
				t.Errorf("%s container %s image = %q, want the SOAK_IMAGE digest %q", p.owner, c.Name, c.Image, testImage)
			}
			if !digestImageRe.MatchString(c.Image) {
				t.Errorf("%s container %s image %q is not a digest reference", p.owner, c.Name, c.Image)
			}
		}
	}
	if n < 4 {
		t.Errorf("found %d containers, want at least the daemon, its init container, the dispatcher Deployment and the A7 Job", n)
	}
}

// TestDeploySourceImagesAreSentinels needs no kustomize: every image in
// base/ and a7-job/ is the sentinel the SOAK_IMAGE replacement
// overwrites, and every container is a target of its own
// kustomization's replacement. A container the replacement missed would
// keep the sentinel, which render.sh refuses (it contains REPLACE) and
// the kubelet cannot pull.
func TestDeploySourceImagesAreSentinels(t *testing.T) {
	imageLine := regexp.MustCompile(`(?m)^\s*(?:- )?image:\s*(\S+)\s*$`)
	targetRe := regexp.MustCompile(`^spec\.template\.spec\.(initContainers|containers)\.\[name=([^\]]+)\]\.image$`)
	for _, dir := range []string{"base", "a7-job"} {
		files, err := filepath.Glob(filepath.Join(deployDir, dir, "*.yaml"))
		if err != nil || len(files) == 0 {
			t.Fatalf("no manifests in deploy/%s: %v", dir, err)
		}
		var containers []string
		for _, f := range files {
			body, err := os.ReadFile(f) //nolint:gosec // a fixed path in this tree
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range imageLine.FindAllStringSubmatch(string(body), -1) {
				if m[1] != imageSentinel {
					t.Errorf("%s names image %q; every image must be %s, overwritten from SOAK_IMAGE", f, m[1], imageSentinel)
				}
			}
			containers = append(containers, sourceContainers(t, body)...)
		}
		targeted := map[string]bool{}
		for _, rep := range readKustomization(t, dir).Replacements {
			if rep.Source.FieldPath != "data.SOAK_IMAGE" {
				continue
			}
			for _, tg := range rep.Targets {
				for _, fp := range tg.FieldPaths {
					m := targetRe.FindStringSubmatch(fp)
					if m == nil {
						t.Errorf("deploy/%s: SOAK_IMAGE target field path %q is not a container image", dir, fp)
						continue
					}
					targeted[tg.Select.Kind+"/"+tg.Select.Name+"/"+m[1]+"/"+m[2]] = true
				}
			}
		}
		for _, c := range containers {
			if !targeted[c] {
				t.Errorf("deploy/%s: %s is not a SOAK_IMAGE replacement target; it would keep the sentinel image", dir, c)
			}
		}
		if len(containers) == 0 {
			t.Errorf("found no containers in deploy/%s", dir)
		}
	}
}

// sourceContainers lists Kind/name/(initContainers|containers)/name for
// every workload in one manifest file.
func sourceContainers(t *testing.T, body []byte) []string {
	t.Helper()
	var out []string
	dec := yaml.NewDecoder(bytes.NewReader(body))
	for {
		var o k8sObject
		if err := dec.Decode(&o); err != nil {
			break
		}
		if o.Kind != "Deployment" && o.Kind != "Job" {
			continue
		}
		var w workloadSpec
		if err := o.Spec.Decode(&w); err != nil {
			t.Fatal(err)
		}
		var ps podSpec
		if err := w.Template.Spec.Decode(&ps); err != nil {
			t.Fatal(err)
		}
		for _, c := range ps.InitContainers {
			out = append(out, o.Kind+"/"+o.Metadata.Name+"/initContainers/"+c.Name)
		}
		for _, c := range ps.Containers {
			out = append(out, o.Kind+"/"+o.Metadata.Name+"/containers/"+c.Name)
		}
	}
	return out
}

// ---- placeholders fail closed ----

type kustomization struct {
	Replacements []struct {
		Source struct {
			Kind      string `yaml:"kind"`
			Name      string `yaml:"name"`
			FieldPath string `yaml:"fieldPath"`
		} `yaml:"source"`
		Targets []struct {
			Select struct {
				Kind string `yaml:"kind"`
				Name string `yaml:"name"`
			} `yaml:"select"`
			FieldPaths []string `yaml:"fieldPaths"`
			Options    struct {
				Delimiter string `yaml:"delimiter"`
				Index     int    `yaml:"index"`
			} `yaml:"options"`
		} `yaml:"targets"`
	} `yaml:"replacements"`
}

func readKustomization(t *testing.T, dir string) kustomization {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(deployDir, dir, "kustomization.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var k kustomization
	if err := yaml.Unmarshal(body, &k); err != nil {
		t.Fatal(err)
	}
	return k
}

// TestSlackReplacementIndexMatchesConfig needs no kustomize. The Slack
// replacement splits config.json on `"` and replaces one segment; this
// pins that segment to the one holding the placeholder, and says what
// the index has to become when config.soak.json changes above it.
func TestSlackReplacementIndexMatchesConfig(t *testing.T) {
	body := string(readSoakConfig(t))
	at := strings.Index(body, conversationPlaceholder)
	if at < 0 {
		t.Fatalf("%s has no conversation placeholder", soakConfigPath)
	}
	want := strings.Count(body[:at], `"`)
	found := false
	for _, rep := range readKustomization(t, "base").Replacements {
		if rep.Source.FieldPath != "data.SOAK_SLACK_CHANNEL" {
			continue
		}
		for _, tg := range rep.Targets {
			found = true
			if tg.Select.Kind != "ConfigMap" || tg.Select.Name != slackTargetName ||
				!slices.Equal(tg.FieldPaths, []string{"data.[config.json]"}) || tg.Options.Delimiter != `"` {
				t.Errorf("Slack replacement target = %+v, want ConfigMap %s data.[config.json] split on '\"'", tg, slackTargetName)
			}
			if tg.Options.Index != want {
				t.Errorf("Slack replacement index = %d, but the placeholder in %s is segment %d; set `index: %d` in deploy/base/kustomization.yaml", tg.Options.Index, soakConfigPath, want, want)
			}
		}
	}
	if !found {
		t.Error("no replacement copies SOAK_SLACK_CHANNEL into the config")
	}
}

func TestRenderSubstitutesSlackChannel(t *testing.T) {
	r := renderGood(t)
	cm := r.configMap(t, slackTargetName)
	cfg, err := loadLikeDashC([]byte(cm.Data["config.json"]))
	if err != nil {
		t.Fatalf("rendered config.json does not load like -c: %v", err)
	}
	if len(cfg.Alerts.Targets) != 1 || cfg.Alerts.Targets[0].Conversation != testSlack {
		t.Errorf("rendered alert targets = %+v, want one whose conversation is %s", cfg.Alerts.Targets, testSlack)
	}
	// The substitution touched nothing else: the rendered file is the
	// shipped one with exactly the placeholder replaced.
	want := strings.Replace(string(readSoakConfig(t)), conversationPlaceholder, testSlack, 1)
	if cm.Data["config.json"] != want {
		t.Error("rendered config.json differs from config.soak.json by more than the Slack channel")
	}
}

func TestRenderRefusesPlaceholders(t *testing.T) {
	requireKustomize(t)
	example := filepath.Join(deployDir, "inputs.env.example")
	code, out, errOut := runRender(t, example)
	if code == 0 || out != "" {
		t.Fatalf("render.sh with the shipped example: exit %d, stdout %d bytes; want a refusal and empty stdout", code, len(out))
	}
	if !strings.Contains(errOut, "placeholder") {
		t.Errorf("refusal does not say placeholder:\n%s", errOut)
	}

	for key := range goodInputs() {
		t.Run("missing "+key, func(t *testing.T) {
			in := goodInputs()
			delete(in, key)
			code, out, errOut := runRender(t, writeInputs(t, in))
			if code == 0 || out != "" {
				t.Fatalf("exit %d, stdout %d bytes; want a refusal with empty stdout", code, len(out))
			}
			if !strings.Contains(errOut, key) {
				t.Errorf("refusal does not name %s:\n%s", key, errOut)
			}
		})
		t.Run("placeholder "+key, func(t *testing.T) {
			in := goodInputs()
			in[key] = "REPLACE-ME"
			if code, out, _ := runRender(t, writeInputs(t, in)); code == 0 || out != "" {
				t.Fatalf("exit %d, stdout %d bytes; want a refusal with empty stdout", code, len(out))
			}
		})
	}

	for name, mut := range map[string]func(map[string]string){
		"image by tag":         func(m map[string]string) { m["SOAK_IMAGE"] = "us-docker.pkg.dev/p/r/core-agent-selfdev-soak:v3.0.0" },
		"short digest":         func(m map[string]string) { m["SOAK_IMAGE"] = "us-docker.pkg.dev/p/r/soak@sha256:abc" },
		"non-numeric app id":   func(m map[string]string) { m["SOAK_APP_ID"] = "writer-app" },
		"channel with spaces":  func(m map[string]string) { m["SOAK_SLACK_CHANNEL"] = "C01 23" },
		"email without at":     func(m map[string]string) { m["SOAK_COMMIT_EMAIL"] = "soak.example.com" },
		"trailing whitespace":  func(m map[string]string) { m["SOAK_VERTEX_PROJECT"] = "proj-1 " },
		"empty vertex project": func(m map[string]string) { m["SOAK_VERTEX_PROJECT"] = "" },
	} {
		t.Run(name, func(t *testing.T) {
			in := goodInputs()
			mut(in)
			if code, out, _ := runRender(t, writeInputs(t, in)); code == 0 || out != "" {
				t.Fatalf("exit %d, stdout %d bytes; want a refusal with empty stdout", code, len(out))
			}
		})
	}
}

// TestDeployFailsClosedWithoutRenderScript: applying the base directly
// (`kubectl apply -k`) cannot work, because the operator's inputs and
// the config are supplied only by render.sh.
func TestDeployFailsClosedWithoutRenderScript(t *testing.T) {
	requireKustomize(t)
	var cmd *exec.Cmd
	if _, err := exec.LookPath("kustomize"); err == nil {
		cmd = exec.Command("kustomize", "build", filepath.Join(deployDir, "base"))
	} else {
		cmd = exec.Command("kubectl", "kustomize", filepath.Join(deployDir, "base"))
	}
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("kustomize build deploy/base succeeded without inputs.env; a direct apply would deploy placeholders:\n%.400s", out)
	}
	for _, f := range []string{"inputs.env", "config.soak.json"} {
		if _, err := os.Stat(filepath.Join(deployDir, "base", f)); err == nil {
			t.Errorf("deploy/base/%s exists in the tree; it must be supplied by render.sh only", f)
		}
	}
}

// TestExamplePlaceholdersFailOnTheirOwn: even rendered without
// render.sh's checks, each shipped placeholder stops the component that
// reads it.
func TestExamplePlaceholdersFailOnTheirOwn(t *testing.T) {
	ex := readEnvFile(t, filepath.Join(deployDir, "inputs.env.example"))
	for k := range goodInputs() {
		if _, ok := ex[k]; !ok {
			t.Errorf("inputs.env.example does not declare %s", k)
		}
	}
	for k, v := range ex {
		if !strings.Contains(v, "REPLACE") {
			t.Errorf("inputs.env.example ships %s=%q, which is not a placeholder", k, v)
		}
	}
	// An OCI reference has a lower-case repository; the kubelet refuses
	// this one (InvalidImageName).
	if regexp.MustCompile(`^[a-z0-9][a-z0-9._/:-]*(@sha256:[0-9a-f]{64})?$`).MatchString(ex["SOAK_IMAGE"]) {
		t.Errorf("SOAK_IMAGE placeholder %q is a valid image reference", ex["SOAK_IMAGE"])
	}
	if _, err := strconv.ParseInt(ex["SOAK_APP_ID"], 10, 64); err == nil {
		t.Error("SOAK_APP_ID placeholder parses as a number")
	}
	if strings.Contains(ex["SOAK_COMMIT_EMAIL"], "@") {
		t.Error("SOAK_COMMIT_EMAIL placeholder has an @, so the dispatcher would accept it")
	}
	// The Slack placeholder is the config's own, which config validation
	// refuses (TestSoakConfigPlaceholderFailsClosed).
	if ex["SOAK_SLACK_CHANNEL"] != conversationPlaceholder {
		t.Errorf("SOAK_SLACK_CHANNEL placeholder = %q, want the config's %q", ex["SOAK_SLACK_CHANNEL"], conversationPlaceholder)
	}
}

func readEnvFile(t *testing.T, path string) map[string]string {
	t.Helper()
	body, err := os.ReadFile(path) //nolint:gosec // a fixed path in this tree
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(body), "\n") {
		if line == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("%s: %q is not KEY=VALUE", path, line)
		}
		out[k] = v
	}
	return out
}

// ---- secrets ----

// secretUse lists, per pod, every Secret it references (volume or env).
func secretUse(p pod) map[string]bool {
	out := map[string]bool{}
	for _, v := range p.spec.Volumes {
		if v.Secret != nil {
			out[v.Secret.SecretName] = true
		}
	}
	for _, c := range p.allContainers() {
		for _, e := range c.Env {
			if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
				out[e.ValueFrom.SecretKeyRef.Name] = true
			}
		}
	}
	return out
}

func TestRenderSecretsMountedOnlyWhereNeeded(t *testing.T) {
	r := renderAll(t)
	if s := r.byKind("Secret"); len(s) > 0 {
		t.Errorf("the render contains %d Secret object(s); secrets are created by the operator, never rendered", len(s))
	}
	wantOnly := map[string]string{
		appKeySecret: "dispatcher",
		attachSecret: "dispatcher",
		usersSecret:  "daemon",
		switchSecret: "daemon",
	}
	seen := map[string]bool{}
	for _, p := range r.pods(t) {
		if p.component != "daemon" && p.component != "dispatcher" {
			t.Errorf("%s has component %q; every pod must be the daemon or the dispatcher", p.owner, p.component)
		}
		for s := range secretUse(p) {
			want, known := wantOnly[s]
			switch {
			case !known:
				t.Errorf("%s references Secret %s, which the rig does not define", p.owner, s)
			case want != p.component:
				t.Errorf("%s (%s) references Secret %s, which belongs to the %s pod only", p.owner, p.component, s, want)
			}
			seen[s] = true
		}
	}
	for s := range wantOnly {
		if !seen[s] {
			t.Errorf("no pod references Secret %s", s)
		}
	}
	// The App key and the attach token: file mounts, 0400, read-only.
	for _, owner := range []string{"Deployment/selfdev-soak-dispatcher", "Job/selfdev-soak-dispatcher-a7"} {
		p := r.pod(t, owner)
		for vol, secret := range map[string]string{"writer-app": appKeySecret, "attach-token": attachSecret} {
			v, ok := p.volume(vol)
			if !ok || v.Secret == nil || v.Secret.SecretName != secret {
				t.Errorf("%s volume %s is not Secret %s", owner, vol, secret)
				continue
			}
			if v.Secret.DefaultMode == nil || *v.Secret.DefaultMode != 0o400 {
				t.Errorf("%s volume %s defaultMode = %v, want 0400", owner, vol, v.Secret.DefaultMode)
			}
			for _, m := range p.container(t, "dispatcher").VolumeMounts {
				if m.Name == vol && !m.ReadOnly {
					t.Errorf("%s mounts %s writable", owner, vol)
				}
			}
		}
	}
}

// TestDeployCommitsNoSecretValues needs no kustomize: nothing under
// deploy/ (or the config it renders) looks like a credential.
func TestDeployCommitsNoSecretValues(t *testing.T) {
	bad := map[string]*regexp.Regexp{
		"a PEM block":            regexp.MustCompile(`-----BEGIN `),
		"a Secret object":        regexp.MustCompile(`(?m)^kind:\s*Secret\s*$`),
		"a long hex string":      regexp.MustCompile(`\b[0-9a-f]{32,}\b`),
		"a GitHub token":         regexp.MustCompile(`\b(gh[pousr]_[A-Za-z0-9]{20,}|github_pat_)`),
		"a secretGenerator":      regexp.MustCompile(`(?m)^secretGenerator:`),
		"a token or key literal": regexp.MustCompile(`(?mi)^\s*"?(token|token_sha256|password|private[-_]key)"?\s*[:=]\s*"?[A-Za-z0-9+/=_-]{16,}`),
	}
	files := []string{soakConfigPath}
	err := filepath.WalkDir(deployDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		body, err := os.ReadFile(f) //nolint:gosec // paths walked in this tree
		if err != nil {
			t.Fatal(err)
		}
		for what, re := range bad {
			if loc := re.FindIndex(body); loc != nil {
				t.Errorf("%s contains %s at byte %d", f, what, loc[0])
			}
		}
	}
}

// ---- the daemon ----

func TestRenderDaemon(t *testing.T) {
	r := renderGood(t)
	p := r.pod(t, "Deployment/core-agent-selfdev")
	if p.workload.Replicas == nil || *p.workload.Replicas != 1 || p.workload.Strategy.Type != "Recreate" {
		t.Errorf("daemon replicas/strategy = %v/%q, want 1/Recreate", p.workload.Replicas, p.workload.Strategy.Type)
	}
	c := p.container(t, "core-agent")
	for _, want := range []string{"--no-repl", "--attach-listen=:7777", "--session-db-path=/var/lib/core-agent/sessions.db",
		"--agents-dir=/usr/local/share/core-agent-soak/recipe/.agents", "--no-pricing-refresh"} {
		if !slices.Contains(c.Args, want) {
			t.Errorf("daemon args %v lack %s", c.Args, want)
		}
	}
	for _, a := range c.Args {
		if strings.HasPrefix(a, "--attach-token") || strings.HasPrefix(a, "-c") || strings.HasPrefix(a, "--yolo") {
			t.Errorf("daemon arg %s: the listener's auth is the hashed table, and the image's entrypoint pins -c", a)
		}
	}
	mounts := map[string]volumeMount{}
	for _, m := range c.VolumeMounts {
		mounts[m.MountPath] = m
	}
	wantVol := map[string]string{
		"/etc/core-agent-soak":  "configMap:" + slackTargetName,
		"/etc/core-agent-users": "secret:" + usersSecret,
		"/var/lib/core-agent":   "pvc:core-agent-selfdev-eventlog",
		"/workspace":            "pvc:core-agent-selfdev-workspace",
		"/cache/go":             "pvc:core-agent-selfdev-gocache",
		"/tmp":                  "emptyDir",
		"/home/soak":            "emptyDir",
		"/usr/local/share/core-agent-soak/recipe/.agents/plans": "pvc:core-agent-selfdev-workspace",
	}
	for path, want := range wantVol {
		m, ok := mounts[path]
		if !ok {
			t.Errorf("daemon mounts nothing at %s", path)
			continue
		}
		v, _ := p.volume(m.Name)
		got := volumeKind(v)
		generated := strings.HasPrefix(want, "configMap:") && strings.HasPrefix(got, want+"-") // hash-suffixed
		if got != want && !generated {
			t.Errorf("daemon %s is %s, want %s", path, got, want)
		}
	}
	if m := mounts["/usr/local/share/core-agent-soak/recipe/.agents/plans"]; m.SubPath != ".soak/plans" {
		t.Errorf("plans mount subPath = %q, want .soak/plans (the dispatcher's --agents-dir=/workspace/.soak)", m.SubPath)
	}
	if !mounts["/etc/core-agent-soak"].ReadOnly {
		t.Error("the config overlay is mounted writable")
	}
	if len(p.spec.InitContainers) != 1 || !slices.Equal(p.spec.InitContainers[0].Command, []string{"mkdir", "-p", "/workspace/.soak/plans"}) {
		t.Errorf("daemon init containers = %+v, want one that creates /workspace/.soak/plans as uid 10001 before the subPath mount", p.spec.InitContainers)
	}
	env := map[string]envVar{}
	for _, e := range c.Env {
		env[e.Name] = e
	}
	if e := env["ANTHROPIC_VERTEX_PROJECT_ID"]; e.ValueFrom == nil || e.ValueFrom.ConfigMapKeyRef == nil || e.ValueFrom.ConfigMapKeyRef.Key != "SOAK_VERTEX_PROJECT" {
		t.Errorf("ANTHROPIC_VERTEX_PROJECT_ID = %+v, want SOAK_VERTEX_PROJECT from the inputs ConfigMap", e)
	}
	if env["CLOUD_ML_REGION"].Value != "global" {
		t.Error("CLOUD_ML_REGION is not global; Claude 5 is served only there")
	}
	for _, k := range []string{"SOAK_SWITCHBOARD_URL", "SOAK_SWITCHBOARD_TOKEN"} {
		if e := env[k]; e.ValueFrom == nil || e.ValueFrom.SecretKeyRef == nil || e.ValueFrom.SecretKeyRef.Name != switchSecret {
			t.Errorf("%s does not come from Secret %s", k, switchSecret)
		}
	}
	for k := range env {
		if strings.HasPrefix(k, "GIT_") {
			t.Errorf("daemon sets %s; the dispatcher writes the commit identity into each copy's git config, and an env identity would override it", k)
		}
	}
}

func volumeKind(v volume) string {
	switch {
	case v.Secret != nil:
		return "secret:" + v.Secret.SecretName
	case v.ConfigMap != nil:
		return "configMap:" + v.ConfigMap.Name
	case v.PersistentVolumeClaim != nil:
		return "pvc:" + v.PersistentVolumeClaim.ClaimName
	case v.EmptyDir != nil:
		return "emptyDir"
	}
	return "unknown"
}

// TestRenderUsersTableReadOnly: the hashed table is mounted read-only,
// from a Secret, at the path the rendered config's table_file names, and
// multi-session auth refuses anonymous callers.
func TestRenderUsersTableReadOnly(t *testing.T) {
	r := renderGood(t)
	cfg, err := loadLikeDashC([]byte(r.configMap(t, slackTargetName).Data["config.json"]))
	if err != nil {
		t.Fatal(err)
	}
	ms := cfg.Attach.MultiSession
	if !ms.Enabled || ms.AllowAnonymous {
		t.Fatalf("multi_session enabled=%v allow_anonymous=%v; want an enforced bearer table", ms.Enabled, ms.AllowAnonymous)
	}
	if ms.Auth.Kind != "" && ms.Auth.Kind != "bearer_table" {
		t.Errorf("multi_session.auth.kind = %q, want bearer_table", ms.Auth.Kind)
	}
	p := r.pod(t, "Deployment/core-agent-selfdev")
	c := p.container(t, "core-agent")
	var mount *volumeMount
	for i, m := range c.VolumeMounts {
		if m.MountPath == filepath.Dir(ms.Auth.TableFile) {
			mount = &c.VolumeMounts[i]
		}
	}
	if mount == nil {
		t.Fatalf("nothing is mounted at %s, the directory of table_file %s", filepath.Dir(ms.Auth.TableFile), ms.Auth.TableFile)
	}
	if !mount.ReadOnly || mount.SubPath != "" {
		t.Errorf("users table mount readOnly=%v subPath=%q; want a read-only directory mount (an agent that can write the table can add itself)", mount.ReadOnly, mount.SubPath)
	}
	v, _ := p.volume(mount.Name)
	if v.Secret == nil || v.Secret.SecretName != usersSecret || v.Secret.DefaultMode == nil || *v.Secret.DefaultMode != 0o400 {
		t.Errorf("users volume = %+v, want Secret %s with defaultMode 0400", v, usersSecret)
	}
	if !slices.Contains(cfg.Permissions.Auto.TaskFrom, "sa:selfdev-dispatcher") {
		t.Errorf("task_from = %v; the dispatcher's identity must be the listed caller", cfg.Permissions.Auto.TaskFrom)
	}
}

// ---- the dispatcher ----

func TestRenderDispatcher(t *testing.T) {
	r := renderAll(t)
	dep := r.pod(t, "Deployment/selfdev-soak-dispatcher")
	job := r.pod(t, "Job/selfdev-soak-dispatcher-a7")
	if dep.workload.Replicas == nil || *dep.workload.Replicas != 0 || dep.workload.Strategy.Type != "Recreate" {
		t.Errorf("dispatcher Deployment replicas/strategy = %v/%q, want 0/Recreate (it must not run beside the A7 Job)", dep.workload.Replicas, dep.workload.Strategy.Type)
	}
	if job.workload.BackoffLimit == nil || *job.workload.BackoffLimit != 0 || job.spec.RestartPolicy != "Never" {
		t.Errorf("A7 Job backoffLimit/restartPolicy = %v/%q, want 0/Never: a retry would hide a stop", job.workload.BackoffLimit, job.spec.RestartPolicy)
	}
	jc := job.container(t, "dispatcher")
	if !slices.Contains(jc.Args, "--once") {
		t.Errorf("A7 Job args %v lack --once", jc.Args)
	}
	if slices.Contains(dep.container(t, "dispatcher").Args, "--once") {
		t.Error("the polling Deployment runs --once")
	}

	// The two pod specs are one spec: decode both generically, take out
	// the two intended differences, and compare.
	norm := func(p pod) map[string]any {
		var m map[string]any
		if err := p.raw.Decode(&m); err != nil {
			t.Fatal(err)
		}
		delete(m, "restartPolicy")
		for _, c := range m["containers"].([]any) {
			cm := c.(map[string]any)
			var args []any
			for _, a := range cm["args"].([]any) {
				if a != "--once" {
					args = append(args, a)
				}
			}
			cm["args"] = args
		}
		return m
	}
	if !reflect.DeepEqual(norm(dep), norm(job)) {
		t.Error("the A7 Job's pod spec has drifted from the dispatcher Deployment's (beyond --once and restartPolicy); keep 51- and 52- in step")
	}

	known := dispatcherFlags(t)
	for _, a := range jc.Args {
		name, _, _ := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if !known[name] {
			t.Errorf("dispatcher arg %s is not a flag dev/uat/selfdev-soak/dispatcher defines", a)
		}
	}
	args := strings.Join(jc.Args, " ")
	for _, want := range []string{
		"--attach-url=http://core-agent-selfdev:7777",
		"--token-file=/var/run/selfdev-soak/attach/token",
		"--app-key-file=/var/run/selfdev-soak/app/private-key.pem",
		"--worktrees-dir=/workspace/issues",
		"--agents-dir=/workspace/.soak",
		"--state-file=/var/lib/selfdev-soak-dispatcher/state.json",
		"--private-repo=/var/lib/selfdev-soak-dispatcher-repo/mirror.git",
		"--app-id=$(SOAK_APP_ID)",
		"--commit-name=$(SOAK_COMMIT_NAME)",
		"--commit-email=$(SOAK_COMMIT_EMAIL)",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("dispatcher args lack %s", want)
		}
	}
	mounts := map[string]volumeMount{}
	for _, m := range jc.VolumeMounts {
		mounts[m.MountPath] = m
	}
	for path, want := range map[string]string{
		"/var/lib/selfdev-soak-dispatcher":      "pvc:selfdev-soak-dispatcher-state",
		"/var/lib/selfdev-soak-dispatcher-repo": "emptyDir",
		"/workspace":                            "pvc:core-agent-selfdev-workspace",
		"/tmp":                                  "emptyDir",
	} {
		v, _ := job.volume(mounts[path].Name)
		if got := volumeKind(v); got != want {
			t.Errorf("dispatcher %s is %s, want %s", path, got, want)
		}
	}
	// The state PVC and the private repo belong to the dispatcher alone.
	for _, p := range r.pods(t) {
		if p.component == "dispatcher" {
			continue
		}
		for _, v := range p.spec.Volumes {
			if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == "selfdev-soak-dispatcher-state" {
				t.Errorf("%s mounts the dispatcher's state PVC", p.owner)
			}
		}
	}
	// ReadWriteOnce workspace: the dispatcher lands on the daemon's node.
	for _, p := range []pod{dep, job} {
		req := p.spec.Affinity.PodAffinity.Required
		if len(req) != 1 || req[0].TopologyKey != "kubernetes.io/hostname" || req[0].LabelSelector.MatchLabels["app.kubernetes.io/component"] != "daemon" {
			t.Errorf("%s podAffinity = %+v, want required co-location with the daemon by hostname", p.owner, req)
		}
	}
}

// dispatcherFlags reads the flag names dispatcher/main.go registers.
func dispatcherFlags(t *testing.T) map[string]bool {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("dispatcher", "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, m := range regexp.MustCompile(`fs\.\w+Var\([^,]+,\s*"([a-z-]+)"`).FindAllStringSubmatch(string(body), -1) {
		out[m[1]] = true
	}
	if len(out) < 10 {
		t.Fatalf("found only %d dispatcher flags; the parse is broken", len(out))
	}
	return out
}

// ---- pod security ----

func TestRenderSecurityContext(t *testing.T) {
	r := renderAll(t)
	pods := r.pods(t)
	if len(pods) != 3 {
		t.Errorf("found %d pod templates, want 3 (daemon, dispatcher Deployment, A7 Job)", len(pods))
	}
	workspace := map[string]string{}
	for _, p := range pods {
		sc := p.spec.SecurityContext
		for name, got := range map[string]*int64{"runAsUser": sc.RunAsUser, "runAsGroup": sc.RunAsGroup, "fsGroup": sc.FSGroup} {
			if got == nil || *got != 10001 {
				t.Errorf("%s securityContext.%s = %v, want 10001", p.owner, name, got)
			}
		}
		if sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
			t.Errorf("%s runAsNonRoot is not true", p.owner)
		}
		if sc.SeccompProfile.Type != "RuntimeDefault" {
			t.Errorf("%s seccompProfile = %q, want RuntimeDefault", p.owner, sc.SeccompProfile.Type)
		}
		if p.spec.Automount == nil || *p.spec.Automount {
			t.Errorf("%s mounts its service account token", p.owner)
		}
		for _, c := range p.allContainers() {
			csc := c.SecurityContext
			if csc.AllowPrivilegeEscalation == nil || *csc.AllowPrivilegeEscalation {
				t.Errorf("%s/%s allowPrivilegeEscalation is not false", p.owner, c.Name)
			}
			if csc.ReadOnlyRootFilesystem == nil || !*csc.ReadOnlyRootFilesystem {
				t.Errorf("%s/%s readOnlyRootFilesystem is not true", p.owner, c.Name)
			}
			if !slices.Equal(csc.Capabilities.Drop, []string{"ALL"}) {
				t.Errorf("%s/%s drops %v, want [ALL]", p.owner, c.Name, csc.Capabilities.Drop)
			}
		}
		for _, c := range p.spec.Containers {
			got := map[string]string{}
			for _, m := range c.VolumeMounts {
				v, _ := p.volume(m.Name)
				got[m.MountPath] = volumeKind(v)
				if m.MountPath == "/workspace" {
					workspace[p.owner] = volumeKind(v)
				}
			}
			for _, path := range []string{"/tmp", "/home/soak"} {
				if got[path] != "emptyDir" {
					t.Errorf("%s/%s has %q at %s, want an emptyDir (the root filesystem is read-only)", p.owner, c.Name, got[path], path)
				}
			}
		}
	}
	// Same uid AND same workspace path in both pods: the task names the
	// copy's path, and the agent must be able to write what the
	// dispatcher created.
	if len(workspace) != 3 {
		t.Errorf("/workspace is mounted in %d of 3 pods: %v", len(workspace), workspace)
	}
	for owner, kind := range workspace {
		if kind != "pvc:core-agent-selfdev-workspace" {
			t.Errorf("%s /workspace is %s", owner, kind)
		}
	}
	for _, sa := range r.byKind("ServiceAccount") {
		if sa.Automount == nil || *sa.Automount {
			t.Errorf("ServiceAccount %s automounts its token", sa.Metadata.Name)
		}
		if len(sa.Metadata.Annotations) > 0 {
			t.Errorf("ServiceAccount %s carries annotations %v; Workload Identity is granted to the KSA principal directly, and the dispatcher's KSA gets nothing", sa.Metadata.Name, sa.Metadata.Annotations)
		}
	}
	for _, kind := range []string{"Role", "RoleBinding", "ClusterRole", "ClusterRoleBinding"} {
		if n := len(r.byKind(kind)); n > 0 {
			t.Errorf("the render grants Kubernetes RBAC (%d %s); neither identity needs any", n, kind)
		}
	}
}

func TestRenderNamespace(t *testing.T) {
	r := renderAll(t)
	ns := r.get(t, "Namespace", soakNamespace)
	if ns.Metadata.Labels["pod-security.kubernetes.io/enforce"] != "restricted" {
		t.Errorf("namespace labels = %v, want restricted Pod Security enforced", ns.Metadata.Labels)
	}
	for _, o := range r.objects {
		if o.Kind == "Namespace" {
			continue
		}
		if o.Metadata.Namespace != soakNamespace {
			t.Errorf("%s/%s is in namespace %q, want %s", o.Kind, o.Metadata.Name, o.Metadata.Namespace, soakNamespace)
		}
	}
	svc := r.get(t, "Service", "core-agent-selfdev")
	var spec struct {
		Type string `yaml:"type"`
	}
	if err := svc.Spec.Decode(&spec); err != nil {
		t.Fatal(err)
	}
	if spec.Type != "ClusterIP" {
		t.Errorf("daemon Service type = %q, want ClusterIP", spec.Type)
	}
}

// ---- network policy ----

type npPeer struct {
	PodSelector *struct {
		MatchLabels map[string]string `yaml:"matchLabels"`
	} `yaml:"podSelector"`
	NamespaceSelector *struct {
		MatchLabels map[string]string `yaml:"matchLabels"`
	} `yaml:"namespaceSelector"`
	IPBlock *struct {
		CIDR   string   `yaml:"cidr"`
		Except []string `yaml:"except"`
	} `yaml:"ipBlock"`
}

type npPort struct {
	Protocol string `yaml:"protocol"`
	Port     int    `yaml:"port"`
}

type npRule struct {
	From  []npPeer `yaml:"from"`
	To    []npPeer `yaml:"to"`
	Ports []npPort `yaml:"ports"`
}

type npSpec struct {
	PodSelector struct {
		MatchLabels map[string]string `yaml:"matchLabels"`
	} `yaml:"podSelector"`
	PolicyTypes []string `yaml:"policyTypes"`
	Ingress     []npRule `yaml:"ingress"`
	Egress      []npRule `yaml:"egress"`
}

// grants flattens every NetworkPolicy rule selecting a component into
// "direction peer port" strings, so a test can compare the whole set.
func grants(t *testing.T, r rendered, component string) (ingress, egress []string) {
	t.Helper()
	for _, o := range r.byKind("NetworkPolicy") {
		var s npSpec
		if err := o.Spec.Decode(&s); err != nil {
			t.Fatal(err)
		}
		sel := s.PodSelector.MatchLabels
		if len(sel) > 0 && sel["app.kubernetes.io/component"] != component {
			continue
		}
		flat := func(rules []npRule, peers func(npRule) []npPeer) []string {
			var out []string
			for _, rule := range rules {
				for _, peer := range peers(rule) {
					var p string
					switch {
					case peer.IPBlock != nil:
						ex := slices.Clone(peer.IPBlock.Except)
						sort.Strings(ex)
						p = "ip:" + peer.IPBlock.CIDR
						if len(ex) > 0 {
							p += "-except:" + strings.Join(ex, ",")
						}
					case peer.NamespaceSelector != nil:
						p = "ns:" + peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"]
						if peer.PodSelector != nil {
							p += "/pod:" + peer.PodSelector.MatchLabels["k8s-app"]
						}
					case peer.PodSelector != nil:
						p = "pod:" + peer.PodSelector.MatchLabels["app.kubernetes.io/component"]
					}
					if len(rule.Ports) == 0 {
						out = append(out, p+" any")
					}
					for _, port := range rule.Ports {
						out = append(out, p+" "+port.Protocol+"/"+strconv.Itoa(port.Port))
					}
				}
				if len(peers(rule)) == 0 {
					out = append(out, "ANYWHERE")
				}
			}
			return out
		}
		ingress = append(ingress, flat(s.Ingress, func(r npRule) []npPeer { return r.From })...)
		egress = append(egress, flat(s.Egress, func(r npRule) []npPeer { return r.To })...)
	}
	sort.Strings(ingress)
	sort.Strings(egress)
	return ingress, egress
}

const publicExcept = "ip:0.0.0.0/0-except:10.0.0.0/8,100.64.0.0/10,169.254.0.0/16,172.16.0.0/12,192.168.0.0/16"

var dnsGrants = []string{
	"ip:169.254.20.10/32 TCP/53", "ip:169.254.20.10/32 UDP/53",
	"ip:169.254.169.254/32 TCP/53", "ip:169.254.169.254/32 UDP/53",
	"ns:kube-system/pod:kube-dns TCP/53", "ns:kube-system/pod:kube-dns UDP/53",
}

func sorted(s ...[]string) []string {
	var out []string
	for _, x := range s {
		out = append(out, x...)
	}
	sort.Strings(out)
	return out
}

func checkNetworkPolicy(t *testing.T, r rendered, https bool) {
	t.Helper()
	dd := r.get(t, "NetworkPolicy", "default-deny")
	var s npSpec
	if err := dd.Spec.Decode(&s); err != nil {
		t.Fatal(err)
	}
	if len(s.PodSelector.MatchLabels) != 0 || !slices.Equal(s.PolicyTypes, []string{"Ingress", "Egress"}) || len(s.Ingress)+len(s.Egress) != 0 {
		t.Errorf("default-deny = %+v, want every pod, both directions, no rules", s)
	}
	daemonHTTPS, dispatcherHTTPS := []string{publicExcept + " TCP/443"}, []string{publicExcept + " TCP/443"}
	if !https {
		daemonHTTPS, dispatcherHTTPS = nil, nil
	}
	in, out := grants(t, r, "daemon")
	if want := []string{"pod:dispatcher TCP/7777"}; !slices.Equal(in, want) {
		t.Errorf("daemon ingress = %v, want %v", in, want)
	}
	wantOut := sorted(dnsGrants, []string{"ip:169.254.169.252/32 TCP/988", "ip:169.254.169.254/32 TCP/80"}, daemonHTTPS)
	if !slices.Equal(out, wantOut) {
		t.Errorf("daemon egress =\n  %v\nwant\n  %v", out, wantOut)
	}
	in, out = grants(t, r, "dispatcher")
	if len(in) != 0 {
		t.Errorf("dispatcher ingress = %v, want none", in)
	}
	wantOut = sorted(dnsGrants, []string{"pod:daemon TCP/7777"}, dispatcherHTTPS)
	if !slices.Equal(out, wantOut) {
		t.Errorf("dispatcher egress =\n  %v\nwant\n  %v", out, wantOut)
	}
}

func TestRenderNetworkPolicy(t *testing.T) {
	checkNetworkPolicy(t, renderGood(t), true)
}

type fqdnSpec struct {
	PodSelector struct {
		MatchLabels map[string]string `yaml:"matchLabels"`
	} `yaml:"podSelector"`
	Egress []struct {
		Matches []struct {
			Name    string `yaml:"name"`
			Pattern string `yaml:"pattern"`
		} `yaml:"matches"`
		Ports []npPort `yaml:"ports"`
	} `yaml:"egress"`
}

func TestRenderFQDNEgress(t *testing.T) {
	r := renderGood(t, "--fqdn-egress")
	checkNetworkPolicy(t, r, false)
	want := map[string][]string{
		"daemon":     {"*-aiplatform.googleapis.com", "aiplatform.googleapis.com", "proxy.golang.org", "sum.golang.org", "vuln.go.dev"},
		"dispatcher": {"api.github.com", "github.com"},
	}
	got := map[string][]string{}
	for _, o := range r.byKind("FQDNNetworkPolicy") {
		var s fqdnSpec
		if err := o.Spec.Decode(&s); err != nil {
			t.Fatal(err)
		}
		comp := s.PodSelector.MatchLabels["app.kubernetes.io/component"]
		for _, e := range s.Egress {
			if !slices.Equal(e.Ports, []npPort{{Protocol: "TCP", Port: 443}}) {
				t.Errorf("FQDNNetworkPolicy %s ports = %v, want TCP/443 only", o.Metadata.Name, e.Ports)
			}
			for _, m := range e.Matches {
				got[comp] = append(got[comp], m.Name+m.Pattern)
			}
		}
	}
	for comp, hosts := range want {
		g := got[comp]
		sort.Strings(g)
		if !slices.Equal(g, hosts) {
			t.Errorf("%s FQDN egress = %v, want %v", comp, g, hosts)
		}
	}
	if len(got) != len(want) {
		t.Errorf("FQDN egress covers %v, want exactly the daemon and the dispatcher", got)
	}
}
