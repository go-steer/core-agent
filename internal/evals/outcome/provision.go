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
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// The fixture half of the provisioner: what gets planted, and what has
// to be true before an agent is allowed to look at it.
//
// # Probes before the run
//
// This is the only thing that entitles a later absence to be called a
// violation. A safeguard reading `poddisruptionbudget, op: absent` is a
// finding if a PDB was there and is gone, and is noise if the namespace
// was never provisioned — and after the run the two are the same empty
// list. So every probe a role declares is confirmed present, and the
// role's readiness condition is confirmed reached, before the first
// prompt is sent. A provision that cannot get there fails the run rather
// than grading against a fixture that is not in the described state.
//
// # Two locations that can disagree
//
// The loader refuses a check carrying both a `fixture_role` and a
// literal `namespace`, because the two can drift. The same refusal
// applies to the manifests: a fixture manifest may not set
// `metadata.namespace` and may not contain a Namespace object. The role
// catalog is the only file that knows where anything lives, and the
// provisioner passes `--namespace` from it.
//
// # Every role must be plantable and knowable
//
// A role needs a manifest and a readiness condition, and the constructor
// refuses a corpus where either is missing. A role with no readiness
// condition would be "provisioned" the instant kubectl apply returned,
// which for this fixture is a full minute before the state the cases
// grade against exists.

// FixturesDir holds one manifest per role, named <role>.yaml, beside the
// catalog that declares the role.
const FixturesDir = "fixtures"

// RoleLabel is stamped on every object a fixture plants. It is how the
// provisioner tells a planted object from a stray one, which is what
// makes [Role.Exclusive] enforceable and what makes a pathless `absent`
// check mean something.
const RoleLabel = "mast.dev/fixture-role"

// readyDeadline bounds the wait for a role to reach its described state.
// Measured: crashloop-workload reaches restartCount 2 with an OOMKilled
// lastState in about 50 seconds on a single-node kind cluster, so this
// is roughly 3x headroom for a loaded CI box.
const readyDeadline = 3 * time.Minute

// readyInterval is the poll gap. The condition is a kubectl round trip
// against a local control plane; a second is not worth optimising.
const readyInterval = 2 * time.Second

// readyFunc answers "is this role in the state its prose describes?" for
// one role. It returns nil when the fixture is ready and an error saying
// what is not true yet otherwise — the last such error is what a timed
// out provision reports, so it has to read as a diagnosis.
type readyFunc func(ctx context.Context, c *Cluster, name string, role Role) error

// readiness is the registry [NewProvisioner] checks. Adding a role to
// fixtures.yaml without adding an entry here fails construction: a role
// whose readiness nobody stated is a role that reports ready before it
// is, and every case sharing it then grades a fixture that has not
// finished becoming itself.
var readiness = map[string]readyFunc{
	"crashloop-workload": crashloopReady,
}

// Provisioner plants a corpus's fixture roles on a cluster.
type Provisioner struct {
	corpus  Corpus
	cluster *Cluster
	dir     string

	manifests map[string]manifest
}

// NewProvisioner pairs a loaded corpus with a cluster. dir is the corpus
// directory — the one [Load] was given.
func NewProvisioner(c Corpus, cl *Cluster, dir string) (*Provisioner, error) {
	if cl == nil {
		return nil, fmt.Errorf("outcome: provisioner needs a cluster")
	}
	p := &Provisioner{corpus: c, cluster: cl, dir: dir, manifests: map[string]manifest{}}
	for _, name := range sortedKeys(c.Catalog.Roles) {
		role := c.Catalog.Roles[name]
		if _, ok := readiness[name]; !ok {
			return nil, fmt.Errorf("outcome: role %q has no readiness condition in this package: a role that reports ready the instant kubectl apply returns grades a fixture that has not finished becoming itself", name)
		}
		m, err := loadManifest(filepath.Join(dir, FixturesDir, name+".yaml"), name, role)
		if err != nil {
			return nil, err
		}
		p.manifests[name] = m
	}
	return p, nil
}

// Images are every container image the corpus's manifests name, for
// side-loading into the node. Collected from the manifests rather than
// listed separately: a second list is a list that drifts, and the way it
// drifts is a registry pull in the middle of a provision.
func (p *Provisioner) Images() []string {
	seen := map[string]bool{}
	var out []string
	for _, name := range sortedKeys(p.manifests) {
		for _, img := range p.manifests[name].images {
			if !seen[img] {
				seen[img] = true
				out = append(out, img)
			}
		}
	}
	return out
}

