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

package recipecheck_test

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-steer/core-agent/v2/examples/internal/recipecheck"
	"github.com/go-steer/core-agent/v2/pkg/config"
)

var update = flag.Bool("update", false, "rewrite testdata/config-surface.txt from the current pkg/config schema")

func TestParseVersion(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in   string
		want string // canonical form, "" for a parse error
	}{
		{"2.8.0", "2.8.0"},
		{"v2.8.0", "2.8.0"}, // git tag spelling
		{"2.9.0-dev.1", "2.9.0-dev.1"},
		{"2.9.0-rc.1", "2.9.0-rc.1"},
		{"2.9.0+build.5", "2.9.0"}, // semver §10: metadata is ignored
		{" 2.8.0 ", "2.8.0"},
		{"main", ""},
		{"main-1a2b3c4", ""},
		{"latest", ""},
		{"2.9", ""},
		{"", ""},
		{"sha256:0000", ""},
	} {
		got, err := recipecheck.ParseVersion(tc.in)
		switch {
		case tc.want == "" && err == nil:
			t.Errorf("ParseVersion(%q) = %s, want an error", tc.in, got)
		case tc.want != "" && err != nil:
			t.Errorf("ParseVersion(%q): %v", tc.in, err)
		case tc.want != "" && got.String() != tc.want:
			t.Errorf("ParseVersion(%q) = %s, want %s", tc.in, got, tc.want)
		}
	}
}

