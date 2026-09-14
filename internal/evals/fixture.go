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

package evals

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// SharedDir is the fixture directory whose contents are laid down under
// every fixture before the fixture's own files.
//
// It is how the kubectl shim is shared without any fixture naming its
// path: materialize copies _shared/ first and the fixture second, so a
// fixture inherits the shim by default and overrides it by shipping a
// file at the same relative path. One rule, no new concept, and nothing
// in a fixture.json points at another directory.
const SharedDir = "_shared"

// A Fixture is the world a case runs against, and the only thing in the
// corpus that knows a path.
//
// Roles are why. A case says "the answer must name ${fact.workload}";
// it never says which file holds the workload or which directory the
// agent starts in. That indirection is not tidiness — it is what lets
// the probe rule mean something. Probes are confirmed present in the
// materialized world before the agent starts, so a check that fails on
// an absence is entitled to be called a violation rather than an
// environment nobody provisioned.
type Fixture struct {
	// Name is the fixture directory's name. Set by LoadFixture from the
	// directory, and checked against the file so a renamed directory
	// cannot quietly keep answering to its old name.
	Name string `json:"name"`

	// Description says what world this is, in one sentence.
	Description string `json:"description"`

	// Roles are the fixture's addressable parts.
	Roles map[string]Role `json:"roles"`

	// PathRole names the role whose directory is prepended to PATH.
	// That is how the agent's `bash` reaches the fixture's tools
	// instead of the host's. Optional: a fixture with no tools of its
	// own leaves it empty.
	PathRole string `json:"path_role,omitempty"`

	// WorkdirRole names the role the agent runs in. Required.
	WorkdirRole string `json:"workdir_role"`

	// FactsFrom locates the fixture's facts, as "<role>#<key>". The
	// role's file is JSON, the key holds an object, and every value in
	// it must be a string.
	//
	// Facts live inside the world file rather than beside it on
	// purpose. The shim renders the world from that same file, so the
	// value a check asserts and the value the agent can observe are one
	// string, not two that agree today. A name asserted on both sides
	// of a test is untested.
	FactsFrom string `json:"facts_from"`

	// Witnesses map a name to a path, relative to the materialized
	// root, that the *world* writes during the run.
	//
	// Deliberately not pre-created. A witness that does not exist after
	// a run is a different fact from a witness that exists and is
	// empty — the first means nothing ever reached the world, and a
	// none_of check against it is vacuous rather than satisfied.
	Witnesses map[string]string `json:"witnesses"`

	// Env is handed to the agent process. Values may contain
	// ${role.<name>} and ${witness.<name>}, expanded to absolute paths
	// in the materialized root.
	Env map[string]string `json:"env,omitempty"`

	// Facts are loaded from FactsFrom. Not serialized: they are derived.
	Facts map[string]string `json:"-"`

	// dir is the fixture's source directory.
	dir string
	// root is the fixtures root, which holds _shared.
	root string
}

// A Role is one addressable part of a fixture's world.
type Role struct {
	// Path is relative to the fixture root, and may be a file or a
	// directory.
	Path string `json:"path"`

	// Probes are paths relative to Path that must exist in the
	// materialized world before the agent starts. Empty means Path
	// itself is the probe.
	Probes []string `json:"probes,omitempty"`
}

// LoadFixture reads <root>/<name>/fixture.json and its facts.
func LoadFixture(root, name string) (*Fixture, error) {
	if name == SharedDir {
		return nil, fmt.Errorf("evals: %q is the shared overlay, not a fixture", SharedDir)
	}
	dir := filepath.Join(root, name)
	raw, err := os.ReadFile(filepath.Join(dir, "fixture.json"))
	if err != nil {
		return nil, fmt.Errorf("evals: read fixture: %w", err)
	}
	var f Fixture
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("evals: parse fixture %s: %w", name, err)
	}
	f.dir, f.root = dir, root
	if f.Name != name {
		return nil, fmt.Errorf("evals: fixture %s declares name %q; the directory is the identity", name, f.Name)
	}
	if err := f.validate(); err != nil {
		return nil, fmt.Errorf("evals: fixture %s: %w", name, err)
	}
	if err := f.loadFacts(); err != nil {
		return nil, fmt.Errorf("evals: fixture %s: %w", name, err)
	}
	return &f, nil
}

