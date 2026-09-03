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
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"

	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/permissions"
)

// File-tool argument and result types. Field tags double as the
// JSON-schema descriptions the model sees, so write them like prompts.

type readFileArgs struct {
	Path   string `json:"path" jsonschema:"absolute or relative file path"`
	Offset int    `json:"offset,omitempty" jsonschema:"line number to start reading from (0-based)"`
	Limit  int    `json:"limit,omitempty" jsonschema:"max lines to return; 0 = read to EOF"`
}

type readFileResult struct {
	Content string `json:"content"`
}

type writeFileArgs struct {
	Path    string `json:"path" jsonschema:"absolute or relative file path"`
	Content string `json:"content" jsonschema:"new file contents (replaces any existing file)"`
}

type writeFileResult struct {
	Status string `json:"status"`
	Bytes  int    `json:"bytes"`
}

type editFileArgs struct {
	Path      string `json:"path"`
	OldString string `json:"old_string" jsonschema:"exact string to replace; must occur exactly once unless replace_all is true"`
	NewString string `json:"new_string" jsonschema:"replacement string"`
	// ReplaceAll opts out of the uniqueness requirement. It is a
	// separate argument rather than the default because the failure it
	// suppresses is the useful one: a snippet that matches in two places
	// usually means the model has not read enough of the file to know
	// which one it wants, and silently editing both hides that.
	ReplaceAll bool `json:"replace_all,omitempty" jsonschema:"replace EVERY occurrence instead of failing when old_string is not unique. Default false. Only set this when you intend to change all of them."`
}

type editFileResult struct {
	Status       string `json:"status"`
	Replacements int    `json:"replacements"`
}

type listDirArgs struct {
	Path string `json:"path" jsonschema:"directory path; defaults to current directory if empty"`
}

type listDirResult struct {
	Entries []dirEntry `json:"entries"`
}

type dirEntry struct {
	Name  string `json:"name"`
	IsDir bool   `json:"is_dir"`
	Size  int64  `json:"size"`
}

type deleteFileArgs struct {
	Path string `json:"path" jsonschema:"absolute or relative file path to remove. Must be a regular file — refuses to delete directories (use a future delete_dir if that's ever wanted)."`
}

type deleteFileResult struct {
	Status string `json:"status"`
}

type statArgs struct {
	Path string `json:"path" jsonschema:"absolute or relative file or directory path to inspect"`
}

type statResult struct {
	Path    string `json:"path"`
	Exists  bool   `json:"exists"`
	IsDir   bool   `json:"is_dir,omitempty"`
	Size    int64  `json:"size,omitempty"`
	ModTime string `json:"mod_time,omitempty"` // RFC3339, UTC
	Mode    string `json:"mode,omitempty"`     // os.FileMode.String(), e.g. "-rw-r--r--"
}

// readFileFunc returns the ADK functiontool handler for read_file. The
// returned closure consults gate.CheckFileRead before touching disk.
func readFileFunc(gate *permissions.Gate, cfg *config.Config) functiontool.Func[readFileArgs, readFileResult] {
	return func(ctx tool.Context, in readFileArgs) (readFileResult, error) {
		path, err := absolutize(in.Path)
		if err != nil {
			return readFileResult{}, err
		}
		if err := gate.CheckFileRead(ctx, "read_file", path); err != nil {
			return readFileResult{}, err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return readFileResult{}, fmt.Errorf("read_file: %w", err)
		}
		text := string(data)
		if in.Offset > 0 || in.Limit > 0 {
			text = sliceLines(text, in.Offset, in.Limit)
		}
		caps := capsFor(cfg, "read_file", 256*1024, 5000)
		return readFileResult{Content: Truncate(text, caps.bytes, caps.lines)}, nil
	}
}

func writeFileFunc(gate *permissions.Gate) functiontool.Func[writeFileArgs, writeFileResult] {
	return func(ctx tool.Context, in writeFileArgs) (writeFileResult, error) {
		path, err := absolutize(in.Path)
		if err != nil {
			return writeFileResult{}, err
		}
		if err := gate.CheckFileWrite(ctx, "write_file", path); err != nil {
			return writeFileResult{}, err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return writeFileResult{}, fmt.Errorf("write_file: mkdir: %w", err)
		}
		if err := atomicWrite(path, []byte(in.Content), 0o644); err != nil {
			return writeFileResult{}, fmt.Errorf("write_file: %w", err)
		}
		return writeFileResult{Status: "wrote " + path, Bytes: len(in.Content)}, nil
	}
}

