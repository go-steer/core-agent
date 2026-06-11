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

package eventlog

import (
	"encoding/json"
)

// MetadataKey* are the well-known keys agent.Agent uses when wiring
// the per-request caller context into the eventlog metadata sidecar.
// They're exported so audit consumers can read what's there without
// guessing at conventions.
const (
	// MetadataKeyCaller is the effective Caller.Identity that
	// originated the turn this event belongs to. Empty when no auth
	// context was available (legacy / single-user / out-of-band code
	// paths).
	MetadataKeyCaller = "caller"
	// MetadataKeyProxyBy is set when the effective Caller was
	// asserted via the proxy path (X-Asserted-Caller header): records
	// the proxying identity (e.g., "sa:slack-bot"). Empty for direct
	// authentication.
	MetadataKeyProxyBy = "proxy_by"
)

// encodeMetadata serializes the sidecar map for storage. We use JSON
// (vs. a separate key-value table) so the on-disk layout stays a
// single overlay row per event, and rows written before the sidecar
// shipped read back as empty Metadata after decodeMetadata returns
// nil for the empty string.
//
// Returns "" + nil when md is empty so we don't pay storage for the
// common no-metadata case.
func encodeMetadata(md map[string]string) (string, error) {
	if len(md) == 0 {
		return "", nil
	}
	b, err := json.Marshal(md)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// decodeMetadata parses the sidecar JSON back into a map. Empty input
// (which covers pre-sidecar rows where the column was added by
// AutoMigrate with the default "") returns nil so consumers can
// distinguish "no metadata" from "empty metadata."
//
// Malformed JSON returns nil rather than panicking — the sidecar is
// best-effort by design (lossy if the column is corrupted, never
// stop a read).
func decodeMetadata(s string) map[string]string {
	if s == "" {
		return nil
	}
	var out map[string]string
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