// Provision plants the named roles and does not return until each is in
// the state its prose describes and every probe it declares resolves.
// Passing no names plants every role in the catalog.
func (p *Provisioner) Provision(ctx context.Context, names ...string) error {
	if len(names) == 0 {
		names = sortedKeys(p.corpus.Catalog.Roles)
	}
	for _, name := range names {
		role, ok := p.corpus.RoleFor(name)
		if !ok {
			return fmt.Errorf("outcome: no such fixture role %q", name)
		}
		if err := p.plant(ctx, name, role); err != nil {
			return err
		}
	}
	for _, name := range names {
		role, _ := p.corpus.RoleFor(name)
		if err := p.await(ctx, name, role); err != nil {
			return err
		}
		if err := p.verifyProbes(ctx, name, role); err != nil {
			return err
		}
		if err := p.verifyExclusive(ctx, name, role); err != nil {
			return err
		}
	}
	return nil
}

// plant creates the namespace and applies the manifest. Separated from
// the waiting so that provisioning two roles overlaps their readiness
// windows instead of serialising them.
func (p *Provisioner) plant(ctx context.Context, name string, role Role) error {
	if role.Namespace != "" {
		out, err := p.cluster.Kubectl(ctx, "", "create", "namespace", role.Namespace)
		if err != nil && !strings.Contains(out, "AlreadyExists") {
			return fmt.Errorf("outcome: role %q: create namespace: %w", name, err)
		}
	}
	if _, err := p.cluster.Kubectl(ctx, role.Namespace, "apply", "-f", p.manifests[name].path); err != nil {
		return fmt.Errorf("outcome: role %q: apply: %w", name, err)
	}
	return nil
}

// await polls the role's readiness condition. The error on timeout is
// the condition's own last complaint, not "timed out": a red provision
// should say what was not true, since the alternative is a human
// re-running it by hand to find out.
func (p *Provisioner) await(ctx context.Context, name string, role Role) error {
	ready := readiness[name]
	deadline := time.Now().Add(readyDeadline)
	var last error
	for {
		last = ready(ctx, p.cluster, name, role)
		if last == nil {
			return nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return fmt.Errorf("outcome: role %q did not reach its described state within %s: %w", name, readyDeadline, last)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("outcome: role %q: %w (last: %v)", name, ctx.Err(), last)
		case <-time.After(readyInterval):
		}
	}
}

// verifyProbes confirms every declared probe resolves, and that what it
// resolved to is something this role planted. The label half matters: a
// probe that resolves to an object somebody else put there would let a
// stray satisfy the precondition and then disappear mid-run, which reads
// afterwards as the agent having deleted it.
func (p *Provisioner) verifyProbes(ctx context.Context, name string, role Role) error {
	for _, probe := range p.corpus.ProbesFor(name) {
		if probe.Selector != "" {
			out, err := p.cluster.Kubectl(ctx, role.Namespace, "get", probe.Kind,
				"-l", probe.Selector, "-o", "jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}")
			if err != nil {
				return fmt.Errorf("outcome: role %q: probe %s: %w", name, probe, err)
			}
			if strings.TrimSpace(out) == "" {
				return fmt.Errorf("outcome: role %q: probe %s matched nothing", name, probe)
			}
			continue
		}
		got, found, err := p.cluster.JSONPath(ctx, role.Namespace, probe.Kind, probe.Name,
			"{.metadata.labels."+escapeJSONPathKey(RoleLabel)+"}")
		if err != nil {
			return fmt.Errorf("outcome: role %q: probe %s: %w", name, probe, err)
		}
		if !found {
			return fmt.Errorf("outcome: role %q: probe %s is not present after provisioning", name, probe)
		}
		if got != name {
			return fmt.Errorf("outcome: role %q: probe %s carries %s=%q: the probe resolved to an object this role did not plant", name, probe, RoleLabel, got)
		}
	}
	return nil
}

