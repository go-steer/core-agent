// Copyright 2026 The Cogo Authors.
// SPDX-License-Identifier: Apache-2.0

package permissions

import (
	"fmt"
	"sort"
)

// Built-in allow bundle names. Use these constants (rather than literal
// strings) when constructing a config so a rename surfaces at compile
// time.
const (
	BundleReadOnly       = "read_only"
	BundleDevTools       = "dev_tools"
	BundleCoreAgentTools = "core_agent_tools"
)

// Bundles maps each built-in bundle name to its allowlist entries.
// Entries use the standard `<tool>:<glob>` grammar from policy.go.
//
// `read_only` is enabled by default (via use_builtin_allow=true).
// `dev_tools` and `core_agent_tools` are opt-in via the
// `permissions.builtin_allow_extras` config field.
//
// Conservative-by-construction: every entry here is a verb the LLM
// commonly uses for inspection, with no `-i`/`-w`/`--delete`-style
// mutating flag in the pattern. `find *` is the one knowing
// concession — find has a `-delete` / `-exec rm` escape hatch, but
// excluding it makes the read-only baseline frustrating in practice
// and the bash denylist still blocks `rm -rf /` style targets if the
// LLM tries to chain destructively through it.
var Bundles = map[string][]string{
	BundleReadOnly: {
		// Identity / environment
		"bash:pwd",
		"bash:whoami",
		"bash:id",
		"bash:groups",
		"bash:hostname",
		"bash:uname",
		"bash:uname *",
		"bash:date",
		"bash:uptime",
		"bash:env",
		"bash:printenv",
		"bash:printenv *",
		// Listing
		"bash:ls",
		"bash:ls *",
		"bash:tree",
		"bash:tree *",
		// Reading files
		"bash:cat *",
		"bash:head",
		"bash:head *",
		"bash:tail",
		"bash:tail *",
		"bash:wc",
		"bash:wc *",
		"bash:file *",
		// Text utilities (no in-place / -i flags)
		"bash:echo",
		"bash:echo *",
		"bash:printf *",
		"bash:true",
		"bash:false",
		"bash:grep *",
		"bash:egrep *",
		"bash:fgrep *",
		"bash:rg *",
		"bash:sort *",
		"bash:uniq *",
		"bash:cut *",
		"bash:tr *",
		"bash:awk *",
		"bash:sed -n*",
		// Tool lookup
		"bash:which *",
		"bash:type *",
		"bash:whereis *",
		"bash:command -v *",
		// System info
		"bash:df",
		"bash:df *",
		"bash:du",
		"bash:du *",
		"bash:free",
		"bash:free *",
		"bash:ps",
		"bash:ps *",
		// Find (read-only by convention; documented caveat above)
		"bash:find *",
	},
	BundleDevTools: {
		// Git — read-only inspection
		"bash:git status*",
		"bash:git log*",
		"bash:git diff*",
		"bash:git show*",
		"bash:git branch*",
		"bash:git tag*",
		"bash:git remote*",
		"bash:git config --get *",
		"bash:git config --list*",
		"bash:git rev-parse*",
		"bash:git rev-list*",
		"bash:git ls-files*",
		"bash:git ls-remote*",
		"bash:git blame *",
		"bash:git reflog*",
		"bash:git stash list*",
		// Go — read-only inspection
		"bash:go version",
		"bash:go env*",
		"bash:go list*",
		"bash:go doc*",
		"bash:go vet*",
		// gofmt — diff/list only (no -w)
		"bash:gofmt -l*",
		"bash:gofmt -d*",
	},
	BundleCoreAgentTools: {
		// core-agent's own file tools are already path-scoped, but
		// allowlisting here means they never prompt even when the user
		// configures the scope loosely.
		"read_file:**",
		"list_dir:**",
		"grep:**",
	},
}

// KnownBundles returns the sorted list of recognized bundle names.
// Use this for config validation messages so the list stays in lockstep
// with the actual map.
func KnownBundles() []string {
	out := make([]string, 0, len(Bundles))
	for name := range Bundles {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// ResolveBuiltinAllow returns the merged set of allowlist patterns
// implied by useBuiltin + extras, deduplicated and stably ordered.
//
// useBuiltin=false drops everything regardless of extras (matches the
// `permissions.use_builtin_allow: false` master-switch semantics).
//
// Unknown bundle names in extras error rather than silently no-op so
// typos surface at config-validation time, not by quietly missing
// permissions at runtime.
func ResolveBuiltinAllow(useBuiltin bool, extras []string) ([]string, error) {
	if !useBuiltin {
		return nil, nil
	}
	seen := make(map[string]struct{})
	out := make([]string, 0, len(Bundles[BundleReadOnly]))
	add := func(name string) error {
		entries, ok := Bundles[name]
		if !ok {
			return fmt.Errorf("permissions: unknown built-in bundle %q (want one of %v)", name, KnownBundles())
		}
		for _, e := range entries {
			if _, dup := seen[e]; dup {
				continue
			}
			seen[e] = struct{}{}
			out = append(out, e)
		}
		return nil
	}
	if err := add(BundleReadOnly); err != nil {
		return nil, err
	}
	for _, name := range extras {
		if err := add(name); err != nil {
			return nil, err
		}
	}
	return out, nil
}
