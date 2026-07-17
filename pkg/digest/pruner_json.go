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
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Pruner limits. Fixed for v1 per docs/digest-design.md open question 2
// ("operator-tunable, or fixed for v1? Proposal: fixed, with
// package-level overrides for tests"). Add flags only if telemetry
// shows operators need them.
const (
	// MaxStringChars caps individual string values. Longer strings
	// collapse to "<truncated, N chars>". Identifier-shaped values
	// (see identifierKey) are exempt — losing the tail of a URL or
	// an ID field defeats the whole purpose.
	MaxStringChars = 500

	// MaxArrayElems caps arrays. Longer arrays collapse to a summary
	// object with the first and last (MaxArrayElems/2) elements plus
	// a total + dropped count. Preserves head + tail so paginated /
	// sorted views stay legible.
	MaxArrayElems = 20

	// MaxDepth caps object recursion. Subtrees deeper than this
	// collapse to "<truncated, deep subtree>". Guards against
	// pathological nesting from adversarial inputs.
	MaxDepth = 8
)

// identifierKey matches keys whose values must NEVER be truncated,
// because they carry the semantic identity of the record. Missing an
// ID or a status field silently defeats the whole digesting story.
//
// Defaults informed by common MCP responses (Kubernetes objects,
// tool metadata, HTTP-shaped payloads). Case-insensitive. Overridable
// via SetIdentifierKeyPattern for tests + specialist callers;
// operators don't tune per-tool in v1.
var identifierKey = regexp.MustCompile(
	`(?i)^(id|name|status|kind|type|error|code|apiversion|namespace|uid|resourceversion)$|` +
		`_id$|` +
		`(url|uri|path|href|link)`,
)

// SetIdentifierKeyPattern overrides the default identifier-key regex.
// Test-only hook — production code should not call this. Passing nil
// resets to the default.
func SetIdentifierKeyPattern(re *regexp.Regexp) {
	if re == nil {
		identifierKey = defaultIdentifierKey
		return
	}
	identifierKey = re
}

// defaultIdentifierKey is a copy of the initial regex so
// SetIdentifierKeyPattern(nil) can restore it. Set at package init.
var defaultIdentifierKey = identifierKey

// PruneJSON deterministically compresses a JSON payload using the
// rules documented in docs/digest-design.md. Returns the pruned JSON
// as a string plus metadata describing what happened (arrays
// collapsed, strings truncated, subtrees dropped).
//
// Idempotent: PruneJSON(PruneJSON(x)) equals PruneJSON(x). The pruned
// output is always valid JSON — callers can hand it back into a
// second Process pass without any special-case wiring.
//
// Never returns an error: payloads that fail to parse fall through as
// a "<invalid_json>" wrapper so the caller still gets *something* and
// the router's structural_json dispatch stays observable in
// telemetry. Callers who need "did this actually prune JSON" can
// inspect the returned metadata for the "parse_error" key.
func PruneJSON(payload []byte) (string, map[string]any) {
	var doc any
	if err := json.Unmarshal(payload, &doc); err != nil {
		return string(payload), map[string]any{"parse_error": err.Error()}
	}
	stats := &pruneStats{}
	pruned := prune(doc, 0, stats)
	// Marshal with SetEscapeHTML(false) so the truncation markers
	// ("<truncated, N chars>") aren't unicode-escaped into
	// "<truncated, N chars>". Escaping wastes model tokens
	// with zero safety benefit (the digest goes to a model context,
	// not an HTML page).
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(pruned); err != nil {
		// Shouldn't happen — pruned values are always JSON-safe by
		// construction. Fall back to the input rather than dropping
		// the payload on the floor.
		return string(payload), map[string]any{"marshal_error": err.Error()}
	}
	// json.Encoder.Encode appends a trailing newline; strip it for
	// consistency with json.Marshal's output.
	out := bytes.TrimRight(buf.Bytes(), "\n")
	return string(out), stats.metadata()
}

