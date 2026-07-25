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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSave_PreservesUnknownTopLevelKeys is the core regression guard for
// #387: an older binary that loads → mutates → saves must NOT drop a
// top-level section written by a newer binary.
func TestSave_PreservesUnknownTopLevelKeys(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, ConfigFileName)

	// A file written by a "newer" binary carrying a section this build
	// doesn't know about, plus a section it does.
	orig := `{
  "version": 1,
  "permissions": {
    "allow": ["read_file"]
  },
  "future_feature": {
    "knob": 42,
    "nested": {"deep": true}
  }
}`
	if err := os.WriteFile(path, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Simulate the "always allow" TUI flow: append a permission.
	cfg.Permissions.Allow = append(cfg.Permissions.Allow, "write_file")
	if err := Save(path, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("saved file is not valid JSON: %v\n%s", err, raw)
	}
	// Unknown section survived, byte-for-byte in content.
	ff, ok := got["future_feature"]
	if !ok {
		t.Fatalf("future_feature section was dropped:\n%s", raw)
	}
	var ffObj struct {
		Knob   int `json:"knob"`
		Nested struct {
			Deep bool `json:"deep"`
		} `json:"nested"`
	}
	if err := json.Unmarshal(ff, &ffObj); err != nil {
		t.Fatalf("future_feature not preserved as-is: %v", err)
	}
	if ffObj.Knob != 42 || !ffObj.Nested.Deep {
		t.Errorf("future_feature content corrupted: %+v", ffObj)
	}
	// The mutation landed.
	if !strings.Contains(string(got["permissions"]), "write_file") {
		t.Errorf("permission mutation not persisted:\n%s", raw)
	}
}

// TestSave_DoesNotMaterializeDefaults verifies that saving a partial
// config keeps it partial — default sections the operator never set
// must not be pinned to disk, or a future default bump stops applying.
func TestSave_DoesNotMaterializeDefaults(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, ConfigFileName)

	// Operator's file only sets permissions; no model section.
	orig := `{"version":1,"permissions":{"allow":["read_file"]}}`
	if err := os.WriteFile(path, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Sanity: Load merged the default model into memory.
	if cfg.Model.Name == "" {
		t.Fatal("expected default model in memory")
	}
	cfg.Permissions.Deny = append(cfg.Permissions.Deny, "bash")
	if err := Save(path, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["model"]; ok {
		t.Errorf("default model section was materialized into a partial config:\n%s", raw)
	}
	if _, ok := got["agent"]; ok {
		t.Errorf("default agent section was materialized:\n%s", raw)
	}
	if _, ok := got["tool_output"]; ok {
		t.Errorf("default tool_output section was materialized:\n%s", raw)
	}
	// The file still loads and yields the current default model — i.e. a
	// future default bump would reach this operator.
	reloaded, err := Load(dir)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.Model.Name != DefaultConfig().Model.Name {
		t.Errorf("model no longer tracks the default: got %q", reloaded.Model.Name)
	}
}

// TestSave_NewFileMode0600 verifies a freshly created config file is
// owner-only (the schema can hold api_key).
func TestSave_NewFileMode0600(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, ConfigFileName)
	cfg := DefaultConfig()
	cfg.Permissions.Allow = []string{"read_file"}
	if err := Save(path, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("new config file mode = %o, want 0600", got)
	}
}

// TestSave_RetainsExistingMode verifies Save does not silently re-chmod
// an existing file (the old 0644 hardcode bug).
func TestSave_RetainsExistingMode(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, ConfigFileName)
	if err := os.WriteFile(path, []byte(`{"version":1,"permissions":{"allow":["x"]}}`), 0o640); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cfg.Permissions.Allow = append(cfg.Permissions.Allow, "y")
	if err := Save(path, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o640 {
		t.Errorf("existing file mode = %o, want retained 0640", got)
	}
}

// TestSave_RoundTripReloads asserts the saved file is valid and reloads
// to an equivalent config (no corruption from the ordered emitter).
func TestSave_RoundTripReloads(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, ConfigFileName)
	body := `{"version":1,"model":{"name":"gemini-3-flash-preview","provider":"gemini"},"permissions":{"mode":"ask","allow":["a","b"]},"compaction":{"threshold":0.5}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := Save(path, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}
	reloaded, err := Load(dir)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.Model.Name != "gemini-3-flash-preview" || reloaded.Model.Provider != "gemini" {
		t.Errorf("model not round-tripped: %+v", reloaded.Model)
	}
	if reloaded.Compaction.Threshold == nil || *reloaded.Compaction.Threshold != 0.5 {
		t.Errorf("compaction.threshold not round-tripped: %v", reloaded.Compaction.Threshold)
	}
}

// TestLoad_WarnsOnUnknownKey covers the security-relevant typo case: a
// misspelled key must not be silently ignored.
func TestLoad_WarnsOnUnknownKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ConfigFileName)
	// A mistyped top-level key ("denies" instead of nesting deny under
	// permissions) — the real gate never gets configured.
	body := `{"version":1,"model":{"name":"gemini-3.1-pro-preview"},"denies":["bash"]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	old := warnw
	warnw = &buf
	defer func() { warnw = old }()

	if _, err := Load(dir); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !strings.Contains(buf.String(), "denies") {
		t.Errorf("expected warning naming the mistyped key; got %q", buf.String())
	}
}
