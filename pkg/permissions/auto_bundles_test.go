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
	"os"
	"strings"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/config"
)

// Every bundle pattern parses, and an unknown bundle name fails rather
// than leaving the approver nothing to decide (#1252).
func TestAutoBundles_ParseAndResolve(t *testing.T) {
	t.Parallel()
	for name, patterns := range AutoBundles {
		if _, err := NewPolicy(patterns, nil); err != nil {
			t.Errorf("bundle %q: %v", name, err)
		}
	}
	got, err := ResolveAutoEligible([]string{"bash:make deploy-preview", "write_file:*"}, []string{AutoBundleCoding})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 || got[0] != "bash:make deploy-preview" || strings.Count(strings.Join(got, "\n"), "write_file:*") != 1 {
		t.Errorf("explicit patterns first, duplicates dropped: %v", got)
	}
	if _, err := ResolveAutoEligible(nil, []string{"codign"}); err == nil || !strings.Contains(err.Error(), "coding") {
		t.Errorf("unknown bundle = %v, want an error naming the known ones", err)
	}
}

// What each bundle makes the approver able to decide — and, as much,
// what it never does: a chained or redirected command never matches a
// prefix pattern, coding reaches nothing outside the workspace, and
// github leaves merging out.
func TestAutoBundles_MatchWhatTheyClaim(t *testing.T) {
	t.Parallel()
	pol := func(b string) *Policy {
		p, err := NewPolicy(AutoBundles[b], nil)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	cases := []struct {
		bundle, tool, key string
		eligible          bool
	}{
		{AutoBundleCoding, "bash", "go test ./pkg/config/...", true},
		{AutoBundleCoding, "edit_file", "/workspace/pkg/config/config.go", true},
		{AutoBundleCoding, "bash", "git commit -s -m fix", true},
		{AutoBundleCoding, "bash", "go test ./... && rm -rf ~", false},
		{AutoBundleCoding, "bash", "go test ./... > /etc/passwd", false},
		{AutoBundleCoding, "bash", "git push origin main", false},
		{AutoBundleCoding, "bash", "git reset --hard origin/main", false},
		{AutoBundleCoding, "bash", "git switch --discard-changes main", false},
		{AutoBundleCoding, "bash", "git switch -C main origin/main", false},
		{AutoBundleCoding, "bash", "git checkout -B main", false},
		{AutoBundleCoding, "bash", "git diff --output=/etc/x", false},
		{AutoBundleCoding, "bash", "git switch -c fix/cache-lint", true},
		// Eligible by design: the approver sees these flags and decides.
		{AutoBundleCoding, "bash", "git commit --amend --no-edit", true},
		{AutoBundleCoding, "bash", "go test -exec /tmp/x ./...", true},
		{AutoBundleCoding, "bash", "CGO_ENABLED=0 go build ./...", false}, // an env prefix never matches; it escalates
		{AutoBundleK8sRead, "bash", "kubectl get pods -n staging", true},
		{AutoBundleK8sRead, "bash", "kubectl delete pod api-0", false},
		{AutoBundleGitHub, "bash", "gh pr create --fill", true},
		{AutoBundleGitHub, "bash", "git push origin fix/cache-lint", true},
		{AutoBundleGitHub, "bash", "gh pr merge 42 --admin --squash", false},
		// Eligible by design: the approver's policy treats it as destructive (#1251).
		{AutoBundleGitHub, "bash", "git push --force origin main", true},
	}
	for _, c := range cases {
		got := pol(c.bundle).Match(c.tool, c.key) == OutcomeAllow
		if got != c.eligible {
			t.Errorf("%s: %s %q eligible = %v, want %v", c.bundle, c.tool, c.key, got, c.eligible)
		}
	}
}

// permissions.auto.eligible_bundles reaches the gate built from config.
func TestFromConfig_EligibleBundles(t *testing.T) {
	t.Parallel()
	cfg := defaultGateConfig(t)
	cfg.Permissions.Auto = &config.AutoApproverConfig{EligibleBundles: []string{AutoBundleK8sRead}}
	p, err := autoEligibleFromConfig(cfg.Permissions.Auto)
	if err != nil || p == nil || p.Match("bash", "kubectl logs deploy/api") != OutcomeAllow {
		t.Fatalf("policy = %v, err = %v; want k8s_read's patterns eligible", p, err)
	}
	cfg.Permissions.Auto.EligibleBundles = []string{"nope"}
	if _, err := FromConfig(cfg, t.TempDir(), t.TempDir(), nil); err == nil {
		t.Error("FromConfig accepted an unknown bundle")
	}
}

// The site lists every bundle's patterns, so an operator can see what a
// bundle opens up without reading Go. This keeps that list honest.
func TestAutoBundles_DocumentedOnTheSite(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("../../docs/site/src/content/docs/concepts/permissions.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	for name, patterns := range AutoBundles {
		if !strings.Contains(doc, "`"+name+"`") {
			t.Errorf("bundle %q is not on the permissions page", name)
		}
		for _, p := range patterns {
			if !strings.Contains(doc, "`"+p+"`") {
				t.Errorf("bundle %q pattern %q is not on the permissions page", name, p)
			}
		}
	}
}
