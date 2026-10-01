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

package testutil_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/go-steer/core-agent/v2/internal/testutil"
)

// plainGateChecks are the Check* forms that hold no call arguments. In
// ModeAuto a call through one never reaches the approver (#1175
// decision 3), which is safe — it escalates to a person — but silent:
// an operator who made a tool approver-eligible sees it prompt anyway
// and nothing says why. So a model's tool call must use the WithArgs
// sibling, and this sweep is what notices one that does not.
var plainGateChecks = map[string]bool{
	"CheckBash":             true,
	"CheckFileWrite":        true,
	"CheckGeneric":          true,
	"CheckToolCall":         true,
	"CheckReadOnlyToolCall": true,
}

// plainGateCallers are the non-test call sites that keep a plain form on
// purpose, keyed file:method, with how many such calls the file makes.
// A census, not a floor: the count must match exactly, so a second
// plain call added beside an exempt one fails, and so does an entry
// whose call is gone.
var plainGateCallers = map[string]int{
	// Hooks are operator-authored config, not a model's tool call, so
	// there is no call for an approver to judge; in auto they escalate.
	"pkg/hooks/dispatcher.go:CheckBash": 1,
	// A host calling the gate for its own action, not a model's.
	"examples/compose-multi-session/main.go:CheckBash": 1,
}

// This is a violation scanner, so the fatal direction is missing a call:
// that passes silently forever. It parses Go rather than matching text,
// so a comment or string that names a method is never mistaken for a
// call, and a real call is never hidden by one. It errs toward
// over-reporting: any selector call with one of these names counts,
// whatever the receiver's type.
func TestModelCallSitesPassArgsToTheGate(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	plain, withArgs, err := gateCallSites(root)
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	if withArgs == 0 {
		t.Fatalf("found no WithArgs gate call under %s: the sweep would pass vacuously", root)
	}
	seen := map[string][]int{}
	for _, site := range plain {
		key := site.file + ":" + site.method
		if _, ok := plainGateCallers[key]; ok {
			seen[key] = append(seen[key], site.line)
			continue
		}
		t.Errorf("%s:%d calls %s, which holds no call arguments, so ModeAuto's approver can never judge this call (#1175). Use %sWithArgs and pass the tool's decoded input, or add the site to plainGateCallers with the reason it is not a model's call", site.file, site.line, site.method, site.method)
	}
	keys := make([]string, 0, len(plainGateCallers))
	for key := range plainGateCallers {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if want, got := plainGateCallers[key], len(seen[key]); got != want {
			t.Errorf("plainGateCallers expects %d plain call(s) at %s, found %d (lines %v): a new one needs its own reason, a removed one its entry updated", want, key, got, seen[key])
		}
	}
}

type gateCallSite struct {
	file, method string
	line         int
}

// gateCallSites parses every non-test Go file under root outside
// pkg/permissions and returns its plain Check* calls and the number of
// WithArgs calls.
func gateCallSites(root string) (plain []gateCallSite, withArgs int, err error) {
	fset := token.NewFileSet()
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if testutil.PruneWalkDir(root, path, d.Name()) || rel == "pkg/permissions" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") {
			return nil
		}
		f, parseErr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if parseErr != nil {
			return parseErr
		}
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			name := sel.Sel.Name
			switch {
			case plainGateChecks[name]:
				plain = append(plain, gateCallSite{file: rel, method: name, line: fset.Position(sel.Pos()).Line})
			case plainGateChecks[strings.TrimSuffix(name, "WithArgs")] && strings.HasSuffix(name, "WithArgs"):
				withArgs++
			}
			return true
		})
		return nil
	})
	return plain, withArgs, err
}
