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

package background

import (
	"context"
	"errors"
	"strings"
	"testing"

	"google.golang.org/adk/tool"
)

// A rejected spawn has to say what WOULD have worked. A parent with no
// tools of its own was seen live guessing two dozen subagent names (k8s,
// sre, devops, triage, oncall, ...) and a run of tool names (kubectl,
// bash, shell, exec, ...), one rejected spawn each, because the errors
// named only the value that was wrong. The watchdog warned three
// different ways; the guessing only ended when the model gave up.

func TestUnknownTool_NamesWhatIsGrantable(t *testing.T) {
	t.Parallel()
	prov := &recordingProvider{llm: &completingLLM{detail: "d"}}
	mgr, err := NewManager(WithProvider(prov, "parent-model"),
		WithCatalog([]tool.Tool{newNamedStubTool(t, "read_file"), newNamedStubTool(t, "grep")}))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer mgr.Close()

	_, err = mgr.resolveTools([]string{"kubectl"})
	if !errors.Is(err, ErrUnknownTool) {
		t.Fatalf("want ErrUnknownTool, got %v", err)
	}
	if want := "the names a subagent can be granted here are: grep, read_file"; !strings.Contains(err.Error(), want) {
		t.Errorf("error does not list the grantable tools:\n got %q\nwant it to contain %q", err, want)
	}
}

func TestUnknownTool_SaysWhenNothingIsGrantable(t *testing.T) {
	t.Parallel()
	prov := &recordingProvider{llm: &completingLLM{detail: "d"}}
	mgr, err := NewManager(WithProvider(prov, "parent-model"))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer mgr.Close()

	_, err = mgr.resolveTools([]string{"bash"})
	if !errors.Is(err, ErrUnknownTool) {
		t.Fatalf("want ErrUnknownTool, got %v", err)
	}
	if want := "this build has no tools that can be granted to a subagent"; !strings.Contains(err.Error(), want) {
		t.Errorf("error does not say nothing is grantable:\n got %q\nwant it to contain %q", err, want)
	}
}

func TestUnknownSubagent_NamesTheRoster(t *testing.T) {
	t.Parallel()
	prov := &recordingProvider{llm: &completingLLM{detail: "d"}}
	mgr := newTemplateManager(t, prov, rosterTemplates(prov))
	defer mgr.Close()

	want := "the subagents that can be referenced by name are: cluster, docs"
	// Both doors a name can be rejected at: the declarative template
	// lookup and the predefined-spec lookup.
	_, err := mgr.SpawnTemplate(context.Background(), "", "k8s", RefOverrides{Goal: "g"}, "")
	if !errors.Is(err, ErrUnknownSubagent) || !strings.Contains(err.Error(), want) {
		t.Errorf("SpawnTemplate error = %v, want ErrUnknownSubagent naming %q", err, want)
	}
	_, err = mgr.resolvePredefinedSpec("sre", RefOverrides{Goal: "g"})
	if !errors.Is(err, ErrUnknownSubagent) || !strings.Contains(err.Error(), want) {
		t.Errorf("resolvePredefinedSpec error = %v, want ErrUnknownSubagent naming %q", err, want)
	}
}

func TestUnknownSubagent_SaysWhenNoneAreConfigured(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		adhoc bool
		want  string
	}{
		"inline allowed":  {adhoc: true, want: "leave agent empty and describe the subagent inline with system_prompt"},
		"inline disabled": {adhoc: false, want: "inline subagents are disabled, so no spawn can succeed"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			prov := &recordingProvider{llm: &completingLLM{detail: "d"}}
			mgr, err := NewManager(WithProvider(prov, "parent-model"), WithAllowAdhoc(tc.adhoc))
			if err != nil {
				t.Fatalf("NewManager: %v", err)
			}
			defer mgr.Close()

			_, err = mgr.resolvePredefinedSpec("kubernetes", RefOverrides{Goal: "g"})
			if !errors.Is(err, ErrUnknownSubagent) {
				t.Fatalf("want ErrUnknownSubagent, got %v", err)
			}
			if !strings.Contains(err.Error(), "no subagents are configured for reference by name") ||
				!strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to say none are configured and %q", err, tc.want)
			}
		})
	}
}

