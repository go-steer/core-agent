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

package testutil

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// Complexity ratchet ceilings (#1371).
//
// dev/tools/.golangci.yml applies a single repo-wide funlen ceiling, which
// means a 254-line UAT tool under dev/uat/scheduled-monitor would otherwise
// set the line ceiling for library code under pkg/. These constants enforce:
//  1. Separate funlen line and statement ceilings for pkg/ vs dev/.
//  2. A ratchet on the *count* of non-test functions in pkg/ over 150 lines,
//     not only the single worst function.
//  3. A hard cap on cmd/core-agent's `run()` (lines, statements, and
//     parameters) after #685 decomposed it and removed its .golangci.yml
//     exemption.
//
// Like .golangci.yml, these numbers only go down.
const (
	maxPkgFuncLines        = 229 // pkg/agent/background/spawn.go (*Manager).launch
	maxPkgTestFuncLines    = 208 // pkg/agent/run_facade_test.go TestWithMaxSteps_EnforcesPerTurnCapAndFinalReport
	maxPkgFuncStatements   = 142 // pkg/config/config.go (*Config).Validate
	maxPkgFuncsOver150Line = 8   // non-test functions in pkg/ strictly > 150 lines
	pkgTailLineThreshold   = 150

	maxDevFuncLines      = 254 // dev/uat/scheduled-monitor/main.go run
	maxDevFuncStatements = 114 // dev/uat/scheduled-monitor/main.go run

	maxCmdRunLines      = 12 // cmd/core-agent/main.go run (#685)
	maxCmdRunStatements = 8  // cmd/core-agent/main.go run (#685)
	maxCmdRunParams     = 1  // cmd/core-agent/main.go run (#685)
)

// TestComplexityRatchet_PkgAndDevThresholds enforces separate funlen line and
// statement ceilings for pkg/ and dev/, plus the count ratchet on pkg/
// functions over 150 lines (#1371).
func TestComplexityRatchet_PkgAndDevThresholds(t *testing.T) {
	t.Parallel()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root, err := RepoRoot(thisFile)
	if err != nil {
		t.Fatal(err)
	}

	var pkgOver150 []string
	var maxSeenPkgLines, maxSeenPkgTestLines, maxSeenPkgStmts int
	walkGoFuncs(t, filepath.Join(root, "pkg"), root, func(rel string, name string, isTest bool, lines, stmts int) {
		lineCap := maxPkgFuncLines
		if isTest {
			lineCap = maxPkgTestFuncLines
			if lines > maxSeenPkgTestLines {
				maxSeenPkgTestLines = lines
			}
		} else if lines > maxSeenPkgLines {
			maxSeenPkgLines = lines
		}
		if stmts > maxSeenPkgStmts {
			maxSeenPkgStmts = stmts
		}
		if lines > lineCap {
			t.Errorf("%s: func %s has %d non-comment lines, exceeding pkg/ ceiling %d", rel, name, lines, lineCap)
		}
		if stmts > maxPkgFuncStatements {
			t.Errorf("%s: func %s has %d statements, exceeding pkg/ ceiling %d", rel, name, stmts, maxPkgFuncStatements)
		}
		if !isTest && lines > pkgTailLineThreshold {
			pkgOver150 = append(pkgOver150, rel+":"+name)
		}
	})
	if len(pkgOver150) > maxPkgFuncsOver150Line {
		t.Errorf("pkg/ has %d non-test functions over %d non-comment lines (ratchet ceiling %d): %v",
			len(pkgOver150), pkgTailLineThreshold, maxPkgFuncsOver150Line, pkgOver150)
	}
	if maxSeenPkgLines != maxPkgFuncLines || maxSeenPkgStmts != maxPkgFuncStatements {
		t.Logf("pkg/ maximums: lines=%d (cap %d), testLines=%d (cap %d), stmts=%d (cap %d), >150=%d (cap %d)",
			maxSeenPkgLines, maxPkgFuncLines, maxSeenPkgTestLines, maxPkgTestFuncLines, maxSeenPkgStmts, maxPkgFuncStatements, len(pkgOver150), maxPkgFuncsOver150Line)
	}

	walkGoFuncs(t, filepath.Join(root, "dev"), root, func(rel string, name string, _ bool, lines, stmts int) {
		if lines > maxDevFuncLines {
			t.Errorf("%s: func %s has %d non-comment lines, exceeding dev/ ceiling %d", rel, name, lines, maxDevFuncLines)
		}
		if stmts > maxDevFuncStatements {
			t.Errorf("%s: func %s has %d statements, exceeding dev/ ceiling %d", rel, name, stmts, maxDevFuncStatements)
		}
	})
}

// TestComplexityRatchet_CmdRunCap caps cmd/core-agent's `run` at its current
// lines, statements, and parameter count after #685 decomposed it and removed
// its funlen/gocognit exemption from dev/tools/.golangci.yml (#1371, #685).
func TestComplexityRatchet_CmdRunCap(t *testing.T) {
	t.Parallel()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root, err := RepoRoot(thisFile)
	if err != nil {
		t.Fatal(err)
	}

	mainPath := filepath.Join(root, "cmd", "core-agent", "main.go")
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, mainPath, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", mainPath, err)
	}
	cmap := ast.NewCommentMap(fset, file, file.Comments)

	var found bool
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv != nil || fn.Name.Name != "run" {
			continue
		}
		found = true
		lines, stmts := funcLinesAndStatements(fset, cmap, fn)
		params := countFuncParams(fn)
		if lines > maxCmdRunLines {
			t.Errorf("cmd/core-agent/main.go: func run grew to %d non-comment lines (cap %d; extract to pkg/compose or a named helper, see #685)", lines, maxCmdRunLines)
		}
		if stmts > maxCmdRunStatements {
			t.Errorf("cmd/core-agent/main.go: func run grew to %d statements (cap %d; extract to pkg/compose or a named helper, see #685)", stmts, maxCmdRunStatements)
		}
		if params > maxCmdRunParams {
			t.Errorf("cmd/core-agent/main.go: func run grew to %d parameters (cap %d; bundle options or extract to pkg/compose, see #685)", params, maxCmdRunParams)
		}
	}
	if !found {
		t.Fatalf("cmd/core-agent/main.go: func run not found")
	}
}

