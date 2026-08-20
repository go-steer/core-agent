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

// Command coretui-guard-check holds the capability-guard lists in
// cmd/core-agent/coretui_guards.go and internal/coretuiremote/guards.go
// exhaustive against the core-tui this repo actually pins.
//
// The problem it exists for (#812): the `var _ coretui.X = (*T)(nil)`
// guards #810 added are a snapshot of one core-tui version. A guard
// makes the compiler object when core-tui adds a *method* to an
// interface we already name. Nothing objects when core-tui adds a whole
// new interface — it lands in neither list, no host implements it, and
// the first anyone hears about the capability is when someone reads the
// release notes. That is the same silent omission as #802/#803, one
// level up.
//
// So: enumerate the exported interfaces of core-tui's tui package at
// the pinned version, and require every one of them to be *accounted
// for* by each adapter — either guarded (implemented, compiler-checked)
// or declined (a `//coretui:declined` directive with prose saying why).
//
// Modes:
//
//	coretui-guard-check           # verify; non-zero exit on any finding
//	coretui-guard-check --print   # dump the interface x adapter matrix
//
// Run it through dev/tools/verify-coretui-guards, which is what
// dev/ci/presubmits/verify-coretui-guards and dev/tools/ci call.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"text/tabwriter"
)

// coretuiPkg is the one import path this whole check is about. It is a
// package path, not a version: the version comes out of `go list`,
// which reads go.mod. Hardcoding a version here would make this gate an
// instance of the bug it exists to catch.
const coretuiPkg = "github.com/go-steer/core-tui/tui"

// adapter is one core-tui host in this repo: a guard file, the file
// whose build constraint that guard file has to match, and the type the
// guards assert about (used only to write a copy-pasteable remedy into
// the failure message).
//
// This list is the one thing a new host has to be added to. It cannot
// be silently forgotten: scanStray below fails on any `_ coretui.X =`
// assertion outside these files, so an unregistered host's guards are
// themselves the error that points here.
type adapter struct {
	Label    string
	Guards   string
	TagPeer  string
	Receiver string
}

var adapters = []adapter{
	{
		Label:    "local --tui host",
		Guards:   "cmd/core-agent/coretui_guards.go",
		TagPeer:  "cmd/core-agent/coretui_enabled.go",
		Receiver: "*coreAgentAdapter",
	},
	{
		Label:    "attach-mode adapter",
		Guards:   "internal/coretuiremote/guards.go",
		TagPeer:  "internal/coretuiremote/adapter.go",
		Receiver: "*Adapter",
	},
}

// skipDirs are trees the stray-guard scan does not walk, on top of the
// dot/underscore rule in skipDir below.
var skipDirs = map[string]bool{
	"node_modules": true,
	"vendor":       true,
	"testdata":     true,
}

// skipDir reports whether the walk should prune a directory. The
// dot/underscore rule is the go tool's own (cmd/go/internal/search):
// a package it will not build is not a place a guard can hide, and
// hardcoding just `.git` would have let `.claude/worktrees/` through.
func skipDir(name string) bool {
	if skipDirs[name] {
		return true
	}
	return strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")
}

func main() {
	printMatrix := flag.Bool("print", false, "print the interface x adapter matrix instead of verifying")
	root := flag.String("repo-root", "", "repository root (default: the working directory)")
	flag.Parse()

	if err := run(*root, *printMatrix); err != nil {
		fmt.Fprintf(os.Stderr, "verify-coretui-guards: %v\n", err)
		os.Exit(1)
	}
}

