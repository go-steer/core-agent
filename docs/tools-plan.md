# Glob + grep built-in tools

## Recommendation summary

Add two file-discovery tools to `tools/` — `glob` (pattern → matching paths) and `grep` (regex + path tree → match lines). Both default-on in `tools.Default()` (passive read-only surface, mirrors `read_file` / `list_dir`); both flow through the existing `permissions.Gate` and `Truncate` paths so policy and output-cap behavior is automatic. **Implement using the standard library only** — `filepath.WalkDir` + `filepath.Match` for glob, `regexp` + `bufio.Scanner` for grep. The cost of dropping `**` recursive-glob support (which would force pulling in `bmatcuk/doublestar`) is acceptable: callers can pass an explicit root directory and let grep's recursion or list_dir's listing handle the walk.

## Context

`docs/DESIGN.md:321` flags glob/grep as deferred under the rationale "cogo doesn't have them; not needed for the immediate downstream consumers. Adding them is straightforward when one shows up." With the agent loop now exercised end-to-end via mocks and the Anthropic web_search shipped, code-search and file-discovery are the most plausible next consumer ask: an agent that's working in a 5,000-file repo needs `grep "TODO" .` and `glob "**/*.go"` more than it needs another model backend. Writing the plan now means the next ask becomes a one-day ship rather than a one-week design session.

This is **a plan, not an implementation** — don't merge code until a real consumer surfaces. The goal is to lock in the design surface so we don't relitigate when the moment comes.

## Design decisions