// TestGolangCILintConfig_NoStaleExcludedPaths verifies that every `- path:`
// exclusion rule in dev/tools/.golangci.yml matches at least one existing
// `.go` file in the repository and that removed directories like `internal/tui`
// are not referenced (#1371).
func TestGolangCILintConfig_NoStaleExcludedPaths(t *testing.T) {
	t.Parallel()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root, err := RepoRoot(thisFile)
	if err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(root, "dev", "tools", ".golangci.yml")
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read %s: %v", cfgPath, err)
	}
	text := string(raw)
	if strings.Contains(text, "internal/tui") {
		t.Errorf("dev/tools/.golangci.yml still references removed directory internal/tui")
	}

	var goFiles []string
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if strings.HasPrefix(d.Name(), ".") || d.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".go") {
			if rel, relErr := filepath.Rel(root, path); relErr == nil {
				goFiles = append(goFiles, filepath.ToSlash(rel))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk repo: %v", err)
	}

	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "- path:") {
			continue
		}
		pattern := strings.TrimSpace(strings.TrimPrefix(trimmed, "- path:"))
		re, err := regexp.Compile(pattern)
		if err != nil {
			t.Errorf("dev/tools/.golangci.yml: invalid path regex %q: %v", pattern, err)
			continue
		}
		matched := false
		for _, f := range goFiles {
			if re.MatchString(f) {
				matched = true
				break
			}
		}
		if !matched {
			t.Errorf("dev/tools/.golangci.yml: exclusion `- path: %s` matches zero .go files in repository", pattern)
		}
	}
}

func walkGoFuncs(t *testing.T, dir, root string, visit func(rel, name string, isTest bool, lines, stmts int)) {
	t.Helper()
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if d.Name() == "testdata" || strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		isTest := strings.HasSuffix(path, "_test.go")
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		cmap := ast.NewCommentMap(fset, file, file.Comments)
		rel, err := filepath.Rel(root, path)
		if err != nil {
			rel = path
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			lines, stmts := funcLinesAndStatements(fset, cmap, fn)
			visit(rel, fn.Name.Name, isTest, lines, stmts)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
}

// funcLinesAndStatements computes funlen-identical non-comment body lines and
// statement count for fn, matching github.com/ultraware/funlen@v0.2.0.
func funcLinesAndStatements(fset *token.FileSet, cmap ast.CommentMap, fn *ast.FuncDecl) (int, int) {
	if fn.Body == nil {
		return 0, 0
	}
	startLine := fset.Position(fn.Pos()).Line
	endLine := fset.Position(fn.End()).Line
	if endLine <= startLine {
		return 0, countFunlenStatements(fn.Body.List)
	}
	totalLines := endLine - startLine - 1
	commentLines := 0
	for _, cg := range cmap.Filter(fn).Comments() {
		cPos := fset.Position(cg.Pos()).Line
		cEnd := fset.Position(cg.End()).Line
		if cPos > startLine && cEnd < endLine {
			commentLines += len(cg.List)
		}
	}
	lines := totalLines - commentLines
	if lines < 0 {
		lines = 0
	}
	return lines, countFunlenStatements(fn.Body.List)
}

// countFunlenStatements matches github.com/ultraware/funlen@v0.2.0's parseStmts.
func countFunlenStatements(stmts []ast.Stmt) int {
	total := 0
	for _, v := range stmts {
		total++
		switch stmt := v.(type) {
		case *ast.BlockStmt:
			total += countFunlenStatements(stmt.List) - 1
		case *ast.ForStmt:
			total += countFunlenStatements(stmt.Body.List)
		case *ast.RangeStmt:
			total += countFunlenStatements(stmt.Body.List)
		case *ast.IfStmt:
			total += countFunlenStatements(stmt.Body.List)
		case *ast.SwitchStmt:
			total += countFunlenStatements(stmt.Body.List)
		case *ast.TypeSwitchStmt:
			total += countFunlenStatements(stmt.Body.List)
		case *ast.SelectStmt:
			total += countFunlenStatements(stmt.Body.List)
		case *ast.CaseClause:
			total += countFunlenStatements(stmt.Body)
		case *ast.AssignStmt:
			if len(stmt.Rhs) > 0 {
				total += countInlineFuncStatements(stmt.Rhs[0])
			}
		case *ast.GoStmt:
			total += countInlineFuncStatements(stmt.Call.Fun)
		case *ast.DeferStmt:
			total += countInlineFuncStatements(stmt.Call.Fun)
		}
	}
	return total
}

func countInlineFuncStatements(n ast.Node) int {
	if fn, ok := n.(*ast.FuncLit); ok && fn.Body != nil {
		return countFunlenStatements(fn.Body.List)
	}
	return 0
}

func countFuncParams(fn *ast.FuncDecl) int {
	if fn.Type == nil || fn.Type.Params == nil {
		return 0
	}
	total := 0
	for _, field := range fn.Type.Params.List {
		if len(field.Names) == 0 {
			total++
		} else {
			total += len(field.Names)
		}
	}
	return total
}
