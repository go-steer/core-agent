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
	"unicode"
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
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("config: read %q: %w", path, err)
		}
	} else {
		// Decode into the existing defaults so unspecified fields keep their
		// default values. Non-critical unknown fields warn and survive Save
		// round-trips (forward-compat: an older binary still loads a config a
		// newer binary wrote), while unknown keys inside safety, permission,
		// scope, attach, provider, or budget sections fail closed in
		// strictProviders / checkUnknownKeys (#1375, #1442).
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
	}
	for i := range cfg.Subagents {
		if cfg.Subagents[i].baseDir == "" {
			cfg.Subagents[i].baseDir = agentsDir
		}
	}
	if err := cfg.loadMarkdownSubagents(agentsDir); err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// checkUnknownKeys walks the raw JSON config recursively against the
// reflect-derived schema of Config. It emits a one-line warning per
// non-fatal unknown key path and returns an error when an unknown key
// falls inside a safety/permission/scope/auth/budget block or targets a
// safety or spending guardrail (#1375, #1442).
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
		return fmt.Errorf("config: %s: unknown safety/budget key(s) %s (refusing to ignore a typo in safety, permission, scope, auth, or budget settings)", path, strings.Join(fatal, ", "))
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

type jsonField struct {
	name string
	typ  reflect.Type
}

func walkUnknownStructKeys(raw json.RawMessage, t reflect.Type, prefix string, out *[]string) {
	pairs, err := parseOrderedObject(raw)
	if err != nil {
		return
	}
	fields := structFields(t)
	for _, p := range pairs {
		f, ok := lookupJSONField(fields, p.key)
		if !ok {
			full := joinKeyPath(prefix, p.key)
			*out = append(*out, full)
			walkFatalNestedInUnknownRaw(p.val, full, out)
			continue
		}
		canonical := joinKeyPath(prefix, f.name)
		// Top-level "providers" (any casing) is validated strictly by strictProviders(data).
		if canonical == "providers" {
			continue
		}
		walkUnknownKeys(p.val, f.typ, canonical, out)
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

func structFields(t reflect.Type) []jsonField {
	out := make([]jsonField, 0, t.NumField())
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
				out = append(out, structFields(derefType(f.Type))...)
				continue
			}
			name = f.Name
		}
		out = append(out, jsonField{name: name, typ: f.Type})
	}
	return out
}

