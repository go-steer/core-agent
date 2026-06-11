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
	"reflect"
	"testing"
)

func TestEncodeMetadata_EmptyReturnsEmpty(t *testing.T) {
	t.Parallel()
	got, err := encodeMetadata(nil)
	if err != nil {
		t.Fatalf("encode nil: %v", err)
	}
	if got != "" {
		t.Errorf("encode(nil): got %q, want empty string (the no-storage signal)", got)
	}

	got, err = encodeMetadata(map[string]string{})
	if err != nil {
		t.Fatalf("encode empty: %v", err)
	}
	if got != "" {
		t.Errorf("encode(empty): got %q, want empty string", got)
	}
}

func TestEncodeMetadata_RoundTrip(t *testing.T) {
	t.Parallel()
	in := map[string]string{
		"caller":   "alice@example.com",
		"proxy_by": "sa:slack-bot",
	}
	encoded, err := encodeMetadata(in)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	out := decodeMetadata(encoded)
	if !reflect.DeepEqual(in, out) {
		t.Errorf("round-trip: got %v, want %v", out, in)
	}
}

func TestDecodeMetadata_EmptyReturnsNil(t *testing.T) {
	t.Parallel()
	// Pre-sidecar rows have the column at its zero value (""). decoder
	// must return nil so the public Entry.Metadata correctly reads as
	// "no metadata recorded for this row."
	if got := decodeMetadata(""); got != nil {
		t.Errorf("decode(\"\"): got %v, want nil", got)
	}
}

func TestDecodeMetadata_MalformedReturnsNil(t *testing.T) {
	t.Parallel()
	// Defensive: a corrupted column must NOT panic and MUST NOT stop
	// the read. The sidecar is best-effort by design.
	got := decodeMetadata("not json at all")
	if got != nil {
		t.Errorf("decode malformed: got %v, want nil", got)
	}
}

func TestDecodeMetadata_EmptyObjectReturnsNil(t *testing.T) {
	t.Parallel()
	// "{}" round-trips to an empty map; we collapse to nil for
	// consistency with the "no metadata" signal.
	got := decodeMetadata("{}")
	if got != nil {
		t.Errorf("decode(\"{}\"): got %v, want nil", got)
	}
}

func TestMetadataKeys_Stable(t *testing.T) {
	t.Parallel()
	// These constants are read by audit consumers. Changing them is a
	// wire-format break — fail loudly if someone renames them.
	if MetadataKeyCaller != "caller" {
		t.Errorf("MetadataKeyCaller changed to %q; audit consumers expect %q", MetadataKeyCaller, "caller")
	}
	if MetadataKeyProxyBy != "proxy_by" {
		t.Errorf("MetadataKeyProxyBy changed to %q; audit consumers expect %q", MetadataKeyProxyBy, "proxy_by")
	}
}