func run(root string, printMatrix bool) error {
	if root == "" {
		wd, err := os.Getwd()
		if err != nil {
			return err
		}
		root = wd
	}

	up, err := resolveUpstream(root)
	if err != nil {
		return err
	}

	files := make([]*guardFile, 0, len(adapters))
	for _, a := range adapters {
		gf, err := parseGuardFile(root, a)
		if err != nil {
			return err
		}
		files = append(files, gf)
	}

	strays, err := scanStray(root)
	if err != nil {
		return err
	}

	if printMatrix {
		printReport(os.Stdout, up, files)
		return nil
	}

	tagFindings, err := checkConstraints(root, files)
	if err != nil {
		return err
	}

	findings := append(check(up, files, strays), tagFindings...)
	if len(findings) > 0 {
		for _, f := range findings {
			fmt.Fprintf(os.Stderr, "\n%s\n", f)
		}
		return fmt.Errorf("%d finding(s) against core-tui %s", len(findings), up.Version)
	}

	guards, declines := 0, 0
	for _, gf := range files {
		guards += len(gf.Guarded)
		declines += len(gf.Declined)
	}
	fmt.Printf("verify-coretui-guards: OK (core-tui %s, %d exported interfaces, "+
		"%d adapters, %d guards + %d declines, all accounted for)\n",
		up.Version, len(up.Interfaces), len(files), guards, declines)
	return nil
}

// ---------------------------------------------------------------------
// Upstream: what does the pinned core-tui export?
// ---------------------------------------------------------------------

// upstream is the answer to "which capability interfaces exist", at the
// version go.mod resolves to.
type upstream struct {
	Version    string
	Dir        string
	Interfaces map[string]string // name -> "agent.go:28"
}

// goListPkg is the subset of `go list -json` this tool reads.
type goListPkg struct {
	Dir     string
	GoFiles []string
	Module  *goListModule
}

type goListModule struct {
	Path    string
	Version string
	Dir     string
	Replace *goListModule
}

// describe names the module the way the gate should print it. A
// `replace` to a local checkout has an empty Version — that is a
// legitimate way to work on core-tui and this repo together, so it is
// reported by path rather than rejected. Naming it explicitly matters:
// "OK (core-tui v0.22.0, …)" under a replace would be a lie of exactly
// the kind this gate exists to catch.
func (m *goListModule) describe() string {
	switch {
	case m == nil:
		return "(unknown)"
	case m.Replace != nil && m.Replace.Version != "":
		return m.Replace.Version + " (replaced)"
	case m.Replace != nil && m.Replace.Dir != "":
		return "(replaced by " + m.Replace.Dir + ")"
	case m.Version != "":
		return m.Version
	default:
		return "(no version; replaced or a workspace module)"
	}
}

// resolveUpstream asks the go command where the pinned core-tui/tui
// package is and which files are in its build, then parses those files.
//
// Why the go command rather than a hand-built module-cache path: the go
// command is the only thing that knows how go.mod, the build list, any
// `replace`, GOFLAGS and GOMODCACHE combine into a directory — and it
// reports the resolved version back, so the check can name it. Building
// "$(go env GOMODCACHE)/github.com/go-steer/core-tui@$(grep go.mod)"
// here would reimplement module resolution badly and break the first
// time someone points a `replace` at a local checkout.
//
// GoFiles (not the directory listing) is also already filtered by the
// go tool's build constraints and excludes _test.go, so a
// GOOS-conditional file in core-tui cannot skew the roster.
func resolveUpstream(root string) (upstream, error) {
	cmd := exec.Command("go", "list", "-json", coretuiPkg)
	cmd.Dir = root
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return upstream{}, fmt.Errorf(
			"go list %s failed (run `go mod download` if the module cache is cold): %w",
			coretuiPkg, err)
	}

	var pkg goListPkg
	if err := json.Unmarshal(out, &pkg); err != nil {
		return upstream{}, fmt.Errorf("decoding go list output: %w", err)
	}
	if pkg.Dir == "" || pkg.Module == nil {
		return upstream{}, fmt.Errorf(
			"go list %s reported no directory or no module; is %s still a dependency?",
			coretuiPkg, coretuiPkg)
	}

	ifaces, err := exportedInterfaces(pkg.Dir, pkg.GoFiles)
	if err != nil {
		return upstream{}, err
	}
	if len(ifaces) == 0 {
		return upstream{}, fmt.Errorf("no exported interfaces found in %s (%s) — "+
			"that is almost certainly a bug in this checker, not an empty package",
			coretuiPkg, pkg.Dir)
	}
	return upstream{Version: pkg.Module.describe(), Dir: pkg.Dir, Interfaces: ifaces}, nil
}

