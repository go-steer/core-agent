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

package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
)

// warnw is where Load emits unknown-key warnings. Defaults to stderr;
// tests swap it for a buffer. A package-level var (rather than a new
// return value) keeps Load's signature — and every existing caller —
// unchanged.
var warnw io.Writer = os.Stderr

// AgentsDirName is the project-local directory we discover, analogous
// to `.git`. It contains config.json, mcp.json, skills/, sessions/, etc.
const AgentsDirName = ".agents"

// ConfigFileName is the per-project config file inside AgentsDirName.
const ConfigFileName = "config.json"

// Find walks up from startDir looking for a directory named .agents/.
// On match it returns the absolute path of that directory and ok=true.
// When no match is found up to the filesystem root, ok=false (not an error).
func Find(startDir string) (string, bool, error) {
	dir, err := filepath.Abs(startDir)
	if err != nil {
		return "", false, fmt.Errorf("config: resolve start dir: %w", err)
	}
	for {
		candidate := filepath.Join(dir, AgentsDirName)
		info, err := os.Stat(candidate)
		if err == nil && info.IsDir() {
			return candidate, true, nil
		}
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return "", false, fmt.Errorf("config: stat %q: %w", candidate, err)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false, nil
		}
		dir = parent
	}
}

// Load reads <agentsDir>/config.json, merges it over DefaultConfig(), and
// validates the result. agentsDir must be the absolute path returned by
// Find. Missing config.json is treated as "use defaults" (not an error)
// so that an empty .agents/ directory still yields a working config.
func Load(agentsDir string) (*Config, error) {
	cfg := DefaultConfig()
	path := filepath.Join(agentsDir, ConfigFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return cfg, nil
		}
		return nil, fmt.Errorf("config: read %q: %w", path, err)
	}
	// Decode into the existing defaults so unspecified fields keep their
	// default values. Unknown fields are deliberately tolerated rather
	// than rejected (forward-compat: an older binary must still load a
	// config a newer binary wrote, and Save preserves those keys on
	// round-trip). But a silently-ignored key is often a typo in a
	// security-relevant list (a mistyped "deny"/"allow"), so we warn.
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("config: parse %q: %w", path, err)
	}
	// Provider profiles first, naming the file: a profile error points
	// at a nested block whose index alone is hard to find. Strict where
	// the rest of the file is tolerant: a misspelt key inside a profile
	// (base_ur) is a profile that silently lacks the field, with no
	// forward-compat case to protect — the schema is core-models'.
	if err := strictProviders(data); err != nil {
		return nil, fmt.Errorf("config: %s: %w", path, err)
	}
	if err := checkUnknownKeys(path, data); err != nil {
		return nil, err
	}
	if err := cfg.validateProviders(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// checkUnknownKeys walks the raw JSON config recursively against the
// reflect-derived schema of Config. It emits a one-line warning per
// non-fatal unknown key path and returns an error when an unknown key
// falls under permissions.* or targets a cost/budget setting, where a
// silent typo removes a safety or spending guardrail (#1375).
func checkUnknownKeys(path string, data []byte) error {
	var raw json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil // not valid JSON; json.Unmarshal(data, cfg) already surfaced it
	}
	var unknown []string
	walkUnknownKeys(raw, reflect.TypeOf(Config{}), "", &unknown)
	if len(unknown) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(unknown))
	deduped := make([]string, 0, len(unknown))
	for _, k := range unknown {
		if !seen[k] {
			seen[k] = true
			deduped = append(deduped, k)
		}
	}
	sort.Strings(deduped)
	var fatal []string
	for _, k := range deduped {
		if isFatalUnknownKey(k) {
			fatal = append(fatal, k)
			continue
		}
		if warnw != nil {
			if strings.ContainsAny(k, ".[") {
				fmt.Fprintf(warnw, "config: %s: unknown key %q ignored (typo? nested unknown keys have no effect)\n", path, k)
			} else {
				fmt.Fprintf(warnw, "config: %s: unknown key %q ignored (typo? it is preserved on save but has no effect)\n", path, k)
			}
		}
	}
	if len(fatal) > 0 {
		return fmt.Errorf("config: %s: unknown safety/budget key(s) %s (refusing to ignore a typo in permissions or cost ceilings)", path, strings.Join(fatal, ", "))
	}
	return nil
}

func walkUnknownKeys(raw json.RawMessage, t reflect.Type, prefix string, out *[]string) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Struct:
		walkUnknownStructKeys(raw, t, prefix, out)
	case reflect.Slice, reflect.Array:
		var elems []json.RawMessage
		if err := json.Unmarshal(raw, &elems); err != nil {
			return
		}
		for i, elem := range elems {
			walkUnknownKeys(elem, t.Elem(), fmt.Sprintf("%s[%d]", prefix, i), out)
		}
	case reflect.Map:
		elem := t.Elem()
		for elem.Kind() == reflect.Pointer {
			elem = elem.Elem()
		}
		if elem.Kind() != reflect.Struct && elem.Kind() != reflect.Slice && elem.Kind() != reflect.Array && elem.Kind() != reflect.Map {
			return
		}
		pairs, err := parseOrderedObject(raw)
		if err != nil {
			return
		}
		for _, p := range pairs {
			walkUnknownKeys(p.val, t.Elem(), joinKeyPath(prefix, p.key), out)
		}
	}
}