// pruneStats accumulates counters across one PruneJSON call. Reported
// as Result.Metadata so telemetry can spot pathological inputs
// (huge arrays, deep subtrees) without re-parsing the digest.
type pruneStats struct {
	stringsTruncated   int
	arraysCollapsed    int
	subtreesDropped    int
	nestedJSONExpanded int
}

func (s *pruneStats) metadata() map[string]any {
	m := map[string]any{}
	if s.stringsTruncated > 0 {
		m["strings_truncated"] = s.stringsTruncated
	}
	if s.arraysCollapsed > 0 {
		m["arrays_collapsed"] = s.arraysCollapsed
	}
	if s.subtreesDropped > 0 {
		m["subtrees_dropped"] = s.subtreesDropped
	}
	if s.nestedJSONExpanded > 0 {
		m["nested_json_expanded"] = s.nestedJSONExpanded
	}
	return m
}

// prune recursively transforms doc according to the rules in the
// design doc. depth is the caller's recursion counter (0 at the
// root); stats is the shared accumulator. Callers pass parent-key
// context via the object walk, not through this signature, so each
// call can decide "is this value under an identifier key?" locally.
func prune(v any, depth int, stats *pruneStats) any {
	if depth >= MaxDepth {
		stats.subtreesDropped++
		return "<truncated, deep subtree>"
	}
	switch x := v.(type) {
	case map[string]any:
		return pruneObject(x, depth, stats)
	case []any:
		return pruneArray(x, depth, stats)
	case string:
		return pruneString(x, false, depth, stats)
	default:
		// Numbers, bools, nil — pass through unchanged.
		return v
	}
}

// pruneObject walks a JSON object, applying identifier-key rules per
// key (values under identifier keys skip string truncation) and
// recursing into non-scalar values through prune() so the depth cap
// fires at a single, consistent guard.
func pruneObject(obj map[string]any, depth int, stats *pruneStats) map[string]any {
	out := make(map[string]any, len(obj))
	for k, v := range obj {
		isID := identifierKey.MatchString(k)
		if s, ok := v.(string); ok {
			// String values need the identifier-key context, which
			// prune() doesn't carry — handle inline.
			out[k] = pruneString(s, isID, depth, stats)
			continue
		}
		out[k] = prune(v, depth+1, stats)
	}
	return out
}

// pruneArray applies the head+tail split when an array exceeds
// MaxArrayElems. Small arrays recurse in place via prune() so the
// depth cap fires at the same guard used everywhere else. The
// head+tail split preserves signal on paginated / sorted views
// (which is what most MCP list responses are).
func pruneArray(arr []any, depth int, stats *pruneStats) any {
	if len(arr) <= MaxArrayElems {
		out := make([]any, len(arr))
		for i, v := range arr {
			out[i] = prune(v, depth+1, stats)
		}
		return out
	}
	stats.arraysCollapsed++
	half := MaxArrayElems / 2
	first := make([]any, half)
	for i := 0; i < half; i++ {
		first[i] = prune(arr[i], depth+1, stats)
	}
	last := make([]any, half)
	for i := 0; i < half; i++ {
		last[i] = prune(arr[len(arr)-half+i], depth+1, stats)
	}
	return map[string]any{
		"_summary": true,
		"first":    first,
		"last":     last,
		"total":    len(arr),
		"dropped":  len(arr) - MaxArrayElems,
	}
}

