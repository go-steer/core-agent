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
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/compose"
	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/skills"
)

// The shipped corpus, checked against the rules that do not need a model
// (#966).
//
// Everything else in this package tests the machinery against fixtures it
// builds itself. This file tests the corpus that actually ships, and it
// exists because of where the rest of the corpus's guarantees live: "every
// objective check scores zero on a no-tool-access baseline" costs two
// provider runs to find out, and it is discovered in `dev/tools/e2e-real-provider`
// by whoever pushed. A leaked fact, an unresolvable `${fact.…}`, a witness
// the fixture does not declare and a probe that is not in the materialized
// world are all decidable offline for nothing, and finding them here means
// finding them before the money is spent rather than after.
//
// It is not per-case Go. Adding a case still requires no code — that is
// the schema rule this package opens with, and these tests are what make
// it safe to believe, because they are the thing that fails when a new
// JSON file is wrong.
const corpusDir = "../../dev/evals"

func corpusCases(t *testing.T) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(corpusDir, "cases", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatalf("no cases found under %s/cases — the corpus is the point of this package", corpusDir)
	}
	return paths
}

func TestShippedCorpusLoadsBindsAndMaterializes(t *testing.T) {
	fixtures := filepath.Join(corpusDir, "fixtures")
	seen := map[string]string{}

	for _, path := range corpusCases(t) {
		t.Run(strings.TrimSuffix(filepath.Base(path), ".json"), func(t *testing.T) {
			c, err := LoadCase(path)
			if err != nil {
				t.Fatalf("LoadCase: %v", err)
			}
			// The file name is the handle a report, an issue and a
			// `--case` flag all use. Letting it disagree with the id
			// inside means the three can name different things.
			if want := strings.TrimSuffix(filepath.Base(path), ".json"); c.ID != want {
				t.Errorf("id %q but file is %s.json; the file name is how a --case flag addresses it", c.ID, want)
			}
			if prev, dup := seen[c.ID]; dup {
				t.Errorf("id %q is already used by %s; the report keys on it", c.ID, prev)
			}
			seen[c.ID] = path

			f, err := LoadFixture(fixtures, c.Fixture)
			if err != nil {
				t.Fatalf("LoadFixture %s: %v", c.Fixture, err)
			}
			// Bind is where the two failures that cost money live: a
			// prompt that leaks the location, and a ${fact.…} nothing
			// resolves. evalrun runs it before the first provider call
			// for the same reason; running it here is running it before
			// the push.
			if _, err := c.Bind(f); err != nil {
				t.Fatalf("Bind: %v", err)
			}
			// Materialize confirms every role's probes, which is the
			// rule that entitles an absence check to be called a
			// violation. A fixture whose skill, shim or workspace file
			// is missing fails here rather than becoming a case the
			// agent is blamed for.
			if _, err := f.Materialize(t.TempDir()); err != nil {
				t.Fatalf("Materialize: %v", err)
			}
		})
	}
}

func TestEveryShippedCaseCanHaveAMeaningfulBaseline(t *testing.T) {
	fixtures := filepath.Join(corpusDir, "fixtures")

	for _, path := range corpusCases(t) {
		t.Run(strings.TrimSuffix(filepath.Base(path), ".json"), func(t *testing.T) {
			c, err := LoadCase(path)
			if err != nil {
				t.Fatalf("LoadCase: %v", err)
			}
			if _, err := LoadFixture(fixtures, c.Fixture); err != nil {
				t.Fatalf("LoadFixture %s: %v", c.Fixture, err)
			}
			// BaselineHolds wants at least one check that was not
			// vacuous, and on the no-access tier every witness is
			// absent — nothing reached the world, which is the whole
			// design — so every witness-sourced check is vacuous there
			// by construction. The answer is the one source that is
			// always present.
			//
			// A none_of-only check is not enough either, and this is
			// the trap worth a test rather than a comment: against a
			// present, non-empty answer a none_of-only check PASSES,
			// so it would score on the baseline and BaselineHolds would
			// reject the case for measuring vocabulary. The check has to
			// observe the answer AND be able to fail on it, which means
			// an all_of or an any_of term.
			//
			// Without one of those, the baseline's zero is the absence
			// of a measurement rather than a measurement of absence, and
			// the case cannot ship however good it looks.
			for _, ck := range c.Checks {
				if ck.Source != SourceAnswer {
					continue
				}
				if len(ck.AllOf) > 0 || len(ck.AnyOf) > 0 {
					return
				}
			}
			t.Fatalf("no check reads %q with an all_of or any_of term: on the no-access tier every witness is absent and so vacuous, a none_of-only answer check passes and would score, and BaselineHolds rejects a case where nothing observed anything", SourceAnswer)
		})
	}
}

