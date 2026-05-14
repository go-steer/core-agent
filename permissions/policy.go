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
	"strings"
)

// Policy interprets the allow/deny pattern lists from
// .agents/config.json's `permissions` block.
//
// Pattern grammar:
//
//	<tool>:<glob>     — applies only when the request is for <tool>
//	<glob>            — applies to any tool (matched against request key)
//
// `<glob>` is matched with path/filepath.Match, so it understands `*`,
// `?`, and character classes. The "key" of a request depends on the
// tool: for bash it is the command string, for file tools it is the
// resolved absolute path. Wildcards work the same for both.
type Policy struct {
	allow []rule
	deny  []rule
}

type rule struct {
	tool string // empty = applies to all tools
	pat  string // glob pattern
}

// NewPolicy parses the configured allow/deny patterns. Bad patterns
// fail fast so misconfigurations surface at startup, not when the
// agent first triggers a tool.
func NewPolicy(allow, deny []string) (*Policy, error) {
	a, err := parseRules(allow)
	if err != nil {
		return nil, err
	}
	d, err := parseRules(deny)
	if err != nil {
		return nil, err
	}
	return &Policy{allow: a, deny: d}, nil
}

func parseRules(patterns []string) ([]rule, error) {
	out := make([]rule, 0, len(patterns))
	for _, p := range patterns {
		r := rule{}
		if i := strings.Index(p, ":"); i >= 0 {
			r.tool = p[:i]
			r.pat = p[i+1:]
		} else {
			r.pat = p
		}
		if _, err := filepath.Match(r.pat, ""); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// Outcome is the result of consulting the policy. It is used by the
// gate to decide what to do next; it is not the final say (the gate
// also consults the bash denylist, the path scope, and the mode).
type Outcome int

const (
	OutcomeUnmatched Outcome = iota // no allow/deny rule matched
	OutcomeAllow
	OutcomeDeny
)

// Match returns OutcomeDeny if any deny rule matches the request,
// OutcomeAllow if any allow rule matches and no deny rule matches,
// otherwise OutcomeUnmatched. Deny always wins.
func (p *Policy) Match(tool, key string) Outcome {
	if matchAny(p.deny, tool, key) {
		return OutcomeDeny
	}
	if matchAny(p.allow, tool, key) {
		return OutcomeAllow
	}
	return OutcomeUnmatched
}

func matchAny(rules []rule, tool, key string) bool {
	for _, r := range rules {
		if r.tool != "" && r.tool != tool {
			continue
		}
		if matchGlob(r.pat, key) {
			return true
		}
	}
	return false
}

// matchGlob tries an exact match first (so the pattern "git status" only
// matches the literal command, not "git statusabc"), then a path-style
// glob via filepath.Match. A trailing `*` is treated as an open prefix
// match too, which is friendlier for command patterns like "git diff*".
func matchGlob(pattern, s string) bool {
	if pattern == s {
		return true
	}
	if strings.HasSuffix(pattern, "*") && !strings.ContainsAny(pattern[:len(pattern)-1], "*?[") {
		return strings.HasPrefix(s, pattern[:len(pattern)-1])
	}
	if ok, _ := filepath.Match(pattern, s); ok {
		return true
	}
	return false
}