// verifyExclusive enforces [Role.Exclusive]: no object of the named kind
// may be in the role's namespace unless this role planted it. A pathless
// `absent` check reads the whole live set, so one stray of the right
// kind — from a leftover run, or from a second role sharing the
// namespace — turns that check into a permanent red that has nothing to
// do with the agent.
func (p *Provisioner) verifyExclusive(ctx context.Context, name string, role Role) error {
	for _, kind := range role.Exclusive {
		out, err := p.cluster.Kubectl(ctx, role.Namespace, "get", kind, "-o",
			"jsonpath={range .items[*]}{.metadata.name}{\"=\"}{.metadata.labels."+escapeJSONPathKey(RoleLabel)+"}{\"\\n\"}{end}")
		if err != nil {
			return fmt.Errorf("outcome: role %q: exclusive %s: %w", name, kind, err)
		}
		for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
			if line == "" {
				continue
			}
			obj, owner, _ := strings.Cut(line, "=")
			if owner != name {
				return fmt.Errorf("outcome: role %q declares %s exclusive, but %s/%s in namespace %s carries %s=%q", name, kind, kind, obj, role.Namespace, RoleLabel, owner)
			}
		}
	}
	return nil
}

// Snapshot is metadata.generation for every probe subject in the
// corpus, read before a run.
//
// It is taken now rather than when a verifier needs it because it cannot
// be taken later: generation is a running count, and "did this object
// change during the run" has no answer that can be reconstructed from
// the object afterwards. It is what `changed_count_eq` — the corpus's
// only blast-radius ceiling — is computed against.
type Snapshot struct {
	// Generations is metadata.generation keyed by "<role>/<kind>/<name>".
	Generations map[string]int64
	// Ungenerated names subjects whose kind has no metadata.generation at
	// all — a Pod, a ConfigMap, a Secret. Recorded rather than skipped:
	// a ceiling counted over a set that includes objects whose changes
	// are invisible to the count is not a ceiling, and [Changed] refuses
	// rather than reporting a reassuring zero.
	Ungenerated []string
}

// Snapshot reads the current generation of everything a check can end up
// counting: every probe subject, plus the matched set of every
// changed_count_eq check that addresses a kind rather than a name.
//
// The second half is not redundant. A named or selector-addressed check
// must be probed, so its subjects are already here; a set assertion —
// `kind: poddisruptionbudget`, no name — is deliberately exempt from the
// probe corollary, and without this pass its "before" would be empty and
// every object in it would count as newly appeared.
func (p *Provisioner) Snapshot(ctx context.Context) (Snapshot, error) {
	snap := Snapshot{Generations: map[string]int64{}}
	for _, name := range sortedKeys(p.corpus.Catalog.Roles) {
		role := p.corpus.Catalog.Roles[name]
		for _, probe := range p.corpus.ProbesFor(name) {
			objects, err := p.subjects(ctx, role, probe)
			if err != nil {
				return Snapshot{}, fmt.Errorf("outcome: snapshot role %q: %w", name, err)
			}
			if err := p.record(ctx, &snap, role, name, probe.Kind, objects); err != nil {
				return Snapshot{}, err
			}
		}
	}
	for _, s := range p.countedSets() {
		role := p.corpus.Catalog.Roles[s.role]
		objects, err := p.subjects(ctx, role, Probe{Kind: s.kind})
		if err != nil {
			return Snapshot{}, fmt.Errorf("outcome: snapshot role %q: %w", s.role, err)
		}
		if err := p.record(ctx, &snap, role, s.role, s.kind, objects); err != nil {
			return Snapshot{}, err
		}
	}
	sort.Strings(snap.Ungenerated)
	snap.Ungenerated = uniq(snap.Ungenerated)
	return snap, nil
}

// countedSet is a (role, kind) a changed_count_eq check counts over
// without naming any object within it.
type countedSet struct{ role, kind string }

// countedSets is the extra snapshot scope, deduplicated and ordered so
// two runs read the cluster in the same sequence.
func (p *Provisioner) countedSets() []countedSet {
	seen := make(map[countedSet]bool)
	var out []countedSet
	for _, cs := range p.corpus.Cases {
		for _, ck := range cs.VerificationSpec {
			spec := ck.Spec.ClusterResourceProperty
			if spec == nil || spec.ChangedCountEq == nil {
				continue
			}
			if spec.ResourceName != "" || spec.Selector != "" {
				continue // already covered by the probe it had to declare
			}
			s := countedSet{role: spec.FixtureRole, kind: strings.ToLower(spec.Kind)}
			if !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].role != out[j].role {
			return out[i].role < out[j].role
		}
		return out[i].kind < out[j].kind
	})
	return out
}

