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

// Command corevcs-pin-check verifies that the go-steer/core-vcs pin in go.mod
// resolves cleanly, that every exported interface in github.com/go-steer/core-vcs/vcs
// and every provider package in github.com/go-steer/core-vcs/providers/* is
// accounted for by a compile-time `var _` guard in pkg/tools/vcs/guards.go, and
// that no `var _` guard against corevcs lives outside pkg/tools/vcs/guards.go (#1490).
//
// Modes:
//
//	corevcs-pin-check           # verify; non-zero exit on any finding
//	corevcs-pin-check --print   # dump the interface + provider guard table
//
// Run it through dev/tools/verify-core-vcs-pin, which is what
// dev/ci/presubmits/verify-core-vcs-pin and dev/tools/ci call.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
)

const (
	corevcsModule       = "github.com/go-steer/core-vcs"
	corevcsPkg          = "github.com/go-steer/core-vcs/vcs"
	corevcsProvidersPkg = "github.com/go-steer/core-vcs/providers/..."
	guardFileRel        = "pkg/tools/vcs/guards.go"
)

func main() {
	printTable := flag.Bool("print", false, "print the core-vcs guard matrix instead of verifying")
	root := flag.String("repo-root", "", "repository root (default: working directory)")
	flag.Parse()

	if err := run(*root, *printTable); err != nil {
		fmt.Fprintf(os.Stderr, "verify-core-vcs-pin: %v\n", err)
		os.Exit(1)
	}
}

type upstreamInfo struct {
	Version    string
	Interfaces map[string]string // exported interface name -> file:line
	Providers  []string          // provider package suffixes, e.g. "git", "github", "gitlab"
}

type guardEntry struct {
	Interface string // e.g. "Provider", "Credential"
	RHS       string // e.g. "(*github.Provider)(nil)"
	Line      int
}

func run(root string, printTable bool) error {
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

	entries, err := parseGuards(filepath.Join(root, guardFileRel))
	if err != nil {
		return err
	}

	strays, err := scanStrayGuards(root)
	if err != nil {
		return err
	}

	if printTable {
		printReport(os.Stdout, up, entries)
		return nil
	}

	findings := verify(up, entries, strays)
	if len(findings) > 0 {
		for _, f := range findings {
			fmt.Fprintf(os.Stderr, "\n%s\n", f)
		}
		return fmt.Errorf("%d finding(s) against %s %s", len(findings), corevcsModule, up.Version)
	}

	fmt.Printf("verify-core-vcs-pin: OK (%s %s, %d exported interfaces, %d providers, %d compile-time guards in %s)\n",
		corevcsModule, up.Version, len(up.Interfaces), len(up.Providers), len(entries), guardFileRel)
	return nil
}

type goListPkg struct {
	ImportPath string
	Dir        string
	GoFiles    []string
	Module     *struct {
		Path    string
		Version string
	}
}

func resolveUpstream(root string) (*upstreamInfo, error) {
	cmd := exec.Command("go", "list", "-json", corevcsPkg)
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list %s: %w", corevcsPkg, err)
	}
	var pkg goListPkg
	if err := json.Unmarshal(out, &pkg); err != nil {
		return nil, fmt.Errorf("decode go list %s: %w", corevcsPkg, err)
	}
	if pkg.Module == nil || pkg.Module.Version == "" {
		return nil, fmt.Errorf("%s has no resolved module version in go.mod", corevcsPkg)
	}

	ifaces, err := extractExportedInterfaces(pkg.Dir, pkg.GoFiles)
	if err != nil {
		return nil, err
	}

	provCmd := exec.Command("go", "list", corevcsProvidersPkg)
	provCmd.Dir = root
	provOut, err := provCmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list %s: %w", corevcsProvidersPkg, err)
	}
	prefix := corevcsModule + "/providers/"
	var providers []string
	for _, line := range strings.Split(strings.TrimSpace(string(provOut)), "\n") {
		line = strings.TrimSpace(line)
		if after, ok := strings.CutPrefix(line, prefix); ok && after != "" && !strings.Contains(after, "/") {
			providers = append(providers, after)
		}
	}
	sort.Strings(providers)

	return &upstreamInfo{
		Version:    pkg.Module.Version,
		Interfaces: ifaces,
		Providers:  providers,
	}, nil
}

func extractExportedInterfaces(dir string, files []string) (map[string]string, error) {
	fset := token.NewFileSet()
	out := make(map[string]string)
	for _, name := range files {
		full := filepath.Join(dir, name)
		f, err := parser.ParseFile(fset, full, nil, 0)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", full, err)
		}
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, spec := range gd.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok || !ts.Name.IsExported() {
					continue
				}
				if _, isIface := ts.Type.(*ast.InterfaceType); isIface {
					pos := fset.Position(ts.Pos())
					out[ts.Name.Name] = fmt.Sprintf("%s:%d", name, pos.Line)
				}
			}
		}
	}
	return out, nil
}

