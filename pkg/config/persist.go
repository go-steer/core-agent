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
	"io/fs"
	"os"
	"path/filepath"
)

// defaultConfigFileMode is the mode Save uses when creating a config
// file that does not already exist. 0600 (owner read/write only)
// because the schema invites secrets in — model.api_key,
// model.anthropic.api_key — so a world-readable default would leak
// them. Existing files keep whatever mode they already carry (Save
// never silently re-chmods a file the operator may have tightened or
// loosened on purpose).
const defaultConfigFileMode fs.FileMode = 0o600

// Save writes cfg to path atomically (temp file in the same directory
// followed by rename). The output is human-edit-friendly: stable key
// order, two-space indentation, trailing newline.
//
// Save is safe for round-tripping a config the current binary only
// partially understands:
//
//   - Unknown top-level keys already on disk (e.g. a section written by
//     a newer binary) are preserved rather than dropped. Load →
//     mutate → Save by an older binary no longer deletes a newer
//     binary's fields.
//   - The original file mode is retained; a fresh file is created 0600
//     (the schema can hold API keys). Save never re-chmods to a wider
//     mode the way the previous hardcoded 0644 did.
//   - Default-valued sections that were not already present on disk are
//     NOT materialized. Writing a partial config keeps it partial, so a
//     future bump to a substrate default (e.g. the default model) still
//     reaches operators who never pinned that value.
//
// Save does not validate; callers should run cfg.Validate() first.
func Save(path string, cfg *Config) error {
	if path == "" {
		return fmt.Errorf("config: empty target path")
	}
	if cfg == nil {
		return fmt.Errorf("config: nil config")
	}

	// Read the existing file (if any) so we can preserve unknown keys
	// and its mode. Missing file is fine — we create one fresh.
	mode := defaultConfigFileMode
	var existing []byte
	if b, err := os.ReadFile(path); err == nil {
		existing = b
		if info, serr := os.Stat(path); serr == nil {
			mode = info.Mode().Perm()
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("config: read %q: %w", path, err)
	}

	data, err := marshalForSave(existing, cfg)
	if err != nil {
		return fmt.Errorf("config: marshal: %w", err)
	}
	return atomicWrite(path, data, mode)
}

// marshalForSave produces the on-disk bytes for cfg while honoring the
// round-trip guarantees documented on Save. existing is the current
// file content (nil when the file does not exist yet).
func marshalForSave(existing []byte, cfg *Config) ([]byte, error) {
	// Ordered view of what's currently on disk (preserves unknown keys
	// and their position). Empty when there's no existing file.
	orig, err := parseOrderedObject(existing)
	if err != nil {
		return nil, fmt.Errorf("existing file is not a JSON object: %w", err)
	}
	origKeys := make(map[string]bool, len(orig))
	for _, e := range orig {
		origKeys[e.key] = true
	}

	// The full serialization of the (defaults-merged) config, in struct
	// field order.
	fullBytes, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	full, err := parseOrderedObject(fullBytes)
	if err != nil {
		return nil, err
	}
	fullByKey := make(map[string]json.RawMessage, len(full))
	for _, e := range full {
		fullByKey[e.key] = e.val
	}

	// The serialization of pristine defaults, for the "is this section
	// just a default?" comparison. Both sides come from json.Marshal of
	// the same struct type, so byte-equality is a reliable check.
	defBytes, err := json.Marshal(DefaultConfig())
	if err != nil {
		return nil, err
	}
	defByKey := make(map[string]json.RawMessage)
	{
		def, derr := parseOrderedObject(defBytes)
		if derr != nil {
			return nil, derr
		}
		for _, e := range def {
			defByKey[e.key] = e.val
		}
	}

	var out []kv
	// 1. Walk the existing file in order. Known keys get the current
	//    (possibly mutated) value; unknown keys are preserved verbatim.
	for _, e := range orig {
		if v, ok := fullByKey[e.key]; ok {
			out = append(out, kv{key: e.key, val: v})
		} else {
			out = append(out, e)
		}
	}
	// 2. Append keys the struct produced that weren't already on disk,
	//    in struct field order. Skip sections that are identical to
	//    their default — materializing them would pin today's defaults
	//    and defeat future bumps. `version` is always emitted so the
	//    file stays self-describing (it's the schema anchor, not a
	//    tunable default).
	for _, e := range full {
		if origKeys[e.key] {
			continue
		}
		if e.key != "version" && bytes.Equal(e.val, defByKey[e.key]) {
			continue
		}
		out = append(out, e)
	}

	return encodeOrderedObject(out)
}

// kv is one key/value pair from a JSON object, retaining the raw value
// bytes so unknown keys survive a round-trip untouched.
type kv struct {
	key string
	val json.RawMessage
}

// parseOrderedObject decodes a JSON object into an ordered slice of
// key/value pairs, preserving key insertion order. nil/empty input
// yields an empty slice (not an error). A non-object top-level value is
// an error.
func parseOrderedObject(data []byte) ([]kv, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, fmt.Errorf("expected JSON object, got %v", tok)
	}
	var out []kv
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := keyTok.(string)
		if !ok {
			return nil, fmt.Errorf("expected string key, got %v", keyTok)
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, err
		}
		out = append(out, kv{key: key, val: raw})
	}
	// Consume closing '}'.
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	return out, nil
}

// encodeOrderedObject renders pairs as a human-edit-friendly JSON
// object: two-space indentation, one key per line, trailing newline.
func encodeOrderedObject(pairs []kv) ([]byte, error) {
	var buf bytes.Buffer
	if len(pairs) == 0 {
		buf.WriteString("{}\n")
		return buf.Bytes(), nil
	}
	buf.WriteString("{\n")
	for i, e := range pairs {
		keyBytes, err := json.Marshal(e.key)
		if err != nil {
			return nil, err
		}
		var val bytes.Buffer
		if err := json.Indent(&val, e.val, "  ", "  "); err != nil {
			return nil, err
		}
		buf.WriteString("  ")
		buf.Write(keyBytes)
		buf.WriteString(": ")
		buf.Write(val.Bytes())
		if i < len(pairs)-1 {
			buf.WriteByte(',')
		}
		buf.WriteByte('\n')
	}
	buf.WriteString("}\n")
	return buf.Bytes(), nil
}

func atomicWrite(path string, data []byte, mode fs.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".core-agent-config-*")
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
	return os.Rename(tmpName, path)
}