func editFileFunc(gate *permissions.Gate) functiontool.Func[editFileArgs, editFileResult] {
	return func(ctx tool.Context, in editFileArgs) (editFileResult, error) {
		path, err := absolutize(in.Path)
		if err != nil {
			return editFileResult{}, err
		}
		if err := gate.CheckFileWrite(ctx, "edit_file", path); err != nil {
			return editFileResult{}, err
		}
		if in.OldString == "" {
			return editFileResult{}, fmt.Errorf("edit_file: old_string is required")
		}
		if in.OldString == in.NewString {
			return editFileResult{}, fmt.Errorf("edit_file: old_string and new_string are identical")
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return editFileResult{}, fmt.Errorf("edit_file: %w", err)
		}
		body := string(data)
		count := strings.Count(body, in.OldString)
		if count == 0 {
			return editFileResult{}, fmt.Errorf("edit_file: old_string not found in %s", path)
		}
		// Ambiguity is refused by default, and the refusal names the way
		// out: without it the model's only visible options are to widen
		// the snippet or to give up, and a capability it cannot see is a
		// capability it does not have (#759/#762).
		if count > 1 && !in.ReplaceAll {
			return editFileResult{}, fmt.Errorf("edit_file: old_string appears %d times in %s; provide a unique snippet, or pass replace_all=true to change all %d", count, path, count)
		}
		n := 1
		if in.ReplaceAll {
			n = count
		}
		updated := strings.Replace(body, in.OldString, in.NewString, n)
		if err := atomicWrite(path, []byte(updated), 0o644); err != nil {
			return editFileResult{}, fmt.Errorf("edit_file: %w", err)
		}
		return editFileResult{Status: editedStatus(path, n), Replacements: n}, nil
	}
}

// editedStatus spells the occurrence count into the status line for a
// replace_all edit. Replacements already carries the number, but the
// status is the part a model reliably reads, and "how many did that
// touch" is exactly what the caller could not know before asking — a
// count larger than expected is the signal that the snippet was too
// broad, and it is worth surfacing where it will be seen.
func editedStatus(path string, n int) string {
	if n == 1 {
		return "edited " + path
	}
	return fmt.Sprintf("edited %s (%d occurrences)", path, n)
}

func listDirFunc(gate *permissions.Gate, cfg *config.Config) functiontool.Func[listDirArgs, listDirResult] {
	return func(ctx tool.Context, in listDirArgs) (listDirResult, error) {
		path := in.Path
		if path == "" {
			path = "."
		}
		abs, err := absolutize(path)
		if err != nil {
			return listDirResult{}, err
		}
		if err := gate.CheckFileRead(ctx, "list_dir", abs); err != nil {
			return listDirResult{}, err
		}
		entries, err := os.ReadDir(abs)
		if err != nil {
			return listDirResult{}, fmt.Errorf("list_dir: %w", err)
		}
		out := make([]dirEntry, 0, len(entries))
		for _, e := range entries {
			info, err := e.Info()
			if err != nil {
				continue
			}
			out = append(out, dirEntry{Name: e.Name(), IsDir: e.IsDir(), Size: info.Size()})
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
		// Apply line-style cap to keep the result tractable for very
		// large directories. Each entry is roughly one "line".
		caps := capsFor(cfg, "list_dir", 32*1024, 500)
		if caps.lines > 0 && len(out) > caps.lines {
			out = out[:caps.lines]
		}
		return listDirResult{Entries: out}, nil
	}
}

// absolutize resolves a possibly-relative path against the working
// directory, cleans it, and resolves symlinks (#374). The gate and
// the actual file I/O must see the same real target: os.ReadFile /
// atomicWrite / os.Remove follow symlinks at the OS level, so gating
// the lexical path would let an in-scope symlink alias an
// out-of-scope file. Not-yet-existing paths (new-file writes) are
// resolved through their deepest existing ancestor; when resolution
// fails for any other reason the lexical path is kept and the scope
// check fails closed on its own resolution attempt (see
// permissions.PathScope.AccessFor).
func absolutize(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("path is required")
	}
	if strings.HasPrefix(path, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(path, "~"), "/"))
		}
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	if resolved, err := permissions.ResolvePath(abs); err == nil {
		return resolved, nil
	}
	return abs, nil
}

