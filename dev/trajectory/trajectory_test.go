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

package trajectory

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

const basicRun = "testdata/run-basic"

func loadBasic(t *testing.T) *Trajectory {
	t.Helper()
	tr, err := LoadRun(basicRun)
	if err != nil {
		t.Fatalf("LoadRun(%s): %v", basicRun, err)
	}
	return tr
}

func TestLoadRunMeta(t *testing.T) {
	tr := loadBasic(t)
	if tr.Meta.RunID != "20260101T000000Z-a" {
		t.Errorf("RunID = %q", tr.Meta.RunID)
	}
	if tr.Meta.ScenarioID != "a" || tr.Meta.ModelFlavor != "gemini" {
		t.Errorf("Meta = %+v", tr.Meta)
	}
}

// The last usage-update wins because the frames are cumulative totals,
// not deltas. Summing them would report 1,100 input tokens on a run that
// used 1,000.
func TestUsageIsLastWriteNotSum(t *testing.T) {
	tr := loadBasic(t)
	want := Usage{TokensIn: 1000, TokensOut: 90, CostUSD: 0.0123, Turns: 2}
	if tr.Usage != want {
		t.Errorf("Usage = %+v, want %+v", tr.Usage, want)
	}
	if len(tr.Turns) != 1 || tr.Turns[0].LatencyMS != 19000 {
		t.Errorf("Turns = %+v", tr.Turns)
	}
}

// Parent and subagent frames share one sequence space, so the merged
// order has to interleave them. If it did not, "what was the parent
// doing while the child read that" would be unanswerable.
func TestFramesInterleaveParentAndSubagent(t *testing.T) {
	tr := loadBasic(t)
	var got []string
	for _, f := range tr.Frames {
		got = append(got, f.Agent)
	}
	// Parent seq 1-4, then the child's 6-7, then the parent again from 8.
	want := []string{
		Parent, Parent, Parent, Parent, "cluster", "cluster",
		Parent, Parent, Parent, Parent, Parent, Parent, Parent,
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("agent order = %v\nwant       %v", got, want)
	}
	for i := 1; i < len(tr.Frames); i++ {
		if tr.Frames[i-1].Seq > tr.Frames[i].Seq {
			t.Fatalf("frames not sorted by Seq at %d: %d > %d",
				i, tr.Frames[i-1].Seq, tr.Frames[i].Seq)
		}
	}
}

func TestBuildStepsPairsAndClassifies(t *testing.T) {
	tr := loadBasic(t)

	want := []struct {
		agent   string
		tool    string
		callSeq int
		respSeq int
		status  Status
	}{
		{Parent, "record_plan", 2, 3, StatusOK},
		{Parent, "spawn_agent", 4, 8, StatusError},
		{"cluster", "gke_get_k8s_logs", 6, 7, StatusSuspect},
		{Parent, "gke_get_k8s_resource", 9, 11, StatusOK},
		{Parent, "gke_list_k8s_events", 9, 11, StatusOK},
		{Parent, "gke_get_k8s_logs", 12, -1, StatusNoResponse},
		{Parent, "gke_list_k8s_nodes", 13, 14, StatusOK},
	}
	if len(tr.Steps) != len(want) {
		t.Fatalf("got %d steps, want %d:\n%s", len(tr.Steps), len(want), dumpSteps(tr))
	}
	for i, w := range want {
		s := tr.Steps[i]
		if s.Index != i || s.Agent != w.agent || s.Tool != w.tool ||
			s.CallSeq != w.callSeq || s.RespSeq != w.respSeq || s.Status != w.status {
			t.Errorf("step %d = %+v\nwant agent=%s tool=%s %d→%d %s",
				i, s, w.agent, w.tool, w.callSeq, w.respSeq, w.status)
		}
	}
}

