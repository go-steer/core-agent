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
	"encoding/json"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"

	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"

	"github.com/go-steer/core-agent/pkg/config"
	"github.com/go-steer/core-agent/pkg/permissions"
)

// globArgs is what the model passes when calling glob.
type globArgs struct {
	Path    string `json:"path,omitempty" jsonschema:"directory to walk; defaults to current directory if empty"`
	Pattern string `json:"pattern" jsonschema:"shell-style pattern matched against each file's basename (e.g. *.go, README.*); follows filepath.Match — no ** recursion"`
}

// globResult is the structured output the model receives.
type globResult struct {
	Paths     []string `json:"paths"`
	Truncated bool     `json:"truncated,omitempty"`
}

// skippedDirs are directory basenames the walk-based tools refuse to
// descend into. Matches ripgrep's default exclude set: skipping these
// is what the user wants 99% of the time, and walking them is slow
// and noisy.
var skippedDirs = map[string]bool{
	".git":         true,
	".svn":         true,
	".hg":          true,
	"node_modules": true,
	"vendor":       true,
}

// globFunc returns the ADK functiontool handler for glob. Walks
// path (default ".") with filepath.WalkDir and matches each file's
// basename against pattern via filepath.Match. Symlinks are not
// followed (WalkDir's default Lstat semantics). Hidden directories
// in the skip set are pruned at the directory boundary.
//
// Gate is consulted at the walk root and again for each matched
// path before it lands in the result; any path the gate rejects is
// silently dropped (no leak of forbidden paths into the model's
// context).
//
// Output is JSON-encoded then truncated as a whole via Truncate +
// the per-tool caps.
func globFunc(gate *permissions.Gate, cfg *config.Config) functiontool.Func[globArgs, globResult] {
	return func(ctx tool.Context, in globArgs) (globResult, error) {
		if in.Pattern == "" {
			return globResult{}, fmt.Errorf("glob: pattern is required")
		}
		// Validate pattern up-front so the model gets a clean error
		// rather than a partial walk that finds nothing.
		if _, err := filepath.Match(in.Pattern, ""); err != nil {
			return globResult{}, fmt.Errorf("glob: invalid pattern %q: %w", in.Pattern, err)
		}
		root := in.Path
		if root == "" {
			root = "."
		}
		absRoot, err := absolutize(root)
		if err != nil {
			return globResult{}, err
		}
		if err := gate.CheckFileRead(ctx, "glob", absRoot); err != nil {
			return globResult{}, err
		}

		caps := capsFor(cfg, "glob", 32*1024, 500)

		var paths []string
		walkErr := filepath.WalkDir(absRoot, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				// A permission error or transient I/O failure on one
				// entry shouldn't kill the whole walk. Skip it.
				if d != nil && d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			if d.IsDir() {
				if path != absRoot && skippedDirs[d.Name()] {
					return fs.SkipDir
				}
				return nil
			}
			matched, err := filepath.Match(in.Pattern, d.Name())
			if err != nil || !matched {
				return nil
			}
			if err := gate.CheckFileRead(ctx, "glob", path); err != nil {
				return nil
			}
			paths = append(paths, path)
			// Respect the line cap during the walk so we don't load
			// 100k matches into memory before truncating.
			if caps.lines > 0 && len(paths) > caps.lines {
				return filepath.SkipAll
			}
			return nil
		})
		if walkErr != nil && walkErr != filepath.SkipAll {
			return globResult{}, fmt.Errorf("glob: walk: %w", walkErr)
		}
		sort.Strings(paths)

		truncated := false
		if caps.lines > 0 && len(paths) > caps.lines {
			paths = paths[:caps.lines]
			truncated = true
		}
		// Byte cap is enforced after JSON marshaling so the output
		// the model sees can't blow past the configured size even
		// for unusually long path strings.
		out := globResult{Paths: paths, Truncated: truncated}
		if caps.bytes > 0 {
			body, err := json.Marshal(out)
			if err == nil && len(body) > caps.bytes {
				// Approximate: keep dropping paths until under the
				// cap. Not surgical but predictable.
				for caps.bytes > 0 && len(body) > caps.bytes && len(paths) > 0 {
					paths = paths[:len(paths)-1]
					out = globResult{Paths: paths, Truncated: true}
					body, _ = json.Marshal(out)
				}
			}
		}
		return out, nil
	}
}
