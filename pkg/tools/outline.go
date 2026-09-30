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
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/scanner"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"

	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/permissions"
)

// view_file_outline (#954) answers "what is in this file" without
// paying for the file. grep finds a string and read_file reads a
// range; neither is the survey step, so the model reads whole files to
// discover that one declaration in them matters.
//
// The central constraint is honesty about provenance. Go is parsed
// with go/parser, so the outline is exactly what the compiler sees.
// Everything else either gets a line-based scan that the result
// LABELS as a heuristic, or is declined. A guess that reads like a
// parse is worse than no outline at all: the model cannot tell it was
// guessing and will act on a signature that does not exist.

// Analysis values for viewFileOutlineResult.Analysis.
const (
	analysisParsed    = "parsed"
	analysisHeuristic = "heuristic"
)

// heuristicNote is attached verbatim to every heuristic outline. It is
// model-facing: the whole point is that the model can tell the
// difference between this and a parse.
const heuristicNote = "Line-based heuristic, not a parse: these are lines that begin with a declaration keyword for this language. " +
	"Declarations spanning several lines are reported by their first line only; a matching line inside a string or comment is reported as if it were a declaration; " +
	"nothing here distinguishes a declaration from a struct field, a local, or a nested definition — indentation is preserved verbatim so you can see the nesting, but it is the only clue there is; " +
	"and a declaration whose line does not begin with one of those keywords is not reported at all (a JavaScript or TypeScript class method, for one), so an absence here proves nothing. " +
	"Confirm anything load-bearing by reading the lines themselves."

type viewFileOutlineArgs struct {
	Path string `json:"path" jsonschema:"absolute or relative path of the file to outline"`
}

type viewFileOutlineResult struct {
	Path string `json:"path"`
	// Language is the language the outline was produced for, e.g.
	// "Go", "Python".
	Language string `json:"language"`
	// Analysis is "parsed" when the outline came from a real parser
	// for the language and "heuristic" when it came from a line scan.
	Analysis string `json:"analysis"`
	// Outline is the skeleton itself: one entry per declaration,
	// prefixed with the line it starts on.
	Outline string `json:"outline"`
	// Note carries the caveats that apply to this outline. Empty for
	// a parse.
	Note string `json:"note,omitempty"`
}

// heuristicLang describes the line shapes that start a declaration in
// one language. Prefixes match the start of the trimmed line;
// suffixes match its end (shell functions are `name() {`, which has
// no leading keyword).
type heuristicLang struct {
	name     string
	prefixes []string
	suffixes []string
}

var (
	jsPrefixes = []string{"function ", "async function ", "class ", "export ", "const ", "let ", "var ", "import ", "require("}
	tsPrefixes = append(append([]string{}, jsPrefixes...), "interface ", "type ", "enum ", "declare ", "abstract ")
)

// heuristicLangs maps a file extension to the line shapes worth
// reporting for it. An extension absent from this map and not ".go"
// is DECLINED rather than scanned with a generic guess — a scan tuned
// for nothing finds either everything or nothing, and both are noise
// the model has no way to discount.
//
// C and C++ are declined for the same reason. A C function definition
// has no leading keyword (`static int helper(int x) {`), so a prefix
// scan reports a file's includes and structs and none of its functions
// — an outline that looks complete and is missing the part the caller
// came for.
var heuristicLangs = map[string]heuristicLang{
	".py":   {name: "Python", prefixes: []string{"def ", "async def ", "class ", "import ", "from "}},
	".rb":   {name: "Ruby", prefixes: []string{"def ", "class ", "module ", "require ", "require_relative "}},
	".rs":   {name: "Rust", prefixes: []string{"fn ", "pub ", "pub(", "async fn ", "async unsafe fn ", "unsafe fn ", "unsafe impl ", "unsafe trait ", "extern ", "macro_rules!", "struct ", "enum ", "trait ", "impl ", "mod ", "use ", "const ", "static ", "type "}},
	".java": {name: "Java", prefixes: []string{"package ", "import ", "class ", "interface ", "enum ", "record ", "public ", "protected ", "private ", "abstract ", "final ", "static "}},
	".js":   {name: "JavaScript", prefixes: jsPrefixes},
	".mjs":  {name: "JavaScript", prefixes: jsPrefixes},
	".cjs":  {name: "JavaScript", prefixes: jsPrefixes},
	".jsx":  {name: "JavaScript", prefixes: jsPrefixes},
	".ts":   {name: "TypeScript", prefixes: tsPrefixes},
	".tsx":  {name: "TypeScript", prefixes: tsPrefixes},
	".sh":   {name: "Shell", prefixes: []string{"function "}, suffixes: []string{"() {", "(){"}},
	".bash": {name: "Shell", prefixes: []string{"function "}, suffixes: []string{"() {", "(){"}},
}