// The streaming chunk at seq 1 carries a functionCall that is repeated
// on the settled frame. Counting both would double every tool call in
// every measure built on Steps.
func TestPartialFramesAreNotSteps(t *testing.T) {
	tr := loadBasic(t)
	for _, s := range tr.Steps {
		if s.CallID == "partial-must-be-skipped" {
			t.Fatalf("a partial frame produced a step: %+v", s)
		}
	}
}

// One frame can carry several calls and the runtime can answer them in
// any order. Matching by call id rather than by position is what keeps
// c3's result off c4's step.
func TestResponsesMatchByIDNotPosition(t *testing.T) {
	tr := loadBasic(t)
	for _, s := range tr.Steps {
		switch s.CallID {
		case "c3":
			if got, _ := s.Response["kind"].(string); got != "Deployment" {
				t.Errorf("c3 response = %v, want the Deployment payload", s.Response)
			}
		case "c4":
			if _, ok := s.Response["items"]; !ok {
				t.Errorf("c4 response = %v, want the events payload", s.Response)
			}
		}
	}
}

func TestElapsedSpansTheFramePair(t *testing.T) {
	tr := loadBasic(t)
	spawn := tr.Steps[1]
	if got := spawn.Elapsed(); got != 13*time.Second {
		t.Errorf("spawn_agent Elapsed = %v, want 13s", got)
	}
	if got := tr.Steps[5].Elapsed(); got != 0 {
		t.Errorf("unanswered step Elapsed = %v, want 0", got)
	}
}

func TestArgAccessor(t *testing.T) {
	tr := loadBasic(t)
	spawn := tr.Steps[1]
	if got := spawn.Arg("agent"); got != "cluster" {
		t.Errorf("Arg(agent) = %q", got)
	}
	if got := spawn.Arg("wait"); got != "" {
		t.Errorf("Arg on a non-string = %q, want \"\"", got)
	}
	if got := spawn.Arg("nope"); got != "" {
		t.Errorf("Arg on a missing key = %q", got)
	}
}

// responseStatus is score.py's rule. The asymmetry is the point:
// structural signals mean failure, prose only means look closer. A
// scenario-C log read that SUCCEEDS and returns the word "forbidden" is
// the drill working, not the drill failing.
func TestResponseStatusMirrorsScorePy(t *testing.T) {
	cases := []struct {
		name    string
		payload map[string]any
		want    Status
	}{
		{"plain result", map[string]any{"result": "ok"}, StatusOK},
		{"isError", map[string]any{"isError": true, "result": "x"}, StatusError},
		{"is_error snake", map[string]any{"is_error": true}, StatusError},
		{"error key set", map[string]any{"error": "boom"}, StatusError},
		{"status failed", map[string]any{"status": "failed"}, StatusError},
		{"status FAILURE cased", map[string]any{"status": "FAILURE"}, StatusError},
		{"status success", map[string]any{"status": "success"}, StatusOK},
		{"prose forbidden up front", map[string]any{"logs": "Error from server (Forbidden)"}, StatusSuspect},
		{"prose denial past the window", map[string]any{"logs": strings.Repeat("x", 300) + "permission denied"}, StatusOK},
		{"nil payload", nil, StatusSuspect},

		// The prose scan runs over the whole marshalled object, so a KEY
		// named "error" trips it even when its value says the call
		// succeeded. These four are not what anyone would design; they
		// are what score.py does, asserted here so that the day someone
		// fixes it there, this test fails and the two stay in step.
		// See the note on responseStatus.
		{"falsy isError still scans as prose", map[string]any{"isError": false, "result": "fine"}, StatusSuspect},
		{"empty error key", map[string]any{"error": "", "result": "fine"}, StatusSuspect},
		{"null error key", map[string]any{"error": nil, "result": "fine"}, StatusSuspect},
		{"empty error map", map[string]any{"error": map[string]any{}, "result": "fine"}, StatusSuspect},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := responseStatus(tc.payload, nil); got != tc.want {
				t.Errorf("responseStatus(%v) = %s, want %s", tc.payload, got, tc.want)
			}
		})
	}
}

