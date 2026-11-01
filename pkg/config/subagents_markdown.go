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
	"regexp"
	"sort"
	"strings"
	"unicode"

	"google.golang.org/genai"
	"gopkg.in/yaml.v3"
)

// SubagentsDirName is the directory under .agents/ scanned for
// Markdown-authored subagents (<agentsDir>/subagents/*.md).
const SubagentsDirName = "subagents"

// SubagentMarkdownExt is the file extension for Markdown subagent definitions.
const SubagentMarkdownExt = ".md"

// loadMarkdownSubagents discovers <agentsDir>/subagents/*.md in sorted
// filename order and appends the parsed SubagentSpecs to c.Subagents.
// Missing <agentsDir>/subagents/ is not an error.
func (c *Config) loadMarkdownSubagents(agentsDir string) error {
	if agentsDir == "" {
		return nil
	}
	dir := filepath.Join(agentsDir, SubagentsDirName)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("config: read subagents dir %q: %w", dir, err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), SubagentMarkdownExt) {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		spec, err := LoadSubagentMarkdownFile(filepath.Join(dir, name))
		if err != nil {
			return err
		}
		c.Subagents = append(c.Subagents, spec)
	}
	return nil
}

// subagentFrontmatter is the YAML frontmatter schema for
// .agents/subagents/<name>.md files, decoded with KnownFields(true).
type subagentFrontmatter struct {
	Name         string                `yaml:"name"`
	Description  string                `yaml:"description"`
	Mode         string                `yaml:"mode"`
	OutputSchema string                `yaml:"output_schema"`
	Tier         string                `yaml:"tier"`
	Capability   string                `yaml:"capability"`
	Model        *frontmatterModel     `yaml:"model"`
	MaxDepth     int                   `yaml:"max_depth"`
	Tools        []string              `yaml:"tools"`
	MCP          []string              `yaml:"mcp"`
	MCPTools     []MCPToolAllowlist    `yaml:"mcp_tools"`
	Skills       []string              `yaml:"skills"`
	Root         string                `yaml:"root"`
	Budgets      *SubagentBudgets      `yaml:"budgets"`
	Scheduler    string                `yaml:"scheduler"`
	Remote       *SubagentRemoteConfig `yaml:"remote"`
}

// frontmatterModel decodes either a bare model name string
// (`model: gemini-3.5-flash`) or a full ModelConfig mapping
// (`model: { name: gemini-3.5-flash, provider: gemini }`).
type frontmatterModel struct {
	ModelConfig
}

func (m *frontmatterModel) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode {
		m.ModelConfig = ModelConfig{Name: value.Value}
		return nil
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	if err := enc.Encode(value); err != nil {
		return err
	}
	_ = enc.Close()
	dec := yaml.NewDecoder(&buf)
	dec.KnownFields(true)
	return dec.Decode(&m.ModelConfig)
}

// LoadSubagentMarkdownFile reads and parses a single Markdown subagent
// file (<agentsDir>/subagents/<name>.md).
func LoadSubagentMarkdownFile(path string) (SubagentSpec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return SubagentSpec{}, fmt.Errorf("config: read subagent %q: %w", path, err)
	}
	fm, body, err := splitSubagentFrontmatter(data)
	if err != nil {
		return SubagentSpec{}, fmt.Errorf("config: subagent %q: %w", path, err)
	}
	if err := checkPlaceholders(path, body); err != nil {
		return SubagentSpec{}, err
	}
	stem := strings.TrimSuffix(filepath.Base(path), SubagentMarkdownExt)
	name := fm.Name
	if name == "" {
		name = stem
	}
	if fm.Mode == "" {
		return SubagentSpec{}, fmt.Errorf("config: subagent %q: mode is required in Markdown subagent files (want %q, %q, or %q)",
			path, SubagentModeChat, SubagentModeTask, SubagentModeSingleTurn)
	}
	var modelCfg *ModelConfig
	if fm.Model != nil {
		mc := fm.Model.ModelConfig
		modelCfg = &mc
	}
	baseDir := filepath.Dir(path)
	spec := SubagentSpec{
		Name:         name,
		Description:  fm.Description,
		Instructions: strings.TrimSpace(body),
		Mode:         fm.Mode,
		OutputSchema: fm.OutputSchema,
		Tier:         fm.Tier,
		Capability:   fm.Capability,
		Model:        modelCfg,
		MaxDepth:     fm.MaxDepth,
		Tools:        fm.Tools,
		MCP:          fm.MCP,
		MCPTools:     fm.MCPTools,
		Skills:       fm.Skills,
		Root:         fm.Root,
		Budgets:      fm.Budgets,
		Scheduler:    fm.Scheduler,
		Remote:       fm.Remote,
		fromMarkdown: true,
		baseDir:      baseDir,
	}
	return spec, nil
}

