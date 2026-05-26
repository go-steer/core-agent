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
	"encoding/json"
	"strings"
	"testing"
)

func TestPathScopeConfig_RoundTripAllowPaths(t *testing.T) {
	t.Parallel()
	body := `{
		"version": 1,
		"model": {"name": "claude-opus-4-7"},
		"path_scope": {
			"allow": ["/var/log/myapp/..."],
			"allow_paths": [
				{"path": "/home/me/sibling-repo", "access": "rw"},
				{"path": "/home/me/notes", "access": "r"}
			]
		}
	}`
	var cfg Config
	if err := json.Unmarshal([]byte(body), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if len(cfg.PathScope.Allow) != 1 || cfg.PathScope.Allow[0] != "/var/log/myapp/..." {
		t.Errorf("legacy Allow not preserved: %+v", cfg.PathScope.Allow)
	}
	if len(cfg.PathScope.AllowPaths) != 2 {
		t.Fatalf("expected 2 typed AllowPaths, got %d", len(cfg.PathScope.AllowPaths))
	}
	if cfg.PathScope.AllowPaths[0].Access != "rw" {
		t.Errorf("entry 0 access = %q, want rw", cfg.PathScope.AllowPaths[0].Access)
	}
	if cfg.PathScope.AllowPaths[1].Access != "r" {
		t.Errorf("entry 1 access = %q, want r", cfg.PathScope.AllowPaths[1].Access)
	}
}

func TestPathScopeConfig_Validate_BadAccess(t *testing.T) {
	t.Parallel()
	cfg := &Config{
		Model: ModelConfig{Name: "x"},
		PathScope: PathScopeConfig{
			AllowPaths: []PathScopeAllowEntry{
				{Path: "/tmp", Access: "rwx"},
			},
		},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error for bad access spec")
	}
	if !strings.Contains(err.Error(), "rwx") || !strings.Contains(err.Error(), "must be r, w, or rw") {
		t.Errorf("error message should mention bad spec + valid alternatives, got: %v", err)
	}
}

func TestPathScopeConfig_Validate_MissingPath(t *testing.T) {
	t.Parallel()
	cfg := &Config{
		Model: ModelConfig{Name: "x"},
		PathScope: PathScopeConfig{
			AllowPaths: []PathScopeAllowEntry{
				{Path: "", Access: "rw"},
			},
		},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error for empty path")
	}
	if !strings.Contains(err.Error(), "path is required") {
		t.Errorf("error should mention missing path, got: %v", err)
	}
}

func TestPathScopeConfig_Validate_EmptyAllowPathsIsOK(t *testing.T) {
	t.Parallel()
	cfg := &Config{Model: ModelConfig{Name: "x"}}
	if err := cfg.Validate(); err != nil {
		t.Errorf("empty AllowPaths must validate, got: %v", err)
	}
}

func TestPathScopeConfig_Validate_LegacyAllowAlone(t *testing.T) {
	t.Parallel()
	cfg := &Config{
		Model: ModelConfig{Name: "x"},
		PathScope: PathScopeConfig{
			Allow: []string{"/var/log/..."},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("legacy Allow shape must keep validating, got: %v", err)
	}
}

func TestPathScopeConfig_Validate_AcceptsAllAccessForms(t *testing.T) {
	t.Parallel()
	for _, s := range []string{"r", "w", "rw", "read", "write", "readwrite", "READWRITE", " rw "} {
		cfg := &Config{
			Model: ModelConfig{Name: "x"},
			PathScope: PathScopeConfig{
				AllowPaths: []PathScopeAllowEntry{{Path: "/tmp", Access: s}},
			},
		}
		if err := cfg.Validate(); err != nil {
			t.Errorf("access %q should validate, got: %v", s, err)
		}
	}
}