// A run that never delegated has no subagents.json, and that is a
// legitimate run rather than a broken one.
func TestMissingSubagentsIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	copyFile(t, filepath.Join(basicRun, "meta.json"), filepath.Join(dir, "meta.json"))
	copyFile(t, filepath.Join(basicRun, "transcript.jsonl"), filepath.Join(dir, "transcript.jsonl"))

	tr, err := LoadRun(dir)
	if err != nil {
		t.Fatalf("LoadRun without subagents.json: %v", err)
	}
	for _, s := range tr.Steps {
		if s.Agent != Parent {
			t.Errorf("got a non-parent step %+v with no subagents.json", s)
		}
	}
}

// A directory we cannot parse is a finding about the recorder. Skipping
// it would make a corrupt archive look like a smaller one.
func TestUnreadableRunIsAnErrorNotASkip(t *testing.T) {
	root := t.TempDir()
	good := filepath.Join(root, "good")
	bad := filepath.Join(root, "bad")
	for _, d := range []string{good, bad} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	copyFile(t, filepath.Join(basicRun, "meta.json"), filepath.Join(good, "meta.json"))
	copyFile(t, filepath.Join(basicRun, "transcript.jsonl"), filepath.Join(good, "transcript.jsonl"))
	if err := os.WriteFile(filepath.Join(bad, "transcript.jsonl"), []byte("{not json\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadRuns(root); err == nil {
		t.Fatal("LoadRuns returned nil error over an unparseable run")
	} else if !strings.Contains(err.Error(), "transcript.jsonl:1") {
		t.Errorf("error does not name the offending line: %v", err)
	}
}

// A directory that is not a run at all is skipped, so pointing --all at
// the archive root does not trip over a stray notes/ folder.
func TestLoadRunsSkipsNonRunDirectories(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "not-a-run"), 0o755); err != nil {
		t.Fatal(err)
	}
	runs, err := LoadRuns(root)
	if err != nil {
		t.Fatalf("LoadRuns: %v", err)
	}
	if len(runs) != 0 {
		t.Errorf("got %d runs, want 0", len(runs))
	}
}

