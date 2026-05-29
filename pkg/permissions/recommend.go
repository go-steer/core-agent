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

package permissions

import (
	"path/filepath"
	"sort"
	"strings"
)

// Recommendation describes one suggested allowlist entry the user
// could persist into .agents/config.json's `permissions.allow` block.
// Pattern is in the existing tool:glob form so a recommendation can be
// appended verbatim. Reason and Evidence let a picker explain WHY before
// the user agrees to broaden their permanent allowlist.
type Recommendation struct {
	Pattern  string   // e.g. "bash:git *", "read_file:internal/tui/**"
	Reason   string   // one-line human explanation of what this covers
	Evidence []string // sample keys that motivated the recommendation
}

// Recommend turns a session's interactive approval log into a small
// list of suggested permanent allowlist entries.
func Recommend(approvals []ApprovalLog) []Recommendation {
	if len(approvals) == 0 {
		return nil
	}
	byTool := map[string][]string{}
	seen := map[string]bool{}
	tools := []string{}
	for _, a := range approvals {
		if !seen[a.Tool] {
			tools = append(tools, a.Tool)
		}
		k := a.Tool + "|" + a.Key
		if seen[k] {
			continue
		}
		seen[k] = true
		seen[a.Tool] = true
		byTool[a.Tool] = append(byTool[a.Tool], a.Key)
	}

	var out []Recommendation
	for _, tool := range tools {
		out = append(out, classify(tool, byTool[tool])...)
	}
	return out
}

func classify(tool string, keys []string) []Recommendation {
	if len(keys) == 0 {
		return nil
	}

	if len(keys) == 1 {
		return []Recommendation{{
			Pattern:  tool + ":" + keys[0],
			Reason:   "approved once this session — pin if you'll keep doing it",
			Evidence: keys,
		}}
	}

	if tool == "bash" {
		if verb, all := bashCommonVerb(keys); verb != "" && all {
			return []Recommendation{
				{
					Pattern:  "bash:" + verb + " *",
					Reason:   "all " + plural(len(keys), "command") + " start with `" + verb + "` — persist a verb-wide allow",
					Evidence: keys,
				},
			}
		}
	}

	if isFileTool(tool) {
		if pref := commonDirPrefix(keys); pref != "" && pref != "." && pref != "/" {
			return []Recommendation{{
				Pattern:  tool + ":" + pref + "/**",
				Reason:   plural(len(keys), "path") + " under `" + pref + "/` approved",
				Evidence: keys,
			}}
		}
	}

	return []Recommendation{{
		Pattern:  tool + ":*",
		Reason:   plural(len(keys), "distinct call") + " — broaden to all " + tool + " calls",
		Evidence: keys,
	}}
}

func bashCommonVerb(keys []string) (string, bool) {
	if len(keys) == 0 {
		return "", false
	}
	first := strings.Fields(keys[0])
	if len(first) == 0 {
		return "", false
	}
	verb := first[0]
	for _, k := range keys[1:] {
		toks := strings.Fields(k)
		if len(toks) == 0 || toks[0] != verb {
			return "", false
		}
	}
	return verb, true
}

func isFileTool(tool string) bool {
	switch tool {
	case "read_file", "write_file", "edit_file", "list_dir":
		return true
	}
	return false
}

func commonDirPrefix(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	cleaned := make([][]string, len(paths))
	for i, p := range paths {
		cleaned[i] = strings.Split(filepath.ToSlash(filepath.Clean(p)), "/")
	}
	prefix := append([]string(nil), cleaned[0]...)
	for _, parts := range cleaned[1:] {
		max := len(prefix)
		if len(parts) < max {
			max = len(parts)
		}
		i := 0
		for i < max && prefix[i] == parts[i] {
			i++
		}
		prefix = prefix[:i]
		if len(prefix) == 0 {
			return ""
		}
	}
	for _, parts := range cleaned {
		if len(parts) == len(prefix) {
			prefix = prefix[:len(prefix)-1]
			break
		}
	}
	if len(prefix) == 0 {
		return ""
	}
	return strings.Join(prefix, "/")
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return itoa(n) + " " + word + "s"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// SortRecommendations returns a stable ordering: more specific
// patterns (without `*`) first, then by Pattern lex order.
func SortRecommendations(recs []Recommendation) {
	sort.SliceStable(recs, func(i, j int) bool {
		ai := strings.Contains(recs[i].Pattern, "*")
		aj := strings.Contains(recs[j].Pattern, "*")
		if ai != aj {
			return !ai // non-wildcard first
		}
		return recs[i].Pattern < recs[j].Pattern
	})
}
