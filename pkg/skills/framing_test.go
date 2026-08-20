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
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	adkagent "google.golang.org/adk/agent"
	"google.golang.org/adk/memory"
	"google.golang.org/adk/session"
	adktool "google.golang.org/adk/tool"
	"google.golang.org/adk/tool/skilltoolset/skill"
	"google.golang.org/adk/tool/toolconfirmation"
	"google.golang.org/genai"
)

// stubToolCtx is a minimal adktool.Context so these tests can drive the
// real load_skill tool. ADK's own equivalent lives behind internal/, so
// there is nothing to import. Only the embedded context.Context carries
// anything; every other method returns the zero value. Satisfying the
// whole interface rather than a narrow shim keeps a future ADK bump
// honest — it breaks the build instead of drifting.
type stubToolCtx struct {
	context.Context
}

// ReadonlyContext.
func (stubToolCtx) UserContent() *genai.Content          { return nil }
func (stubToolCtx) InvocationID() string                 { return "test-invocation" }
func (stubToolCtx) AgentName() string                    { return "test-agent" }
func (stubToolCtx) ReadonlyState() session.ReadonlyState { return nil }
func (stubToolCtx) UserID() string                       { return "test-user" }
func (stubToolCtx) AppName() string                      { return "test-app" }
func (stubToolCtx) SessionID() string                    { return "test-session" }
func (stubToolCtx) Branch() string                       { return "" }

// CallbackContext (adds).
func (stubToolCtx) Artifacts() adkagent.Artifacts { return nil }
func (stubToolCtx) State() session.State          { return nil }

// adktool.Context (adds).
func (stubToolCtx) FunctionCallID() string                               { return "test-call" }
func (stubToolCtx) Actions() *session.EventActions                       { return nil }
func (stubToolCtx) ToolConfirmation() *toolconfirmation.ToolConfirmation { return nil }
func (stubToolCtx) RequestConfirmation(string, any) error                { return nil }
func (stubToolCtx) SearchMemory(context.Context, string) (*memory.SearchResponse, error) {
	return nil, nil
}

// loadSkillInstructions drives the real `load_skill` tool off s.Toolset and
// returns the instruction body the MODEL would see.
//
// Asserting on s.source would not do. The wiring bug this guards against is
// building the toolset over the raw source while storing the framed one on
// Skills — a shape in which every source-level assertion passes and the
// model still never reads the framing. Only the tool result settles it.
func loadSkillInstructions(t *testing.T, s Skills, name string) string {
	t.Helper()
	if s.Empty() {
		t.Fatal("no skills loaded")
	}
	list, err := s.Toolset.Tools(nil)
	if err != nil {
		t.Fatalf("Toolset.Tools: %v", err)
	}
	for _, tl := range list {
		if tl.Name() != "load_skill" {
			continue
		}
		runner, ok := tl.(interface {
			Run(adktool.Context, any) (map[string]any, error)
		})
		if !ok {
			t.Fatalf("load_skill is %T, which is not runnable", tl)
		}
		res, runErr := runner.Run(stubToolCtx{Context: t.Context()}, map[string]any{"name": name})
		if runErr != nil {
			t.Fatalf("load_skill(%q): %v", name, runErr)
		}
		instr, ok := res["instructions"].(string)
		if !ok {
			t.Fatalf("load_skill(%q) result has no string \"instructions\": %#v", name, res)
		}
		return instr
	}
	t.Fatal("the skill toolset exposes no load_skill tool")
	return ""
}

// TestLoadSkillFramesTheInstructionBody is the #711 acceptance criterion:
// the text the model reads after a skill's own Step 0 says the skill does
// not get to change the task.
func TestLoadSkillFramesTheInstructionBody(t *testing.T) {
	t.Parallel()
	project := t.TempDir()
	writeSkill(t, project, "alpha", "the alpha skill")
	s, err := LoadAll(context.Background(), project, "", nil)
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}

	instr := loadSkillInstructions(t, s, "alpha")
	if !strings.Contains(instr, "body for alpha") {
		t.Errorf("the skill's own body was lost:\n%s", instr)
	}
	if !strings.HasSuffix(instr, InstructionFraming) {
		t.Errorf("instructions do not end with the framing:\n%s", instr)
	}
	// The framing must come AFTER the skill body. Recency is the whole
	// mechanism of the bug — a preamble is read before the Step 0 it is
	// meant to override, which is the position that already failed.
	if strings.Index(instr, "body for alpha") > strings.Index(instr, "End of skill guidance") {
		t.Error("the framing precedes the skill body; it has to speak last")
	}
}

