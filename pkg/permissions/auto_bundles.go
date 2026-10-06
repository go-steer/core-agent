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
	"fmt"
	"sort"
	"strings"
)

// Auto-mode eligibility bundle names (#1252). A bundle names calls the
// approver may decide in mode "auto"; it allows nothing by itself. An
// eligible call still goes to the approver, which judges it against the
// operator's task, and every call auto never lets the approver decide
// (path scope, control plane, background subagents) still goes to a
// person.
//
// The approver is the only check on an eligible call. It sees the whole
// command line, so a flag like `git commit --amend`, `go test -exec` or
// `git push --force` is in front of it, but it never sees the code a
// test or build runs. Making `go test` eligible lets the agent run code
// it wrote; the repository and pod boundary contain that, not this
// list.
const (
	AutoBundleCoding  = "coding"
	AutoBundleK8sRead = "k8s_read"
	AutoBundleGitHub  = "github"
)

// AutoBundles maps each auto-mode eligibility bundle to its patterns,
// in the policy.go grammar.
//
// Prefix patterns carry the safe-command guard (safecmd.go), so none of
// them ever matches a chained, piped or redirected command: `go test
// ./... && rm -rf ~` goes to a person, not the approver. A recipe that
// uses a bundle should tell its agent to run commands one at a time.
var AutoBundles = map[string][]string{
	// coding: editing files in the workspace (path scope still sends
	// anything outside it to a person), single test/build/vet/format
	// commands, and committing on a new local branch. Rebase, reset,
	// restore and switching to an existing branch are left out; reading
	// git state is the dev_tools allow bundle's job. Some of these fetch
	// from the network (go mod tidy, cargo build) and all of the test
	// and build commands run code from the workspace.
	AutoBundleCoding: {
		"write_file:*",
		"edit_file:*",
		"bash:go test", "bash:go test *",
		"bash:go vet", "bash:go vet *",
		"bash:go build", "bash:go build *",
		"bash:go mod tidy",
		"bash:gofmt *",
		"bash:make test", "bash:make build", "bash:make lint",
		"bash:npm test", "bash:npm run build", "bash:npm run lint",
		"bash:pytest", "bash:pytest *",
		"bash:cargo test", "bash:cargo test *",
		"bash:cargo build", "bash:cargo build *",
		"bash:git add *",
		"bash:git commit *",
		"bash:git checkout -b *",
		"bash:git switch -c *",
	},
	// k8s_read: kubectl that changes nothing. It can still READ Secret
	// data (`kubectl get secret -o yaml`, `kubectl get --raw ...`); the
	// approver judges whether the task needs it.
	AutoBundleK8sRead: {
		"bash:kubectl get *",
		"bash:kubectl describe *",
		"bash:kubectl logs *",
		"bash:kubectl top *",
		"bash:kubectl explain *",
		"bash:kubectl api-resources", "bash:kubectl api-resources *",
		"bash:kubectl version", "bash:kubectl version *",
		"bash:kubectl auth can-i *",
		"bash:kubectl config current-context",
	},
	// github: pushing a branch and working with pull requests and issues.
	// All of it reaches outside the workspace, so the approver allows a
	// call only when the operator's own words ask for or permit that
	// action, and never a destructive one: `git push *` also matches
	// `--force` and `origin :branch`, which the approver's policy treats
	// as destructive (#1251). Merging is deliberately absent: add
	// "bash:gh pr merge *" to eligible explicitly to make it decidable.
	AutoBundleGitHub: {
		"bash:git push *",
		"bash:gh pr create", "bash:gh pr create *",
		"bash:gh pr view *", "bash:gh pr list", "bash:gh pr list *",
		"bash:gh pr checks *", "bash:gh pr comment *",
		"bash:gh issue create", "bash:gh issue create *",
		"bash:gh issue view *", "bash:gh issue list", "bash:gh issue list *",
		"bash:gh issue comment *",
	},
}

// KnownAutoBundles returns the auto-mode eligibility bundle names,
// sorted.
func KnownAutoBundles() []string {
	out := make([]string, 0, len(AutoBundles))
	for name := range AutoBundles {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// ResolveAutoEligible merges permissions.auto.eligible with the named
// bundles, deduplicated, explicit patterns first. An unknown bundle name
// is an error, so a typo fails at startup rather than leaving the
// approver with nothing to decide.
func ResolveAutoEligible(eligible, bundles []string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, p := range eligible {
		add(p)
	}
	for _, name := range bundles {
		name = strings.TrimSpace(name)
		entries, ok := AutoBundles[name]
		if !ok {
			return nil, fmt.Errorf("permissions.auto.eligible_bundles: unknown bundle %q (want one of %v)", name, KnownAutoBundles())
		}
		for _, p := range entries {
			add(p)
		}
	}
	return out, nil
}