// A tool result carrying a whole Deployment YAML overruns bufio's 64 KiB
// default, which surfaces as a parse error that reads like corruption.
func TestLongTranscriptLineIsNotTruncated(t *testing.T) {
	dir := t.TempDir()
	big := strings.Repeat("y", 200<<10)
	line := `{"sse":"agent","data":{"seq":1,"event":{"InvocationID":"inv","Content":{"role":"user","parts":[{"functionResponse":{"id":"c","name":"big","response":{"yaml":"` + big + `"}}}]}}}}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "transcript.jsonl"), []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	tr, err := LoadRun(dir)
	if err != nil {
		t.Fatalf("LoadRun on a 200 KiB line: %v", err)
	}
	if len(tr.Frames) != 1 {
		t.Fatalf("got %d frames, want 1", len(tr.Frames))
	}
}

func TestWriteStepsRendersEveryStep(t *testing.T) {
	tr := loadBasic(t)
	var b strings.Builder
	if err := WriteSteps(&b, tr); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{
		"20260101T000000Z-a", "spawn_agent", "cluster", "suspect", "error",
		"no-response", "tally", "cost=$0.0123",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q:\n%s", want, out)
		}
	}
	if n := strings.Count(out, "gke_get_k8s_logs"); n < 2 {
		t.Errorf("both gke_get_k8s_logs steps should appear, got %d mentions", n)
	}
}

func TestWriteStepsOnARunWithNoToolCalls(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "transcript.jsonl"), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	tr, err := LoadRun(dir)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	if err := WriteSteps(&b, tr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "no tool calls") {
		t.Errorf("want the empty-run note, got:\n%s", b.String())
	}
}

func TestSummarizeArgsIsBoundedAndOrdered(t *testing.T) {
	got := summarizeArgs(map[string]any{
		"zeta":  strings.Repeat("z", 500),
		"alpha": "a\n b\tc",
		"count": 3,
	})
	if len(got) > 120 {
		t.Errorf("args line is %d chars, want <= 120: %q", len(got), got)
	}
	if !strings.HasPrefix(got, "alpha=a b c count=3 zeta=") {
		t.Errorf("args not rendered in key order: %q", got)
	}
}

func TestFrameTextAndRole(t *testing.T) {
	tr := loadBasic(t)
	last := tr.Frames[len(tr.Frames)-1]
	if !strings.Contains(last.Text(), "tag that does not exist") {
		t.Errorf("Text() = %q", last.Text())
	}
	if last.Role() != "model" {
		t.Errorf("Role() = %q", last.Role())
	}
	var zero Frame
	if zero.Text() != "" || zero.Role() != "" || zero.Partial() {
		t.Error("the zero Frame should be inert")
	}
}

// evidenceTally matches the sentence score.py writes at the top of every
// evidence.md, e.g.
//
//	Tool calls: **14** total — 13 returned cleanly, 0 returned an error,
//	1 returned something that reads like one, 0 never got a response.
var evidenceTally = regexp.MustCompile(
	`Tool calls: \*\*(\d+)\*\* total — (\d+) returned cleanly, (\d+) returned an error, ` +
		`(\d+) returned something that reads like one, (\d+) never got a response`)

// TestAgreesWithScorePyOnTheArchive is the claim this package rests on,
// checked against the only ground truth there is: score.py's own numbers
// on the runs it scored.
//
// It reads the local drill archive and SKIPS when there isn't one, so it
// proves nothing in CI. That is deliberate and it is the honest shape —
// the archive is operator state on an operator's machine, and copying
// fifteen runs into testdata to make a green check appear would be
// exactly the "work that gives a green check" this milestone is
// suspicious of. Run it after a drill; the result as of 2026-09-11 was
// fifteen runs, five counts each, zero disagreements.
func TestAgreesWithScorePyOnTheArchive(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory: %v", err)
	}
	root := filepath.Join(home, ".gke-drill", "runs")
	if _, err := os.Stat(root); err != nil {
		t.Skipf("no drill archive at %s — run dev/uat/gke-drill first", root)
	}

	runs, err := LoadRuns(root)
	if err != nil {
		t.Fatalf("LoadRuns(%s): %v", root, err)
	}
	if len(runs) == 0 {
		t.Skipf("%s holds no runs", root)
	}

	compared := 0
	for _, tr := range runs {
		sheet, err := os.ReadFile(filepath.Join(root, tr.Meta.RunID, "evidence.md"))
		if err != nil {
			t.Logf("%s: no scored sheet, skipping", tr.Meta.RunID)
			continue
		}
		m := evidenceTally.FindStringSubmatch(string(sheet))
		if m == nil {
			t.Logf("%s: sheet has no tool-call tally, skipping", tr.Meta.RunID)
			continue
		}
		compared++

		by := map[Status]int{}
		for _, s := range tr.Steps {
			by[s.Status]++
		}
		got := []int{len(tr.Steps), by[StatusOK], by[StatusError], by[StatusSuspect], by[StatusNoResponse]}
		labels := []string{"total", "ok", "error", "suspect", "no-response"}
		for i, label := range labels {
			want, err := strconv.Atoi(m[i+1])
			if err != nil {
				t.Fatalf("%s: unparseable tally %q", tr.Meta.RunID, m[i+1])
			}
			if got[i] != want {
				t.Errorf("%s: %s = %d, score.py says %d", tr.Meta.RunID, label, got[i], want)
			}
		}
	}
	if compared == 0 {
		t.Skip("no scored sheets to compare against")
	}
	t.Logf("agreed with score.py on %d scored runs", compared)
}

func dumpSteps(t *Trajectory) string {
	var b strings.Builder
	_ = WriteSteps(&b, t)
	return b.String()
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, b, 0o644); err != nil {
		t.Fatal(err)
	}
}