// sliceLines extracts a window of lines [offset, offset+limit). offset
// is 0-based; limit=0 means "to EOF".
func sliceLines(text string, offset, limit int) string {
	lines := strings.SplitAfter(text, "\n")
	if offset > len(lines) {
		return ""
	}
	end := len(lines)
	if limit > 0 && offset+limit < end {
		end = offset + limit
	}
	return strings.Join(lines[offset:end], "")
}

// atomicWrite writes data to path via temp + rename so a crash mid-write
// can't leave the file half-baked. Same directory so rename is atomic.
func atomicWrite(path string, data []byte, mode fs.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".core-agent-write-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // best-effort cleanup if rename fails
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// deleteFileFunc returns the handler for delete_file. Refuses to
// delete directories — keeps the blast radius bounded and surfaces an
// explicit error the model can adapt to. The permissions gate
// (CheckFileWrite, because delete is a destructive write-class op)
// covers path-scope and per-tool denylists.
func deleteFileFunc(gate *permissions.Gate) functiontool.Func[deleteFileArgs, deleteFileResult] {
	return func(ctx tool.Context, in deleteFileArgs) (deleteFileResult, error) {
		path, err := absolutize(in.Path)
		if err != nil {
			return deleteFileResult{}, err
		}
		if err := gate.CheckFileWrite(ctx, "delete_file", path); err != nil {
			return deleteFileResult{}, err
		}
		info, err := os.Lstat(path)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				// Idempotent: deleting a missing file is a no-op
				// success so the model can drive cleanup without
				// pre-existence checks.
				//
				// Deliberately does NOT set agent.ToolResultNoOpKey the
				// way record_plan's unchanged outcome does (#918).
				// NoOpStreakSignal counts consecutive inert results
				// without looking at their arguments, and sweeping three
				// already-absent scratch files in a row — the exact
				// batch this tool's own description advertises — is a
				// productive cleanup, not a loop. Opting in here would
				// buy a Critical halt on a documented workload.
				return deleteFileResult{Status: "no-op (not found): " + path}, nil
			}
			return deleteFileResult{}, fmt.Errorf("delete_file: lstat: %w", err)
		}
		if info.IsDir() {
			return deleteFileResult{}, fmt.Errorf("delete_file: %q is a directory; this tool only removes regular files", path)
		}
		if err := os.Remove(path); err != nil {
			return deleteFileResult{}, fmt.Errorf("delete_file: %w", err)
		}
		return deleteFileResult{Status: "deleted " + path}, nil
	}
}

// statFunc returns the handler for stat. Metadata-only (size, mtime,
// mode, is_dir); does not read file contents. Treated as a read for
// gate purposes since no state is mutated. A missing path returns a
// success result with Exists=false rather than an error — lets the
// model do "does X exist yet?" checks without exception handling.
func statFunc(gate *permissions.Gate) functiontool.Func[statArgs, statResult] {
	return func(ctx tool.Context, in statArgs) (statResult, error) {
		path, err := absolutize(in.Path)
		if err != nil {
			return statResult{}, err
		}
		if err := gate.CheckFileRead(ctx, "stat", path); err != nil {
			return statResult{}, err
		}
		info, err := os.Stat(path)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return statResult{Path: path, Exists: false}, nil
			}
			return statResult{}, fmt.Errorf("stat: %w", err)
		}
		return statResult{
			Path:    path,
			Exists:  true,
			IsDir:   info.IsDir(),
			Size:    info.Size(),
			ModTime: info.ModTime().UTC().Format(time.RFC3339),
			Mode:    info.Mode().String(),
		}, nil
	}
}

type outputCaps struct {
	bytes int
	lines int
}

// capsFor returns the per-tool truncation caps from cfg, falling back
// to the global defaults and finally to compile-time defaults.
func capsFor(cfg *config.Config, toolName string, defaultBytes, defaultLines int) outputCaps {
	caps := outputCaps{bytes: defaultBytes, lines: defaultLines}
	if cfg.ToolOutput.MaxBytes > 0 {
		caps.bytes = cfg.ToolOutput.MaxBytes
	}
	if cfg.ToolOutput.MaxLines > 0 {
		caps.lines = cfg.ToolOutput.MaxLines
	}
	if per, ok := cfg.ToolOutput.PerTool[toolName]; ok {
		if per.MaxBytes > 0 {
			caps.bytes = per.MaxBytes
		}
		if per.MaxLines > 0 {
			caps.lines = per.MaxLines
		}
	}
	return caps
}
