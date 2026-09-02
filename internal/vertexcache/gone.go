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

import "strings"

// IsCacheGone reports whether err says the cache reference we are
// holding is dead — reaped, expired, or otherwise unusable — as
// opposed to the RPC merely having failed. A true answer means no
// later call naming the same cache can succeed, so the only recovery
// is to drop the handle and create a new one.
//
// This lives in one place, exported, because the two call sites that
// need it see the *same* dead cache through different RPCs and Vertex
// describes it differently each time. #902 is what happens when they
// each carry their own predicate: Caches.Update learned the cache was
// gone and discarded the fact, GenerateContent's detector did not
// recognise the shape it was handed, and a daemon 27 hours into its
// life failed every cached turn in every session until it restarted.
//
// The three observed shapes, all for one reaped cache:
//
//	Caches.Update    404 NOT_FOUND        Cached content <id> is not found.
//	GenerateContent  400 INVALID_ARGUMENT Cache content <id> is expired.
//	GenerateContent  404 NOT_FOUND        Not found: cached content metadata for <id>.
//
// Note "Cache content" without the d on the middle one. Matching is by
// substring because the genai SDK exposes no typed error for any of
// them; the noun phrase is the discriminator, and requiring it keeps a
// generic NOT_FOUND (wrong model, wrong region) from being read as an
// eviction. A status code alone would be far too broad — 400
// INVALID_ARGUMENT in particular is the single most overloaded answer
// Vertex gives.
func IsCacheGone(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	lower := strings.ToLower(s)
	// Both spellings. "cached content" does not contain "cache
	// content" as a substring, so both checks are load-bearing.
	if !strings.Contains(lower, "cached content") && !strings.Contains(lower, "cache content") {
		return false
	}
	if strings.Contains(s, "NOT_FOUND") {
		return true
	}
	// Vertex reports an elapsed TTL on the generate path as
	// INVALID_ARGUMENT rather than NOT_FOUND. Pair the code with the
	// word so an unrelated malformed-cache-argument 400 — a bad name,
	// a cache belonging to another project — is not swallowed as an
	// eviction and silently retried uncached.
	return strings.Contains(s, "INVALID_ARGUMENT") && strings.Contains(lower, "expired")
}
