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

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// store is the on-disk contract between the generator, the checker
// and the preflight library — bouncer's SESSION_STATE_DIR /
// PERSISTENT_DIR split (shared_tools/config.py), minus the ambient
// global state.
//
// Upstream, the two agents are separate OS processes that agree on
// paths through environment variables, and the generator learns the
// checker's answer by grepping "success: True" out of the checker
// subprocess's stdout. In-process both agents share this struct, and
// the verdict comes back as a typed value (see verdict.go).
type store struct {
	// sessionDir holds the per-derivation scratch: source.yaml,
	// candidate.yaml, and the experience log.
	sessionDir string
	// libraryDir is the durable preflight library the checker writes
	// verified manifests into.
	libraryDir string
}

const (
	sourceFile     = "source.yaml"
	candidateFile  = "candidate.yaml"
	experienceFile = "experience.log"
)

func newStore(stateDir, libraryDir, sessionID string) (*store, error) {
	s := &store{
		sessionDir: filepath.Join(stateDir, "sessions", sessionID),
		libraryDir: libraryDir,
	}
	for _, dir := range []string{s.sessionDir, s.libraryDir} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("store: mkdir %s: %w", dir, err)
		}
	}
	return s, nil
}

func (s *store) writeSource(manifest string) error {
	return s.write(sourceFile, manifest)
}

func (s *store) readSource() (string, error) { return s.read(sourceFile) }

func (s *store) writeCandidate(manifest string) error {
	return s.write(candidateFile, manifest)
}

func (s *store) readCandidate() (string, error) { return s.read(candidateFile) }

func (s *store) write(name, body string) error {
	switch name {
	case sourceFile, candidateFile, experienceFile:
	default:
		// Unreachable today — every caller passes a package constant —
		// but the scratch dir is the one place a model-authored string
		// must never become a filename, so say so in code.
		return fmt.Errorf("store: refusing to write unexpected scratch file %q", name)
	}
	path := filepath.Join(s.sessionDir, name)
	// #nosec G703 -- name is one of the three constants checked above;
	// sessionDir comes from the operator's --state-dir, not the model.
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		return fmt.Errorf("store: write %s: %w", name, err)
	}
	return nil
}

func (s *store) read(name string) (string, error) {
	body, err := os.ReadFile(filepath.Join(s.sessionDir, name)) // #nosec G304 -- name is a package constant
	if err != nil {
		return "", fmt.Errorf("store: read %s: %w", name, err)
	}
	return string(body), nil
}

// appendExperience records a lesson learned. bouncer scatters one
// uuid-named file per entry under memories/ephemeral_logs/raw and
// greps them later; a single append-only log is the same capability
// with less filesystem.
func (s *store) appendExperience(topic, note string) error {
	line := fmt.Sprintf("%s\t%s\t%s\n",
		time.Now().UTC().Format(time.RFC3339), strings.TrimSpace(topic), strings.TrimSpace(note))
	f, err := os.OpenFile(filepath.Join(s.sessionDir, experienceFile),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("store: open experience log: %w", err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(line); err != nil {
		return fmt.Errorf("store: append experience log: %w", err)
	}
	return nil
}

// libraryEntry is one verified preflight.
type libraryEntry struct {
	Name        string    `json:"name"`
	Features    []string  `json:"features,omitempty"`
	TargetLabel string    `json:"target_label,omitempty"`
	Metadata    string    `json:"metadata,omitempty"`
	SourceJob   string    `json:"source_job,omitempty"`
	SavedAt     time.Time `json:"saved_at"`
}

// saveLibraryEntry writes the verified manifest plus its metadata
// sidecar and returns the manifest path.
func (s *store) saveLibraryEntry(e libraryEntry, manifest string) (string, error) {
	slug := slugify(e.Name)
	if slug == "" {
		return "", fmt.Errorf("store: preflight name %q has no usable characters", e.Name)
	}
	if strings.TrimSpace(manifest) == "" {
		return "", fmt.Errorf("store: refusing to save an empty manifest for %q", e.Name)
	}
	e.SavedAt = time.Now().UTC()

	manifestPath := filepath.Join(s.libraryDir, slug+".yaml")
	if err := os.WriteFile(manifestPath, []byte(manifest), 0o600); err != nil {
		return "", fmt.Errorf("store: write library manifest: %w", err)
	}
	meta, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return "", fmt.Errorf("store: marshal library metadata: %w", err)
	}
	if err := os.WriteFile(filepath.Join(s.libraryDir, slug+".json"), append(meta, '\n'), 0o600); err != nil {
		return "", fmt.Errorf("store: write library metadata: %w", err)
	}
	return manifestPath, nil
}

// listLibrary returns the slugs currently in the library, so the
// generator can check for a reusable preflight before deriving a new
// one (bouncer's "CRITICAL EFFICIENCY RULE").
func (s *store) listLibrary() ([]string, error) {
	entries, err := os.ReadDir(s.libraryDir)
	if err != nil {
		return nil, fmt.Errorf("store: read library: %w", err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		names = append(names, strings.TrimSuffix(e.Name(), ".yaml"))
	}
	return names, nil
}

var slugUnsafe = regexp.MustCompile(`[^a-z0-9]+`)

// slugify reduces a model-authored preflight name to a safe filename.
// The name reaches us straight off a tool call, so "../../etc/cron.d/
// pwn" must not become a path — this is the single most important
// line in the file.
func slugify(name string) string {
	s := slugUnsafe.ReplaceAllString(strings.ToLower(name), "-")
	s = strings.Trim(s, "-")
	const maxSlug = 80
	if len(s) > maxSlug {
		s = strings.Trim(s[:maxSlug], "-")
	}
	return s
}

// checkerInstructionLine matches the annotation the generator is
// allowed to leave for the checker inside the candidate YAML.
// bouncer strips these before submitting so they never reach the API
// server (generator/agent.py: re.sub of `^\s*checker-instruction:.*$`).
var checkerInstructionLine = regexp.MustCompile(`(?m)^[ \t]*checker-instruction:.*$[\r\n]?`)

func stripCheckerInstructions(manifest string) string {
	return checkerInstructionLine.ReplaceAllString(manifest, "")
}
