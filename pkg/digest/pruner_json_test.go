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

// TestPruneJSON_NestedJSONString_SingleValueRecurses pins the load-
// bearing GKE MCP fix: when a long string value under a non-identifier
// key parses cleanly as JSON, the pruner recurses into it and re-
// serializes, so the model sees the inner structure instead of a
// `<truncated, N chars>` marker. Without this, `gke_get_k8s_resource`-
// shape responses (`{"output": {"output": "<JSON as string>"}}`)
// digest to 64 bytes of envelope + zero semantic content — the exact
// pathology that drove the 2026-07-17 demo's runaway list_skills loop
// (turn 11 spent 61k uncached tokens re-orienting because MCP digests
// had no useful data).
func TestPruneJSON_NestedJSONString_SingleValueRecurses(t *testing.T) {
	t.Parallel()
	// Inner cluster object with one long-string field (would be
	// truncated by the recursion) and identifier fields (must survive).
	inner := map[string]any{
		"name":        "prod-cluster-west",
		"status":      "RUNNING",
		"description": strings.Repeat("verbose description ", 100), // >500 chars → truncates
	}
	innerJSON, _ := json.Marshal(inner)
	// Outer envelope in the GKE MCP shape: text-content wrapping.
	outer := map[string]any{
		"output": map[string]any{
			"output": string(innerJSON),
		},
	}
	raw, _ := json.Marshal(outer)

	digest, meta := PruneJSON(raw)

	// The nested-JSON expansion counter MUST fire.
	if got := meta["nested_json_expanded"]; got != 1 {
		t.Errorf("nested_json_expanded = %v, want 1 (was the inner JSON string not detected?)", got)
	}
	// Identifier fields from the INNER object must appear verbatim
	// after recursion — the whole point of the fix.
	if !strings.Contains(digest, `"prod-cluster-west"`) {
		t.Errorf("inner cluster name missing from digest (expansion did not recurse):\n%s", digest)
	}
	if !strings.Contains(digest, `"RUNNING"`) {
		t.Errorf("inner cluster status missing from digest:\n%s", digest)
	}
	// The long inner description SHOULD have been truncated (that's
	// what the recursive prune does with over-threshold non-identifier
	// strings).
	if !strings.Contains(digest, "<truncated,") {
		t.Errorf("expected inner long-string truncation marker after recursion, got:\n%s", digest)
	}
	// Regression signal: the old bug is the outer envelope marker.
	// If the whole inner payload got replaced with one <truncated>
	// marker, the model has nothing to work with.
	if strings.Contains(digest, `"output":"<truncated,`) {
		t.Errorf("regression: outer opaque-string truncation fired instead of recursing:\n%s", digest)
	}
}

// TestPruneJSON_NestedJSONString_ArrayOfSerializedObjects pins the
// gke_list_clusters shape: `{"output": {"clusters": ["<JSON obj>",
// "<JSON obj>", ...]}}`. Each element is a JSON-serialized cluster
// wrapped in quotes. Pre-fix, the pruner saw 18 opaque long strings
// and truncated each to `<truncated, N chars>` — model got 18 cluster
// counts, zero cluster names. Same runaway-loop pathology.
func TestPruneJSON_NestedJSONString_ArrayOfSerializedObjects(t *testing.T) {
	t.Parallel()
	// Build 5 cluster objects (under MaxArrayElems so head+tail
	// collapse doesn't kick in and confuse the assertion).
	elems := make([]any, 5)
	for i := range elems {
		obj := map[string]any{
			"name":   fmt.Sprintf("cluster-%d", i),
			"status": "RUNNING",
			"blurb":  strings.Repeat("x", MaxStringChars+50), // truncates on recursion
		}
		b, _ := json.Marshal(obj)
		elems[i] = string(b)
	}
	outer := map[string]any{
		"output": map[string]any{"clusters": elems},
	}
	raw, _ := json.Marshal(outer)

	digest, meta := PruneJSON(raw)

	// One expansion per array element.
	if got, _ := meta["nested_json_expanded"].(int); got != 5 {
		t.Errorf("nested_json_expanded = %v, want 5 (one per array element)", got)
	}
	// Each cluster's name and status must survive verbatim.
	for i := 0; i < 5; i++ {
		want := fmt.Sprintf(`"cluster-%d"`, i)
		if !strings.Contains(digest, want) {
			t.Errorf("cluster %d name missing after recursion — model would see opaque markers:\n%s", i, digest)
		}
	}
	// Inner long strings still truncate (that's the normal behavior
	// once we've recursed into the actual object).
	if got, _ := meta["strings_truncated"].(int); got != 5 {
		t.Errorf("strings_truncated = %v, want 5 (one per inner blurb)", got)
	}
}

