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

// Argument canonicalization for the loop detectors (#649).
//
// The v1 repeat detector compared the JSON-serialized argument blob as
// a literal string, which its own docstring flagged as an evasion: an
// agent that alternates "main.go" and "/workspace/main.go" is stuck on
// one file, and the detector saw two different calls. Both shapes were
// observed in live UAT.
//
// Canonicalization is deliberately narrow. It normalizes *path-shaped
// arguments* and nothing else: no case folding, no whitespace
// squashing, no numeric coercion beyond what a JSON round-trip already
// does. A detector that decides two calls are "the same" too eagerly
// halts an agent doing legitimate work, which is worse than missing a
// loop the cost ceiling will catch anyway.

package watchdog

import (
	"encoding/json"
	"path"
	"strings"
)

// canonicalArgs returns a stable, path-normalized rendering of a
// JSON-serialized argument blob, for use as a map/equality key.
//
// Path-shaped values (see pathishKey) are cleaned with path.Clean, so
// "./main.go", "main.go" and "dir/../main.go" collapse to one form.
// Everything else round-trips unchanged. Non-object or unparseable
// input is returned verbatim — args the agent could not marshal are
// still comparable to themselves, which is all the detectors need.
//
// Note what this does NOT do: "/workspace/main.go" and "main.go" stay
// distinct here, because collapsing them requires comparing two values
// against each other rather than mapping one to a canonical form (a
// basename key would make "a/doc.go" and "b/doc.go" equal, which is a
// false positive on any codebase with repeated filenames). The
// consecutive-repeat detector handles that case pairwise via
// argsEquivalent; the cycle detector, which needs hashable keys,
// accepts the miss.
func canonicalArgs(args string) string {
	if args == "" {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(args), &m); err != nil || m == nil {
		return args
	}
	changed := false
	for k, v := range m {
		s, ok := v.(string)
		if !ok || !pathishKey(k) {
			continue
		}
		if c := path.Clean(s); c != s {
			m[k] = c
			changed = true
		}
	}
	if !changed {
		return args
	}
	// encoding/json sorts map keys, so the re-render is deterministic.
	b, err := json.Marshal(m)
	if err != nil {
		return args
	}
	return string(b)
}

// pathishKey reports whether an argument name denotes a filesystem
// path. Keyed on the name rather than on whether the value "looks like
// a path" on purpose: a bash command ("ls /tmp") and a grep pattern
// ("a/b") both look like paths and neither should be path-normalized.
//
// The list covers the built-in tools (read_file/write_file "path",
// edit_file "file_path", glob/grep "path" + "dir") and the common MCP
// spellings; an unrecognized key just misses the normalization, which
// degrades to v1 behavior.
func pathishKey(k string) bool {
	k = strings.ToLower(k)
	switch k {
	case "target", "source", "destination", "src", "dst", "cwd":
		return true
	}
	return strings.Contains(k, "path") ||
		strings.Contains(k, "file") ||
		strings.Contains(k, "dir") ||
		strings.Contains(k, "folder")
}

// argsEquivalent reports whether two argument blobs describe the same
// call for loop-detection purposes: equal after canonicalization, or
// equal except that one or more path-shaped values differ only by a
// leading directory prefix ("/workspace/main.go" vs "main.go").
//
// The prefix relation is checked on a path-component boundary, so
// "a/main.go" and "b/main.go" are NOT equivalent — only a genuine
// suffix is. It is reflexive and symmetric but not transitive, which
// is why it is a pairwise predicate rather than a canonical form; the
// repeat detector compares every call against the run's first call, so
// non-transitivity can extend a run by one hop at most ("main.go" ~
// "foo/main.go" because both are compared to the run's "main.go"), and
// a run of calls all naming a file called main.go is loop-shaped
// regardless.
func argsEquivalent(a, b string) bool {
	if a == b {
		return true
	}
	ca, cb := canonicalArgs(a), canonicalArgs(b)
	if ca == cb {
		return true
	}
	var ma, mb map[string]any
	if json.Unmarshal([]byte(ca), &ma) != nil || json.Unmarshal([]byte(cb), &mb) != nil {
		return false
	}
	if len(ma) != len(mb) || ma == nil || mb == nil {
		return false
	}
	for k, va := range ma {
		vb, ok := mb[k]
		if !ok {
			return false
		}
		sa, aIsStr := va.(string)
		sb, bIsStr := vb.(string)
		if aIsStr && bIsStr && pathishKey(k) {
			if !pathSuffixEquivalent(sa, sb) {
				return false
			}
			continue
		}
		// Non-path values must match exactly. Compare the JSON
		// rendering so nested maps/slices are handled without a
		// reflect.DeepEqual dependency on float formatting.
		ja, ea := json.Marshal(va)
		jb, eb := json.Marshal(vb)
		if ea != nil || eb != nil || string(ja) != string(jb) {
			return false
		}
	}
	return true
}

// pathSuffixEquivalent reports whether one cleaned path is the other
// with a leading directory prefix removed, on a component boundary.
// Equal paths qualify; "" never matches a non-empty path.
func pathSuffixEquivalent(a, b string) bool {
	a, b = path.Clean(a), path.Clean(b)
	if a == b {
		return true
	}
	if a == "" || b == "" || a == "." || b == "." {
		return false
	}
	long, short := a, b
	if len(short) > len(long) {
		long, short = short, long
	}
	return strings.HasSuffix(long, "/"+short)
}
