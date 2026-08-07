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

// Resolution helper for external content roots (config content_roots +
// --agents-content-dir). Split out of main.go so the merge/resolve rules
// can be tested without standing up a full run(). See
// docs/external-content-root-design.md.

package main

import (
	"path/filepath"
	"strings"
)

// resolveContentRoots merges the config content_roots list with the
// repeatable --agents-content-dir flag values and resolves each into an
// absolute directory to hand the instruction/skills loaders as an additional
// trusted scope.
//
// Ordering is config-first, CLI-after: config content_roots keep their listed
// order, then flag values append in the order given. Precedence among roots on
// a skill-name collision follows that same listed order (the loaders' first-
// declarer-wins fold), so a flag can only shadow a config root by preceding it
// — which it cannot, by design; config roots are the recipe's declaration and
// flags are the operator's additions layered after.
//
// Empty/whitespace-only entries are dropped (the flag parser and Validate
// already reject them, but resolving stays defensive). Relative paths resolve
// against base — the agents dir when the config was discovered under one, else
// the cwd — mirroring how projectRoot itself is derived. Absolute paths pass
// through unchanged. Returns nil when nothing is declared, which the loaders
// treat as today's behavior exactly.
func resolveContentRoots(cfgRoots, cliDirs []string, base string) []string {
	merged := make([]string, 0, len(cfgRoots)+len(cliDirs))
	merged = append(merged, cfgRoots...)
	merged = append(merged, cliDirs...)

	var out []string
	for _, root := range merged {
		if strings.TrimSpace(root) == "" {
			continue
		}
		if !filepath.IsAbs(root) {
			root = filepath.Join(base, root)
		}
		out = append(out, root)
	}
	return out
}
