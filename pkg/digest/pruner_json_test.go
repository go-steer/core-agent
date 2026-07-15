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

package digest

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestPruneJSON_InvalidJSON_PassesThroughWithMetadata(t *testing.T) {
	t.Parallel()
	// The pruner is best-effort. Malformed JSON returns as-is with a
	// parse_error hint in metadata so telemetry can see how often the
	// router misclassifies.
	digest, meta := PruneJSON([]byte(`{"unterminated":`))
	if digest != `{"unterminated":` {
		t.Errorf("malformed input should pass through, got %q", digest)
	}
	if _, ok := meta["parse_error"]; !ok {
		t.Errorf("expected parse_error metadata, got %+v", meta)
	}
}

func TestPruneJSON_ScalarsPassThrough(t *testing.T) {
	t.Parallel()
	// Numbers, bools, and null are already minimal — the pruner
	// shouldn't touch them.
	cases := []string{`42`, `3.14`, `true`, `false`, `null`, `"short"`}
	for _, in := range cases {
		digest, meta := PruneJSON([]byte(in))
		if digest != in {
			t.Errorf("scalar %s → %q, want unchanged", in, digest)
		}
		if len(meta) != 0 {
			t.Errorf("scalar %s should have no metadata, got %+v", in, meta)
		}
	}
}

func TestPruneJSON_IdentifierKeysNeverTruncated(t *testing.T) {
	t.Parallel()
	// Identifier-shaped keys carry semantic identity — losing the tail
	// of a URL or an ID destroys the whole point. The pruner MUST keep
	// them verbatim regardless of length.
	longVal := strings.Repeat("x", MaxStringChars+100)
	input := fmt.Sprintf(`{
	  "id":         %q,
	  "user_id":    %q,
	  "name":       %q,
	  "status":     %q,
	  "self_url":   %q,
	  "resourceVersion": %q,
	  "some_uri":   %q,
	  "kind":       %q,
	  "type":       %q,
	  "error":      %q,
	  "code":       %q,
	  "apiVersion": %q,
	  "namespace":  %q,
	  "uid":        %q,
	  "prose_body": %q
	}`, longVal, longVal, longVal, longVal, longVal, longVal, longVal,
		longVal, longVal, longVal, longVal, longVal, longVal, longVal, longVal)

	digest, meta := PruneJSON([]byte(input))

	var got map[string]any
	if err := json.Unmarshal([]byte(digest), &got); err != nil {
		t.Fatalf("digest is not valid JSON: %v", err)
	}
	idKeys := []string{"id", "user_id", "name", "status", "self_url",
		"resourceVersion", "some_uri", "kind", "type", "error", "code",
		"apiVersion", "namespace", "uid"}
	for _, k := range idKeys {
		if got[k] != longVal {
			t.Errorf("identifier key %q was truncated: %v", k, got[k])
		}
	}
	// The one non-identifier key ("prose_body") MUST be truncated.
	if got["prose_body"] == longVal {
		t.Errorf("prose_body should have been truncated")
	}
	if truncated, _ := meta["strings_truncated"].(int); truncated != 1 {
		t.Errorf("strings_truncated = %v, want 1 (only prose_body)", meta["strings_truncated"])
	}
}

func TestPruneJSON_LongStringTruncated(t *testing.T) {
	t.Parallel()
	longVal := strings.Repeat("a", MaxStringChars+50)
	input := fmt.Sprintf(`{"prose": %q}`, longVal)
	digest, meta := PruneJSON([]byte(input))

	var got map[string]any
	if err := json.Unmarshal([]byte(digest), &got); err != nil {
		t.Fatalf("invalid digest: %v", err)
	}
	s, _ := got["prose"].(string)
	if !strings.HasPrefix(s, "<truncated,") {
		t.Errorf("prose value not truncated: %q", s)
	}
	if !strings.Contains(s, fmt.Sprintf("%d", MaxStringChars+50)) {
		t.Errorf("truncation marker should include original length: %q", s)
	}
	if truncated, _ := meta["strings_truncated"].(int); truncated != 1 {
		t.Errorf("strings_truncated = %v, want 1", meta["strings_truncated"])
	}
}