// exportedInterfaces returns every exported interface type declared in
// the given files, mapped to "file:line".
//
// Parse-only, no type checking: the roster is a list of type
// declarations, and go/parser answers that from the stdlib. Type
// checking core-tui would mean loading its whole dependency graph
// (x/tools' go/packages, a new *direct* module requirement — x/tools is
// not in go.mod today, not even as an indirect) to answer a question
// the AST already answers.
//
// The one shape this misses is an exported alias to an interface
// declared in a DIFFERENT package (`type X = other.Y`), which needs
// import resolution. Same-package aliases are resolved below, including
// an exported alias to an *unexported* interface. core-tui exports no
// type aliases at all as of v0.22.0.
func exportedInterfaces(dir string, files []string) (map[string]string, error) {
	fset := token.NewFileSet()
	out := map[string]string{}
	ifaces := map[string]bool{}    // every interface in the package, exported or not
	aliases := map[string]string{} // exported alias name -> same-package target ident
	pos := map[string]string{}     // exported name -> "file:line"

	for _, name := range files {
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, fmt.Errorf("parsing %s: %w", filepath.Join(dir, name), err)
		}
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, spec := range gd.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				at := fmt.Sprintf("%s:%d", name, fset.Position(ts.Name.Pos()).Line)
				switch t := ts.Type.(type) {
				case *ast.InterfaceType:
					// Unexported interfaces are tracked too, purely as
					// possible alias targets: `type Public = private` is
					// an exported capability whichever way it is spelled.
					ifaces[ts.Name.Name] = true
					if ts.Name.IsExported() {
						out[ts.Name.Name] = at
					}
				case *ast.Ident:
					if ts.Assign.IsValid() && ts.Name.IsExported() {
						aliases[ts.Name.Name] = t.Name
					}
				}
				if ts.Name.IsExported() {
					pos[ts.Name.Name] = at
				}
			}
		}
	}

	// Resolve same-package alias chains (bounded by the alias count, so
	// a cycle — which would not compile upstream anyway — terminates).
	for name, target := range aliases {
		for i := 0; i <= len(aliases); i++ {
			if ifaces[target] {
				out[name] = pos[name]
				break
			}
			next, ok := aliases[target]
			if !ok {
				break
			}
			target = next
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------
// This side: what do the guard files claim?
// ---------------------------------------------------------------------

// declineRE matches the structured decline directive. Go's directive
// convention — `//name:value`, no space after the slashes — is
// deliberate: gofmt leaves such lines alone, and go/ast recognises them
// as directives rather than prose.
//
// The directive carries the machine-readable half of a decline. The
// prose bullet underneath carries the half that matters to a human, and
// bindProse below refuses to let the two drift apart.
var declineRE = regexp.MustCompile(`^//coretui:declined[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]*$`)

type guardFile struct {
	adapter
	Alias      string
	Constraint string
	Guarded    map[string]string
	Declined   map[string]string
	Problems   []string
}

// parseGuardFile reads one adapter's guard file.
//
// It parses the file BY PATH and does not consult build constraints,
// which is load-bearing rather than lazy: cmd/core-agent's guard file
// carries `//go:build !no_tui`, so anything that enumerated packages
// through the build system would see zero guards under `-tags no_tui`
// and report the whole roster as unaccounted for. Reading the source
// makes the answer the same whichever tags the caller happens to have
// set. The constraint is not ignored — it is checked, against TagPeer,
// further down.
func parseGuardFile(root string, a adapter) (*guardFile, error) {
	path := filepath.Join(root, a.Guards)
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", a.Guards, err)
	}

	gf := &guardFile{
		adapter:  a,
		Alias:    coretuiAlias(f),
		Guarded:  map[string]string{},
		Declined: map[string]string{},
	}
	if gf.Alias == "" {
		return nil, fmt.Errorf("%s does not import %s", a.Guards, coretuiPkg)
	}

	for name, line := range guardedIn(f, gf.Alias, fset) {
		gf.Guarded[name] = fmt.Sprintf("%s:%d", a.Guards, line)
	}
	for _, d := range declinedIn(f, fset, a.Guards, gf.Alias) {
		if d.problem != "" {
			gf.Problems = append(gf.Problems, d.problem)
			continue
		}
		gf.Declined[d.name] = fmt.Sprintf("%s:%d", a.Guards, d.line)
	}

	gf.Constraint, err = buildConstraintOf(path)
	if err != nil {
		return nil, err
	}
	return gf, nil
}

// coretuiAlias returns the local name this file imports core-tui under
// (resolved, not assumed: a file that imported it unaliased would name
// the interfaces `tui.X`).
func coretuiAlias(f *ast.File) string {
	for _, imp := range f.Imports {
		if imp.Path == nil || strings.Trim(imp.Path.Value, `"`) != coretuiPkg {
			continue
		}
		if imp.Name != nil {
			return imp.Name.Name
		}
		return filepath.Base(coretuiPkg)
	}
	return ""
}

// guardedIn returns the interface names asserted by `_ <alias>.X = ...`
// value specs, whether they sit in a `var (...)` block or alone.
func guardedIn(f *ast.File, alias string, fset *token.FileSet) map[string]int {
	out := map[string]int{}
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.VAR {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok || len(vs.Names) != 1 || vs.Names[0].Name != "_" {
				continue
			}
			sel, ok := vs.Type.(*ast.SelectorExpr)
			if !ok {
				continue
			}
			if id, ok := sel.X.(*ast.Ident); !ok || id.Name != alias {
				continue
			}
			out[sel.Sel.Name] = fset.Position(sel.Pos()).Line
		}
	}
	return out
}