// viewFileOutlineFunc returns the ADK functiontool handler for
// view_file_outline. Same gate, path-scope and truncation treatment as
// read_file: gate.CheckFileRead before touching disk, capsFor +
// Truncate on the way out.
func viewFileOutlineFunc(gate *permissions.Gate, cfg *config.Config) functiontool.Func[viewFileOutlineArgs, viewFileOutlineResult] {
	return func(ctx tool.Context, in viewFileOutlineArgs) (viewFileOutlineResult, error) {
		path, err := absolutize(in.Path)
		if err != nil {
			return viewFileOutlineResult{}, err
		}
		if err := gate.CheckFileRead(ctx, "view_file_outline", path); err != nil {
			return viewFileOutlineResult{}, err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return viewFileOutlineResult{}, fmt.Errorf("view_file_outline: %w", err)
		}
		res, err := outlineFor(path, data)
		if err != nil {
			return viewFileOutlineResult{}, err
		}
		caps := capsFor(cfg, "view_file_outline", 64*1024, 2000)
		res.Outline = Truncate(res.Outline, caps.bytes, caps.lines)
		return res, nil
	}
}

// outlineFor dispatches on extension: Go gets a real parse, a known
// extension gets a labelled heuristic, anything else is declined.
func outlineFor(path string, data []byte) (viewFileOutlineResult, error) {
	ext := strings.ToLower(filepath.Ext(path))
	if ext == ".go" {
		body, err := outlineGo(path, data)
		if err != nil {
			return viewFileOutlineResult{}, err
		}
		return viewFileOutlineResult{Path: path, Language: "Go", Analysis: analysisParsed, Outline: body}, nil
	}
	lang, ok := heuristicLangs[ext]
	if !ok {
		return viewFileOutlineResult{}, fmt.Errorf("view_file_outline: %s; this tool parses Go (.go) and scans %s. Read the file's lines instead", declineSubject(ext), knownHeuristicExtensions())
	}
	return viewFileOutlineResult{
		Path:     path,
		Language: lang.name,
		Analysis: analysisHeuristic,
		Outline:  outlineHeuristic(data, lang),
		Note:     heuristicNote,
	}, nil
}

// declineSubject names what was refused. filepath.Ext returns "" for
// a file with no dot in its name (Makefile, a suffixless script), and
// `no outline available for "" files` reads like a bug rather than an
// answer — the caller should be able to tell a missing extension from
// an unsupported one.
func declineSubject(ext string) string {
	if ext == "" {
		return "no outline available for a file with no extension"
	}
	return fmt.Sprintf("no outline available for %q files", ext)
}

// knownHeuristicExtensions renders the scanned extensions for the
// decline message, so a declined call tells the model what would have
// worked rather than only what did not.
func knownHeuristicExtensions() string {
	exts := make([]string, 0, len(heuristicLangs))
	for ext := range heuristicLangs {
		exts = append(exts, ext)
	}
	sort.Strings(exts)
	return strings.Join(exts, ", ")
}

// outlineGo parses the file and renders every top-level declaration
// with its body removed.
//
// Bodies are stripped on the AST before printing rather than filtered
// out of the printed text afterwards: a text filter has to guess where
// a body ends, and the whole promise of this path is that it does not
// guess. Function literals assigned at top level (`var H = func() {
// ... }`) are emptied by the same walk — without it, a var declaration
// would smuggle a whole function body into an outline that claims to
// have none.
func outlineGo(path string, data []byte) (string, error) {
	fset := token.NewFileSet()
	// No ParseComments: doc comments are prose, often longer than the
	// declaration, and the caller asked for structure.
	file, err := parser.ParseFile(fset, filepath.Base(path), data, parser.SkipObjectResolution)
	if err != nil {
		return "", fmt.Errorf("view_file_outline: parse: %w", physicalParseError(err, filepath.Base(path), data))
	}
	cfg := &printer.Config{Mode: printer.TabIndent, Tabwidth: 4}

	var b strings.Builder
	fmt.Fprintf(&b, "%5d  package %s\n", sourceLine(fset, file.Package), file.Name.Name)
	for _, decl := range file.Decls {
		rendered, err := renderDecl(cfg, fset, decl)
		if err != nil {
			return "", fmt.Errorf("view_file_outline: render: %w", err)
		}
		b.WriteString(numberBlock(sourceLine(fset, decl.Pos()), rendered))
	}
	return b.String(), nil
}

