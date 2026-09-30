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

package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"google.golang.org/adk/tool"

	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/permissions"
)

// go/printer aligns struct fields and const values with tabs, so
// `Retry int` reaches the outline as `Retry\tint`. Assertions are made
// against whitespace-collapsed text: the tool's contract is which
// declarations appear, not how the printer aligns them.
var wsRun = regexp.MustCompile(`[ \t]+`)

func collapseWS(s string) string { return wsRun.ReplaceAllString(s, " ") }

// goFixture exercises every declaration kind #954 lists, plus the two
// shapes that could smuggle a body into an outline that promises none:
// a top-level func literal and a method.
const goFixture = `package demo

import (
	"fmt"
	al "strings"
)

// Doc comment that should not survive into the outline.
const Version = "1.2.3"

const (
	Alpha = iota
	Beta
)

var Registry = map[string]int{}

// A top-level function literal: its body is a body like any other.
var Handler = func(in string) string {
	bodyTokenInFuncLit := al.ToUpper(in)
	return bodyTokenInFuncLit
}

type Config struct {
	Name  string
	Retry int
}

type Doer interface {
	Do(n int) error
}

type Alias = Config

func Top(a string, b int) (string, error) {
	bodyTokenInFunc := fmt.Sprintf("%s%d", a, b)
	return bodyTokenInFunc, nil
}

func (c *Config) Method() string {
	bodyTokenInMethod := c.Name
	return bodyTokenInMethod
}
`

func outlineGoFixture(t *testing.T) viewFileOutlineResult {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "demo.go")
	if err := os.WriteFile(path, []byte(goFixture), 0o644); err != nil {
		t.Fatal(err)
	}
	fn := viewFileOutlineFunc(gateFor(t, dir), config.DefaultConfig())
	res, err := fn(tool.Context(nil), viewFileOutlineArgs{Path: path})
	if err != nil {
		t.Fatalf("view_file_outline: %v", err)
	}
	return res
}

// Every declaration kind the issue names has to appear, and appear as
// a signature rather than a mention.
func TestViewFileOutline_GoCoversEveryDeclarationKind(t *testing.T) {
	t.Parallel()
	res := outlineGoFixture(t)
	if res.Analysis != analysisParsed {
		t.Errorf("analysis = %q, want %q", res.Analysis, analysisParsed)
	}
	if res.Language != "Go" {
		t.Errorf("language = %q, want Go", res.Language)
	}
	if res.Note != "" {
		t.Errorf("a parse must carry no heuristic caveat; got note = %q", res.Note)
	}
	flat := collapseWS(res.Outline)
	for _, want := range []string{
		"package demo",
		`"fmt"`,
		`al "strings"`,
		`const Version = "1.2.3"`,
		"Alpha = iota",
		"var Registry = map[string]int{}",
		"var Handler = func(in string) string",
		"type Config struct",
		"Retry int",
		"type Doer interface",
		"Do(n int) error",
		"type Alias = Config",
		"func Top(a string, b int) (string, error)",
		"func (c *Config) Method() string",
	} {
		if !strings.Contains(flat, want) {
			t.Errorf("outline missing %q:\n%s", want, res.Outline)
		}
	}
}

// The other half of the promise: no body text. Asserted against
// tokens that exist ONLY inside bodies, so a regression that prints
// whole declarations fails here rather than passing on a coincidence.
//
// The func-literal case is the one a naive implementation gets wrong:
// `var Handler = func(...) {...}` is a GenDecl, so nil-ing FuncDecl
// bodies alone leaves its body fully printed.
func TestViewFileOutline_GoOmitsBodies(t *testing.T) {
	t.Parallel()
	res := outlineGoFixture(t)
	for _, banned := range []string{
		"bodyTokenInFunc",
		"bodyTokenInFuncLit",
		"bodyTokenInMethod",
		"fmt.Sprintf",
		"al.ToUpper",
		"return ",
		"Doc comment",
	} {
		if strings.Contains(res.Outline, banned) {
			t.Errorf("outline leaked body text %q:\n%s", banned, res.Outline)
		}
	}
}

