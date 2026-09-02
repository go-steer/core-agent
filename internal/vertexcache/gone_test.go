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

package vertexcache

import (
	"errors"
	"testing"
)

// TestIsCacheGone pins the predicate both recovery paths depend on.
// A false negative is #902: the daemon keeps a dead handle and every
// cached turn fails until the process restarts. A false positive is
// cheaper but not free — a pointless invalidate plus an uncached
// retry, and a cache re-created for no reason.
//
// The three real-shape cases are verbatim strings observed in
// production, not paraphrases. That is the whole point: the bug was a
// predicate written against a remembered shape.
func TestIsCacheGone(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil error", nil, false},

		// --- the three real shapes ---
		{"GenerateContent expiry, verbatim (the #902 miss)",
			errors.New("Error 400, Message: Cache content 7016131366404227072 is expired., Status: INVALID_ARGUMENT, Details: []"),
			true},
		{"Caches.Update not-found, verbatim",
			errors.New("Error 404, Message: Cached content 7016131366404227072 is not found., Status: NOT_FOUND, Details: []"),
			true},
		{"GenerateContent not-found, verbatim",
			errors.New("Error 404, Message: Not found: cached content metadata for 6116704758662168576., Status: NOT_FOUND, Details: []"),
			true},

		// --- must NOT match: generic failures that name no cache ---
		{"NOT_FOUND on missing model",
			errors.New("Error 404, Message: publisher model not found, Status: NOT_FOUND"), false},
		{"NOT_FOUND on wrong region", errors.New("resource not found: NOT_FOUND"), false},
		{"bare INVALID_ARGUMENT under load (this is #898, not #902)",
			errors.New("Error 400, Message: Request contains an invalid argument., Status: INVALID_ARGUMENT, Details: []"),
			false},
		{"rate limit", errors.New("Error 429, Status: RESOURCE_EXHAUSTED"), false},

		// --- must NOT match: names a cache, but the cache is fine ---
		{"cached content quota", errors.New("cached content quota exceeded"), false},
		{"cache content 400 that is not an expiry",
			errors.New("Error 400, Message: Cache content name is malformed., Status: INVALID_ARGUMENT"),
			false},

		// --- spelling and case ---
		{"case-insensitive on the noun",
			errors.New("Error 404, Message: Cached Content missing, Status: NOT_FOUND"), true},
		{"case-insensitive on expired",
			errors.New("Error 400, Message: Cache content 1 is EXPIRED., Status: INVALID_ARGUMENT"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := IsCacheGone(tc.err); got != tc.want {
				t.Errorf("IsCacheGone(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestIsCacheGone_SpellingsAreBothLoadBearing guards the one thing a
// future simplification is most likely to get wrong: "cached content"
// does not contain "cache content" as a substring, so collapsing the
// two checks into one silently drops a real shape. Both directions
// are asserted so the failure names which spelling was lost.
func TestIsCacheGone_SpellingsAreBothLoadBearing(t *testing.T) {
	t.Parallel()
	withD := errors.New("Error 404, Message: Cached content 1 is not found., Status: NOT_FOUND")
	withoutD := errors.New("Error 400, Message: Cache content 1 is expired., Status: INVALID_ARGUMENT")
	if !IsCacheGone(withD) {
		t.Error(`lost the "cached content" spelling (Caches.Update / GenerateContent NOT_FOUND)`)
	}
	if !IsCacheGone(withoutD) {
		t.Error(`lost the "cache content" spelling (GenerateContent expiry) — this is the #902 regression`)
	}
}