// TestPruneJSON_NestedJSONString_NonJSONFallsThroughToTruncate pins
// that real prose / logs / stdout aren't accidentally re-parsed.
// Long strings that don't look like JSON (don't start with { or [)
// continue to take the truncate-and-mark path.
func TestPruneJSON_NestedJSONString_NonJSONFallsThroughToTruncate(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("this is normal log output ", 100)
	raw, _ := json.Marshal(map[string]any{"stdout": long})

	digest, meta := PruneJSON(raw)

	if _, ok := meta["nested_json_expanded"]; ok {
		t.Errorf("nested_json_expanded fired on non-JSON prose: %+v", meta)
	}
	if got, _ := meta["strings_truncated"].(int); got != 1 {
		t.Errorf("strings_truncated = %v, want 1 (non-JSON should still truncate)", got)
	}
	if !strings.Contains(digest, "<truncated,") {
		t.Errorf("expected truncation marker on non-JSON prose:\n%s", digest)
	}
}

// TestPruneJSON_NestedJSONString_LooksLikeJSONButBroken pins that
// strings starting with `{` or `[` but that fail to parse (truncated
// JSON, malformed responses) fall through cleanly to the truncation
// marker — no panic, no infinite loop, just the same behavior as
// pre-fix.
func TestPruneJSON_NestedJSONString_LooksLikeJSONButBroken(t *testing.T) {
	t.Parallel()
	// Long string that starts with `{` but doesn't parse.
	broken := "{" + strings.Repeat("garbage", 200)
	raw, _ := json.Marshal(map[string]any{"data": broken})

	digest, meta := PruneJSON(raw)

	if _, ok := meta["nested_json_expanded"]; ok {
		t.Errorf("nested_json_expanded fired on broken JSON: %+v", meta)
	}
	if got, _ := meta["strings_truncated"].(int); got != 1 {
		t.Errorf("strings_truncated = %v, want 1 (broken JSON should fall through)", got)
	}
	if !strings.Contains(digest, "<truncated,") {
		t.Errorf("expected truncation marker on broken JSON:\n%s", digest)
	}
}

// TestPruneJSON_NestedJSONString_IdempotentAcrossPasses pins that
// re-running PruneJSON on its own output is stable — an operator (or
// double-wrap) running the pruner twice must produce byte-identical
// results the second time, otherwise telemetry counters and caching
// break.
func TestPruneJSON_NestedJSONString_IdempotentAcrossPasses(t *testing.T) {
	t.Parallel()
	inner := map[string]any{
		"name":  "svc-1",
		"blurb": strings.Repeat("z", MaxStringChars+20),
	}
	innerJSON, _ := json.Marshal(inner)
	outer := map[string]any{"output": map[string]any{"output": string(innerJSON)}}
	raw, _ := json.Marshal(outer)

	pass1, _ := PruneJSON(raw)
	pass2, _ := PruneJSON([]byte(pass1))
	if pass1 != pass2 {
		t.Errorf("expected byte-identical second pass:\npass1: %s\npass2: %s", pass1, pass2)
	}
}

// TestPruneJSON_NestedJSONString_DepthCapPreventsRunaway pins the
// depth guard: nested-JSON expansion respects MaxDepth. A payload
// with JSON-in-string-in-JSON-in-string-in... nesting won't blow the
// stack — the recursion falls back to the truncation marker at the
// cap.
func TestPruneJSON_NestedJSONString_DepthCapPreventsRunaway(t *testing.T) {
	t.Parallel()
	// Build a payload where nested-JSON expansion would keep recursing.
	// Each level wraps a long JSON string containing another long JSON
	// string. MaxDepth (8) should stop us before things explode.
	deep := map[string]any{"leaf": strings.Repeat("x", MaxStringChars+10)}
	for i := 0; i < 15; i++ { // more than MaxDepth
		asJSON, _ := json.Marshal(deep)
		deep = map[string]any{"wrap": string(asJSON)}
	}
	raw, _ := json.Marshal(deep)

	// Should complete without panicking. Whatever comes out is fine as
	// long as depth was respected — either subtree drop or truncation.
	_, meta := PruneJSON(raw)
	if _, dropped := meta["subtrees_dropped"]; !dropped {
		// Alternative: could bottom out with a truncation marker if
		// the depth cap triggers via a different branch. Both are OK;
		// what matters is we didn't loop.
		if _, truncated := meta["strings_truncated"]; !truncated {
			t.Errorf("neither subtrees_dropped nor strings_truncated fired — did the depth cap not trigger? meta=%+v", meta)
		}
	}
}