// TestInstructionFramingStatesItsThreeRules pins the wording against being
// hollowed out. Each clause is a distinct failure mode from the UAT
// session: the subagent changed WHICH cluster it worked on, re-derived
// parameters it had already been handed, and improvised when a step named
// tooling the distroless image does not carry.
func TestInstructionFramingStatesItsThreeRules(t *testing.T) {
	t.Parallel()
	for _, want := range []string{
		// A skill governs how, not what or where.
		"does not change",
		"*what* you were asked to do",
		"*which* subject",
		// Do not re-acquire parameters the task already supplied.
		"use the ones your task already gave you",
		// A missing tool is not licence to change the target.
		"not a reason to change target or widen scope",
	} {
		if !strings.Contains(InstructionFraming, want) {
			t.Errorf("InstructionFraming no longer says %q:\n%s", want, InstructionFraming)
		}
	}
}

// TestScopedSkillsStayFramed is the composition claim in the framedSource
// doc: a declarative subagent narrowed with subagents[].skills reaches
// load_skill through filteredSource, and the framing must survive that
// layer. The #711 session was a SUBAGENT, so a fix that only covered the
// parent would not have covered the reported bug at all.
func TestScopedSkillsStayFramed(t *testing.T) {
	t.Parallel()
	s := loadThree(t)
	scoped, err := s.Scoped(context.Background(), []string{"alpha"})
	if err != nil {
		t.Fatalf("Scoped: %v", err)
	}
	instr := loadSkillInstructions(t, scoped, "alpha")
	if !strings.HasSuffix(instr, InstructionFraming) {
		t.Errorf("a scoped subagent's skill is unframed:\n%s", instr)
	}
}

// TestFramingDoesNotTouchFrontmatterOrResources: the trailer belongs at the
// end of the instruction body and nowhere else. A framed description would
// land in every /skills listing and in the system-instruction skill index;
// a framed reference file would repeat the trailer once per resource read.
func TestFramingDoesNotTouchFrontmatterOrResources(t *testing.T) {
	t.Parallel()
	project := t.TempDir()
	writeSkill(t, project, "alpha", "the alpha skill")
	refs := filepath.Join(project, SkillDirName, "alpha", "references")
	if err := os.MkdirAll(refs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(refs, "detail.md"), []byte("reference detail"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := LoadAll(context.Background(), project, "", nil)
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	ctx := context.Background()

	for _, in := range s.Infos {
		if strings.Contains(in.Description, "End of skill guidance") {
			t.Errorf("skill %q's description carries the framing: %q", in.Name, in.Description)
		}
	}
	fm, err := s.source.LoadFrontmatter(ctx, "alpha")
	if err != nil {
		t.Fatalf("LoadFrontmatter: %v", err)
	}
	if strings.Contains(fm.Description, "End of skill guidance") {
		t.Errorf("frontmatter description carries the framing: %q", fm.Description)
	}

	rc, err := s.source.LoadResource(ctx, "alpha", "references/detail.md")
	if err != nil {
		t.Fatalf("LoadResource: %v", err)
	}
	defer func() { _ = rc.Close() }()
	body, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(body); got != "reference detail" {
		t.Errorf("resource body = %q, want it unframed and verbatim", got)
	}
}

// TestFramedSourcePropagatesErrors: a decorator that swallowed the
// not-found sentinel would answer a bad skill name with a successful load
// of nothing but the framing.
//
// It goes at the UNSCOPED source deliberately. Asking a scoped source for a
// hidden skill proves nothing about this layer — filteredSource returns the
// sentinel before framedSource is ever reached, so that call exercises the
// filter and leaves the decorator's error path untested.
func TestFramedSourcePropagatesErrors(t *testing.T) {
	t.Parallel()
	s := loadThree(t)
	if _, ok := s.source.(*framedSource); !ok {
		t.Fatalf("Skills.source is %T, want *framedSource", s.source)
	}
	got, err := s.source.LoadInstructions(context.Background(), "no-such-skill")
	if !errors.Is(err, skill.ErrSkillNotFound) {
		t.Errorf("LoadInstructions(no-such-skill) err = %v, want ErrSkillNotFound", err)
	}
	if got != "" {
		t.Errorf("LoadInstructions(no-such-skill) returned %q on error, want empty", got)
	}
}

func TestFrameInstructions(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "appends once, after the body",
			body: "do the thing",
			want: "do the thing" + InstructionFraming,
		},
		{
			// A file ending in a newline is the normal case; the framing
			// opens with its own blank line, so keeping the body's trailing
			// newlines would widen the gap by however many the author left.
			name: "normalises trailing newlines",
			body: "do the thing\n\n\n",
			want: "do the thing" + InstructionFraming,
		},
		{
			// "End of skill guidance" is a false statement about a bundle
			// that gave none, and a frontmatter-only skill has nothing to
			// override the task with.
			name: "leaves a blank body alone",
			body: "  \n\t\n",
			want: "  \n\t\n",
		},
		{
			name: "leaves an empty body alone",
			body: "",
			want: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := frameInstructions(tc.body); got != tc.want {
				t.Errorf("frameInstructions(%q) =\n%q\nwant\n%q", tc.body, got, tc.want)
			}
		})
	}
}