type decline struct {
	name    string
	line    int
	problem string
}

// declinedIn collects `//coretui:declined X` directives and binds each
// one to the prose that follows it.
//
// The binding is the point. A directive on its own would be a second
// list to keep in sync — exactly the failure this gate exists to
// prevent, relocated into a comment. Requiring the next prose line to
// name `<alias>.X` means renaming the interface in one place and not
// the other is an error, so the directive cannot outlive the reason it
// was written for.
//
// alias is the name the file imports core-tui under, not the literal
// "coretui": the guard half already resolves it, and hardcoding it here
// would make the two halves disagree the first time a host imported the
// package unaliased.
func declinedIn(f *ast.File, fset *token.FileSet, display, alias string) []decline {
	var out []decline
	for _, cg := range f.Comments {
		for i, c := range cg.List {
			m := declineRE.FindStringSubmatch(trimDirective(c.Text))
			if m == nil {
				continue
			}
			d := decline{name: m[1], line: fset.Position(c.Pos()).Line}
			if !bindsProse(cg.List[i+1:], alias, d.name) {
				d.problem = fmt.Sprintf(
					"%s:%d: //coretui:declined %s is not followed by prose naming %s.%s\n\n"+
						"  A decline is a decision, and the decision is the sentence, not the\n"+
						"  directive. Put the reason on the line(s) below it, naming the\n"+
						"  interface, so a rename can't leave the directive pointing at prose\n"+
						"  about something else:\n\n"+
						"      //coretui:declined %s\n"+
						"      //   - %s.%s: <why this host does not implement it>",
					display, d.line, d.name, alias, d.name, d.name, alias, d.name)
			}
			out = append(out, d)
		}
	}
	return out
}

// mentionRE matches `<alias>.<Name>` as a whole selector rather than as
// a substring.
//
// Substring matching is not good enough in either direction, and both
// directions have a live counterexample: with alias "tui", the string
// "coretui.Reloader" contains "tui.Reloader", so prose about a
// different package would satisfy the binding; and a decline for
// `Asker` would be satisfied by a bullet that only ever mentions
// `coretui.AskerV2`. Anchoring on both sides costs one regexp.
func mentionRE(alias, name string) *regexp.Regexp {
	return regexp.MustCompile(`(^|[^\w.])` + regexp.QuoteMeta(alias+"."+name) + `([^\w]|$)`)
}