// lookupJSONField mirrors encoding/json's struct-key matching: exact
// tag match wins first, falling back to Unicode simple case-folding
// (strings.EqualFold) in declaration order so mixed-case containers
// (e.g. "Attach") still descend into their typed schema rather than
// stopping at a top-level warning (#1442).
func lookupJSONField(fields []jsonField, key string) (jsonField, bool) {
	for _, f := range fields {
		if f.name == key {
			return f, true
		}
	}
	for _, f := range fields {
		if strings.EqualFold(f.name, key) {
			return f, true
		}
	}
	return jsonField{}, false
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

var fatalPathPrefixes = []string{
	"permissions.",
	"pricing.",
	"safety.",
	"url_scope.",
	"path_scope.",
	"attach.",
	"a2a.auth.",
}

var fatalPathSegments = []string{
	".budgets.",
	".pricing.",
	".builtin_tools.",
	".remote.",
}

var fatalLeafTargets = []string{
	"permissions",
	"pricing",
	"safety",
	"url_scope",
	"path_scope",
	"builtin_tools",
	"multi_session",
	"readonly",
	"allow_anonymous",
	"admin_identities",
	"proxy_identities",
	"allow_card_host_change",
	"allow_metadata_endpoints",
	"max_session_cost_usd",
	"max_turn_cost_usd",
	"budgets",
	"max_cost_usd",
	"max_turns",
	"max_wallclock_seconds",
	"cost_rate_limit",
}

// isFatalUnknownKey reports whether an unknown key path must fail Load
// rather than warn. Unknown keys inside safety, permission, scope,
// attach-auth, server-side built-in tool, remote-subagent, or
// budget/pricing blocks — or typos of those container and ceiling names
// — are fatal so a typo cannot silently disarm a gate or spending
// ceiling (#1375, #1442).
func isFatalUnknownKey(keyPath string) bool {
	lowerPath := strings.ToLower(keyPath)
	if hasFatalContainer(lowerPath) {
		return true
	}
	leaf := keyPath
	if idx := strings.LastIndexByte(keyPath, '.'); idx >= 0 {
		leaf = keyPath[idx+1:]
	}
	lowerLeaf := strings.ToLower(leaf)
	if isFatalScopedTypo(lowerPath, lowerLeaf) || hasFatalLeafToken(leaf, lowerLeaf) {
		return true
	}
	for _, target := range fatalLeafTargets {
		if editDistance(lowerLeaf, target) <= 2 {
			return true
		}
	}
	return false
}

func hasFatalContainer(lowerPath string) bool {
	for _, prefix := range fatalPathPrefixes {
		if strings.HasPrefix(lowerPath, prefix) {
			return true
		}
	}
	for _, seg := range fatalPathSegments {
		if strings.Contains(lowerPath, seg) {
			return true
		}
	}
	return false
}

func isFatalScopedTypo(lowerPath, lowerLeaf string) bool {
	if !strings.Contains(lowerPath, ".") {
		if editDistance(lowerLeaf, "attach") <= 2 || editDistance(lowerLeaf, "providers") <= 2 || editDistance(lowerLeaf, "tools") <= 1 || editDistance(lowerLeaf, "a2a") <= 1 {
			return true
		}
	}
	if rest, ok := strings.CutPrefix(lowerPath, "a2a."); ok && !strings.Contains(rest, ".") {
		if editDistance(lowerLeaf, "auth") <= 2 ||
			editDistance(lowerLeaf, "expose") <= 2 ||
			editDistance(lowerLeaf, "input_schema") <= 2 ||
			editDistance(lowerLeaf, "output_schema") <= 2 {
			return true
		}
	}
	if rest, ok := strings.CutPrefix(lowerPath, "tools."); ok && !strings.Contains(rest, ".") {
		if editDistance(lowerLeaf, "disable") <= 2 {
			return true
		}
	}
	if isDirectSubagentField(lowerPath) {
		if editDistance(lowerLeaf, "tools") <= 1 ||
			editDistance(lowerLeaf, "mcp") <= 1 ||
			lowerLeaf == "mcp_servers" ||
			lowerLeaf == "mcpservers" ||
			editDistance(lowerLeaf, "skills") <= 1 {
			return true
		}
	}
	return false
}

func isDirectSubagentField(lowerPath string) bool {
	rest, ok := strings.CutPrefix(lowerPath, "subagents[")
	if !ok {
		return false
	}
	_, afterIdx, ok := strings.Cut(rest, "].")
	return ok && !strings.Contains(afterIdx, ".")
}

func hasFatalLeafToken(leaf, lowerLeaf string) bool {
	for _, sub := range []string{"cost", "budget", "rate_limit"} {
		if strings.Contains(lowerLeaf, sub) {
			return true
		}
	}
	words := splitKeyWords(leaf)
	for i, w := range words {
		if w == "usd" || w == "mtok" {
			return true
		}
		if w == "m" && i+1 < len(words) && words[i+1] == "tok" {
			return true
		}
	}
	return false
}

func splitKeyWords(key string) []string {
	var words []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			words = append(words, strings.ToLower(cur.String()))
			cur.Reset()
		}
	}
	runes := []rune(key)
	var prev rune
	for i, r := range runes {
		if r == '_' || r == '-' || !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			flush()
			prev = 0
			continue
		}
		if prev != 0 && shouldSplitWordBoundary(prev, r, runes, i) {
			flush()
		}
		cur.WriteRune(r)
		prev = r
	}
	flush()
	return words
}

func shouldSplitWordBoundary(prev, r rune, runes []rune, i int) bool {
	if unicode.IsLower(prev) && unicode.IsUpper(r) {
		return true
	}
	if unicode.IsDigit(prev) != unicode.IsDigit(r) {
		return true
	}
	return unicode.IsUpper(prev) && unicode.IsUpper(r) && i+1 < len(runes) && unicode.IsLower(runes[i+1])
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
