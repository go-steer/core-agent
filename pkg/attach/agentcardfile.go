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

package attach

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// AgentCardFileName is the filename inside .agents/ that the bundled
// core-agent binary reads to populate AgentCardConfig. Embedders
// building their own daemon populate Options.AgentCard directly and
// don't need this file at all.
const AgentCardFileName = "agent-card.json"

// agentCardFile is the on-disk JSON shape. Mirrors AgentCardConfig
// with snake_case field names; agent_version is renamed from version
// so it doesn't collide with the envelope's own schema version.
type agentCardFile struct {
	Version          int                  `json:"version"`
	Name             string               `json:"name,omitempty"`
	Description      string               `json:"description,omitempty"`
	ExternalURL      string               `json:"external_url,omitempty"`
	AgentVersion     string               `json:"agent_version,omitempty"`
	DocumentationURL string               `json:"documentation_url,omitempty"`
	Provider         agentCardFileProv    `json:"provider,omitempty"`
	ExtraSkills      []agentCardFileSkill `json:"extra_skills,omitempty"`
}

type agentCardFileProv struct {
	Organization string `json:"organization,omitempty"`
	URL          string `json:"url,omitempty"`
}

type agentCardFileSkill struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Tags        []string `json:"tags,omitempty"`
	Examples    []string `json:"examples,omitempty"`
}

// LoadAgentCardFile reads the on-disk card config at path and returns
// the equivalent AgentCardConfig. A missing path is not an error —
// the returned config is the zero value (endpoint disabled) and the
// second return value is false.
//
// Malformed JSON or an unknown envelope version is a startup error:
// the file's purpose is the public-discovery surface, and silently
// disabling the endpoint on misconfig is worse than failing closed.
//
// A file that only sets extra_skills (no description) is also
// rejected — the loader refuses to treat the file as a side-channel
// skill library. external_url is optional (the card handler derives
// the URL from each request); set it only when overriding the
// fetch-URL with a canonical alternative.
func LoadAgentCardFile(path string) (AgentCardConfig, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return AgentCardConfig{}, false, nil
		}
		return AgentCardConfig{}, false, fmt.Errorf("attach: read agent-card file %q: %w", path, err)
	}
	var file agentCardFile
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&file); err != nil {
		return AgentCardConfig{}, false, fmt.Errorf("attach: parse agent-card file %q: %w", path, err)
	}
	if file.Version != 1 {
		return AgentCardConfig{}, false, fmt.Errorf("attach: agent-card file %q: unsupported version %d (only version 1 is recognised)", path, file.Version)
	}
	if file.Description == "" && len(file.ExtraSkills) > 0 {
		return AgentCardConfig{}, false, fmt.Errorf("attach: agent-card file %q: extra_skills requires description to be set — the card is the public-discovery surface, not a skill-library side-channel", path)
	}
	cfg := AgentCardConfig{
		Name:             file.Name,
		Description:      file.Description,
		ExternalURL:      file.ExternalURL,
		Version:          file.AgentVersion,
		DocumentationURL: file.DocumentationURL,
		Provider: AgentCardProvider{
			Organization: file.Provider.Organization,
			URL:          file.Provider.URL,
		},
	}
	if len(file.ExtraSkills) > 0 {
		cfg.ExtraSkills = make([]AgentCardSkill, 0, len(file.ExtraSkills))
		for _, s := range file.ExtraSkills {
			cfg.ExtraSkills = append(cfg.ExtraSkills, AgentCardSkill{
				ID:          s.ID,
				Name:        s.Name,
				Description: s.Description,
				Tags:        append([]string(nil), s.Tags...),
				Examples:    append([]string(nil), s.Examples...),
			})
		}
	}
	if err := cfg.Validate(); err != nil {
		return AgentCardConfig{}, false, fmt.Errorf("attach: agent-card file %q: %w", path, err)
	}
	return cfg, true, nil
}