// trimDirective strips the trailing whitespace declineRE's `$` anchor
// would otherwise reject, including the \r a CRLF checkout leaves on
// the end of every comment.
func trimDirective(text string) string {
	return strings.TrimRight(text, " \t\r")
}

// bindsProse reports whether the first prose line after a directive
// names <alias>.<name>. Stacked directives are skipped over, so three
// declines can share one bullet that names all three.
func bindsProse(rest []*ast.Comment, alias, name string) bool {
	want := mentionRE(alias, name)
	for _, c := range rest {
		text := strings.TrimSpace(strings.TrimPrefix(c.Text, "//"))
		if text == "" {
			continue
		}
		if declineRE.MatchString(trimDirective(c.Text)) {
			continue
		}
		return want.MatchString(c.Text)
	}
	return false
}

// buildConstraintOf returns the normalized `//go:build` expression of a
// file, or "" when it has none.
func buildConstraintOf(path string) (string, error) {
	src, err := os.ReadFile(path) // #nosec G304 -- paths come from the adapter table above
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(src), "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.HasPrefix(line, "package ") {
			break
		}
		if !constraint.IsGoBuild(line) {
			continue
		}
		expr, err := constraint.Parse(line)
		if err != nil {
			return "", fmt.Errorf("%s: %w", path, err)
		}
		return expr.String(), nil
	}
	return "", nil
}

// ---------------------------------------------------------------------
// Stray guards: the property that makes the guard files "the list"
// ---------------------------------------------------------------------

type stray struct {
	file string
	line int
	what string
}

// scanStray finds capability guards and decline directives that live
// anywhere other than the registered guard files.
//
// Without this the whole check is advisory: a guard added beside its
// method still compiles, still catches a method-signature change, and
// makes the interface look implemented to a reader — while this tool,
// which only reads the two registered files, reports it as unaccounted
// for. That would be a false positive nobody could fix without finding
// this scan first. With it, "the guard files are the complete list" is
// a property rather than an aspiration (#812), and registering a third
// host is forced rather than optional.
func scanStray(root string) ([]stray, error) {
	registered := map[string]bool{}
	for _, a := range adapters {
		registered[filepath.FromSlash(a.Guards)] = true
	}

	// Read through an os.Root rather than by absolute path: the walk
	// hands back paths it resolved a moment ago, and reopening one by
	// name is the symlink-TOCTOU shape gosec's G122 is about. Rooting
	// the reads costs nothing here and keeps the scan inside the tree it
	// was pointed at.
	dir, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer func() { _ = dir.Close() }()

	var out []stray
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// path == root is exempt: the repo itself may well sit under a
			// dot-directory (git worktrees here live in .claude/worktrees),
			// and pruning the root would silently scan nothing.
			if path != root && skipDir(d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil || registered[rel] {
			return err
		}
		src, err := readRooted(dir, rel)
		if err != nil {
			return err
		}
		if !strings.Contains(string(src), coretuiPkg) && !strings.Contains(string(src), "coretui:declined") {
			return nil
		}
		out = append(out, strayIn(rel, src)...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].file != out[j].file {
			return out[i].file < out[j].file
		}
		return out[i].line < out[j].line
	})
	return out, nil
}

func readRooted(dir *os.Root, rel string) ([]byte, error) {
	f, err := dir.Open(rel)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(f)
}

func strayIn(rel string, src []byte) []stray {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, rel, src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		// Not parseable as Go: the compiler will say so far more
		// usefully than this tool would.
		return nil
	}
	var out []stray
	alias := coretuiAlias(f)
	if alias != "" {
		for name, line := range guardedIn(f, alias, fset) {
			out = append(out, stray{rel, line, "guard for coretui." + name})
		}
	} else {
		// No core-tui import, so there is no alias to bind prose against.
		// Only the directive scan below applies, and it does not use one
		// beyond composing the (here discarded) problem message.
		alias = filepath.Base(coretuiPkg)
	}
	for _, d := range declinedIn(f, fset, rel, alias) {
		out = append(out, stray{rel, d.line, "//coretui:declined " + d.name})
	}
	return out
}

