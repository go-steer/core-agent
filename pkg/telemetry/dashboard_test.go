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

package telemetry_test

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// A metric that gets renamed in code takes a docs table entry and a
// Grafana panel with it, and neither fails loudly — the panel just
// draws an empty chart and the table keeps describing a series nobody
// exports. These two tests chain the three artifacts together so a
// rename has to touch all of them:
//
//	dashboard panel  →  the Metrics page  →  a name in the Go tree
//
// Deliberately NOT a check that the docs list every shipped
// instrument: a new metric landing undocumented is a gap, but a
// silently-wrong document is a defect, and only the second one is
// worth a gate that blocks a merge.
const (
	repoRoot      = "../.."
	dashboardPath = "dev/grafana/core-agent-overview.json"
	metricsDoc    = "docs/site/src/content/docs/concepts/metrics.md"
)

// promMetricRE matches a Prometheus metric identifier in a PromQL
// expression: our three families only. Anything else in an expr
// (functions, `le`) is out of scope by construction.
var promMetricRE = regexp.MustCompile(`\b(core_agent_[a-z0-9_]+|gen_ai_[a-z0-9_]+|go_[a-z0-9_]+)\b`)

// Label keys share the metric families' prefixes once dots become
// underscores (gen_ai_request_model, gen_ai_agent_name), so the two
// label positions have to come out of the expression before the
// metric names are extracted: `{...}` matchers and the parenthesized
// list after by / without.
var (
	labelMatcherRE = regexp.MustCompile(`\{[^}]*\}`)
	groupingRE     = regexp.MustCompile(`\b(?:by|without)\s*\([^)]*\)`)
)

func metricNamesIn(expr string) []string {
	stripped := labelMatcherRE.ReplaceAllString(expr, "")
	stripped = groupingRE.ReplaceAllString(stripped, "")
	return promMetricRE.FindAllString(stripped, -1)
}

// otelMetricRE matches a dotted OTel instrument name as it appears in
// the doc's tables, e.g. `gen_ai.client.token.usage`.
var otelMetricRE = regexp.MustCompile("`(core_agent\\.[a-z0-9_.]+|gen_ai\\.[a-z0-9_.]+)`")

func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

// TestGrafanaDashboardQueriesDocumentedMetrics fails when a panel
// queries a series the Metrics page doesn't describe — either because
// the panel was written against a name that no longer exists, or
// because someone added a panel without documenting what it shows.
func TestGrafanaDashboardQueriesDocumentedMetrics(t *testing.T) {
	var dash struct {
		Panels []struct {
			Title   string `json:"title"`
			Targets []struct {
				Expr string `json:"expr"`
			} `json:"targets"`
		} `json:"panels"`
	}
	if err := json.Unmarshal([]byte(readRepoFile(t, dashboardPath)), &dash); err != nil {
		t.Fatalf("parse %s: %v", dashboardPath, err)
	}
	if len(dash.Panels) == 0 {
		t.Fatalf("%s parsed to zero panels — the gate would pass vacuously", dashboardPath)
	}

	doc := readRepoFile(t, metricsDoc)

	// Histogram and counter suffixes the Prometheus exporter appends;
	// the doc names the base series, so strip these before looking up.
	suffixes := []string{"_bucket", "_sum", "_count"}

	seen := map[string]bool{}
	queried := 0
	for _, p := range dash.Panels {
		for _, target := range p.Targets {
			for _, m := range metricNamesIn(target.Expr) {
				queried++
				base := m
				for _, s := range suffixes {
					base = strings.TrimSuffix(base, s)
				}
				if seen[base] {
					continue
				}
				seen[base] = true
				if !strings.Contains(doc, base) {
					t.Errorf("panel %q queries %q (base %q), which %s does not mention",
						p.Title, m, base, metricsDoc)
				}
			}
		}
	}
	if queried == 0 {
		t.Fatalf("no metric names extracted from %s — the regex or the schema drifted", dashboardPath)
	}
}

// TestDocumentedMetricNamesExistInCode fails when the Metrics page
// names an instrument the daemon does not construct. That is the
// direction that rots quietly: code renames land with a green build,
// and the doc goes on describing the old series.
func TestDocumentedMetricNamesExistInCode(t *testing.T) {
	doc := readRepoFile(t, metricsDoc)

	names := map[string]bool{}
	for _, m := range otelMetricRE.FindAllStringSubmatch(doc, -1) {
		names[m[1]] = true
	}
	if len(names) < 15 {
		t.Fatalf("only %d dotted metric names found in %s; expected the full inventory — "+
			"the tables were reshaped and this gate stopped seeing them", len(names), metricsDoc)
	}

	// Every name has to appear as a literal somewhere under pkg/,
	// which is where the instrument constants live.
	var sources []string
	err := filepath.WalkDir(filepath.Join(repoRoot, "pkg"), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		b, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		sources = append(sources, string(b))
		return nil
	})
	if err != nil {
		t.Fatalf("walk pkg/: %v", err)
	}

	var missing []string
	for name := range names {
		found := false
		for _, src := range sources {
			if strings.Contains(src, `"`+name+`"`) {
				found = true
				break
			}
		}
		if !found {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	for _, name := range missing {
		t.Errorf("%s documents %q, but no Go file under pkg/ contains that string literal — "+
			"either the instrument was renamed or the doc invented it", metricsDoc, name)
	}
}