func splitSubagentFrontmatter(data []byte) (subagentFrontmatter, string, error) {
	const sep = "---"

	trimmed := bytes.TrimLeft(data, " \t\r\n")
	if !bytes.HasPrefix(trimmed, []byte(sep)) {
		return subagentFrontmatter{}, "", fmt.Errorf("missing frontmatter (file must start with `---`)")
	}
	rest := trimmed[len(sep):]
	nl := bytes.IndexByte(rest, '\n')
	if nl < 0 {
		return subagentFrontmatter{}, "", fmt.Errorf("missing frontmatter terminator (no `---` on its own line)")
	}
	if len(bytes.TrimRight(rest[:nl], "\r")) != 0 {
		return subagentFrontmatter{}, "", fmt.Errorf("missing frontmatter (opening `---` must be on its own line)")
	}
	rest = rest[nl+1:]

	end := -1
	afterEnd := 0
	for pos := 0; pos <= len(rest); {
		lineEnd := bytes.IndexByte(rest[pos:], '\n')
		var line []byte
		var nextPos int
		if lineEnd < 0 {
			line = rest[pos:]
			nextPos = len(rest) + 1
		} else {
			line = rest[pos : pos+lineEnd]
			nextPos = pos + lineEnd + 1
		}
		if string(bytes.TrimRight(line, "\r")) == sep {
			end = pos
			if lineEnd < 0 {
				afterEnd = len(rest)
			} else {
				afterEnd = pos + lineEnd + 1
			}
			break
		}
		pos = nextPos
	}
	if end < 0 {
		return subagentFrontmatter{}, "", fmt.Errorf("missing frontmatter terminator (no closing `---`)")
	}
	yamlBlock := rest[:end]
	body := rest[afterEnd:]

	var fm subagentFrontmatter
	dec := yaml.NewDecoder(bytes.NewReader(yamlBlock))
	dec.KnownFields(true)
	if err := dec.Decode(&fm); err != nil && !errors.Is(err, io.EOF) {
		return subagentFrontmatter{}, "", fmt.Errorf("parse frontmatter yaml: %w", err)
	}
	return fm, string(body), nil
}

var (
	placeholderRegex = regexp.MustCompile(`{+[^{}]*}+`)
	statePrefixes    = []string{"app:", "user:", "temp:"}
	artifactPrefix   = "artifact."
)

type staleInjection struct {
	text     string
	key      string
	artifact bool
	line     int
}

// CheckSubagentPlaceholders rejects stale ADK-style optional session-state
// ({key?}, {app:key?}) and artifact ({artifact.name?}) placeholders in
// subagent instructions (#1401, #1450).
func CheckSubagentPlaceholders(path, body string) error {
	return checkPlaceholders(path, body)
}

func checkPlaceholders(path, body string) error {
	stale := findStaleInjections(body)
	if len(stale) == 0 {
		return nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "config: subagent %q: %s in the subagent instruction no longer %s anything — instructions are sent verbatim",
		path, pluralPlaceholder(len(stale)), verbPlaceholder(len(stale)))
	for _, p := range stale {
		what := fmt.Sprintf("asked for session-state key %q", p.key)
		if p.artifact {
			what = fmt.Sprintf("asked for artifact %q", strings.TrimPrefix(p.key, artifactPrefix))
		}
		fmt.Fprintf(&b, "\n  line %d: %s -> %s", p.line, p.text, what)
	}
	return errors.New(b.String())
}