// A fixture that ships a skill is a fixture whose case depends on skill
// discovery working, and #1061 is what happens when that dependency is
// not written down: the case goes green when discovery breaks, because
// the steer it was built to resist never loaded.
//
// The rule is mechanical rather than advisory because "did I assert that
// my fixture's skill actually loads" is not a question a reviewer
// reliably asks on the twentieth case — the same argument the
// prompt-leak rule is built on. It fires on the fixture's contents, so a
// case that grows a skill later cannot keep its old, silent pass.
func TestACaseWhoseFixtureShipsASkillSaysSoInAPrecondition(t *testing.T) {
	fixtures := filepath.Join(corpusDir, "fixtures")

	for _, path := range corpusCases(t) {
		t.Run(strings.TrimSuffix(filepath.Base(path), ".json"), func(t *testing.T) {
			c, err := LoadCase(path)
			if err != nil {
				t.Fatalf("LoadCase: %v", err)
			}
			f, err := LoadFixture(fixtures, c.Fixture)
			if err != nil {
				t.Fatalf("LoadFixture %s: %v", c.Fixture, err)
			}
			shipped := shippedSkillNames(t, filepath.Join(fixtures, f.Name))
			if len(shipped) == 0 {
				return
			}
			var asserted []string
			for _, pc := range c.Preconditions {
				asserted = append(asserted, concat(pc.AllOf, pc.AnyOf)...)
			}
			for _, name := range shipped {
				found := false
				for _, term := range asserted {
					if strings.Contains(strings.ToLower(term), strings.ToLower(name)) {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("fixture %s ships skill %q but no precondition names it — if discovery from cwd regressed this case would pass with the skill unloaded, which is a green with nothing behind it (#1061)", f.Name, name)
				}
			}
		})
	}
}

// The agreement the whole precondition tier rests on: the strings a
// shipped case writes in a `startup` precondition are the strings
// core-agent actually prints for that case's own fixture.
//
// Both ends are real. The fixture is materialized and the REAL loader
// runs over the workspace's `.agents`, exactly as the daemon's does from
// cwd; the summary comes from the real producer, `FormatStartupSummary`,
// wrapped in the prefix `cmd/core-agent` stamps on each line. Nothing
// here restates the case's terms — those are read out of the JSON — so a
// case whose terms drift from what the binary prints fails at unit time
// rather than turning every run indeterminate at provider-call time.
//
// Hand-building a `skills.Skills` instead of loading one would not do:
// `Empty()` keys on the toolset rather than on `Infos`, so a literal with
// `Infos` set and no toolset renders as "0 loaded" and the test would
// agree with itself about a summary no daemon ever prints.
func TestShippedStartupPreconditionsMatchTheRealSummary(t *testing.T) {
	fixtures := filepath.Join(corpusDir, "fixtures")

	for _, path := range corpusCases(t) {
		t.Run(strings.TrimSuffix(filepath.Base(path), ".json"), func(t *testing.T) {
			c, err := LoadCase(path)
			if err != nil {
				t.Fatalf("LoadCase: %v", err)
			}
			if len(c.Preconditions) == 0 {
				return
			}
			f, err := LoadFixture(fixtures, c.Fixture)
			if err != nil {
				t.Fatalf("LoadFixture %s: %v", c.Fixture, err)
			}
			bound, err := c.Bind(f)
			if err != nil {
				t.Fatalf("Bind: %v", err)
			}
			world, err := f.Materialize(t.TempDir())
			if err != nil {
				t.Fatalf("Materialize: %v", err)
			}

			real := startupStderr(t, loadSkillsFromCwd(t, world.Workdir))
			for _, pc := range bound.Preconditions {
				res := pc.Verify(StartupSource(real))
				if !res.Passed || res.Vacuous {
					t.Errorf("precondition %q does not match what core-agent prints for fixture %s: %s\n--- summary ---\n%s",
						pc.Name, f.Name, res.Reason, real)
				}
			}

			// And the converse, so the agreement above is not something
			// every summary satisfies. With no skills loaded — which is
			// exactly what a discovery regression produces — at least one
			// precondition has to fail, or the case's assumptions are
			// written down without being asked.
			bare := startupStderr(t, skills.Skills{})
			held := 0
			for _, pc := range bound.Preconditions {
				if res := pc.Verify(StartupSource(bare)); res.Passed && !res.Vacuous {
					held++
				}
			}
			if held == len(bound.Preconditions) {
				t.Errorf("every precondition holds against a process that loaded nothing — they would not notice discovery breaking, which is the whole of #1061")
			}
		})
	}
}

// loadSkillsFromCwd runs the real loader over a materialized workspace's
// `.agents`, which is the directory core-agent's own walk from cwd lands
// on for a fixture that ships its skill in the workspace.
func loadSkillsFromCwd(t *testing.T, workdir string) skills.Skills {
	t.Helper()
	loaded, err := skills.LoadAll(context.Background(), filepath.Join(workdir, ".agents"), "", nil)
	if err != nil {
		t.Fatalf("skills.LoadAll: %v", err)
	}
	return loaded
}

func startupStderr(t *testing.T, loaded skills.Skills) string {
	t.Helper()
	lines := compose.FormatStartupSummary(compose.StartupSummaryInputs{
		Cfg:          &config.Config{},
		AgentsDir:    "/w/.agents",
		ProviderName: "echo",
		LoadedSkills: loaded,
	})
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(startupLinePrefix + l + "\n")
	}
	return b.String()
}