// The declaration half: with nothing to grant, `tools` says so instead
// of staying silent — silence on a tool-less build is what sent the
// model looking for a name. The description and `agent` stay as shipped
// (TestSpawnAgentSchema_UnchangedWithoutSubagents).
func TestSpawnAgentSchema_ToolsSaysWhenNothingIsGrantable(t *testing.T) {
	t.Parallel()
	prov := &recordingProvider{llm: &completingLLM{detail: "d"}}
	mgr, err := NewManager(WithProvider(prov, "parent-model"))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer mgr.Close()

	decl := spawnDeclaration(t, mgr)
	desc := toolsProperty(t, decl).Description
	if want := "No tools can be granted to an inline subagent in this build"; !strings.Contains(desc, want) {
		t.Errorf("tools description does not say nothing is grantable:\n%s", desc)
	}
	if decl.Description != spawnAgentDescription {
		t.Errorf("the tool description changed on a build with no subagents:\n%s", decl.Description)
	}
}

// The delegation this must not discourage: a parent with nothing to
// grant (--no-builtin-tools) and a configured subagent that brings its
// own tools. The empty-catalog note is about the inline form; read as a
// claim about the configured subagent it would be false, and would talk
// the model out of the one spawn that can succeed.
func TestSpawnAgentSchema_EmptyCatalogDoesNotDisownConfiguredSubagents(t *testing.T) {
	t.Parallel()
	prov := &recordingProvider{llm: &completingLLM{detail: "d"}}
	tmpls := rosterTemplates(prov)
	tmpls[0].Tools = []tool.Tool{newNamedStubTool(t, "kubectl_get")}
	mgr := newTemplateManager(t, prov, tmpls)
	defer mgr.Close()

	desc := toolsProperty(t, spawnDeclaration(t, mgr)).Description
	if !strings.Contains(desc, "inline subagent") || !strings.Contains(desc, "a configured subagent keeps its own grant") {
		t.Errorf("tools description must scope the empty-catalog note to inline subagents:\n%s", desc)
	}
	if strings.Contains(desc, "goal alone") {
		t.Errorf("tools description claims subagents work from their goal alone, which a configured one with its own grant does not:\n%s", desc)
	}
}

// The ad-hoc refusal is the third door. Its fixed text said "reference a
// configured subagent by name" even when none were configured, which
// cost a guessed name and a second rejection to learn no spawn could
// succeed.
func TestAdhocDisabled_NamesTheRemedyThatExists(t *testing.T) {
	t.Parallel()
	inline := spawnAgentArgs{Name: "adhoc", SystemPrompt: "p", Goal: "g"}

	t.Run("roster configured", func(t *testing.T) {
		t.Parallel()
		prov := &recordingProvider{llm: &completingLLM{detail: "d"}}
		mgr := newTemplateManager(t, prov, rosterTemplates(prov))
		defer mgr.Close()
		_, _, err := buildSpawnSpec(mgr, inline)
		if !errors.Is(err, ErrAdhocDisabled) {
			t.Fatalf("want ErrAdhocDisabled, got %v", err)
		}
		want := "reference a configured subagent by name instead: cluster, docs"
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to name the roster: %q", err, want)
		}
	})
	t.Run("nothing configured", func(t *testing.T) {
		t.Parallel()
		prov := &recordingProvider{llm: &completingLLM{detail: "d"}}
		mgr, err := NewManager(WithProvider(prov, "parent-model"))
		if err != nil {
			t.Fatalf("NewManager: %v", err)
		}
		defer mgr.Close()
		_, _, err = buildSpawnSpec(mgr, inline)
		if !errors.Is(err, ErrAdhocDisabled) {
			t.Fatalf("want ErrAdhocDisabled, got %v", err)
		}
		if !strings.Contains(err.Error(), "no subagents are configured for reference by name here, so no spawn can succeed") {
			t.Errorf("error = %q, want it to say no spawn can succeed", err)
		}
		if strings.Contains(err.Error(), "reference a configured subagent by name instead") {
			t.Errorf("error = %q still sends the model to a roster that does not exist", err)
		}
	})
}

func TestJoinNames_CapsALongList(t *testing.T) {
	t.Parallel()
	names := make([]string, maxListedNames+7)
	for i := range names {
		names[i] = "t"
	}
	got := joinNames(names)
	if !strings.HasSuffix(got, ", and 7 more (the spawn_agent parameters list them all)") {
		t.Errorf("joinNames did not cap the list: %q", got)
	}
	if n := strings.Count(got, "t, "); n > maxListedNames {
		t.Errorf("joinNames listed %d names, want at most %d", n, maxListedNames)
	}
}