func TestPruneJSON_ShortStringUntouched(t *testing.T) {
	t.Parallel()
	// Strings under the cap pass through unchanged — and produce no
	// telemetry, so operators can tell the pruner "did nothing" from
	// "did something" cases.
	input := `{"prose":"short enough"}`
	digest, meta := PruneJSON([]byte(input))
	if !strings.Contains(digest, `"short enough"`) {
		t.Errorf("short string was mangled: %q", digest)
	}
	if _, ok := meta["strings_truncated"]; ok {
		t.Errorf("no truncation should have been recorded: %+v", meta)
	}
}

func TestPruneJSON_LongArrayCollapsedWithHeadAndTail(t *testing.T) {
	t.Parallel()
	// Arrays over MaxArrayElems collapse to a _summary object that
	// preserves the first N/2 and last N/2 items — enough signal to
	// tell sorted / paginated views apart from cold dumps.
	total := MaxArrayElems*2 + 5
	items := make([]any, total)
	for i := range items {
		items[i] = i
	}
	raw, _ := json.Marshal(items)

	digest, meta := PruneJSON(raw)
	if collapsed, _ := meta["arrays_collapsed"].(int); collapsed != 1 {
		t.Errorf("arrays_collapsed = %v, want 1", meta["arrays_collapsed"])
	}
	var summary map[string]any
	if err := json.Unmarshal([]byte(digest), &summary); err != nil {
		t.Fatalf("collapsed array should be a JSON object, got %q: %v", digest, err)
	}
	if summary["_summary"] != true {
		t.Errorf("missing _summary marker: %+v", summary)
	}
	if int(summary["total"].(float64)) != total {
		t.Errorf("total = %v, want %d", summary["total"], total)
	}
	if int(summary["dropped"].(float64)) != total-MaxArrayElems {
		t.Errorf("dropped = %v, want %d", summary["dropped"], total-MaxArrayElems)
	}
	first, _ := summary["first"].([]any)
	last, _ := summary["last"].([]any)
	if len(first) != MaxArrayElems/2 || len(last) != MaxArrayElems/2 {
		t.Errorf("first/last sizes = %d/%d, want %d/%d",
			len(first), len(last), MaxArrayElems/2, MaxArrayElems/2)
	}
	// First slice should be the actual head (0..half-1); last slice
	// the actual tail. Guards against a slicing off-by-one that
	// would render the summary meaningless.
	if int(first[0].(float64)) != 0 {
		t.Errorf("first[0] = %v, want 0", first[0])
	}
	if int(last[len(last)-1].(float64)) != total-1 {
		t.Errorf("last[last] = %v, want %d", last[len(last)-1], total-1)
	}
}

func TestPruneJSON_SmallArrayUntouched(t *testing.T) {
	t.Parallel()
	items := make([]any, MaxArrayElems) // exactly at the cap
	for i := range items {
		items[i] = i
	}
	raw, _ := json.Marshal(items)
	digest, meta := PruneJSON(raw)
	var got []any
	if err := json.Unmarshal([]byte(digest), &got); err != nil {
		t.Fatalf("small array should stay an array, got %q: %v", digest, err)
	}
	if len(got) != MaxArrayElems {
		t.Errorf("small array size = %d, want %d", len(got), MaxArrayElems)
	}
	if _, collapsed := meta["arrays_collapsed"]; collapsed {
		t.Errorf("cap-sized array should not report collapsed, got %+v", meta)
	}
}

func TestPruneJSON_DepthCapCollapsesSubtree(t *testing.T) {
	t.Parallel()
	// Build a nested object deeper than MaxDepth so the recursion
	// guard fires. Levels beyond the cap collapse to a marker string.
	nested := any("leaf")
	for i := 0; i < MaxDepth+5; i++ {
		nested = map[string]any{"child": nested}
	}
	raw, _ := json.Marshal(nested)

	digest, meta := PruneJSON(raw)
	if dropped, _ := meta["subtrees_dropped"].(int); dropped == 0 {
		t.Errorf("expected subtrees_dropped > 0, got %+v", meta)
	}
	if !strings.Contains(digest, "deep subtree") {
		t.Errorf("digest should carry the deep-subtree marker: %s", digest)
	}
}

