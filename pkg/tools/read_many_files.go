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
	"os"
	"path/filepath"
	"sort"

	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"

	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/permissions"
)

// readManyFilesPerFileBytes caps how much content one file contributes
// to a read_many_files response. Stops a single huge file from blowing
// the budget before the whole-response cap fires.
const readManyFilesPerFileBytes = 64 * 1024

// readManyFilesArgs is what the model passes when calling
// read_many_files. At least one of Paths or Pattern is required; both
// can be supplied together (results are deduplicated).
type readManyFilesArgs struct {
	Paths   []string `json:"paths,omitempty" jsonschema:"explicit list of file paths to read"`
	Pattern string   `json:"pattern,omitempty" jsonschema:"basename glob (filepath.Match syntax, e.g. *.go) walked from path"`
	Path    string   `json:"path,omitempty" jsonschema:"root directory for the pattern walk; defaults to current directory"`
}

// readManyFile is one entry in the response. Either Content is set (the
// file was read successfully) or Skipped is set with a short reason.
// Truncated signals the per-file cap fired on Content.
type readManyFile struct {
	Path      string `json:"path"`
	Content   string `json:"content,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
	Skipped   string `json:"skipped,omitempty"`
}

// readManyFilesResult is the structured output the model receives.
// Truncated at the top level signals the whole-response cap dropped
// trailing entries.
type readManyFilesResult struct {
	Files     []readManyFile `json:"files"`
	Truncated bool           `json:"truncated,omitempty"`
}

// readManyFilesFunc returns the ADK functiontool handler for
// read_many_files. Reads the union of explicit Paths and pattern-walked
// matches in a single call — Gemini handles one tool call returning a
// list better than N parallel read_file calls.
//
// Gate is consulted at the walk root (when Pattern is set) and again
// for every file before it's opened; gate denials surface as entries
// with Skipped="permission denied" rather than aborting the batch.
// Missing files and read errors are handled the same way.
//
// Per-file content is capped at readManyFilesPerFileBytes (64KB) so a
// single huge file can't dominate. The whole response is then capped
// per cfg.ToolOutput.PerTool["read_many_files"] (default 256KB) by
// dropping trailing entries; the truncated flag at the top level
// signals when this fires.
func readManyFilesFunc(gate *permissions.Gate, cfg *config.Config) functiontool.Func[readManyFilesArgs, readManyFilesResult] {
	return func(ctx tool.Context, in readManyFilesArgs) (readManyFilesResult, error) {
		if len(in.Paths) == 0 && in.Pattern == "" {
			return readManyFilesResult{}, fmt.Errorf("read_many_files: provide paths or pattern (or both)")
		}
		if in.Pattern != "" {
			if _, err := filepath.Match(in.Pattern, ""); err != nil {
				return readManyFilesResult{}, fmt.Errorf("read_many_files: invalid pattern %q: %w", in.Pattern, err)
			}
		}

		caps := capsFor(cfg, "read_many_files", 256*1024, 5000)

		// Collect target paths: explicit Paths first (preserving model-
		// supplied order so an LLM listing dependencies first reads
		// them first), then pattern matches (sorted for determinism).
		// Deduplicate so paths appearing in both sources are read once.
		seen := make(map[string]bool)
		var ordered []string
		for _, p := range in.Paths {
			abs, err := absolutize(p)
			if err != nil {
				// Empty / malformed path — surface as a synthetic
				// skipped entry below so the model sees it.
				ordered = append(ordered, p)
				continue
			}
			if !seen[abs] {
				seen[abs] = true
				ordered = append(ordered, abs)
			}
		}

		if in.Pattern != "" {
			root := in.Path
			if root == "" {
				root = "."
			}
			absRoot, err := absolutize(root)
			if err != nil {
				return readManyFilesResult{}, err
			}
			if err := gate.CheckFileRead(ctx, "read_many_files", absRoot); err != nil {
				return readManyFilesResult{}, err
			}
			var matched []string
			walkErr := filepath.WalkDir(absRoot, func(path string, d fs.DirEntry, err error) error {
				if err != nil {
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
				if !d.Type().IsRegular() {
					return nil
				}
				ok, mErr := filepath.Match(in.Pattern, d.Name())
				if mErr != nil || !ok {
					return nil
				}
				if !seen[path] {
					seen[path] = true
					matched = append(matched, path)
				}
				return nil
			})
			if walkErr != nil && walkErr != filepath.SkipAll {
				return readManyFilesResult{}, fmt.Errorf("read_many_files: walk: %w", walkErr)
			}
			sort.Strings(matched)
			ordered = append(ordered, matched...)
		}

		// Read each path. Gate denials, missing files, and directories-
		// passed-as-files all surface as Skipped entries so the model
		// gets full visibility into what didn't land.
		result := readManyFilesResult{Files: make([]readManyFile, 0, len(ordered))}
		for _, p := range ordered {
			entry := readManyFile{Path: p}
			if err := gate.CheckFileRead(ctx, "read_many_files", p); err != nil {
				entry.Skipped = "permission denied"
				result.Files = append(result.Files, entry)
				continue
			}
			info, statErr := os.Stat(p)
			if statErr != nil {
				entry.Skipped = "stat error: " + statErr.Error()
				result.Files = append(result.Files, entry)
				continue
			}
			if info.IsDir() {
				entry.Skipped = "is a directory"
				result.Files = append(result.Files, entry)
				continue
			}
			data, err := os.ReadFile(p)
			if err != nil {
				entry.Skipped = "read error: " + err.Error()
				result.Files = append(result.Files, entry)
				continue
			}
			content := string(data)
			if len(content) > readManyFilesPerFileBytes {
				content = Truncate(content, readManyFilesPerFileBytes, 1000)
				entry.Truncated = true
			}
			entry.Content = content
			result.Files = append(result.Files, entry)
		}

		// Whole-response cap: drop trailing entries until the JSON-
		// encoded result fits under caps.bytes. Same approximation as
		// glob/grep: not surgical but predictable.
		if caps.bytes > 0 && len(result.Files) > 0 {
			for {
				body, err := json.Marshal(result)
				if err != nil || len(body) <= caps.bytes {
					break
				}
				if len(result.Files) == 0 {
					break
				}
				result.Files = result.Files[:len(result.Files)-1]
				result.Truncated = true
			}
		}

		return result, nil
	}
}