func (f *Fixture) validate() error {
	if strings.TrimSpace(f.Description) == "" {
		return fmt.Errorf("description is required")
	}
	if len(f.Roles) == 0 {
		return fmt.Errorf("at least one role is required")
	}
	for name, r := range f.Roles {
		if strings.TrimSpace(r.Path) == "" {
			return fmt.Errorf("role %q: path is required", name)
		}
		if filepath.IsAbs(r.Path) || strings.Contains(r.Path, "..") {
			return fmt.Errorf("role %q: path %q must be relative and must not escape the fixture", name, r.Path)
		}
	}
	if f.WorkdirRole == "" {
		return fmt.Errorf("workdir_role is required")
	}
	if _, ok := f.Roles[f.WorkdirRole]; !ok {
		return fmt.Errorf("workdir_role %q is not a declared role (have: %s)", f.WorkdirRole, strings.Join(sortedKeys(f.Roles), ", "))
	}
	if f.PathRole != "" {
		if _, ok := f.Roles[f.PathRole]; !ok {
			return fmt.Errorf("path_role %q is not a declared role (have: %s)", f.PathRole, strings.Join(sortedKeys(f.Roles), ", "))
		}
	}
	if len(f.Witnesses) == 0 {
		return fmt.Errorf("at least one witness is required; a fixture the world cannot write to can only be graded on narration")
	}
	for name, p := range f.Witnesses {
		if strings.TrimSpace(p) == "" || filepath.IsAbs(p) || strings.Contains(p, "..") {
			return fmt.Errorf("witness %q: path %q must be relative and must not escape the fixture", name, p)
		}
	}
	if !strings.Contains(f.FactsFrom, "#") {
		return fmt.Errorf("facts_from %q must be \"<role>#<key>\"", f.FactsFrom)
	}
	return nil
}

func (f *Fixture) loadFacts() error {
	roleName, key, _ := strings.Cut(f.FactsFrom, "#")
	role, ok := f.Roles[roleName]
	if !ok {
		return fmt.Errorf("facts_from names role %q, which is not declared (have: %s)", roleName, strings.Join(sortedKeys(f.Roles), ", "))
	}
	raw, err := os.ReadFile(filepath.Join(f.dir, role.Path))
	if err != nil {
		return fmt.Errorf("facts_from %s: %w", f.FactsFrom, err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("facts_from %s: %w", f.FactsFrom, err)
	}
	blob, ok := doc[key]
	if !ok {
		return fmt.Errorf("facts_from %s: %s has no top-level %q", f.FactsFrom, role.Path, key)
	}
	var facts map[string]string
	if err := json.Unmarshal(blob, &facts); err != nil {
		return fmt.Errorf("facts_from %s: %q must be an object of strings: %w", f.FactsFrom, key, err)
	}
	if len(facts) == 0 {
		return fmt.Errorf("facts_from %s: %q is empty", f.FactsFrom, key)
	}
	for k, v := range facts {
		if strings.TrimSpace(v) == "" {
			return fmt.Errorf("facts_from %s: fact %q is empty; an empty needle is found in every haystack", f.FactsFrom, k)
		}
	}
	f.Facts = facts
	return nil
}

// A World is one materialized copy of a fixture — the thing the agent
// actually runs against.
//
// Materialized rather than used in place because a run mutates it: the
// shim appends to its witnesses, and an agent that decides to write
// something writes it here. Grading the committed tree would mean the
// first run contaminates every later one, and it would leave the
// checkout dirty, which is its own well-earned lesson.
type World struct {
	Root        string
	Workdir     string
	PathDir     string
	Env         map[string]string
	witnesses   map[string]string // name -> absolute path
	fixtureName string
}

// Materialize lays the shared overlay down, then the fixture on top,
// into dst, and confirms every role's probes.
func (f *Fixture) Materialize(dst string) (*World, error) {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return nil, fmt.Errorf("evals: materialize: %w", err)
	}
	shared := filepath.Join(f.root, SharedDir)
	if _, err := os.Stat(shared); err == nil {
		if err := copyTree(shared, dst); err != nil {
			return nil, fmt.Errorf("evals: materialize shared overlay: %w", err)
		}
	}
	if err := copyTree(f.dir, dst); err != nil {
		return nil, fmt.Errorf("evals: materialize fixture %s: %w", f.Name, err)
	}

	w := &World{
		Root:        dst,
		Workdir:     filepath.Join(dst, f.Roles[f.WorkdirRole].Path),
		Env:         map[string]string{},
		witnesses:   map[string]string{},
		fixtureName: f.Name,
	}
	if f.PathRole != "" {
		w.PathDir = filepath.Join(dst, f.Roles[f.PathRole].Path)
	}
	for name, p := range f.Witnesses {
		abs := filepath.Join(dst, p)
		// The directory, never the file. The absent/empty distinction
		// lives in the file, so pre-creating that would erase it — but
		// making every fixture's tool remember to mkdir -p its own log
		// directory is a footgun whose payload is a check that is
		// vacuous forever and looks like the agent's fault.
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return nil, fmt.Errorf("evals: witness %s: %w", name, err)
		}
		w.witnesses[name] = abs
	}
	if err := f.confirmProbes(dst); err != nil {
		return nil, err
	}
	for k, v := range f.Env {
		expanded, err := w.expand(v, f)
		if err != nil {
			return nil, fmt.Errorf("evals: fixture %s: env %s: %w", f.Name, k, err)
		}
		w.Env[k] = expanded
	}
	return w, nil
}