func TestPruneJSON_Idempotent(t *testing.T) {
	t.Parallel()
	// Second pass over an already-pruned digest must produce an
	// identical result — the compactor + summarizer may re-run
	// digesting during context management, and re-truncating a
	// truncation marker would inflate the count spuriously.
	longVal := strings.Repeat("z", MaxStringChars+200)
	items := make([]any, MaxArrayElems*3)
	for i := range items {
		items[i] = i
	}
	input := map[string]any{
		"id":     "abc123",
		"prose":  longVal,
		"list":   items,
		"nested": map[string]any{"deep_prose": longVal},
	}
	raw, _ := json.Marshal(input)

	pass1, _ := PruneJSON(raw)
	pass2, meta2 := PruneJSON([]byte(pass1))

	if pass1 != pass2 {
		t.Errorf("PruneJSON not idempotent:\npass1: %s\npass2: %s", pass1, pass2)
	}
	// Second pass should record no NEW truncations (the strings are
	// already markers, the arrays are already collapsed).
	if truncated, _ := meta2["strings_truncated"].(int); truncated > 0 {
		t.Errorf("second pass re-truncated %d strings — should be 0", truncated)
	}
}

func TestPruneJSON_NestedIdentifierPreservationSurvivesRecursion(t *testing.T) {
	t.Parallel()
	// Identifier-key preservation must fire at every level, not just
	// the top. A nested config object with an id field 12 levels down
	// should still get its id preserved (subject to the depth cap).
	longVal := strings.Repeat("v", MaxStringChars+10)
	input := map[string]any{
		"level1": map[string]any{
			"level2": map[string]any{
				"level3": map[string]any{
					"id":    longVal,
					"prose": longVal,
				},
			},
		},
	}
	raw, _ := json.Marshal(input)
	digest, _ := PruneJSON(raw)

	// The `id` value must still be present verbatim. The `prose`
	// value must be truncated.
	if !strings.Contains(digest, longVal) {
		t.Errorf("nested identifier value was truncated: %s", digest)
	}
	if strings.Count(digest, longVal) != 1 {
		t.Errorf("expected exactly one verbatim longVal (the id), got %d occurrences",
			strings.Count(digest, longVal))
	}
}

func TestPruneJSON_ArrayOfObjectsHeadTailStillPrunesRecursively(t *testing.T) {
	t.Parallel()
	// Head+tail elements of a collapsed array must themselves be
	// pruned — otherwise a huge array of huge objects still blows
	// past the token budget.
	longVal := strings.Repeat("q", MaxStringChars+10)
	items := make([]any, MaxArrayElems*2)
	for i := range items {
		items[i] = map[string]any{
			"id":    fmt.Sprintf("id-%d", i),
			"prose": longVal,
		}
	}
	raw, _ := json.Marshal(items)

	digest, meta := PruneJSON(raw)
	if _, collapsed := meta["arrays_collapsed"]; !collapsed {
		t.Errorf("expected arrays_collapsed, got %+v", meta)
	}
	// Each element in the head+tail should have had its prose value
	// truncated. Full count = MaxArrayElems (half in first, half in
	// last).
	if truncated, _ := meta["strings_truncated"].(int); truncated != MaxArrayElems {
		t.Errorf("expected %d string truncations across head+tail, got %v",
			MaxArrayElems, meta["strings_truncated"])
	}
	// The identifier id-0 (head) and id-{len-1} (tail) MUST survive
	// verbatim so operators can navigate paginated views.
	if !strings.Contains(digest, `"id-0"`) {
		t.Errorf("head element's id missing from digest")
	}
	if !strings.Contains(digest, fmt.Sprintf(`"id-%d"`, len(items)-1)) {
		t.Errorf("tail element's id missing from digest")
	}
}