// TestVersionCompare pins the ordering rules this gate leans on. The
// prerelease cases are the ones that matter: 2.9.0-dev.1 is the only
// v2.9 image that exists today, so a comparator that put it above
// 2.9.0 or below 2.8.0 would make the whole check say the wrong thing.
func TestVersionCompare(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"2.8.0", "2.9.0", -1},
		{"2.9.0", "2.8.0", 1},
		{"2.8.0", "2.8.0", 0},
		{"2.9.0-dev.1", "2.9.0", -1}, // semver §11.3: prerelease < release
		{"2.8.0", "2.9.0-dev.1", -1}, // ...but still above the previous GA
		{"2.9.0-dev.1", "2.9.0-dev.2", -1},
		{"2.9.0-dev.2", "2.9.0-dev.10", -1}, // numeric, not lexical
		{"2.9.0-dev.9", "2.9.0-rc.1", -1},   // alphanumeric, lexical
		{"2.9.0-dev", "2.9.0-dev.1", -1},    // §11.4.4: longer wins the tie
		{"2.10.0", "2.9.0", 1},
		{"3.0.0", "2.9.0", 1},
		{"2.8.1", "2.8.0", 1},
	} {
		a, err := recipecheck.ParseVersion(tc.a)
		if err != nil {
			t.Fatalf("ParseVersion(%q): %v", tc.a, err)
		}
		b, err := recipecheck.ParseVersion(tc.b)
		if err != nil {
			t.Fatalf("ParseVersion(%q): %v", tc.b, err)
		}
		if got := a.Compare(b); got != tc.want {
			t.Errorf("%s.Compare(%s) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
		if got := b.Compare(a); got != -tc.want {
			t.Errorf("%s.Compare(%s) = %d, want %d (asymmetric comparator)", tc.b, tc.a, got, -tc.want)
		}
	}
}

func TestRequiredVersion(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		cfg  config.Config
		want string // "" means no requirement
	}{
		{name: "bare config", cfg: config.Config{Version: 1}},
		{
			name: "an alert target needs the release that registers the alert tool",
			cfg: config.Config{Alerts: config.AlertsConfig{
				Targets: []config.AlertTarget{{Name: "oncall", URLEnv: "HOOK"}}}},
			want: "2.9.0-dev.1",
		},
		{
			name: "an empty targets list asserts nothing",
			cfg:  config.Config{Alerts: config.AlertsConfig{Targets: []config.AlertTarget{}}},
		},
		{
			name: "wait_and_verify config",
			cfg: config.Config{Tools: config.ToolsConfig{
				WaitAndVerify: config.WaitAndVerifyConfig{PollAllow: []string{"x"}}}},
			want: "2.9.0-dev.1",
		},
		{
			name: "auto_continue alone stops at 2.8.0",
			cfg:  config.Config{Agent: config.AgentConfig{AutoContinue: &config.AutoContinueConfig{MaxPerBoot: 3}}},
			want: "2.8.0",
		},
		{
			name: "the highest requirement wins",
			cfg: config.Config{
				Agent:  config.AgentConfig{AutoContinue: &config.AutoContinueConfig{MaxPerBoot: 3}},
				Alerts: config.AlertsConfig{Targets: []config.AlertTarget{{Name: "oncall"}}},
			},
			want: "2.9.0-dev.1",
		},
		{
			name: "a subagent root is found through the slice",
			cfg: config.Config{Subagents: []config.SubagentSpec{
				{Name: "a"}, {Name: "cluster", Root: "../cluster"}}},
			want: "2.9.0-dev.1",
		},
		{
			// A cap the daemon silently drops is worse than no cap: the
			// operator reads the config and believes the delegation is
			// bounded.
			name: "a subagent budget needs the release that honors it",
			cfg: config.Config{Subagents: []config.SubagentSpec{
				{Name: "a"}, {Name: "b", Budgets: &config.SubagentBudgets{MaxTurns: 8}}}},
			want: "2.9.0-dev.4",
		},
		{
			name: "a subagent with no budgets block asserts nothing about caps",
			cfg:  config.Config{Subagents: []config.SubagentSpec{{Name: "a"}}},
			want: "2.9.0-dev.1", // the roster itself, not the cap
		},
		{
			// The rule is PRESENCE, not non-zeroness. pkg/config made
			// auto_continue.enabled a *bool precisely so an explicit
			// opt-out could be told from an absent block (#559); an older
			// daemon drops the opt-out along with everything else, and a
			// check that read the block as "all fields zero, therefore
			// unused" would have contradicted its own GatedFeatures row,
			// which promises to catch exactly this.
			name: "an explicit opt-out is still a config the daemon has to understand",
			cfg: config.Config{Agent: config.AgentConfig{
				AutoContinue: &config.AutoContinueConfig{Enabled: new(bool)}}},
			want: "2.8.0",
		},
		{
			name: "an empty block that was written on purpose still counts",
			cfg:  config.Config{Agent: config.AgentConfig{AutoContinue: &config.AutoContinueConfig{}}},
			want: "2.8.0",
		},
		{
			name: "an absent block does not",
			cfg:  config.Config{Agent: config.AgentConfig{AutoContinue: nil}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := tc.cfg
			req, err := recipecheck.RequiredVersion(&cfg)
			if err != nil {
				t.Fatalf("RequiredVersion: %v", err)
			}
			if tc.want == "" {
				if !req.Empty() {
					t.Fatalf("want no requirement, got ≥ %s: %v", req.Min, req.Reasons)
				}
				return
			}
			if req.Empty() {
				t.Fatalf("want ≥ %s, got no requirement", tc.want)
			}
			if got := req.Min.String(); got != tc.want {
				t.Errorf("Min = %s, want %s (reasons: %v)", got, tc.want, req.Reasons)
			}
		})
	}
}