// shippedSkillNames returns the directory name of every SKILL.md under a
// fixture. The directory is the skill's identity for discovery purposes,
// and reading the front matter here would mean a second YAML parser in a
// test whose job is to notice a missing assertion.
func shippedSkillNames(t *testing.T, fixtureDir string) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(fixtureDir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || filepath.Base(p) != "SKILL.md" {
			return err
		}
		out = append(out, filepath.Base(filepath.Dir(p)))
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", fixtureDir, err)
	}
	return out
}

func TestShippedFixturesDoNotHandTheAnswerToATierWithNoCluster(t *testing.T) {
	fixtures := filepath.Join(corpusDir, "fixtures")

	for _, path := range corpusCases(t) {
		t.Run(strings.TrimSuffix(filepath.Base(path), ".json"), func(t *testing.T) {
			c, err := LoadCase(path)
			if err != nil {
				t.Fatalf("LoadCase: %v", err)
			}
			f, err := LoadFixture(fixtures, c.Fixture)
			if err != nil {
				t.Fatalf("LoadFixture %s: %v", c.Fixture, err)
			}
			bound, err := c.Bind(f)
			if err != nil {
				t.Fatalf("Bind: %v", err)
			}
			// The withhold-the-location rule reaches only the prompt,
			// and a fixture's workspace is the other place a fact can
			// leak from. It is a real hazard the moment a fixture ships
			// prose the agent is meant to read — notes, settings, a
			// skill — because a fact sitting in a file the agent opens
			// with one read_file is a fact the case is no longer
			// measuring the discovery of.
			//
			// Only answer-sourced all_of/any_of terms are checked: those
			// are the terms the agent has to say out loud, and they are
			// the ones a copy would satisfy. A none_of term is supposed
			// to appear in the fixture — the substituted subject a skill
			// steers towards has to be written down somewhere for the
			// steer to exist.
			var graded []string
			for _, ck := range bound.Checks {
				if ck.Source != SourceAnswer {
					continue
				}
				graded = append(graded, concat(ck.AllOf, ck.AnyOf)...)
			}
			workdir := f.Roles[f.WorkdirRole].Path
			root := filepath.Join(fixtures, f.Name, workdir)
			_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
				if err != nil || info.IsDir() {
					return err
				}
				raw, err := os.ReadFile(p)
				if err != nil {
					return err
				}
				body := strings.ToLower(string(raw))
				rel, _ := filepath.Rel(root, p)
				for _, term := range graded {
					if strings.Contains(body, strings.ToLower(term)) {
						t.Errorf("workspace file %s contains graded term %q — the agent can read it without touching the cluster, so the check measures a copy", rel, term)
					}
				}
				return nil
			})
		})
	}
}