func findStaleInjections(body string) []staleInjection {
	var out []staleInjection
	for _, loc := range placeholderRegex.FindAllStringIndex(body, -1) {
		match := body[loc[0]:loc[1]]
		key := strings.TrimSpace(strings.Trim(match, "{}"))
		if !strings.HasSuffix(key, "?") {
			continue
		}
		key = strings.TrimSuffix(key, "?")
		artifact := strings.HasPrefix(key, artifactPrefix)
		if artifact {
			rest := strings.TrimPrefix(key, artifactPrefix)
			if rest == "" || strings.ContainsAny(rest, " \t\r\n:\",") {
				continue
			}
		} else if !isValidStateName(key) {
			continue
		}
		out = append(out, staleInjection{
			text:     match,
			key:      key,
			artifact: artifact,
			line:     1 + strings.Count(body[:loc[0]], "\n"),
		})
	}
	return out
}

func isValidStateName(name string) bool {
	prefix, rest, found := strings.Cut(name, ":")
	if !found {
		return isIdentifier(name)
	}
	if strings.Contains(rest, ":") {
		return false
	}
	for _, p := range statePrefixes {
		if prefix+":" == p {
			return isIdentifier(rest)
		}
	}
	return false
}

func isIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if i == 0 && !unicode.IsLetter(r) && r != '_' {
			return false
		}
		if i > 0 && !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' {
			return false
		}
	}
	return true
}

func pluralPlaceholder(n int) string {
	if n == 1 {
		return "a placeholder"
	}
	return fmt.Sprintf("%d placeholders", n)
}

func verbPlaceholder(n int) string {
	if n == 1 {
		return "injects"
	}
	return "inject"
}

var schemaExts = map[string]bool{".json": true, ".yaml": true, ".yml": true}

// LoadOutputSchema resolves ref against baseDir (enforcing containment
// within the enclosing .agents/ scope root), or parses an inline JSON
// object schema ("{...}"), normalizes type names to uppercase, and
// validates that ADK's output-schema validator can enforce the resulting tree.
func LoadOutputSchema(baseDir, ref string) (*genai.Schema, error) {
	trimmed := strings.TrimSpace(ref)
	if trimmed == "" {
		return nil, errors.New("output_schema is empty")
	}
	if strings.HasPrefix(trimmed, "{") {
		schema, err := decodeOutputSchema([]byte(trimmed))
		if err != nil {
			return nil, fmt.Errorf("output_schema: %w", err)
		}
		normalizeOutputSchema(schema)
		if err := checkOutputSchema(schema, "$"); err != nil {
			return nil, fmt.Errorf("output_schema: %w", err)
		}
		return schema, nil
	}
	if filepath.IsAbs(ref) || strings.HasPrefix(ref, "/") || (len(ref) >= 2 && ref[1] == ':') {
		return nil, fmt.Errorf("output_schema %q must be a relative path, not absolute", ref)
	}
	ext := strings.ToLower(filepath.Ext(ref))
	if !schemaExts[ext] {
		return nil, fmt.Errorf("output_schema %q has extension %q (want .json, .yaml, or .yml)", ref, ext)
	}
	path, err := resolveOutputSchemaPath(baseDir, ref)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("output_schema %q: %w", ref, err)
	}
	schema, err := decodeOutputSchema(data)
	if err != nil {
		return nil, fmt.Errorf("output_schema %s: %w", path, err)
	}
	normalizeOutputSchema(schema)
	if err := checkOutputSchema(schema, "$"); err != nil {
		return nil, fmt.Errorf("output_schema %s: %w", path, err)
	}
	return schema, nil
}