// TestGatedFeaturePathsResolve is the other half of the table's
// integrity: every Path must still name something in pkg/config.
// RequiredVersion errors on an unresolvable path, so a renamed or
// deleted field fails the build instead of leaving a row that quietly
// never matches.
func TestGatedFeaturePathsResolve(t *testing.T) {
	t.Parallel()
	surface := map[string]bool{}
	for _, p := range recipecheck.ConfigSurface() {
		surface[p] = true
		// A table entry may name an interior node ("alerts.targets",
		// "subagents"), which the surface only lists via its leaves.
		for i, c := range p {
			if c == '.' {
				surface[strings.TrimSuffix(p[:i], "[]")] = true
			}
		}
	}
	for _, f := range recipecheck.GatedFeatures {
		if !surface[f.Path] && !surface[strings.TrimSuffix(f.Path, "[]")] {
			t.Errorf("GatedFeatures path %q is not in the pkg/config surface any more — "+
				"it was renamed or removed, and this row now matches nothing", f.Path)
		}
		if _, err := recipecheck.ParseVersion(f.Min); err != nil {
			t.Errorf("GatedFeatures[%q].Min: %v", f.Path, err)
		}
		if strings.TrimSpace(f.Why) == "" {
			t.Errorf("GatedFeatures[%q].Why is empty; the failure message is the whole product here", f.Path)
		}
	}
	// RequiredVersion walks every Path against a real value, so a zero
	// config is enough to prove each one resolves.
	if _, err := recipecheck.RequiredVersion(&config.Config{}); err != nil {
		t.Errorf("RequiredVersion on a zero config: %v", err)
	}
}

// TestConfigSurfaceIsAccountedFor is the guard that makes the
// hand-maintained GatedFeatures table survive contact with a third
// version-gated feature.
//
// A table like GatedFeatures does not fail by holding a wrong row; it
// fails by missing one, silently, on the PR that adds the next
// `alerts`-shaped block. Nobody thinks about deployed daemons while
// adding a config field. So instead of asking the author to remember,
// this fingerprints the entire config schema: any new path fails this
// test, the failure message asks the one question that matters, and
// `-update` records the answer.
//
// It deliberately does NOT require a version for every field — see the
// GatedFeatures doc for why most fields do not deserve one. It requires
// that someone looked.
func TestConfigSurfaceIsAccountedFor(t *testing.T) {
	golden := filepath.Join("testdata", "config-surface.txt")
	got := strings.Join(recipecheck.ConfigSurface(), "\n") + "\n"
	if *update {
		if err := os.WriteFile(golden, []byte(surfaceHeader+got), 0o644); err != nil {
			t.Fatalf("write %s: %v", golden, err)
		}
		t.Logf("wrote %s", golden)
		return
	}
	wantBytes, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read %s: %v", golden, err)
	}
	want := stripComments(string(wantBytes))
	if got == want {
		return
	}
	added, removed := diffLines(strings.Split(want, "\n"), strings.Split(got, "\n"))
	t.Errorf(`pkg/config's schema no longer matches %s.

  added:   %v
  removed: %v

If an added path is version-gated — it registers a tool, changes the
agent topology or the content the agent loads, or asserts a safety
property — add it to GatedFeatures in minversion.go with the first
release that ships it, so a recipe using it cannot be deployed onto an
older daemon that would silently drop it (#680). Most fields are not;
that is a fine answer.

Then record the new surface:
  go test ./examples/internal/recipecheck -run TestConfigSurfaceIsAccountedFor -update`,
		golden, added, removed)
}

// surfaceHeader orients a cold reader who opens the golden file first
// and has no idea what a list of 147 dotted paths is for.
const surfaceHeader = `# Fingerprint of every JSON path in pkg/config.Config, sorted.
#
# Generated — do not hand-edit. Regenerate with:
#   go test ./examples/internal/recipecheck -run TestConfigSurfaceIsAccountedFor -update
#
# This is not a schema and nothing reads it at runtime. It exists so
# that adding a field to pkg/config fails a test, which asks whether the
# new field needs a row in GatedFeatures (minversion.go) — the table
# that stops a recipe from being deployed onto a daemon too old to
# understand its own config (#680). Most fields do not need a row. The
# point is that somebody answered.
#
# Grammar: "." joins struct fields, "[]" is a slice of structs, "{}" is
# a map of structs. A container of scalars is a leaf.

`