// The line numbers are the point of the tool as much as the
// signatures are: they are what the caller turns into a narrower
// read. A skeleton with wrong offsets is worse than none.
func TestViewFileOutline_GoReportsStartLines(t *testing.T) {
	t.Parallel()
	res := outlineGoFixture(t)
	fixtureLines := strings.Split(goFixture, "\n")
	for _, decl := range []string{"func Top(", "func (c *Config) Method()", "type Doer interface"} {
		wantLine := 0
		for i, l := range fixtureLines {
			if strings.HasPrefix(l, decl) {
				wantLine = i + 1
				break
			}
		}
		if wantLine == 0 {
			t.Fatalf("fixture no longer contains %q", decl)
		}
		want := fmt.Sprintf("%5d  %s", wantLine, decl)
		if !strings.Contains(res.Outline, want) {
			t.Errorf("%q is not reported at line %d:\n%s", decl, wantLine, res.Outline)
		}
	}
}

// A Go file that does not parse must fail loudly. Returning a partial
// skeleton would be the exact dishonesty the tool is built to avoid:
// the result would still say `analysis: "parsed"`.
func TestViewFileOutline_GoParseErrorIsAnError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "broken.go")
	if err := os.WriteFile(path, []byte("package demo\n\nfunc Oops( {\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fn := viewFileOutlineFunc(gateFor(t, dir), config.DefaultConfig())
	if _, err := fn(tool.Context(nil), viewFileOutlineArgs{Path: path}); err == nil {
		t.Fatal("expected an error for an unparseable Go file")
	}
}

// A known non-Go extension is outlined heuristically — and says so.
// The label and the caveat are the contract, not a nicety: without
// them the model cannot tell a scan from a parse.
func TestViewFileOutline_NonGoIsLabelledHeuristic(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "thing.py")
	src := "import os\n\n\nclass Thing:\n    def method(self):\n        secret_body_token = 1\n        return secret_body_token\n\n\ndef top(a, b):\n    return a + b\n"
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	fn := viewFileOutlineFunc(gateFor(t, dir), config.DefaultConfig())
	res, err := fn(tool.Context(nil), viewFileOutlineArgs{Path: path})
	if err != nil {
		t.Fatalf("view_file_outline: %v", err)
	}
	if res.Analysis != analysisHeuristic {
		t.Errorf("analysis = %q, want %q", res.Analysis, analysisHeuristic)
	}
	if res.Language != "Python" {
		t.Errorf("language = %q, want Python", res.Language)
	}
	if res.Note == "" {
		t.Error("a heuristic outline must carry its caveats in note")
	}
	if !strings.Contains(res.Note, "not a parse") {
		t.Errorf("note must say it is not a parse; got %q", res.Note)
	}
	for _, want := range []string{"import os", "class Thing:", "def top(a, b):"} {
		if !strings.Contains(res.Outline, want) {
			t.Errorf("outline missing %q:\n%s", want, res.Outline)
		}
	}
	if strings.Contains(res.Outline, "secret_body_token") {
		t.Errorf("heuristic outline leaked body text:\n%s", res.Outline)
	}
}