func resolveOutputSchemaPath(baseDir, ref string) (string, error) {
	if baseDir == "" {
		return "", fmt.Errorf("output_schema %q: no base directory available to resolve relative path", ref)
	}
	absBase, err := filepath.Abs(baseDir)
	if err != nil {
		return "", fmt.Errorf("output_schema %q: resolve base dir: %w", ref, err)
	}
	scopeRoot := absBase
	if base := filepath.Base(absBase); base == SubagentsDirName || base == SpecialistsDirName {
		scopeRoot = filepath.Dir(absBase)
	}
	fromSlash := filepath.FromSlash(ref)
	candidate := filepath.Join(absBase, fromSlash)
	if scopeRoot != absBase && !strings.HasPrefix(filepath.ToSlash(ref), "../") {
		if _, err := os.Stat(candidate); errors.Is(err, fs.ErrNotExist) {
			alt := filepath.Join(scopeRoot, fromSlash)
			if _, aerr := os.Stat(alt); aerr == nil {
				candidate = alt
			}
		}
	}
	if err := ensureSchemaWithinScope(candidate, scopeRoot); err != nil {
		return "", fmt.Errorf("output_schema %q: %w", ref, err)
	}
	return candidate, nil
}

func ensureSchemaWithinScope(target, scopeRoot string) error {
	canonScope := evalSymlinksLongestPrefix(scopeRoot)
	canonTarget := evalSymlinksLongestPrefix(target)
	rel, err := filepath.Rel(canonScope, canonTarget)
	if err != nil {
		return err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("path escapes scope root (%s)", canonScope)
	}
	return nil
}

func evalSymlinksLongestPrefix(p string) string {
	cur := filepath.Clean(p)
	var tail []string
	for {
		if resolved, err := filepath.EvalSymlinks(cur); err == nil {
			for i := len(tail) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, tail[i])
			}
			return resolved
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return filepath.Clean(p)
		}
		tail = append(tail, filepath.Base(cur))
		cur = parent
	}
}

func decodeOutputSchema(data []byte) (*genai.Schema, error) {
	var doc any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	if doc == nil {
		return nil, errors.New("document is empty")
	}
	buf, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("re-encode: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(buf))
	dec.DisallowUnknownFields()
	var schema genai.Schema
	if err := dec.Decode(&schema); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return &schema, nil
}

func normalizeOutputSchema(s *genai.Schema) {
	if s == nil {
		return
	}
	s.Type = genai.Type(strings.ToUpper(string(s.Type)))
	normalizeOutputSchema(s.Items)
	for _, p := range s.Properties {
		normalizeOutputSchema(p)
	}
	for _, a := range s.AnyOf {
		normalizeOutputSchema(a)
	}
}

var supportedSchemaTypes = map[genai.Type]bool{
	genai.TypeString:  true,
	genai.TypeInteger: true,
	genai.TypeNumber:  true,
	genai.TypeBoolean: true,
	genai.TypeArray:   true,
	genai.TypeObject:  true,
}

func checkOutputSchema(s *genai.Schema, path string) error {
	if s == nil {
		return fmt.Errorf("%s: null schema", path)
	}
	if s.Type == "" {
		return fmt.Errorf("%s: no type — every node needs one", path)
	}
	if !supportedSchemaTypes[s.Type] {
		return fmt.Errorf("%s: type %q is not one of object, array, string, integer, number, boolean", path, s.Type)
	}
	if path == "$" && s.Type != genai.TypeObject {
		return fmt.Errorf("$: top-level type must be object, got %q", s.Type)
	}
	if len(s.Enum) > 0 && s.Type != genai.TypeString {
		return fmt.Errorf("%s: enum is only supported on string type, got %q", path, s.Type)
	}
	for i, branch := range s.AnyOf {
		if err := checkOutputSchema(branch, fmt.Sprintf("%s.anyOf[%d]", path, i)); err != nil {
			return err
		}
	}
	switch s.Type {
	case genai.TypeArray:
		if s.Items == nil {
			return fmt.Errorf("%s: array has no items schema", path)
		}
		return checkOutputSchema(s.Items, path+"[]")
	case genai.TypeObject:
		if len(s.Properties) == 0 && len(s.AnyOf) == 0 {
			return fmt.Errorf("%s: object has no properties", path)
		}
		keys := make([]string, 0, len(s.Properties))
		for k := range s.Properties {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, name := range keys {
			if err := checkOutputSchema(s.Properties[name], path+"."+name); err != nil {
				return err
			}
		}
		for _, req := range s.Required {
			if _, ok := s.Properties[req]; !ok {
				return fmt.Errorf("%s: required name %q is not a property (have: %s)", path, req, strings.Join(keys, ", "))
			}
		}
	}
	return nil
}