// stripComments drops the header so the comparison is against paths
// only.
func stripComments(s string) string {
	var b strings.Builder
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(line, "#") || line == "" {
			continue
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

func diffLines(want, got []string) (added, removed []string) {
	in := func(ls []string) map[string]bool {
		m := make(map[string]bool, len(ls))
		for _, l := range ls {
			if l != "" {
				m[l] = true
			}
		}
		return m
	}
	w, g := in(want), in(got)
	for _, l := range got {
		if l != "" && !w[l] {
			added = append(added, l)
		}
	}
	for _, l := range want {
		if l != "" && !g[l] {
			removed = append(removed, l)
		}
	}
	return added, removed
}

// TestReleasedVersions reads the real changelog: this is the oracle for
// "does this image tag exist", and a format change would silently blind
// the existence check rather than fail it.
//
// The versions asserted here are deliberately all GA sections and folded
// pre-releases — nothing that a future `cut-ga-tag.sh` run could delete.
// An earlier revision asserted 2.9.0-dev.1, the version the recipes pin
// today, which would have started failing the moment v2.9.0 GA folded
// that section away. That is the same bug this test is here to guard,
// one level up.
func TestReleasedVersions(t *testing.T) {
	t.Parallel()
	got, err := recipecheck.ReleasedVersions(changelogPath)
	if err != nil {
		t.Fatalf("ReleasedVersions: %v", err)
	}
	seen := map[string]bool{}
	for _, v := range got {
		seen[v.String()] = true
	}
	for _, want := range []string{
		"2.8.0", "2.7.0", // GA headings
		"2.8.0-dev.7", "2.7.0-dev.5", // recoverable only from a fold trailer
	} {
		if !seen[want] {
			t.Errorf("ReleasedVersions did not find %s; parsed %d versions", want, len(got))
		}
	}
	if seen["9.9.9"] {
		t.Error("ReleasedVersions invented 9.9.9; the heading parse is matching more than release sections")
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].Compare(got[i]) <= 0 {
			t.Fatalf("not sorted newest-first: %s before %s", got[i-1], got[i])
		}
	}
}

// TestReleasedVersionsSkipsHeadingsItCannotParse: a changelog is prose.
// A heading that is not a release must be ignored, not fatal — the
// alternative is an editorial choice red-building the examples tree.
func TestReleasedVersionsSkipsHeadingsItCannotParse(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "CHANGELOG.md")
	body := "# Changelog\n\n## [Unreleased]\n\n## [2025 retrospective]\n\n## [2.8.0] — 2026-08-07\n\n- a bullet\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := recipecheck.ReleasedVersions(path)
	if err != nil {
		t.Fatalf("ReleasedVersions: %v", err)
	}
	if len(got) != 1 || got[0].String() != "2.8.0" {
		t.Fatalf("ReleasedVersions = %v, want just [2.8.0]", got)
	}
}

// TestFoldTrailerMatchesReleaseScript ties this package's parser to the
// script that writes what it parses.
//
// ReleasedVersions recovers folded pre-release tags out of the trailer
// cut-ga-tag.sh emits. That is a contract between two files that have no
// other reason to agree, and the failure mode if they stop agreeing is
// silent: the check just forgets that 2.9.0-dev.1 was ever released and
// starts calling every recipe's pin unpublished. Asserting the literal
// makes a reword over there a red build over here.
func TestFoldTrailerMatchesReleaseScript(t *testing.T) {
	t.Parallel()
	script := filepath.Join("..", "..", "..", "dev", "release", "cut-ga-tag.sh")
	body, err := os.ReadFile(script)
	if err != nil {
		t.Fatalf("read %s: %v", script, err)
	}
	if !strings.Contains(string(body), recipecheck.FoldTrailerPrefix) {
		t.Fatalf("dev/release/cut-ga-tag.sh no longer writes %q.\n\n"+
			"ReleasedVersions parses that trailer to remember the pre-release tags the fold\n"+
			"deletes. If the wording changed, update foldTrailerPrefix in minversion.go to\n"+
			"match; if the trailer went away entirely, every folded dev tag is now invisible\n"+
			"to the #680 pin check and it needs a different oracle.", recipecheck.FoldTrailerPrefix)
	}
}
