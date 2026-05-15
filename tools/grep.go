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
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"

	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"

	"github.com/go-steer/core-agent/config"
	"github.com/go-steer/core-agent/permissions"
)

// grepArgs is what the model passes when calling grep.
type grepArgs struct {
	Path    string `json:"path,omitempty" jsonschema:"file or directory to search; defaults to current directory if empty. Directories are walked recursively"`
	Pattern string `json:"pattern" jsonschema:"RE2 regular expression matched per line. Use literal-string syntax when not actually using regex features"`
}

// grepMatch is one hit: the file path, 1-based line number, and the
// full matching line text. The line text is included so the model
// doesn't have to follow up with read_file for context.
type grepMatch struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

// grepResult is the structured output. Truncated signals whether the
// result was capped by tool_output limits.
type grepResult struct {
	Matches   []grepMatch `json:"matches"`
	Truncated bool        `json:"truncated,omitempty"`
}

// grepFunc returns the ADK functiontool handler for grep. Walks
// path (default ".") with filepath.WalkDir, opens each regular file,
// runs the compiled regex per line. Symlinks are not followed
// (WalkDir's default Lstat semantics). Hidden directories in the
// skip set are pruned.
//
// When path points at a single file, that file alone is searched.
// When path points at a directory, the walk is recursive.
//
// Gate is consulted at the walk root and again for each file before
// it's opened; rejected files are silently skipped.
//
// Output is capped per cfg.ToolOutput.PerTool["grep"]. Defaults are
// 256KB / 5000 lines (ripgrep-scale).
func grepFunc(gate *permissions.Gate, cfg *config.Config) functiontool.Func[grepArgs, grepResult] {
	return func(_ tool.Context, in grepArgs) (grepResult, error) {
		if in.Pattern == "" {
			return grepResult{}, fmt.Errorf("grep: pattern is required")
		}
		re, err := regexp.Compile(in.Pattern)
		if err != nil {
			return grepResult{}, fmt.Errorf("grep: invalid pattern %q: %w", in.Pattern, err)
		}

		root := in.Path
		if root == "" {
			root = "."
		}
		absRoot, err := absolutize(root)
		if err != nil {
			return grepResult{}, err
		}
		if err := gate.CheckFileRead(context.Background(), "grep", absRoot); err != nil {
			return grepResult{}, err
		}

		caps := capsFor(cfg, "grep", 256*1024, 5000)

		// Single-file mode when the root is a regular file.
		info, statErr := os.Stat(absRoot)
		if statErr == nil && !info.IsDir() {
			matches, truncated, err := grepFile(re, absRoot, caps.lines)
			if err != nil {
				return grepResult{}, fmt.Errorf("grep: %w", err)
			}
			return capByBytes(grepResult{Matches: matches, Truncated: truncated}, caps.bytes), nil
		}

		var (
			matches   []grepMatch
			truncated bool
		)
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
			if err := gate.CheckFileRead(context.Background(), "grep", path); err != nil {
				return nil
			}
			fileMatches, fileTrunc, ferr := grepFile(re, path, caps.lines-len(matches))
			if ferr != nil {
				// Binary files / permission errors / encoding issues
				// shouldn't abort the whole walk. Skip silently.
				return nil
			}
			matches = append(matches, fileMatches...)
			if fileTrunc || (caps.lines > 0 && len(matches) >= caps.lines) {
				truncated = true
				return filepath.SkipAll
			}
			return nil
		})
		if walkErr != nil && !errors.Is(walkErr, filepath.SkipAll) {
			return grepResult{}, fmt.Errorf("grep: walk: %w", walkErr)
		}
		return capByBytes(grepResult{Matches: matches, Truncated: truncated}, caps.bytes), nil
	}
}

// grepFile scans one file for regex matches. Returns at most maxLines
// matches (when maxLines > 0), with truncated=true when the cap fired.
// Lines longer than bufio's max token length are skipped silently.
func grepFile(re *regexp.Regexp, path string, maxLines int) ([]grepMatch, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024) // 1 MiB max line
	var matches []grepMatch
	lineNo := 0
	truncated := false
	for scanner.Scan() {
		lineNo++
		text := scanner.Text()
		if !re.MatchString(text) {
			continue
		}
		matches = append(matches, grepMatch{Path: path, Line: lineNo, Text: text})
		if maxLines > 0 && len(matches) >= maxLines {
			truncated = true
			break
		}
	}
	// Scanner.Err() catches genuine I/O errors but not "line too
	// long" — that's an expected occurrence in mixed-content
	// directories and we just skip the rest of the file.
	if err := scanner.Err(); err != nil && !errors.Is(err, bufio.ErrTooLong) {
		return matches, truncated, err
	}
	return matches, truncated, nil
}

// capByBytes drops trailing matches until the JSON-encoded result
// fits under maxBytes. Approximate (not surgical) but predictable.
// When maxBytes <= 0 this is a no-op.
func capByBytes(r grepResult, maxBytes int) grepResult {
	if maxBytes <= 0 || len(r.Matches) == 0 {
		return r
	}
	// Cheap upper-bound estimate: each match averages ~200 bytes
	// in JSON. Skip the marshal if we're nowhere near the cap.
	if len(r.Matches)*200 < maxBytes {
		return r
	}
	for len(r.Matches) > 0 {
		body, err := json.Marshal(r)
		if err != nil || len(body) <= maxBytes {
			return r
		}
		r.Matches = r.Matches[:len(r.Matches)-1]
		r.Truncated = true
	}
	return r
}
