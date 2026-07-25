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
	warnUnknownKeys(path, data)
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// warnUnknownKeys emits a one-line warning per unknown top-level key in
// the raw config bytes. Unknown keys are not fatal (see Load), but a
// mistyped "deny"/"allow" would otherwise be silently ignored —
// dangerous when the mistyped key was meant to gate access.
func warnUnknownKeys(path string, data []byte) {
	if warnw == nil {
		return
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return // not an object; Validate/Unmarshal will have surfaced it
	}
	known := knownTopLevelKeys()
	var unknown []string
	for k := range top {
		if !known[k] {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) == 0 {
		return
	}
	sort.Strings(unknown)
	for _, k := range unknown {
		fmt.Fprintf(warnw, "config: %s: unknown key %q ignored (typo? it is preserved on save but has no effect)\n", path, k)
	}
}

// knownTopLevelKeys returns the set of JSON keys the Config struct
// recognizes at the top level, derived from struct tags so it can't
// drift from the schema.
func knownTopLevelKeys() map[string]bool {
	out := make(map[string]bool)
	t := reflect.TypeOf(Config{})
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		if comma := bytes.IndexByte([]byte(tag), ','); comma >= 0 {
			tag = tag[:comma]
		}
		if tag != "" {
			out[tag] = true
		}
	}
	return out
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