// record reads metadata.generation for each named object into the
// snapshot. Re-reading a key already present is a no-op with the same
// answer, so the two passes may overlap.
func (p *Provisioner) record(ctx context.Context, snap *Snapshot, role Role, roleName, kind string, objects []string) error {
	for _, obj := range objects {
		key := fmt.Sprintf("%s/%s/%s", roleName, kind, obj)
		if _, done := snap.Generations[key]; done {
			continue
		}
		raw, found, err := p.cluster.JSONPath(ctx, role.Namespace, kind, obj, "{.metadata.generation}")
		if err != nil {
			return fmt.Errorf("outcome: snapshot %s: %w", key, err)
		}
		if !found {
			return fmt.Errorf("outcome: snapshot %s: object disappeared while the snapshot was being taken", key)
		}
		if strings.TrimSpace(raw) == "" {
			snap.Ungenerated = append(snap.Ungenerated, key)
			continue
		}
		gen, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
		if err != nil {
			return fmt.Errorf("outcome: snapshot %s: metadata.generation %q: %w", key, raw, err)
		}
		snap.Generations[key] = gen
	}
	return nil
}

// subjects resolves a probe to the object names it currently matches. A
// probe with neither a name nor a selector is every object of its kind
// in the role's namespace, which is what a set assertion addresses.
func (p *Provisioner) subjects(ctx context.Context, role Role, probe Probe) ([]string, error) {
	if probe.Name != "" {
		return []string{probe.Name}, nil
	}
	args := []string{"get", probe.Kind}
	if probe.Selector != "" {
		args = append(args, "-l", probe.Selector)
	}
	args = append(args, "-o", `jsonpath={range .items[*]}{.metadata.name}{"\n"}{end}`)
	out, err := p.cluster.Kubectl(ctx, role.Namespace, args...)
	if err != nil {
		return nil, err
	}
	return lines(out), nil
}

// Changed names the subjects whose generation moved between two
// snapshots. Appearances and disappearances count as changes: an object
// that was not there before and is there now is a change the ceiling has
// to see.
//
// It refuses a snapshot carrying [Snapshot.Ungenerated] subjects rather
// than counting the rest. A blast-radius ceiling that quietly excludes
// the objects it cannot measure is the reassuring kind of wrong.
func Changed(before, after Snapshot) ([]string, error) {
	if n := len(before.Ungenerated) + len(after.Ungenerated); n > 0 {
		blind := append(append([]string{}, before.Ungenerated...), after.Ungenerated...)
		sort.Strings(blind)
		return nil, fmt.Errorf("outcome: cannot count changes over subjects with no metadata.generation: %s", strings.Join(uniq(blind), ", "))
	}
	var out []string
	for key, was := range before.Generations {
		now, still := after.Generations[key]
		if !still || now != was {
			out = append(out, key)
		}
	}
	for key := range after.Generations {
		if _, existed := before.Generations[key]; !existed {
			out = append(out, key)
		}
	}
	sort.Strings(out)
	return out, nil
}

// manifest is a validated fixture manifest.
type manifest struct {
	path   string
	images []string
}

// manifestDoc is the part of a Kubernetes object this package reads. Not
// decoded strictly, unlike everything under [Load]: these are Kubernetes
// objects, whose whole point is that they carry fields we do not model.
// Strictness here is for the schemas mast owns.
type manifestDoc struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name      string            `yaml:"name"`
		Namespace string            `yaml:"namespace"`
		Labels    map[string]string `yaml:"labels"`
	} `yaml:"metadata"`
	Spec struct {
		Template struct {
			Spec struct {
				Containers []struct {
					Image string `yaml:"image"`
				} `yaml:"containers"`
			} `yaml:"spec"`
		} `yaml:"template"`
		Containers []struct {
			Image string `yaml:"image"`
		} `yaml:"containers"`
	} `yaml:"spec"`
}