// ---------------------------------------------------------------------
// The check
// ---------------------------------------------------------------------

// check compares the upstream roster against each adapter's
// guarded-union-declined set and returns one message per finding. It
// takes everything it needs as arguments so the tests can drive it with
// a synthetic roster and synthetic guard files, no module cache
// involved.
func check(up upstream, files []*guardFile, strays []stray) []string {
	var out []string
	for _, gf := range files {
		out = append(out, gf.Problems...)
		out = append(out, unaccounted(up, gf)...)
		out = append(out, staleDeclines(up, gf)...)
		out = append(out, contradictions(gf)...)
		out = append(out, unknownGuards(up, gf)...)
	}
	for _, s := range strays {
		out = append(out, fmt.Sprintf(
			"%s:%d: %s lives outside the registered guard files\n\n"+
				"  The guard files are meant to be THE list — a set you have to grep\n"+
				"  for is not a set anyone audits, and this checker only reads the\n"+
				"  registered ones, so a guard out here is invisible to it.\n\n"+
				"  Move it into one of:\n%s\n"+
				"  or, if this is a new core-tui host, register it in the `adapters`\n"+
				"  table in dev/coretui-guard-check/main.go.",
			s.file, s.line, s.what, guardFileList()))
	}
	sort.Strings(out)
	return out
}

func guardFileList() string {
	var b strings.Builder
	for _, a := range adapters {
		fmt.Fprintf(&b, "      %s (%s)\n", a.Guards, a.Label)
	}
	return b.String()
}

func unaccounted(up upstream, gf *guardFile) []string {
	var out []string
	for _, name := range sortedKeys(up.Interfaces) {
		if _, ok := gf.Guarded[name]; ok {
			continue
		}
		if _, ok := gf.Declined[name]; ok {
			continue
		}
		out = append(out, fmt.Sprintf(
			"%s: coretui.%s is neither guarded nor declined (%s)\n\n"+
				"  core-tui %s declares it at %s.\n\n"+
				"  Every exported core-tui interface has to be accounted for here,\n"+
				"  because an absence that nobody wrote down is indistinguishable from\n"+
				"  an oversight — and core-tui feature-detects by type assertion, so an\n"+
				"  unimplemented capability fails silently at runtime, not at build time.\n\n"+
				"  Implement it and add a guard to the var block:\n\n"+
				"      _ %s.%s = (%s)(nil) // <the methods it needs>\n\n"+
				"  or record a decline in the trailing comment, with the reason:\n\n"+
				"      //coretui:declined %s\n"+
				"      //   - %s.%s: <why this host does not implement it>",
			gf.Guards, name, gf.Label, up.Version, up.Interfaces[name],
			gf.Alias, name, gf.Receiver, name, gf.Alias, name))
	}
	return out
}

func staleDeclines(up upstream, gf *guardFile) []string {
	var out []string
	for _, name := range sortedKeys(gf.Declined) {
		if _, ok := up.Interfaces[name]; ok {
			continue
		}
		out = append(out, fmt.Sprintf(
			"%s: //coretui:declined %s names an interface core-tui %s does not export\n\n"+
				"  Declared at %s.\n\n"+
				"  Either the interface was renamed or removed upstream, or the\n"+
				"  directive has a typo. A decline for something that no longer exists\n"+
				"  reads to the next person as a live decision about a live capability.\n"+
				"  Delete it, or point it at the new name.",
			gf.Guards, name, up.Version, gf.Declined[name]))
	}
	return out
}