func parseGuards(path string) ([]guardEntry, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", guardFileRel, err)
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, 0)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", guardFileRel, err)
	}

	alias := "vcs"
	for _, imp := range f.Imports {
		if strings.Trim(imp.Path.Value, `"`) == corevcsPkg && imp.Name != nil {
			alias = imp.Name.Name
		}
	}

	var entries []guardEntry
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.VAR {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok || len(vs.Names) != 1 || vs.Names[0].Name != "_" || len(vs.Values) != 1 {
				continue
			}
			sel, ok := vs.Type.(*ast.SelectorExpr)
			if !ok {
				continue
			}
			ident, ok := sel.X.(*ast.Ident)
			if !ok || ident.Name != alias {
				continue
			}
			val := vs.Values[0]
			start := fset.Position(val.Pos()).Offset
			end := fset.Position(val.End()).Offset
			rhs := ""
			if start >= 0 && end <= len(src) && start < end {
				rhs = string(src[start:end])
			}
			entries = append(entries, guardEntry{
				Interface: sel.Sel.Name,
				RHS:       rhs,
				Line:      fset.Position(vs.Pos()).Line,
			})
		}
	}
	return entries, nil
}

func scanStrayGuards(root string) ([]string, error) {
	type candidate struct {
		path string
		rel  string
	}
	var candidates []candidate
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if name == "node_modules" || name == "vendor" || name == "testdata" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
				if path != root {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == guardFileRel {
			return nil
		}
		candidates = append(candidates, candidate{path: path, rel: rel})
		return nil
	})
	if err != nil {
		return nil, err
	}
	var strays []string
	for _, c := range candidates {
		data, err := os.ReadFile(c.path)
		if err != nil {
			return nil, err
		}
		if !strings.Contains(string(data), corevcsPkg) {
			continue
		}
		entries, err := parseGuards(c.path)
		if err == nil && len(entries) > 0 {
			for _, e := range entries {
				strays = append(strays, fmt.Sprintf("%s:%d: stray core-vcs guard for %s (move to %s)", c.rel, e.Line, e.Interface, guardFileRel))
			}
		}
	}
	return strays, nil
}

func verify(up *upstreamInfo, entries []guardEntry, strays []string) []string {
	var findings []string
	findings = append(findings, strays...)

	guardedIfaces := make(map[string]int)
	var providerGuards []string
	for _, e := range entries {
		guardedIfaces[e.Interface]++
		if _, exists := up.Interfaces[e.Interface]; !exists {
			findings = append(findings, fmt.Sprintf("%s:%d: guards corevcs.%s, which is not an exported interface in %s@%s",
				guardFileRel, e.Line, e.Interface, corevcsPkg, up.Version))
		}
		if e.Interface == "Provider" {
			providerGuards = append(providerGuards, e.RHS)
		}
	}

	var ifaceNames []string
	for name := range up.Interfaces {
		ifaceNames = append(ifaceNames, name)
	}
	sort.Strings(ifaceNames)
	for _, name := range ifaceNames {
		if guardedIfaces[name] == 0 {
			findings = append(findings, fmt.Sprintf("%s: exported interface corevcs.%s (%s) has no compile-time guard in %s",
				corevcsPkg, name, up.Interfaces[name], guardFileRel))
		}
	}

	for _, prov := range up.Providers {
		needle := prov + ".Provider"
		matched := false
		for _, rhs := range providerGuards {
			if strings.Contains(rhs, needle) {
				matched = true
				break
			}
		}
		if !matched {
			findings = append(findings, fmt.Sprintf("%s/providers/%s: provider package has no `_ corevcs.Provider = (*%s.Provider)(nil)` guard in %s",
				corevcsModule, prov, prov, guardFileRel))
		}
	}

	return findings
}

func printReport(w io.Writer, up *upstreamInfo, entries []guardEntry) {
	fmt.Fprintf(w, "Module: %s %s\n\n", corevcsModule, up.Version)
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "INTERFACE\tUPSTREAM\tGUARD IN "+guardFileRel)
	var ifaceNames []string
	for name := range up.Interfaces {
		ifaceNames = append(ifaceNames, name)
	}
	sort.Strings(ifaceNames)
	for _, name := range ifaceNames {
		var guards []string
		for _, e := range entries {
			if e.Interface == name {
				guards = append(guards, e.RHS)
			}
		}
		status := "UNACCOUNTED"
		if len(guards) > 0 {
			status = strings.Join(guards, ", ")
		}
		fmt.Fprintf(tw, "corevcs.%s\t%s\t%s\n", name, up.Interfaces[name], status)
	}
	_ = tw.Flush()
}
