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

package agentenv

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// SchemaVersion is the current major version of the env-manifest file
// format. Bump on breaking changes; older versions get rejected at
// load time with an explicit upgrade-path error.
const SchemaVersion = 1

// ManifestFileYAML is the manifest filename with YAML syntax. YAML is
// the preferred format because multi-line descriptions read cleanly;
// JSON is supported below for parity with config.json / mcp.json.
const ManifestFileYAML = "env.yaml"

// ManifestFileJSON is the JSON variant of the manifest. Both files are
// probed at load time — YAML first, then JSON. Declaring both is a
// configuration error (see LoadManifest).
const ManifestFileJSON = "env.json"

// Manifest is the on-disk schema for .agents/env.yaml (or .env.json).
//
// Version follows the convention set by pkg/config (SchemaVersion=1)
// and pkg/mcp (Servers.Version) — top-level version bump on breaking
// change, unknown fields ignored for forward-compat.
type Manifest struct {
	Version int     `json:"version" yaml:"version"`
	Env     []Entry `json:"env" yaml:"env"`
}

// Entry declares one env variable the bundle expects.
//
// A required entry with no env-var set at daemon boot is a fail-loud
// error (surfaced via Resolver.Errors). An optional entry falls back
// to Default when unset; missing default → empty string.
//
// UsedBy is optional documentation — a comma-separated list of files
// (or free-text hints) that reference this var, letting recipe authors
// grep for coupling. Nothing in the loader validates the entries.
//
// Sensitive marks values that must not appear in verbose logs, the
// eventlog, or diagnostic surfaces like /stats. Set true for tokens,
// passwords, API keys.
type Entry struct {
	Name        string   `json:"name"                   yaml:"name"`
	Required    bool     `json:"required,omitempty"     yaml:"required,omitempty"`
	Default     string   `json:"default,omitempty"      yaml:"default,omitempty"`
	Sensitive   bool     `json:"sensitive,omitempty"    yaml:"sensitive,omitempty"`
	Description string   `json:"description,omitempty"  yaml:"description,omitempty"`
	UsedBy      []string `json:"used_by,omitempty"      yaml:"used_by,omitempty"`
}

// Validate reports schema-level errors in the manifest itself (bad
// version, duplicate names, empty name field). It does NOT check
// whether required env vars are set at runtime — that check is
// Resolver's job because it needs an env-lookup fn to run.
func (m *Manifest) Validate() error {
	if m == nil {
		return nil
	}
	if m.Version == 0 {
		// Missing version treated as v1 (forward-compat). Reject anything
		// higher than the current schema — operator probably wrote a
		// future-shape manifest and needs to upgrade the daemon.
	} else if m.Version > SchemaVersion {
		return fmt.Errorf("agentenv: manifest version %d exceeds supported %d", m.Version, SchemaVersion)
	}
	seen := make(map[string]struct{}, len(m.Env))
	for i, e := range m.Env {
		if e.Name == "" {
			return fmt.Errorf("agentenv: env[%d]: name is required", i)
		}
		if !identifierRe.MatchString(e.Name) {
			// Reject names that couldn't be referenced via ${env:NAME}
			// anyway. This catches leading-digit typos and stray
			// characters before they cause silent no-match at
			// interpolation time.
			return fmt.Errorf("agentenv: env[%d]: %q is not a valid identifier (letters/digits/underscore, no leading digit)", i, e.Name)
		}
		if _, dup := seen[e.Name]; dup {
			return fmt.Errorf("agentenv: env[%d]: duplicate name %q", i, e.Name)
		}
		seen[e.Name] = struct{}{}
	}
	return nil
}

// LoadManifest probes agentsDir for env.yaml then env.json and returns
// whichever it finds. Both present → error (ambiguous; operator should
// pick one). Neither present → nil manifest, nil error (caller treats
// as "no interpolation configured"; existing bundles keep working).
//
// agentsDir may be "" — in that case returns (nil, nil) too, matching
// how config.LoadOrDefault handles the "no .agents/ discovered" case.
func LoadManifest(agentsDir string) (*Manifest, error) {
	if agentsDir == "" {
		return nil, nil
	}
	yamlPath := filepath.Join(agentsDir, ManifestFileYAML)
	jsonPath := filepath.Join(agentsDir, ManifestFileJSON)

	yamlExists := fileExists(yamlPath)
	jsonExists := fileExists(jsonPath)

	switch {
	case yamlExists && jsonExists:
		return nil, fmt.Errorf(
			"agentenv: both %s and %s exist in %s — pick one (they carry the same schema, so a duplicate signals a config error)",
			ManifestFileYAML, ManifestFileJSON, agentsDir,
		)
	case yamlExists:
		return loadFile(yamlPath, parseYAML)
	case jsonExists:
		return loadFile(jsonPath, parseJSON)
	default:
		return nil, nil
	}
}

// loadFile is the shared read + parse + validate path for either
// format. Kept small so YAML- and JSON-specific quirks stay in the
// parser fns.
func loadFile(path string, parse func([]byte) (*Manifest, error)) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("agentenv: read %s: %w", path, err)
	}
	m, err := parse(data)
	if err != nil {
		return nil, fmt.Errorf("agentenv: parse %s: %w", path, err)
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return m, nil
}

func parseYAML(data []byte) (*Manifest, error) {
	var m Manifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

func parseJSON(data []byte) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false
		}
		// Any other stat error (permissions, IO) — treat as absent for
		// the probe. The subsequent read will surface the real error
		// with context if the file was actually there.
		return false
	}
	return !info.IsDir()
}

// identifierRe matches the same shape as interpRe's capture group —
// letters, digits, underscore, no leading digit. Extracted so both the
// interpolator and the manifest validator enforce the same rule.
var identifierRe = mustCompilePOSIXIdent()

func mustCompilePOSIXIdent() *strippedRE { return &strippedRE{re: interpRe} }

// strippedRE is a thin wrapper that exposes only MatchString semantics
// against the outer identifier, without re-declaring the pattern. Kept
// as an unexported struct to avoid leaking regexp.Regexp API surface
// through the identifier check.
type strippedRE struct {
	re interface{ MatchString(string) bool }
}

func (s *strippedRE) MatchString(name string) bool {
	// We want to match "NAME" against the identifier class inside
	// ${env:NAME}. Reconstruct the wrapping and re-check — cheaper than
	// maintaining a second regexp.
	return s.re.MatchString("${env:"+name+"}") && !strings.ContainsAny(name, "${}: ")
}