func walkUnknownStructKeys(raw json.RawMessage, t reflect.Type, prefix string, out *[]string) {
	pairs, err := parseOrderedObject(raw)
	if err != nil {
		return
	}
	known := structFieldsByJSONKey(t)
	for _, p := range pairs {
		full := joinKeyPath(prefix, p.key)
		// Top-level "providers" is validated strictly by strictProviders(data).
		if full == "providers" {
			continue
		}
		ft, ok := known[p.key]
		if !ok {
			*out = append(*out, full)
			walkFatalNestedInUnknownRaw(p.val, full, out)
			continue
		}
		walkUnknownKeys(p.val, ft, full, out)
	}
}

// walkFatalNestedInUnknownRaw inspects the raw JSON subtree under an
// unrecognized container key (e.g. "agents" instead of "agent", or
// "subagent" instead of "subagents") and records any nested path that
// matches isFatalUnknownKey. Non-fatal nested keys inside an unknown
// container are not emitted so a forward-compatible top-level extension
// block still produces a single top-level warning (#387, #1375).
func walkFatalNestedInUnknownRaw(raw json.RawMessage, prefix string, out *[]string) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return
	}
	switch raw[0] {
	case '{':
		pairs, err := parseOrderedObject(raw)
		if err != nil {
			return
		}
		for _, p := range pairs {
			full := joinKeyPath(prefix, p.key)
			if isFatalUnknownKey(full) {
				*out = append(*out, full)
			}
			walkFatalNestedInUnknownRaw(p.val, full, out)
		}
	case '[':
		var elems []json.RawMessage
		if err := json.Unmarshal(raw, &elems); err != nil {
			return
		}
		for i, elem := range elems {
			walkFatalNestedInUnknownRaw(elem, fmt.Sprintf("%s[%d]", prefix, i), out)
		}
	}
}

func structFieldsByJSONKey(t reflect.Type) map[string]reflect.Type {
	out := make(map[string]reflect.Type, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" {
			continue
		}
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if name == "" {
			if f.Anonymous {
				for k, ft := range structFieldsByJSONKey(derefType(f.Type)) {
					out[k] = ft
				}
				continue
			}
			name = f.Name
		}
		out[name] = f.Type
	}
	return out
}

func derefType(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t
}

func joinKeyPath(prefix, key string) string {
	if prefix == "" {
		return key
	}
	return prefix + "." + key
}

// isFatalUnknownKey reports whether an unknown key path must fail Load
// rather than warn. Unknown keys under permissions.*, inside budget /
// cost_rate_limit / pricing blocks, or whose key name targets a cost,
// budget, or permission setting are fatal so a typo cannot silently
// disarm a gate or spending ceiling (#1375).
func isFatalUnknownKey(keyPath string) bool {
	if strings.HasPrefix(keyPath, "permissions.") ||
		strings.HasPrefix(keyPath, "pricing.") ||
		strings.HasPrefix(keyPath, "attach.cost_rate_limit.") ||
		strings.Contains(keyPath, ".budgets.") ||
		strings.Contains(keyPath, ".pricing.") {
		return true
	}
	leaf := keyPath
	if idx := strings.LastIndexByte(keyPath, '.'); idx >= 0 {
		leaf = keyPath[idx+1:]
	}
	lower := strings.ToLower(leaf)
	for _, token := range []string{"cost", "budget", "usd", "mtok", "rate_limit"} {
		if strings.Contains(lower, token) {
			return true
		}
	}
	for _, target := range []string{
		"permissions",
		"pricing",
		"max_session_cost_usd",
		"max_turn_cost_usd",
		"budgets",
		"max_cost_usd",
		"max_turns",
		"max_wallclock_seconds",
		"cost_rate_limit",
	} {
		if editDistance(lower, target) <= 2 {
			return true
		}
	}
	return false
}

func editDistance(a, b string) int {
	if a == b {
		return 0
	}
	ra, rb := []rune(a), []rune(b)
	la, lb := len(ra), len(rb)
	if la == 0 {
		return lb
	}
	if lb == 0 {
		return la
	}
	prev := make([]int, lb+1)
	curr := make([]int, lb+1)
	for j := 0; j <= lb; j++ {
		prev[j] = j
	}
	for i := 1; i <= la; i++ {
		curr[0] = i
		for j := 1; j <= lb; j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			curr[j] = min(
				prev[j]+1,
				curr[j-1]+1,
				prev[j-1]+cost,
			)
		}
		prev, curr = curr, prev
	}
	return prev[lb]
}

// LoadOrDefault is the convenience entry point used by main: it walks up
// from startDir, loads `.agents/config.json` if found, otherwise returns
// pristine defaults. The returned agentsDir is "" when no .agents/ was
// discovered — callers can use that to skip writes that require a project.
func LoadOrDefault(startDir string) (cfg *Config, agentsDir string, err error) {
	dir, ok, err := Find(startDir)
	if err != nil {
		return nil, "", err
	}
	if !ok {
		cfg := DefaultConfig()
		return cfg, "", cfg.Validate()
	}
	cfg, err = Load(dir)
	if err != nil {
		return nil, dir, err
	}
	return cfg, dir, nil
}