// sourceLine is the physical line pos sits on in the file that was
// read. fset.Position honours `//line` directives even without
// ParseComments, so generated Go (goyacc, templ) would report lines
// in the file it was generated FROM — a number the caller then hands
// to read_file as an offset into the wrong file.
func sourceLine(fset *token.FileSet, pos token.Pos) int {
	return fset.PositionFor(pos, false).Line
}

// physicalParseError rewrites a parse error's positions to physical
// lines in the file that was read. The parser reports positions
// remapped by `//line` directives, so a broken generated file would
// otherwise be blamed on a line of the grammar it came from. Each
// entry's byte Offset is unaffected by directives, so the line and
// column are recomputed from it.
func physicalParseError(err error, name string, data []byte) error {
	var list scanner.ErrorList
	if !errors.As(err, &list) {
		return err
	}
	out := make(scanner.ErrorList, 0, len(list))
	for _, e := range list {
		pos := e.Pos
		if off := pos.Offset; off >= 0 && off <= len(data) {
			pos.Filename = name
			pos.Line = 1 + bytes.Count(data[:off], []byte("\n"))
			pos.Column = off - (bytes.LastIndexByte(data[:off], '\n') + 1) + 1
		}
		out = append(out, &scanner.Error{Pos: pos, Msg: e.Msg})
	}
	return out
}

// renderDecl prints one top-level declaration with its body (and any
// nested function-literal body) removed.
func renderDecl(cfg *printer.Config, fset *token.FileSet, decl ast.Decl) (string, error) {
	stripFuncLitBodies(decl)
	switch d := decl.(type) {
	case *ast.FuncDecl:
		// Copy rather than nil the caller's fields. This is tidiness,
		// not a guarantee: stripFuncLitBodies above has already
		// emptied function literals in place, and outlineGo discards
		// the AST after rendering.
		shallow := *d
		shallow.Body = nil
		shallow.Doc = nil
		decl = &shallow
	case *ast.GenDecl:
		shallow := *d
		shallow.Doc = nil
		decl = &shallow
	}
	var buf bytes.Buffer
	if err := cfg.Fprint(&buf, fset, decl); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// stripFuncLitBodies empties every function literal reachable from
// decl. Keeping the braces (rather than removing the literal) leaves
// the declaration printable and obviously a function.
//
// Lbrace and Rbrace are collapsed onto the literal's opening position
// so the printer emits `func(x int) int {}` on one line. Reusing the
// original Rbrace would keep the closing brace at its old line, and
// the printer would fill the gap with the blank lines the body used
// to occupy — an outline padded to the height of the code it replaced.
func stripFuncLitBodies(decl ast.Decl) {
	ast.Inspect(decl, func(n ast.Node) bool {
		lit, ok := n.(*ast.FuncLit)
		if !ok || lit.Body == nil {
			return true
		}
		lit.Body = &ast.BlockStmt{Lbrace: lit.Body.Lbrace, Rbrace: lit.Body.Lbrace}
		return true
	})
}

// numberBlock prefixes the first line of a rendered declaration with
// the line it starts on and indents its continuation lines to match,
// so the outline reads as a listing the caller can turn straight into
// a read_file offset.
func numberBlock(line int, rendered string) string {
	lines := strings.Split(strings.TrimRight(rendered, "\n"), "\n")
	var b strings.Builder
	for i, l := range lines {
		if i == 0 {
			fmt.Fprintf(&b, "%5d  %s\n", line, l)
			continue
		}
		fmt.Fprintf(&b, "       %s\n", l)
	}
	return b.String()
}

// outlineHeuristic reports the lines that look like the start of a
// declaration in lang. It reports LINES, not parsed structure, and the
// result that carries it says so.
//
// Leading whitespace is PRESERVED rather than trimmed. The scan cannot
// tell a top-level declaration from a struct field, a local, or a
// nested definition — `fn nested_helper()` inside another function
// matches the same prefix as a top-level `fn` — so stripping the indent
// would present nested things as top-level with nothing to notice it
// by. Indentation is the only structural signal available here, and the
// note says as much; trailing space still goes, since it carries none.
func outlineHeuristic(data []byte, lang heuristicLang) string {
	var b strings.Builder
	for i, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimRight(raw, " \t\r")
		if strings.TrimSpace(line) == "" || !looksLikeDecl(strings.TrimSpace(line), lang) {
			continue
		}
		fmt.Fprintf(&b, "%5d  %s\n", i+1, line)
	}
	return b.String()
}

func looksLikeDecl(trimmed string, lang heuristicLang) bool {
	for _, p := range lang.prefixes {
		if strings.HasPrefix(trimmed, p) {
			return true
		}
	}
	for _, s := range lang.suffixes {
		if strings.HasSuffix(trimmed, s) {
			return true
		}
	}
	return false
}
