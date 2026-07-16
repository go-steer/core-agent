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

package pricing

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// SchemaVersion is the on-disk schema version for pricing files.
// Bumped only on incompatible schema changes; new fields with
// `omitempty` don't require a bump.
const SchemaVersion = 1

// ModelRates is the on-disk per-model rate, mirroring config's
// PricingConfig field names so operator-edited files use the same
// spelling regardless of whether they live in pricing.json or in
// .agents/config.json's `model.pricing` override map.
type ModelRates struct {
	InputPerMTok       float64   `json:"input_per_mtok,omitempty"`
	CachedInputPerMTok float64   `json:"cached_input_per_mtok,omitempty"`
	OutputPerMTok      float64   `json:"output_per_mtok,omitempty"`
	UpdatedAt          time.Time `json:"updated_at,omitempty"`
}

// rates converts the JSON-tagged form into the internal Rates type.
// Field order + names are identical so a direct conversion suffices.
func (m ModelRates) rates() Rates { return Rates(m) }

// ProjectFile is the .agents/pricing.json shape — flat models map.
// Project files are always operator-curated (never auto-fetched), so
// no manual/external split is needed.
type ProjectFile struct {
	Version int                   `json:"version"`
	Models  map[string]ModelRates `json:"models,omitempty"`
}

// UserFile is the ~/.core-agent/pricing.json shape — sectioned so
// PR B's daily refresh can overwrite the `external` section without
// touching `manual` entries the operator hand-edited (or set via
// /pricing set in PR C).
type UserFile struct {
	Version  int             `json:"version"`
	External *ExternalSource `json:"external,omitempty"`
	Manual   *ManualSection  `json:"manual,omitempty"`
}

// ExternalSource is the auto-fetched section. Populated by PR B's
// LiteLLM refresh; unused in PR A. The fetched_at + etag fields
// drive cache-validity logic (skip refresh if <24h, send
// If-None-Match on revalidation).
type ExternalSource struct {
	FetchedAt time.Time             `json:"fetched_at"`
	Source    string                `json:"source"` // canonical URL the data was pulled from
	ETag      string                `json:"etag,omitempty"`
	Models    map[string]ModelRates `json:"models,omitempty"`
}

// ManualSection is the operator-curated section. Round-trips intact
// across refreshes (PR B's fetcher only rewrites External).
type ManualSection struct {
	Models map[string]ModelRates `json:"models,omitempty"`
}

// LoadProjectFile reads .agents/pricing.json from agentsDir, or
// returns an empty file when the file is missing (a missing project
// file is the common case, not an error). Returns an error for I/O
// failures or malformed JSON.
func LoadProjectFile(agentsDir string) (*ProjectFile, error) {
	if agentsDir == "" {
		return &ProjectFile{Version: SchemaVersion}, nil
	}
	path := filepath.Join(agentsDir, ProjectFileName)
	return loadProjectFile(path)
}

func loadProjectFile(path string) (*ProjectFile, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path resolved from caller-supplied agentsDir
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &ProjectFile{Version: SchemaVersion}, nil
		}
		return nil, fmt.Errorf("pricing: read %s: %w", path, err)
	}
	var pf ProjectFile
	if err := json.Unmarshal(data, &pf); err != nil {
		return nil, fmt.Errorf("pricing: parse %s: %w", path, err)
	}
	if pf.Version == 0 {
		pf.Version = SchemaVersion
	}
	return &pf, nil
}

// LoadUserFile reads ~/.core-agent/pricing.json (or whatever
// userHome resolves to), or returns an empty file when missing.
// Same not-an-error treatment for missing files as LoadProjectFile.
func LoadUserFile(userHome string) (*UserFile, error) {
	if userHome == "" {
		return &UserFile{Version: SchemaVersion}, nil
	}
	path := filepath.Join(userHome, UserFileName)
	return loadUserFile(path)
}

func loadUserFile(path string) (*UserFile, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path resolved from caller-supplied userHome
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &UserFile{Version: SchemaVersion}, nil
		}
		return nil, fmt.Errorf("pricing: read %s: %w", path, err)
	}
	var uf UserFile
	if err := json.Unmarshal(data, &uf); err != nil {
		return nil, fmt.Errorf("pricing: parse %s: %w", path, err)
	}
	if uf.Version == 0 {
		uf.Version = SchemaVersion
	}
	return &uf, nil
}

// SaveUserFile writes uf to <userHome>/<UserFileName> atomically.
// Used by PR B's refresher and PR C's /pricing set slash. Empty
// userHome is an error (caller should resolve a real home first).
func SaveUserFile(userHome string, uf *UserFile) error {
	if userHome == "" {
		return errors.New("pricing: cannot save user file: no userHome")
	}
	if uf.Version == 0 {
		uf.Version = SchemaVersion
	}
	if err := os.MkdirAll(userHome, 0o755); err != nil {
		return fmt.Errorf("pricing: mkdir %s: %w", userHome, err)
	}
	path := filepath.Join(userHome, UserFileName)
	data, err := json.MarshalIndent(uf, "", "  ")
	if err != nil {
		return fmt.Errorf("pricing: marshal: %w", err)
	}
	data = append(data, '\n')
	return atomicWrite(path, data, 0o644)
}

// File names. Exposed so callers can reference them in error
// messages and tests.
const (
	ProjectFileName = "pricing.json"
	UserFileName    = "pricing.json"
)

// atomicWrite is the same temp-file + rename pattern used by the
// transcript writer in internal/tui. Duplicated here rather than
// shared because both copies are tiny and pulling in a sibling
// package for two helpers would invert dependency direction.
func atomicWrite(path string, data []byte, mode fs.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".pricing-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
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
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("pricing: rename: %w", err)
	}
	return nil
}

// lowerKeys returns a copy of m with all keys lowercased + trimmed.
// Used during catalog construction so Lookup can compare without
// per-call ToLower allocations.
func lowerKeys(m map[string]ModelRates) map[string]Rates {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]Rates, len(m))
	for k, v := range m {
		k = strings.ToLower(strings.TrimSpace(k))
		if k == "" {
			continue
		}
		out[k] = v.rates()
	}
	return out
}