| Decision | Choice | Why |
|---|---|---|
| One tool or two | **Two**: `glob` (pattern → paths), `grep` (regex + path → matches) | Different shapes — glob returns paths, grep returns line hits. Conflating them into a single "search" tool would force the model to pick a discriminated mode and bloat the schema. Two clean tools mirror how every coding agent (Claude Code, cogo, aider) exposes them. |
| Pattern engine for glob | **`filepath.Match` per filename component, walked manually** | stdlib-only, no new dep. The cost is no `**` (recursive cross-slash) syntax. Workaround: `glob` accepts a `path` argument as the walk root, so `glob(path: "src", pattern: "*.go")` finds every `.go` under `src/`. The model can compose: `glob(path: ".", pattern: "*.go")` for top-level only, or use grep's recursive walk for "find me all .go files mentioning X." |
| Regex engine for grep | **`regexp` (Go's RE2)** | stdlib, predictable, no catastrophic backtracking. Same engine the rest of the codebase uses. |
| Recursion default for grep | **Recursive when `path` is a directory; single-file when `path` is a file** | Matches `grep -r` intuition. The model rarely wants single-file grep (it'd just read the file); directory grep is the high-value path. |
| Output schema | `glob: {paths: []string}`; `grep: {matches: [{path, line, text}]}` | Structured enough to be useful, flat enough to summarize. Line text is included so the model doesn't have to follow up with `read_file` for context — saves a turn. |
| Truncation | Reuse `Truncate` via `capsFor(cfg, "glob"|"grep", defaultBytes, defaultLines)` | Same path as `read_file`/`list_dir`. The output JSON is truncated as a whole; `tool_output.per_tool.{glob,grep}` overrides apply. Default caps: glob 32KB/500 lines (most globs return small result sets); grep 256KB/5000 lines (ripgrep-scale results). |
| Path scope | Each result path checked against `gate.Allowed(...)`; rejected paths silently dropped | Honors path scope without leaking forbidden paths into the model's context. Same semantics as `list_dir`. |
| Default-on | **Yes** — added to `tools.Default()` and `BuiltinToolNames` | Passive read-only surface; same risk class as `read_file`/`list_dir`. No reason to make consumers opt in for these vs. those. |
| Per-tool disable | Inherited automatically — `--disable-tools=glob,grep` works through the existing `Disable` switch | Just two more `case` arms in `BuiltinTools.Disable` and two more entries in `builtinToolNames`. |
| Symlink handling | Don't follow during walk (use `WalkDir` with default `lstat` semantics) | Following symlinks risks loops and walking outside the path scope. If a consumer needs follow-symlinks, expose a future `follow_symlinks: true` arg. |
| Hidden files | Skip directories starting with `.` (e.g. `.git`) by default; include hidden files unless explicitly excluded | The dominant `.git`/`.cache`/`node_modules` walks are slow and useless. Add a `include_hidden: true` arg later if a consumer asks. Document the skip set: `.git`, `.svn`, `.hg`, `node_modules`, `vendor` (matches ripgrep's defaults). |
| `bash`-based shortcut | **Not** wrapping `find`/`grep` external commands | Cross-platform (Windows works), no shell quoting hazards, no PATH dependencies, deterministic output shape. The 100-line stdlib implementation is worth it. |
| Glob pattern escaping | Document that `?`/`*`/`[` characters in literal filenames need escaping per `filepath.Match` rules | Edge case; not worth a custom escape mode. |

## Files

### New
- `tools/glob.go` — `globArgs`/`globResult` types, `globFunc(gate, cfg) functiontool.Func[...]`, walk implementation.
- `tools/grep.go` — `grepArgs`/`grepResult` types, `grepFunc(gate, cfg) functiontool.Func[...]`, walk + regex implementation, exclude-dir set.
- `tools/glob_test.go`, `tools/grep_test.go` — happy path, gate denial, truncation, hidden-file skip, symlink non-follow.

### Modified
- `tools/builtins.go` — extend `BuiltinTools` struct with `Glob bool`, `Grep bool`; add to `Default()`, `builtinToolNames`, `Disable()`. Append two specs in `Build()`.
- `tools/builtins_test.go` — extend `TestBuild_DefaultProducesSixTools` to expect 8; add the two names to `wantNames`. Extend `TestBuiltinTools_Disable_KnownNames` cases map and the size assertion.
- `config/config.go` — add `"glob"` and `"grep"` to the default `tool_output.per_tool` map (glob = 32KB/500, grep = 256KB/5000).
- `cmd/core-agent/main.go` — no change needed; `--no-builtin-tools` and `--disable-tools` already cover the new tools.
- `extras/scion-agent/main.go` — same, no change needed.
- `docs/DESIGN.md:321` — replace the "Glob / grep — deferred" line with a brief subsection (the way `Subagent tool — see "Subagent tool" section below.` was handled). Cover: stdlib-only choice, no `**` rationale, default-on rationale, exclude-dir defaults.
- `README.md` line 30 — add `glob`, `grep` to the suite enumeration: `read_file, write_file, edit_file, list_dir, glob, grep, bash, todo`.
- `docs/site/content/docs/library-api.md` lines ~159-160 — extend the per-tool list shown in the `--no-builtin-tools` description.
- `docs/site/content/docs/configuration.md` — add `glob` and `grep` rows to the `tool_output.per_tool` example block (around line 155).

### Not modified
- `models/` — no changes; these are general-purpose tools, not provider-specific.
- `permissions/` — no changes; tools use existing `gate.Allowed(path)` and bash-style allowlist patterns work as `glob:**.go` / `grep:internal/**`.

## Implementation

### 1. `tools/glob.go`

```go
package tools

import (
    "context"
    "fmt"
    "io/fs"
    "path/filepath"
    "sort"
    "strings"

    "google.golang.org/adk/tool"
    "google.golang.org/adk/tool/functiontool"

    "github.com/go-steer/core-agent/pkg/config"
    "github.com/go-steer/core-agent/pkg/permissions"
)

type globArgs struct {
    Pattern string `json:"pattern" jsonschema:"glob pattern matched per filename component (e.g. '*.go', 'main_*.go')"`
    Path    string `json:"path,omitempty" jsonschema:"directory to search; defaults to current working directory"`
}

type globResult struct {
    Paths []string `json:"paths"`
}

func globFunc(gate *permissions.Gate, cfg *config.Config) functiontool.Func[globArgs, globResult] {
    return func(ctx tool.Context, in globArgs) (globResult, error) {
        root := in.Path
        if root == "" {
            root = "."
        }
        if err := gate.Allowed(context.Background(), "glob", root); err != nil {
            return globResult{}, err
        }
        if in.Pattern == "" {
            return globResult{}, fmt.Errorf("glob: pattern is required")
        }

        var matches []string
        err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
            if err != nil {
                return nil // tolerate per-entry errors (permission denied, etc)
            }
            if d.IsDir() && shouldSkipDir(d.Name(), path == root) {
                return fs.SkipDir
            }
            if d.IsDir() {
                return nil
            }
            ok, mErr := filepath.Match(in.Pattern, d.Name())
            if mErr != nil {
                return fmt.Errorf("glob: bad pattern: %w", mErr)
            }
            if !ok {
                return nil
            }
            // Path-scope check on each match.
            if err := gate.Allowed(context.Background(), "glob", path); err != nil {
                return nil // silently drop forbidden paths
            }
            matches = append(matches, path)
            return nil
        })
        if err != nil {
            return globResult{}, err
        }
        sort.Strings(matches)

        // Truncate the JSON-encoded result via the standard cap path.
        caps := capsFor(cfg, "glob", 32*1024, 500)
        if len(matches) > caps.lines {
            matches = matches[:caps.lines]
        }
        return globResult{Paths: matches}, nil
    }
}

// excludeDirs is the default skip set — matches ripgrep's stop-at-VCS
// behavior. Skipped only when the entry name (not full path) matches.
var excludeDirs = map[string]bool{
    ".git": true, ".svn": true, ".hg": true,
    "node_modules": true, "vendor": true,
}

// shouldSkipDir returns true for default-excluded directories. The
// root directory itself is never skipped — only children.
func shouldSkipDir(name string, isRoot bool) bool {
    if isRoot {
        return false
    }
    if excludeDirs[name] {
        return true
    }
    if strings.HasPrefix(name, ".") {
        return true // skip hidden directories
    }
    return false
}
```

### 2. `tools/grep.go`

```go
package tools

import (
    "bufio"
    "context"
    "fmt"
    "io/fs"
    "os"
    "path/filepath"
    "regexp"

    "google.golang.org/adk/tool"
    "google.golang.org/adk/tool/functiontool"

    "github.com/go-steer/core-agent/pkg/config"
    "github.com/go-steer/core-agent/pkg/permissions"
)

type grepArgs struct {
    Pattern string `json:"pattern" jsonschema:"RE2 regex; matched per line"`
    Path    string `json:"path,omitempty" jsonschema:"file or directory to search; directories walked recursively (default: cwd)"`
}

type grepMatch struct {
    Path string `json:"path"`
    Line int    `json:"line"`
    Text string `json:"text"`
}

type grepResult struct {
    Matches []grepMatch `json:"matches"`
}

func grepFunc(gate *permissions.Gate, cfg *config.Config) functiontool.Func[grepArgs, grepResult] {
    return func(ctx tool.Context, in grepArgs) (grepResult, error) {
        if in.Pattern == "" {
            return grepResult{}, fmt.Errorf("grep: pattern is required")
        }
        re, err := regexp.Compile(in.Pattern)
        if err != nil {
            return grepResult{}, fmt.Errorf("grep: bad regex: %w", err)
        }
        root := in.Path
        if root == "" {
            root = "."
        }
        if err := gate.Allowed(context.Background(), "grep", root); err != nil {
            return grepResult{}, err
        }
        caps := capsFor(cfg, "grep", 256*1024, 5000)

        var out []grepMatch
        scanFile := func(path string) error {
            if err := gate.Allowed(context.Background(), "grep", path); err != nil {
                return nil // silently drop forbidden paths
            }
            f, err := os.Open(path)
            if err != nil {
                return nil // tolerate per-file errors
            }
            defer f.Close()
            sc := bufio.NewScanner(f)
            sc.Buffer(make([]byte, 64*1024), 1024*1024)
            for line := 1; sc.Scan(); line++ {
                if len(out) >= caps.lines {
                    return errStop
                }
                if re.Match(sc.Bytes()) {
                    out = append(out, grepMatch{Path: path, Line: line, Text: sc.Text()})
                }
            }
            return nil
        }

        info, err := os.Stat(root)
        if err != nil {
            return grepResult{}, fmt.Errorf("grep: stat %q: %w", root, err)
        }
        if !info.IsDir() {
            if err := scanFile(root); err != nil && err != errStop {
                return grepResult{}, err
            }
            return grepResult{Matches: out}, nil
        }

        walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
            if err != nil {
                return nil
            }
            if d.IsDir() && shouldSkipDir(d.Name(), path == root) {
                return fs.SkipDir
            }
            if d.IsDir() {
                return nil
            }
            if err := scanFile(path); err == errStop {
                return errStop
            }
            return nil
        })
        if walkErr != nil && walkErr != errStop {
            return grepResult{}, walkErr
        }
        return grepResult{Matches: out}, nil
    }
}

var errStop = fmt.Errorf("stop")
```

### 3. `tools/builtins.go` — additions

Append to `BuiltinTools`:

```go
Glob bool // Pattern-match files
Grep bool // Regex search across files
```

Update `Default()`, `builtinToolNames`, `Disable()` switch (add `case "glob"` / `case "grep"`).

In `Build()`'s specs slice, append after `Todo`:

```go
{b.Glob, "glob", "Find files matching a pattern.", func() (tool.Tool, error) {
    return functiontool.New(functiontool.Config{
        Name: "glob", Description: "Find files matching a glob pattern (per-filename match; pass path arg to walk a subtree).",
    }, globFunc(gate, cfg))
}},
{b.Grep, "grep", "Search file contents with a regex.", func() (tool.Tool, error) {
    return functiontool.New(functiontool.Config{
        Name: "grep", Description: "Search file contents with an RE2 regex. Recursive when path is a directory.",
    }, grepFunc(gate, cfg))
}},
```

### 4. `config/config.go` — extend default `per_tool` map

```go
PerTool: map[string]ToolOutputPerToolCaps{
    "bash":      {MaxBytes: 64 * 1024, MaxLines: 2000},
    "read_file": {MaxBytes: 256 * 1024, MaxLines: 5000},
    "grep":      {MaxBytes: 256 * 1024, MaxLines: 5000},
    "glob":      {MaxBytes: 32 * 1024, MaxLines: 500},
},
```

## Tests

`tools/glob_test.go`:

- **`TestGlob_TopLevelMatch`** — write `a.go`, `b.go`, `README.md` to a tempdir; `glob(pattern: "*.go", path: tempdir)` returns `[a.go, b.go]` sorted.
- **`TestGlob_Recursive`** — write `pkg1/a.go` and `pkg2/b.go`; `glob(pattern: "*.go", path: tempdir)` walks subtree and returns both.
- **`TestGlob_SkipsHiddenAndExcludedDirs`** — populate `.git/HEAD` and `node_modules/foo.go`; assert neither appears in results.
- **`TestGlob_PathScopeDeniesSilently`** — with a gate restricted to a different directory, `glob` returns no matches but no error.
- **`TestGlob_BadPattern`** — `glob(pattern: "[")` returns a clear "bad pattern" error.
- **`TestGlob_TruncatesAtLineCap`** — write 600 matching files, set `tool_output.per_tool.glob = {max_lines: 500}`, assert exactly 500 returned.

`tools/grep_test.go`:

- **`TestGrep_SingleFile`** — write `foo.txt` with three matching lines; `grep(pattern, path: foo.txt)` returns three `grepMatch` entries with correct line numbers.
- **`TestGrep_RecursiveDirectory`** — populate two files under tempdir, each with one match; recursive walk returns both.
- **`TestGrep_BadRegex`** — `grep(pattern: "[")` returns "bad regex" error.
- **`TestGrep_PathScopeDeniesFile`** — file outside scope is skipped silently.
- **`TestGrep_StopsAtCap`** — 10,000 matching lines, cap = 100, asserts exactly 100 returned and walk stops early (use a counter on the file open path to confirm).
- **`TestGrep_HiddenAndExcludedDirsSkipped`** — same fixture as glob's hidden-dir test, plus a matching line inside `.git/config`; assert no match.

`tools/builtins_test.go` extension:

- Update `TestBuild_DefaultProducesSixTools` → `TestBuild_DefaultProducesEightTools`; add `glob` / `grep` to `wantNames`.
- Extend `TestBuiltinTools_Disable_KnownNames` cases map (and size assertion) with the two new names.

## Documentation

- **`docs/DESIGN.md:321`** — replace the "Glob / grep — deferred" line with the new section. Cover the stdlib-only choice (no new deps), the `**` tradeoff (caller passes a walk root), the exclude-dir defaults (matches ripgrep), default-on rationale (passive read-only).
- **`README.md` line 30** — extend the bullet enumeration: `read_file`, `write_file`, `edit_file`, `list_dir`, **`glob`**, **`grep`**, `bash`, `todo`.
- **`docs/site/content/docs/library-api.md`** — extend the `--no-builtin-tools` description's enumeration if it lists tool names; add a brief usage snippet under "Built-in tools" showing `glob` + `grep` schemas.
- **`docs/site/content/docs/configuration.md`** — add `glob` and `grep` rows to the `tool_output.per_tool` example block (around line 155). Document the default caps.
- **`docs/site/content/docs/permissions.md`** — note that `glob:` and `grep:` are valid policy namespaces (e.g. `permissions.allow: ["glob:**.go", "grep:internal/**"]`).

## Verification

```bash
cd /home/user/projects/core-agent
go test ./tools/... ./config/...
go vet ./...
go build ./...

# End-to-end smoke (no creds — uses echoLLM via the mock provider).
./core-agent --provider=echo -p "use glob to find all .md files"
# The model just echoes the prompt, but the binary must build with the new
# tool decls in the registry. Confirm no panics, no startup errors.

# Real smoke against a credentialled provider:
GEMINI_API_KEY=... ./core-agent -p "find every TODO comment in the codebase using grep"
# Expected: model emits a grep tool call, runner streams `→ grep` to stderr,
# results come back, model summarizes.
```

## Out of scope (defer until asked)

- **`**` recursive glob syntax** — would require pulling in `bmatcuk/doublestar` or rolling our own. The walk-root workaround covers the common case. Revisit if a consumer asks for `glob("src/**/*_test.go")` style explicitly.
- **`include_hidden: true` argument** — covers the rare case of searching `.github/` or similar. Easy to add when needed.
- **`follow_symlinks: true` argument** — same. Loop-detection adds complexity; only worth it for a real ask.
- **Case-insensitive grep flag** — RE2 supports `(?i)pattern` inline, which is the Anthropic-style. No need for a separate `case_sensitive: false` arg.
- **Multi-line / context-line grep (`-A`/`-B`)** — useful, but bloats the result schema. Consumers can `read_file` for context once they have a hit.
- **Replacing `bash` for find/grep use cases** — `bash` stays available; some consumers prefer it for fluency. Glob/grep are for the model's structured-search shape, not a wholesale replacement.
- **Concurrency in the walk** — single-goroutine walk is simpler and fast enough for typical agent workloads. If a real consumer hits a 100k-file repo, parallelize then.