// An extension with neither a parser nor a tuned scan is declined,
// and the decline names what would have worked — a refusal the model
// cannot act on costs a turn to discover.
func TestViewFileOutline_UnknownExtensionDeclines(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "notes.xyz")
	if err := os.WriteFile(path, []byte("some prose\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fn := viewFileOutlineFunc(gateFor(t, dir), config.DefaultConfig())
	_, err := fn(tool.Context(nil), viewFileOutlineArgs{Path: path})
	if err == nil {
		t.Fatal("expected a decline for an unknown extension")
	}
	if !strings.Contains(err.Error(), ".py") {
		t.Errorf("decline should name the extensions that work; got %v", err)
	}
}

// Same path-scope treatment as read_file (mirrors
// TestReadFile_OutOfScope_Denied).
func TestViewFileOutline_OutOfScope_Denied(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	other := t.TempDir()
	outside := filepath.Join(other, "secret.go")
	if err := os.WriteFile(outside, []byte("package secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	scope, _ := permissions.NewPathScope(dir, "", nil)
	gate := permissions.New(permissions.Options{
		Mode:  permissions.ModeAllow, // no prompter, no allowlist match → deny
		Scope: scope,
	})
	fn := viewFileOutlineFunc(gate, config.DefaultConfig())
	if _, err := fn(tool.Context(nil), viewFileOutlineArgs{Path: outside}); err == nil {
		t.Fatal("expected denial for an out-of-scope outline")
	}
}

// The per-tool output cap has to reach the outline, or a 20k-line
// generated file defeats the tool that exists to make big files cheap.
func TestViewFileOutline_HonorsOutputCaps(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "many.go")
	var b strings.Builder
	b.WriteString("package demo\n")
	for i := 0; i < 200; i++ {
		b.WriteString("func F")
		b.WriteString(strconv.Itoa(i))
		b.WriteString("() {}\n")
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultConfig()
	cfg.ToolOutput.PerTool["view_file_outline"] = config.ToolOutputPerToolCaps{MaxLines: 10}
	fn := viewFileOutlineFunc(gateFor(t, dir), cfg)
	res, err := fn(tool.Context(nil), viewFileOutlineArgs{Path: path})
	if err != nil {
		t.Fatalf("view_file_outline: %v", err)
	}
	if !strings.Contains(res.Outline, "truncated by core-agent") {
		t.Errorf("outline was not capped:\n%s", res.Outline)
	}
}

// The three properties the task calls "treated like every other read
// tool" that are not visible from the handler alone: on by default,
// read-only for concurrent dispatch, and runnable before a plan under
// plan_mode: required.
func TestViewFileOutline_IsAnOrdinaryReadTool(t *testing.T) {
	t.Parallel()
	if !Default().ViewFileOutline {
		t.Error("Default() must enable view_file_outline")
	}
	cfg := config.DefaultConfig()
	cfg.Permissions.PlanMode = config.PlanModeRequired
	gate := permissions.New(permissions.Options{
		Mode:                permissions.ModeYolo,
		RequirePlanArtifact: true,
	})
	reg, err := Build(cfg, gate, t.TempDir(), Default())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	var found tool.Tool
	for _, tl := range reg.Tools {
		if tl.Name() == "view_file_outline" {
			found = tl
		}
	}
	if found == nil {
		t.Fatalf("Build did not register view_file_outline; got %v", toolNames(reg))
	}
	if !IsReadOnlyTool(found) {
		t.Error("view_file_outline must be classified read-only so it dispatches concurrently")
	}
	if !IsReadOnlyToolName("view_file_outline") {
		t.Error("IsReadOnlyToolName must know view_file_outline")
	}
	// Plan-exempt: with no plan recorded, a read must still run.
	dir := t.TempDir()
	path := filepath.Join(dir, "x.go")
	if err := os.WriteFile(path, []byte("package x\n\nfunc F() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	scope, err := permissions.NewPathScope(dir, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	planGate := permissions.New(permissions.Options{
		Mode:                permissions.ModeYolo,
		Scope:               scope,
		RequirePlanArtifact: true,
	})
	res, err := viewFileOutlineFunc(planGate, config.DefaultConfig())(tool.Context(nil), viewFileOutlineArgs{Path: path})
	if err != nil {
		t.Fatalf("view_file_outline must run before a plan is recorded, as read_file does: %v", err)
	}
	if !strings.Contains(res.Outline, "func F()") {
		t.Errorf("unexpected outline: %s", res.Outline)
	}
}
