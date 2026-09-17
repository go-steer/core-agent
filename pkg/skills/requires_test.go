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

package skills

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/adk/tool/skilltoolset/skill"

	"github.com/go-steer/core-agent/v2/pkg/permissions"
)

// gateWithTools returns a gate that has been told exactly which built-in
// tools this build registered — the state tools.Build leaves it in.
func gateWithTools(t *testing.T, catalog map[string]bool) *permissions.Gate {
	t.Helper()
	g := permissions.New(permissions.Options{Mode: permissions.ModeYolo})
	g.SetRegisteredTools(catalog)
	return g
}

// withLookPath is the test seam for the PATH probe. Requirement
// resolution must be decidable without depending on what happens to be
// installed on the machine running `go test`.
func withLookPath(present ...string) Option {
	set := make(map[string]bool, len(present))
	for _, p := range present {
		set[p] = true
	}
	return func(o *loadOptions) {
		o.lookPath = func(name string) (string, error) {
			if set[name] {
				return "/fake/bin/" + name, nil
			}
			return "", fmt.Errorf("exec: %q: executable file not found in $PATH", name)
		}
	}
}

// writeSkillRequires writes a skill whose frontmatter carries a verbatim
// extra line — the `requires:` key under test, in whatever shape the
// case needs, including malformed ones.
func writeSkillRequires(t *testing.T, dir, name, extra string) {
	t.Helper()
	skillPath := filepath.Join(dir, SkillDirName, name)
	if err := os.MkdirAll(skillPath, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: " + name + "\ndescription: the " + name + " skill\n" + extra + "\n---\n\nbody for " + name
	if err := os.WriteFile(filepath.Join(skillPath, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func names(s Skills) []string {
	out := make([]string, 0, len(s.Infos))
	for _, i := range s.Infos {
		out = append(out, i.Name)
	}
	return out
}

func dropReason(s Skills, name string) (string, bool) {
	for _, d := range s.Dropped {
		if d.Skill == name {
			return d.Reason, true
		}
	}
	return "", false
}

// TestParseRequires covers the frontmatter grammar in isolation: which
// shapes are requirements, which are "no key", and which are malformed.
func TestParseRequires(t *testing.T) {
	t.Parallel()
	const head = "---\nname: x\ndescription: d\n"
	cases := []struct {
		name        string
		frontmatter string
		wantTokens  []string
		wantErr     bool
		wantFound   bool
	}{
		{"absent", head + "---\nbody", nil, false, false},
		{"list", head + "requires: [shell, kubectl]\n---\nbody", []string{"shell", "kubectl"}, false, true},
		{"block list", head + "requires:\n  - kubectl\n  - gcloud\n---\nbody", []string{"kubectl", "gcloud"}, false, true},
		{"single string", head + "requires: kubectl\n---\nbody", []string{"kubectl"}, false, true},
		{"whitespace trimmed", head + "requires: [ '  kubectl  ' ]\n---\nbody", []string{"kubectl"}, false, true},
		// An author saying "nothing" out loud is not an error.
		{"empty list", head + "requires: []\n---\nbody", nil, false, false},
		{"empty string", head + "requires: ''\n---\nbody", nil, true, true},
		{"non-string element", head + "requires: [shell, 7]\n---\nbody", nil, true, true},
		{"blank element", head + "requires: [shell, '  ']\n---\nbody", nil, true, true},
		{"mapping", head + "requires:\n  shell: true\n---\nbody", nil, true, true},
		// No fence at all: the key must not be read out of the body.
		{"no frontmatter", "requires: [kubectl]\n\nbody", nil, false, false},
		// A fence PAIR further down the file is a markdown horizontal
		// rule, not frontmatter. This is the only shape the leading-fence
		// check actually decides — a file with no `---`, or one, is
		// already rejected by the three-way split — so without this case
		// removing that check changes nothing any test can see.
		{"fence not at the top", "# Notes\n---\nrequires: [kubectl]\n---\nmore", nil, false, false},
		{"crlf fence", "---\r\nname: x\r\ndescription: d\r\nrequires: [kubectl]\r\n---\r\nbody", []string{"kubectl"}, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, found := parseRequires([]byte(tc.frontmatter))
			if found != tc.wantFound {
				t.Fatalf("found = %v, want %v (decl %+v)", found, tc.wantFound, got)
			}
			if (got.err != "") != tc.wantErr {
				t.Fatalf("err = %q, wantErr %v", got.err, tc.wantErr)
			}
			if strings.Join(got.tokens, ",") != strings.Join(tc.wantTokens, ",") {
				t.Fatalf("tokens = %v, want %v", got.tokens, tc.wantTokens)
			}
		})
	}
}

// TestRequiresBodyTextIsNotAFrontmatterKey: a `requires:` line in the
// markdown body — a plausible thing for a skill to write about itself —
// must not drop the skill. frontmatterYAML is what enforces that, and it
// is shared with sanitizeFrontmatter so the two passes cannot disagree
// about where frontmatter ends.
func TestRequiresBodyTextIsNotAFrontmatterKey(t *testing.T) {
	t.Parallel()
	project := t.TempDir()
	skillPath := filepath.Join(project, SkillDirName, "docs")
	if err := os.MkdirAll(skillPath, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: docs\ndescription: d\n---\n\nThis skill requires: [kubectl] on the box.\n"
	if err := os.WriteFile(filepath.Join(skillPath, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := LoadAll(context.Background(), project, "", nil, withLookPath())
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Dropped) != 0 {
		t.Fatalf("body prose was read as a requirement: %+v", got.Dropped)
	}
	if len(got.Infos) != 1 {
		t.Fatalf("expected the skill to load, got %v", names(got))
	}
}

// TestLoadAll_DropsUnsatisfiableSkill is the headline behaviour: a skill
// naming a binary this runtime does not have is withheld, by name, with
// the missing capability in the reason — and the satisfiable one beside
// it still loads.
func TestLoadAll_DropsUnsatisfiableSkill(t *testing.T) {
	t.Parallel()
	project := t.TempDir()
	writeSkillRequires(t, project, "gke-triage", "requires: [kubectl, gcloud]")
	writeSkillRequires(t, project, "jira-triage", "requires: [curl]")
	writeSkillRequires(t, project, "plain", "")

	got, err := LoadAll(context.Background(), project, "", nil, withLookPath("curl"))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"jira-triage", "plain"}; strings.Join(names(got), ",") != strings.Join(want, ",") {
		t.Fatalf("loaded = %v, want %v", names(got), want)
	}
	reason, ok := dropReason(got, "gke-triage")
	if !ok {
		t.Fatalf("gke-triage was not reported as dropped: %+v", got.Dropped)
	}
	// Both missing binaries have to be named: an operator who installs
	// only the first one and restarts would otherwise get a second,
	// identical-looking failure.
	for _, want := range []string{"kubectl", "gcloud", "not on PATH"} {
		if !strings.Contains(reason, want) {
			t.Errorf("reason %q does not mention %q", reason, want)
		}
	}
}

// TestLoadAll_DroppedSkillIsUnreachable: the drop is a Source filter, not
// a cosmetic omission from Infos. The toolset must not be able to serve
// the withheld body — that is the whole point, since Infos is only what
// the host prints.
func TestLoadAll_DroppedSkillIsUnreachable(t *testing.T) {
	t.Parallel()
	project := t.TempDir()
	writeSkillRequires(t, project, "gke-triage", "requires: [kubectl]")
	writeSkillRequires(t, project, "plain", "")

	got, err := LoadAll(context.Background(), project, "", nil, withLookPath())
	if err != nil {
		t.Fatal(err)
	}
	if got.source == nil {
		t.Fatal("expected a source")
	}
	if _, err := got.source.LoadInstructions(context.Background(), "gke-triage"); !errors.Is(err, skill.ErrSkillNotFound) {
		t.Fatalf("LoadInstructions(gke-triage) err = %v, want ErrSkillNotFound", err)
	}
	if _, err := got.source.LoadInstructions(context.Background(), "plain"); err != nil {
		t.Fatalf("LoadInstructions(plain): %v", err)
	}
	fms, err := got.source.ListFrontmatters(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(fms) != 1 || fms[0].Name != "plain" {
		t.Fatalf("ListFrontmatters = %+v, want just plain", fms)
	}
}

// TestLoadAll_AllDroppedIsEmptyButReported: when nothing survives, the
// host must add no toolset AND still be told why. Losing the report here
// would restore the exact silent failure the key exists to end.
func TestLoadAll_AllDroppedIsEmptyButReported(t *testing.T) {
	t.Parallel()
	project := t.TempDir()
	writeSkillRequires(t, project, "gke-triage", "requires: [kubectl]")

	got, err := LoadAll(context.Background(), project, "", nil, withLookPath())
	if err != nil {
		t.Fatal(err)
	}
	if !got.Empty() {
		t.Fatal("expected Empty() — no skill survived, so no toolset should be registered")
	}
	if len(got.Dropped) != 1 || got.Dropped[0].Skill != "gke-triage" {
		t.Fatalf("Dropped = %+v, want gke-triage", got.Dropped)
	}
}

// TestLoadAll_ShellRequirement: `shell` and `bash` both ask whether THIS
// build registered the bash tool, and WithShellTool is how a host that
// loads skills before building its tool registry answers.
func TestLoadAll_ShellRequirement(t *testing.T) {
	t.Parallel()
	for _, token := range []string{"shell", "bash"} {
		t.Run(token, func(t *testing.T) {
			t.Parallel()
			project := t.TempDir()
			writeSkillRequires(t, project, "scripted", "requires: ["+token+"]")

			off, err := LoadAll(context.Background(), project, "", nil, withLookPath(), WithShellTool(false))
			if err != nil {
				t.Fatal(err)
			}
			if !off.Empty() {
				t.Fatalf("expected the skill withheld with no bash tool, got %v", names(off))
			}
			reason, _ := dropReason(off, "scripted")
			if !strings.Contains(reason, "bash tool is not registered") {
				t.Errorf("reason %q should say the bash tool is missing, not that a binary is off PATH", reason)
			}

			// The token must NOT fall through to a PATH lookup: the
			// failure this key was written for is a build with the bash
			// tool disabled on a machine whose /bin/bash is right there.
			on, err := LoadAll(context.Background(), project, "", nil, withLookPath(), WithShellTool(true))
			if err != nil {
				t.Fatal(err)
			}
			if len(on.Infos) != 1 || len(on.Dropped) != 0 {
				t.Fatalf("expected the skill loaded with bash registered, got %v / %+v", names(on), on.Dropped)
			}
		})
	}
}

// TestLoadAll_ShellDefaultsToTheGate: with no WithShellTool, the answer
// comes off the gate — which is why skill reloads, rooted subagents and
// multi-session hosts need no wiring. A gate that was never told
// fail-opens, matching Gate.HasTool.
func TestLoadAll_ShellDefaultsToTheGate(t *testing.T) {
	t.Parallel()
	project := t.TempDir()
	writeSkillRequires(t, project, "scripted", "requires: [shell]")

	// Nil gate — nobody has answered — is the fail-open case.
	open, err := LoadAll(context.Background(), project, "", nil, withLookPath())
	if err != nil {
		t.Fatal(err)
	}
	if len(open.Infos) != 1 {
		t.Fatalf("an unanswered shell question must not withhold a skill, got %+v", open.Dropped)
	}

	gate := gateWithTools(t, map[string]bool{"bash": false, "read_file": true})
	closed, err := LoadAll(context.Background(), project, "", gate, withLookPath())
	if err != nil {
		t.Fatal(err)
	}
	if !closed.Empty() {
		t.Fatalf("a gate that says bash is unregistered must withhold the skill, got %v", names(closed))
	}
}

// TestLoadAll_MalformedRequiresDropsTheSkill: a `requires:` key that
// cannot be read is a drop, not an ignore. Ignoring it would restore the
// silent failure for the one author who was trying to avoid it.
func TestLoadAll_MalformedRequiresDropsTheSkill(t *testing.T) {
	t.Parallel()
	project := t.TempDir()
	writeSkillRequires(t, project, "bent", "requires:\n  shell: true")

	got, err := LoadAll(context.Background(), project, "", nil, withLookPath())
	if err != nil {
		t.Fatal(err)
	}
	if !got.Empty() {
		t.Fatalf("expected the malformed skill withheld, got %v", names(got))
	}
	reason, _ := dropReason(got, "bent")
	if !strings.Contains(reason, "malformed") {
		t.Errorf("reason = %q, want it to say the key is malformed", reason)
	}
}

// TestLoadAll_RequiresSurvivesSanitization: `requires:` is stripped
// before ADK's parser (which decodes with KnownFields(true)) ever sees
// it, so a skill that uses the key must still parse and serve its body.
// If this regresses, every skill in the bundle fails to list.
func TestLoadAll_RequiresSurvivesSanitization(t *testing.T) {
	t.Parallel()
	project := t.TempDir()
	writeSkillRequires(t, project, "scripted", "requires: [kubectl]")

	got, err := LoadAll(context.Background(), project, "", nil, withLookPath("kubectl"))
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(got.Infos) != 1 || got.Infos[0].Name != "scripted" {
		t.Fatalf("Infos = %+v", got.Infos)
	}
	body, err := got.source.LoadInstructions(context.Background(), "scripted")
	if err != nil {
		t.Fatalf("LoadInstructions: %v", err)
	}
	if !strings.Contains(body, "body for scripted") {
		t.Errorf("instruction body = %q", body)
	}
}

// TestLoadAll_RequiresReadFromTheWinningSource: requirements are read
// through the composed overlay, so a project skill shadowing a
// user-global one of the same name contributes its OWN requirements.
// Reading the shadowed bundle's would withhold a skill on the strength
// of a file that is not being served.
func TestLoadAll_RequiresReadFromTheWinningSource(t *testing.T) {
	t.Parallel()
	project := t.TempDir()
	userHome := t.TempDir()
	writeSkillRequires(t, project, "cli-setup", "")
	writeSkillRequires(t, userHome, "cli-setup", "requires: [kubectl]")

	got, err := LoadAll(context.Background(), project, userHome, nil, withLookPath())
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Dropped) != 0 {
		t.Fatalf("the shadowed bundle's requirements were applied: %+v", got.Dropped)
	}
	if len(got.Infos) != 1 {
		t.Fatalf("Infos = %+v", got.Infos)
	}

	// And the other way round: the project copy's requirement is the one
	// that counts even when the shadowed copy declares none.
	flipped := t.TempDir()
	writeSkillRequires(t, flipped, "cli-setup", "requires: [kubectl]")
	got, err = LoadAll(context.Background(), flipped, userHome, nil, withLookPath())
	if err != nil {
		t.Fatal(err)
	}
	if !got.Empty() {
		t.Fatalf("the winning bundle's requirement was not applied, got %v", names(got))
	}
}

// TestScoped_DroppedSkillExplainsItself: a declarative subagent granted a
// skill this runtime withheld is told which capability is missing. The
// bare "unknown skill" would send the author hunting for a typo in a file
// that is sitting exactly where they put it.
func TestScoped_DroppedSkillExplainsItself(t *testing.T) {
	t.Parallel()
	project := t.TempDir()
	writeSkillRequires(t, project, "gke-triage", "requires: [kubectl]")
	writeSkillRequires(t, project, "plain", "")

	loaded, err := LoadAll(context.Background(), project, "", nil, withLookPath())
	if err != nil {
		t.Fatal(err)
	}
	_, err = loaded.Scoped(context.Background(), []string{"gke-triage"})
	if err == nil {
		t.Fatal("expected an error scoping to a withheld skill")
	}
	for _, want := range []string{"gke-triage", "kubectl", "not loaded in this runtime"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "unknown skill") {
		t.Errorf("error %q still calls a withheld skill unknown", err)
	}

	// A genuinely unknown name keeps the old message — the two failures
	// have different fixes and must not read the same.
	if _, err := loaded.Scoped(context.Background(), []string{"nope"}); err == nil || !strings.Contains(err.Error(), "unknown skill") {
		t.Errorf("unknown-name error = %v, want the unknown-skill message", err)
	}
}

// TestScoped_ExplainsEvenWhenEverySkillWasDropped: the no-skills-loaded
// guard used to fire first and swallow the reason. A subagent granted the
// only skill in the bundle is exactly the operator who needs it most.
func TestScoped_ExplainsEvenWhenEverySkillWasDropped(t *testing.T) {
	t.Parallel()
	project := t.TempDir()
	writeSkillRequires(t, project, "gke-triage", "requires: [kubectl]")

	loaded, err := LoadAll(context.Background(), project, "", nil, withLookPath())
	if err != nil {
		t.Fatal(err)
	}
	_, err = loaded.Scoped(context.Background(), []string{"gke-triage"})
	if err == nil || !strings.Contains(err.Error(), "kubectl") {
		t.Fatalf("error = %v, want the missing capability named", err)
	}
}

// TestScoped_CarriesDroppedThrough: a narrowed view answers the withheld
// question the same way its parent did.
func TestScoped_CarriesDroppedThrough(t *testing.T) {
	t.Parallel()
	project := t.TempDir()
	writeSkillRequires(t, project, "gke-triage", "requires: [kubectl]")
	writeSkillRequires(t, project, "plain", "")

	loaded, err := LoadAll(context.Background(), project, "", nil, withLookPath())
	if err != nil {
		t.Fatal(err)
	}
	scoped, err := loaded.Scoped(context.Background(), []string{"plain"})
	if err != nil {
		t.Fatal(err)
	}
	if len(scoped.Dropped) != 1 || scoped.Dropped[0].Skill != "gke-triage" {
		t.Fatalf("scoped.Dropped = %+v, want it carried through", scoped.Dropped)
	}
}

// TestUnsatisfiedString pins the operator-facing line shape — the host
// prints it verbatim, so the skill name has to lead.
func TestUnsatisfiedString(t *testing.T) {
	t.Parallel()
	got := Unsatisfied{Skill: "gke-triage", Reason: `unmet "requires:" — kubectl (not on PATH)`}.String()
	want := `gke-triage: unmet "requires:" — kubectl (not on PATH)`
	if got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}
