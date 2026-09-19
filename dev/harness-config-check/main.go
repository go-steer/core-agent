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

// Command harness-config-check fails when a harness script runs the
// core-agent binary without pinning its config with -c.
//
// The hazard it closes is #1116/D3. config.Find walks *up* from the
// process cwd and returns the first .agents/ it meets, so a .agents/ at
// the repo root — committed, or just left behind by a local run — is
// inherited by every unpinned invocation anywhere under the checkout.
// The scripts keep passing, against an agent nobody configured: a
// different model, a different permission mode, a tools.disable list.
//
// The pin has to be -c, not --agents-dir. loadConfig() reads config.json
// by walking up from cwd BEFORE resolveAgentsDir() applies the flag, so
// --agents-dir moves the skills, MCP servers and sessions and leaves the
// model, permissions and budgets behind. cmd/core-agent's own
// splitTreeWarning() exists to warn about exactly that split.
package main

import (
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// roots are the trees whose scripts drive a local binary. examples/ is
// out of scope: each recipe ships its own .agents/ and is governed by
// examples/gke-platform-agent/recipe_test.go.
var roots = []string{
	"dev/smoke",
	"dev/uat",
	"dev/tools",
	"dev/ci",
}

func main() {
	print := flag.Bool("print", false, "dump every recognised invocation and its pin, then exit 0")
	flag.Parse()

	var invs []Invocation
	for _, root := range roots {
		found, err := scanTree(root)
		if err != nil {
			fmt.Fprintf(os.Stderr, "harness-config-check: %v\n", err)
			os.Exit(2)
		}
		invs = append(invs, found...)
	}

	if *print {
		for _, in := range invs {
			state := "UNPINNED"
			switch {
			case in.Exempt != "":
				state = "exempt"
			case in.Pinned:
				state = "pinned"
			}
			fmt.Printf("%-8s %s:%d  %s %s\n", state, in.File, in.Line, in.Text, in.Args)
		}
		fmt.Printf("\n%d invocation(s) across %d tree(s)\n", len(invs), len(roots))
		return
	}

	var bad []Invocation
	for _, in := range invs {
		if !in.Pinned && in.Exempt == "" {
			bad = append(bad, in)
		}
	}
	if len(bad) == 0 {
		fmt.Printf("harness-config-check: %d invocation(s), all pinned\n", len(invs))
		return
	}

	fmt.Fprintf(os.Stderr, "harness-config-check: %d unpinned core-agent invocation(s)\n\n", len(bad))
	for _, in := range bad {
		fmt.Fprintf(os.Stderr, "  %s:%d: %s %s\n", in.File, in.Line, in.Text, in.Args)
	}
	fmt.Fprint(os.Stderr, `
Each of these runs the agent with whatever .agents/config.json happens to
be above its cwd — including one at the repo root, committed or left over
from a local run. Pass -c <config.json> so the script says which agent it
is testing.

--agents-dir alone does NOT fix this: the config is loaded by walking up
from cwd before the flag is applied, so the flag moves the skills and MCP
servers and leaves the model, permissions and budgets behind.

If the script wants pristine defaults, point -c at a config that says
{"version": 1} under $TMPDIR. If it builds its own .agents/, point -c at
the config.json it wrote.
`)
	os.Exit(1)
}

func scanTree(root string) ([]Invocation, error) {
	// Read through an os.Root rather than by the path the walk handed
	// back: reopening a path by name a moment after resolving it is the
	// symlink-TOCTOU shape gosec's G122 is about, and rooting the reads
	// keeps the scan inside the tree it was pointed at. The same shape as
	// dev/coretui-guard-check's scanStray, for the same reason.
	dir, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer func() { _ = dir.Close() }()

	var out []Invocation
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		// A read that fails is an error, never a skip. This check fails
		// when it finds something, so a file it cannot see is a file it
		// cannot clear.
		src, err := readRooted(dir, rel)
		if err != nil {
			return err
		}
		if !isShell(p, src) {
			return nil
		}
		out = append(out, Scan(p, string(src))...)
		return nil
	})
	if err != nil {
		return nil, err
	}
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

// isShell reports whether a file is a shell script — by extension, or
// by shebang for the extensionless tools under dev/tools and
// dev/ci/presubmits.
func isShell(p string, src []byte) bool {
	if strings.HasSuffix(p, ".sh") || strings.HasSuffix(p, ".bash") {
		return true
	}
	first := src
	if k := len(first); k > 64 {
		first = first[:64]
	}
	line := string(first)
	if k := strings.IndexByte(line, '\n'); k >= 0 {
		line = line[:k]
	}
	return strings.HasPrefix(line, "#!") && (strings.Contains(line, "bash") || strings.Contains(line, "/sh"))
}
