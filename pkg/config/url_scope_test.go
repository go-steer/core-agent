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
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestURLScopeConfig_RoundTrip(t *testing.T) {
	t.Parallel()

	body := `{
		"version": 1,
		"model": { "name": "claude-opus-4-7" },
		"url_scope": {
			"allow": ["api.github.com", "*.googleapis.com", "http://localhost:*"],
			"deny":  ["*.internal.evil.com"],
			"max_body_bytes":   131072,
			"timeout_seconds":  45,
			"headers": {
				"api.github.com": {
					"Authorization": "Bearer ${GITHUB_TOKEN}",
					"Accept":        "application/vnd.github+json"
				}
			}
		}
	}`

	dir := t.TempDir()
	path := filepath.Join(dir, ConfigFileName)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	want := URLScopeConfig{
		Allow:          []string{"api.github.com", "*.googleapis.com", "http://localhost:*"},
		Deny:           []string{"*.internal.evil.com"},
		MaxBodyBytes:   131072,
		TimeoutSeconds: 45,
		Headers: map[string]map[string]string{
			"api.github.com": {
				"Authorization": "Bearer ${GITHUB_TOKEN}", // unexpanded at load time
				"Accept":        "application/vnd.github+json",
			},
		},
	}
	if !reflect.DeepEqual(cfg.URLScope, want) {
		t.Errorf("URLScope round-trip mismatch:\n got:  %+v\n want: %+v", cfg.URLScope, want)
	}
}

func TestURLScopeConfig_OmittedSection(t *testing.T) {
	t.Parallel()

	body := `{ "version": 1, "model": { "name": "claude-opus-4-7" } }`
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ConfigFileName), []byte(body), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reflect.DeepEqual(cfg.URLScope, URLScopeConfig{}) {
		t.Errorf("URLScope should be zero value when absent, got: %+v", cfg.URLScope)
	}
}