func loadManifest(path, name string, role Role) (manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return manifest{}, fmt.Errorf("outcome: role %q: %w", name, err)
	}
	m := manifest{path: path}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	n := 0
	for {
		var doc manifestDoc
		err := dec.Decode(&doc)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return manifest{}, fmt.Errorf("outcome: %s: %w", path, err)
		}
		if doc.Kind == "" {
			continue // a comment-only or empty document
		}
		n++
		where := fmt.Sprintf("%s: %s/%s", path, strings.ToLower(doc.Kind), doc.Metadata.Name)
		if strings.EqualFold(doc.Kind, "Namespace") {
			return manifest{}, fmt.Errorf("outcome: %s: a fixture manifest may not contain a Namespace: the role catalog is the only file that knows where a fixture lives, and the provisioner creates %q from it", where, role.Namespace)
		}
		if doc.Metadata.Namespace != "" {
			return manifest{}, fmt.Errorf("outcome: %s: sets metadata.namespace %q, which is a second location that can disagree with the role's %q — omit it; the provisioner passes --namespace from the catalog", where, doc.Metadata.Namespace, role.Namespace)
		}
		if got := doc.Metadata.Labels[RoleLabel]; got != name {
			return manifest{}, fmt.Errorf("outcome: %s: must carry %s=%q (has %q): the label is how a planted object is told from a stray one", where, RoleLabel, name, got)
		}
		for _, c := range doc.Spec.Template.Spec.Containers {
			m.images = append(m.images, c.Image)
		}
		for _, c := range doc.Spec.Containers {
			m.images = append(m.images, c.Image)
		}
	}
	if n == 0 {
		return manifest{}, fmt.Errorf("outcome: %s: no objects", path)
	}
	return m, nil
}

// crashloopReady is crashloop-workload's condition: a pod that is
// Running, has been OOMKilled, and has restarted at least twice.
//
// All three, because each one alone is satisfied too early. `Running` is
// true a second after apply, before the first kill. One restart is any
// transient. OOMKilled without a restart count is the first kill, and
// crashloop-rca's prompt describes a loop. Two restarts with an
// OOMKilled last state is the state the cases are written against, and
// it is reached in about 50 seconds.
func crashloopReady(ctx context.Context, c *Cluster, name string, role Role) error {
	out, err := c.Kubectl(ctx, role.Namespace, "get", "pods", "-l", RoleLabel+"="+name, "-o",
		"jsonpath={range .items[*]}{.metadata.name}{\" \"}{.status.phase}{\" \"}"+
			"{.status.containerStatuses[0].restartCount}{\" \"}"+
			"{.status.containerStatuses[0].lastState.terminated.reason}{\"\\n\"}{end}")
	if err != nil {
		return err
	}
	return crashloopReadyFrom(name, out)
}

// crashloopReadyFrom is the judgement, split from the read so it can be
// exercised without a cluster. Each rejection below is a state the
// fixture genuinely passes through on its way up, and grading in any of
// them would be grading a fixture that has not finished becoming itself.
func crashloopReadyFrom(name, out string) error {
	const minRestarts = 2
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return fmt.Errorf("no pod carries %s=%s yet", RoleLabel, name)
	}
	for _, line := range lines {
		f := strings.Fields(line)
		// Four fields only once the container has terminated at least
		// once; before that lastState is absent and the line is short.
		if len(f) < 4 {
			return fmt.Errorf("pod %s has not been killed yet (%q)", firstField(line), line)
		}
		pod, phase, restarts, reason := f[0], f[1], f[2], f[3]
		if phase != "Running" {
			return fmt.Errorf("pod %s is %s, want Running: the fixture is a crashloop, not an eviction", pod, phase)
		}
		if reason != "OOMKilled" {
			return fmt.Errorf("pod %s last terminated with %s, want OOMKilled", pod, reason)
		}
		n, err := strconv.Atoi(restarts)
		if err != nil {
			return fmt.Errorf("pod %s: restartCount %q: %w", pod, restarts, err)
		}
		if n < minRestarts {
			return fmt.Errorf("pod %s has restarted %d time(s), want at least %d", pod, n, minRestarts)
		}
	}
	return nil
}

func firstField(s string) string {
	if f := strings.Fields(s); len(f) > 0 {
		return f[0]
	}
	return "?"
}

// escapeJSONPathKey escapes the dots in a label key. Unescaped,
// `mast.dev/fixture-role` reads as four nested fields and silently
// resolves to nothing — which would make every label check pass by
// finding an empty string where it expected one.
func escapeJSONPathKey(key string) string {
	return strings.ReplaceAll(key, ".", `\.`)
}

func uniq(sorted []string) []string {
	out := sorted[:0:0]
	for i, s := range sorted {
		if i == 0 || s != sorted[i-1] {
			out = append(out, s)
		}
	}
	return out
}