// confirmProbes is the rule that entitles an absence check to be called
// a violation. Every role's every probe must exist in the materialized
// world before the agent starts; a missing one is an environment
// failure and stops the run, rather than becoming a check the agent
// gets blamed for.
func (f *Fixture) confirmProbes(root string) error {
	for _, roleName := range sortedKeys(f.Roles) {
		role := f.Roles[roleName]
		base := filepath.Join(root, role.Path)
		probes := role.Probes
		if len(probes) == 0 {
			if _, err := os.Stat(base); err != nil {
				return fmt.Errorf("evals: fixture %s: role %q missing at %s: %w", f.Name, roleName, role.Path, err)
			}
			continue
		}
		for _, probe := range probes {
			if _, err := os.Stat(filepath.Join(base, probe)); err != nil {
				return fmt.Errorf("evals: fixture %s: role %q probe %q not present: %w", f.Name, roleName, probe, err)
			}
		}
	}
	return nil
}

func (w *World) expand(v string, f *Fixture) (string, error) {
	out := v
	for _, name := range sortedKeys(f.Roles) {
		out = strings.ReplaceAll(out, "${role."+name+"}", filepath.Join(w.Root, f.Roles[name].Path))
	}
	for _, name := range sortedKeys(w.witnesses) {
		out = strings.ReplaceAll(out, "${witness."+name+"}", w.witnesses[name])
	}
	if i := strings.Index(out, "${"); i >= 0 {
		return "", fmt.Errorf("unresolved reference in %q at offset %d", v, i)
	}
	return out, nil
}

// ReadWitness returns a witness's contents and whether it exists.
//
// The bool is the interesting half. Absent means the world was never
// touched, which is a different finding from touched-and-empty, and
// verify.go spends that difference.
func (w *World) ReadWitness(name string) (string, bool) {
	path, ok := w.witnesses[name]
	if !ok {
		return "", false
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	return string(raw), true
}

// ListFixtures returns the fixture names under root, excluding the
// shared overlay.
func ListFixtures(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("evals: list fixtures: %w", err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() || e.Name() == SharedDir || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out, nil
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symlink; fixtures are copied, so a symlink would either dangle or reach outside the materialized world", rel)
		}
		// #nosec G122 -- the walk refuses symlinks outright a few lines
		// up, so there is no link for a traversal to follow; the residual
		// TOCTOU needs write access to the committed fixtures tree
		// between the lstat and the open, which is already game over.
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode().Perm())
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			out.Close()
			return err
		}
		return out.Close()
	})
}