func contradictions(gf *guardFile) []string {
	var out []string
	for _, name := range sortedKeys(gf.Declined) {
		if _, ok := gf.Guarded[name]; !ok {
			continue
		}
		out = append(out, fmt.Sprintf(
			"%s: coretui.%s is both guarded (%s) and declined (%s)\n\n"+
				"  The guard is the stronger claim — it does not compile unless the\n"+
				"  capability really is implemented — so the decline is stale prose.\n"+
				"  This is what #811 had to remember by hand when RemoteInterrupter\n"+
				"  moved out of the declined section and into the list. Delete the\n"+
				"  directive and its bullet.",
			gf.Guards, name, gf.Guarded[name], gf.Declined[name]))
	}
	return out
}

func unknownGuards(up upstream, gf *guardFile) []string {
	var out []string
	for _, name := range sortedKeys(gf.Guarded) {
		if _, ok := up.Interfaces[name]; ok {
			continue
		}
		out = append(out, fmt.Sprintf(
			"%s: guard for coretui.%s, which core-tui %s does not export (%s)\n\n"+
				"  Normally the build catches this first. If you are seeing it, the\n"+
				"  guard file and the pinned core-tui disagree in a way `go build` did\n"+
				"  not reach — check the build constraint on the guard file.",
			gf.Guards, name, up.Version, gf.Guarded[name]))
	}
	return out
}

// checkConstraints compares each guard file's `//go:build` expression
// against the file whose types it asserts about.
//
// A guard file that loses the tag fails to compile in the slim
// (`-tags no_tui`) image; one that widens it drops out of the default
// build along with the adapter it guards — silently, which is the exact
// failure mode the guards exist to prevent. The head comment on
// cmd/core-agent/coretui_guards.go says the tags MUST match; this is
// what makes that sentence enforceable.
func checkConstraints(root string, files []*guardFile) ([]string, error) {
	var out []string
	for _, gf := range files {
		if gf.TagPeer == "" {
			continue
		}
		peer, err := buildConstraintOf(filepath.Join(root, gf.TagPeer))
		if err != nil {
			return nil, err
		}
		if peer == gf.Constraint {
			continue
		}
		out = append(out, fmt.Sprintf(
			"%s: build constraint does not match %s\n\n"+
				"      %-*s %s\n      %-*s %s\n\n"+
				"  The guard file has to be in exactly the builds its adapter is in.\n"+
				"  Narrower and it fails to compile where the adapter still exists;\n"+
				"  wider and the guards leave the default build along with it, which is\n"+
				"  as silent as the bug they catch.",
			gf.Guards, gf.TagPeer,
			len(gf.TagPeer), gf.Guards, orNone(gf.Constraint),
			len(gf.TagPeer), gf.TagPeer, orNone(peer)))
	}
	return out, nil
}

func orNone(s string) string {
	if s == "" {
		return "(no build constraint)"
	}
	return "//go:build " + s
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// printReport writes the interface x adapter matrix. Not part of the
// gate; it is what makes the answer auditable by eye when the gate
// fires or when someone is picking up a core-tui bump.
func printReport(w io.Writer, up upstream, files []*guardFile) {
	fmt.Fprintf(w, "core-tui %s (%s)\n%d exported interfaces\n\n", up.Version, up.Dir, len(up.Interfaces))
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprint(tw, "INTERFACE\tDECLARED AT")
	for _, gf := range files {
		fmt.Fprintf(tw, "\t%s", gf.Label)
	}
	fmt.Fprintln(tw)
	for _, name := range sortedKeys(up.Interfaces) {
		fmt.Fprintf(tw, "%s\t%s", name, up.Interfaces[name])
		for _, gf := range files {
			fmt.Fprintf(tw, "\t%s", statusOf(gf, name))
		}
		fmt.Fprintln(tw)
	}
	_ = tw.Flush()
}

func statusOf(gf *guardFile, name string) string {
	_, guarded := gf.Guarded[name]
	_, declined := gf.Declined[name]
	switch {
	case guarded && declined:
		return "CONTRADICTION"
	case guarded:
		return "guarded"
	case declined:
		return "declined"
	default:
		return "UNACCOUNTED"
	}
}