// pruneString compresses an over-threshold string. Two paths, in
// order of preference:
//
//  1. **Nested-JSON expansion.** If the string looks like a serialized
//     JSON payload (starts with `{` or `[`) and parses cleanly, the
//     string is REPLACED with the parsed-and-recursively-pruned inner
//     structure. This is the load-bearing path for MCP servers whose
//     native wire encoding wraps structured data as a JSON-string
//     inside a JSON envelope (GKE MCP's
//     `{"clusters":["<serialized cluster>",...]}` shape,
//     `mcp/text-content` wrapping in general). Without this, the
//     outer pruner sees an opaque long string and truncates the whole
//     semantic content to a `<truncated, N chars>` marker — model
//     gets zero useful data and falls back to `retrieve_raw` or
//     tool-loops trying to re-orient (root cause of the 2026-07-17
//     demo's runaway `list_skills` loop, where every `gke_*` digest
//     was 18 opaque markers instead of 18 cluster names).
//
//     The return type is `any` (not `string`) so a JSON-in-string can
//     be substituted with the parsed object directly. If we returned
//     a string, the outer marshal would escape-wrap it and the model
//     would still see a nested-string shape — same failure mode with
//     nicer-looking bytes.
//
//  2. **Truncate-and-mark.** Fallback for real prose strings, tool
//     stdout, code snippets, etc. Returns the marker as a string so
//     the outer marshal serializes it verbatim.
//
// Identifier-keyed values (see identifierKey regex) skip both paths —
// truncating a URL, ID, or status field silently defeats the whole
// point of digesting.
//
// Idempotence: previously-produced truncation markers pass through
// untouched. Nested-JSON expansion is stable across passes: after the
// first pass the inner value is a real structure (no longer a JSON
// string), so the second pass takes the object/array branch of prune()
// and gets the same result.
//
// depth is threaded through so nested-JSON expansion respects the
// same MaxDepth guard the top-level pruner uses.
func pruneString(s string, isIdentifier bool, depth int, stats *pruneStats) any {
	if isIdentifier {
		return s
	}
	if len(s) <= MaxStringChars {
		return s
	}
	if isTruncationMarker(s) {
		// Second-pass idempotence: don't count or re-wrap something
		// we already truncated.
		return s
	}
	if expanded, ok := expandNestedJSON(s, depth, stats); ok {
		return expanded
	}
	stats.stringsTruncated++
	return fmt.Sprintf("<truncated, %d chars>", len(s))
}

// expandNestedJSON tries to parse s as JSON (object or array root)
// and recursively prune the result. Returns (parsed, true) on success;
// (nil, false) when s doesn't look like JSON, doesn't parse, or would
// recurse past MaxDepth (in which case the caller falls back to the
// truncation marker).
//
// Prefix sniff before Unmarshal so we don't pay parse cost on obvious
// non-JSON strings (base64 blobs, prose, code) — the vast majority of
// over-threshold strings in practice. Only starts-with-`{`-or-`[`
// strings attempt the parse.
//
// Depth guard: if the caller's depth is already at MaxDepth-1, we
// don't expand (recursion would immediately hit the depth cap). Falls
// through to truncation.
func expandNestedJSON(s string, depth int, stats *pruneStats) (any, bool) {
	if depth+1 >= MaxDepth {
		return nil, false
	}
	if len(s) == 0 {
		return nil, false
	}
	switch s[0] {
	case '{', '[':
		// worth parsing
	default:
		return nil, false
	}
	var inner any
	if err := json.Unmarshal([]byte(s), &inner); err != nil {
		return nil, false
	}
	stats.nestedJSONExpanded++
	return prune(inner, depth+1, stats), true
}

// isTruncationMarker recognizes strings this pruner itself produced,
// so PruneJSON(PruneJSON(x)) == PruneJSON(x). The marker format is
// stable ("<truncated, N chars>" / "<truncated, deep subtree>") —
// callers should not synthesize similar strings by accident.
func isTruncationMarker(s string) bool {
	return strings.HasPrefix(s, "<truncated,") && strings.HasSuffix(s, ">")
}

// truncationSuffix formats the "…<N more bytes>" tail used by the
// prose-passthrough path. Exported (via digest.go's caller) so tests
// can pin the exact wire format.
func truncationSuffix(droppedBytes int) string {
	return fmt.Sprintf("…<%d more bytes>", droppedBytes)
}
