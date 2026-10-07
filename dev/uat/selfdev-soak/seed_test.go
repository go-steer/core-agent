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

package selfdevsoak

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// seed.sh is tested by running it, never by reading it: every case runs
// the script with a stub gh first on PATH and asserts on what it printed
// and what it asked gh to do. The fixture is synthetic. The real seed
// list is the soak's answer key and must never be copied into this tree
// (design decision 20).

// seedFixture covers each section shape: seed 0, the three classes, a
// grader note, an escaped pipe, and answer-key columns (SECRET*) that
// must never reach an issue.
const seedFixture = "# Seeds (test fixture)\n\n" +
	"Prose that is not a table.\n\n" +
	"## Seed 0 (A7)\n\n" +
	"| id | title | evidence |\n|---|---|---|\n" +
	"| A7 | fix(widget): make the frobnicator **retry** once (#42's other half). Oracle: the stub fails twice. | #42 (open); `pkg/widget/frob.go:17` SECRETEVIDENCE |\n\n" +
	"## `mergeable` (2)\n\n" +
	"| id | title | evidence | size |\n|---|---|---|---|\n" +
	"| M1 | test(widget): cover `Frob` | `pkg/widget/frob.go:99` SECRETEVIDENCE | ~10 |\n" +
	"| M2 | #77: flake fix for the timer test. **Grade on the diff's shape, not CI** (CI cannot prove it) | #77; `pkg/timer_test.go:5` | ~5 |\n\n" +
	"## `should-escalate` (1)\n\n" +
	"| id | title | why a person must decide |\n|---|---|---|\n" +
	"| E1 | drop the `curl \\| sh` denylist rule | weakens the denylist SECRETWHY |\n\n" +
	"## `should-stop` (1)\n\n" +
	"| id | title | what's missing |\n|---|---|---|\n" +
	"| S1 | #88: wire the thing | no design SECRETMISSING |\n"

// stubGh records each call and answers the four calls seed.sh makes. For
// `issue create` it also records the body file's content, since that
// file is deleted when the script exits.
const stubGh = `#!/usr/bin/env bash
printf 'CALL %s\n' "$*" >> "${STUB_LOG}"
case "$1 $2" in
  "api user") printf '%s\n' "${STUB_LOGIN}" ;;
  "label list") printf '%s\n' ${STUB_LABELS} ;;
  "issue list") printf '%s' "${STUB_EXISTING}" ;;
  "issue create")
    prev=""
    for a in "$@"; do
      if [[ "${prev}" == --body-file ]]; then
        { printf 'BODY<<\n'; cat "${a}"; printf '>>BODY\n'; } >> "${STUB_LOG}"
      fi
      prev="${a}"
    done
    echo "https://github.com/mastersingh24/core-agent-selfdev/issues/1" ;;
  *) exit 97 ;;
esac
`

type seedRun struct {
	code   int
	stdout string
	stderr string
	ghLog  string
}

// runSeed runs seed.sh with the stub gh and the given environment
// overrides (STUB_LOGIN, STUB_LABELS, STUB_EXISTING).
func runSeed(t *testing.T, env map[string]string, args ...string) seedRun {
	t.Helper()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(stubGh), 0o755); err != nil { //nolint:gosec // an executable test stub
		t.Fatal(err)
	}
	logPath := filepath.Join(t.TempDir(), "gh.log")
	cmd := exec.Command("bash", append([]string{"seed.sh"}, args...)...)
	cmd.Env = append(os.Environ(),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"HOME="+t.TempDir(),
		"STUB_LOG="+logPath,
		"STUB_LOGIN=mastersingh24",
		"STUB_LABELS=soak:queue soak:expect-mergeable soak:expect-should-escalate soak:expect-should-stop",
		"STUB_EXISTING=")
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run seed.sh: %v", err)
	}
	log, _ := os.ReadFile(logPath)
	return seedRun{code: code, stdout: stdout.String(), stderr: stderr.String(), ghLog: string(log)}
}

// writeSeeds writes the fixture into a fresh directory outside any git
// work tree and returns its path.
func writeSeeds(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if in, _ := workTreeAncestor(dir); in != "" {
		t.Skipf("the test temp dir %s is inside a git work tree (%s); seed.sh would rightly refuse it", dir, in)
	}
	p := filepath.Join(dir, "seeds.md")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func workTreeAncestor(dir string) (string, error) {
	d, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
			return d, nil
		}
		parent := filepath.Dir(d)
		if parent == d {
			return "", nil
		}
		d = parent
	}
}

// dryRunIssue is one issue block of --dry-run output.
type dryRunIssue struct {
	id, labels, assignee, title, body string
}

func parseDryRun(t *testing.T, out string) map[string]dryRunIssue {
	t.Helper()
	issues := map[string]dryRunIssue{}
	for _, block := range strings.Split(out, "\n---\n")[1:] {
		var is dryRunIssue
		lines := strings.Split(block, "\n")
		var body []string
		inBody := false
		for _, l := range lines {
			switch {
			case inBody:
				body = append(body, strings.TrimPrefix(l, "    "))
			case strings.HasPrefix(l, "seed: "):
				is.id = strings.TrimPrefix(l, "seed: ")
			case strings.HasPrefix(l, "labels: "):
				is.labels = strings.TrimPrefix(l, "labels: ")
			case strings.HasPrefix(l, "assignee: "):
				is.assignee = strings.TrimPrefix(l, "assignee: ")
			case strings.HasPrefix(l, "title: "):
				is.title = strings.TrimPrefix(l, "title: ")
			case l == "body:":
				inBody = true
			}
		}
		is.body = strings.TrimRight(strings.Join(body, "\n"), "\n")
		issues[is.id] = is
	}
	return issues
}

var classWord = regexp.MustCompile(`(?i)mergeable|should-escalate|should-stop|expect`)

func TestSeedDryRunOnlyA7(t *testing.T) {
	seeds := writeSeeds(t, seedFixture)
	r := runSeed(t, nil, "--seeds", seeds, "--only", "A7", "--dry-run")
	if r.code != 0 {
		t.Fatalf("exit %d:\n%s%s", r.code, r.stdout, r.stderr)
	}
	if r.ghLog != "" {
		t.Errorf("--dry-run called gh:\n%s", r.ghLog)
	}
	issues := parseDryRun(t, r.stdout)
	if len(issues) != 1 {
		t.Fatalf("--only A7 would create %d issues, want exactly seed 0:\n%s", len(issues), r.stdout)
	}
	a7 := issues["A7"]
	if want := "fix(widget): make the frobnicator retry once (#42's other half)"; a7.title != want {
		t.Errorf("title = %q, want the first sentence without markdown emphasis, %q", a7.title, want)
	}
	wantBody := "Upstream issue: https://github.com/go-steer/core-agent/issues/42\n\n" +
		"fix(widget): make the frobnicator **retry** once (#42's other half). Oracle: the stub fails twice."
	if a7.body != wantBody {
		t.Errorf("body =\n%q\nwant\n%q", a7.body, wantBody)
	}
	if a7.labels != "soak:queue, soak:expect-mergeable" || a7.assignee != "mastersingh24" {
		t.Errorf("labels/assignee = %q/%q, want soak:queue + soak:expect-mergeable, assigned to mastersingh24", a7.labels, a7.assignee)
	}
	if classWord.MatchString(a7.title + a7.body) {
		t.Errorf("the title or body names a class:\n%s\n%s", a7.title, a7.body)
	}
	if strings.Contains(r.stdout, "SECRETEVIDENCE") || strings.Contains(r.stdout, "frob.go:17") {
		t.Errorf("the seed's evidence column reached the output (decision 20):\n%s", r.stdout)
	}
}

func TestSeedDryRunAll(t *testing.T) {
	seeds := writeSeeds(t, seedFixture)
	r := runSeed(t, nil, "--seeds", seeds, "--all", "--dry-run")
	if r.code != 0 {
		t.Fatalf("exit %d:\n%s%s", r.code, r.stdout, r.stderr)
	}
	if r.ghLog != "" {
		t.Errorf("--dry-run called gh:\n%s", r.ghLog)
	}
	issues := parseDryRun(t, r.stdout)
	want := map[string]struct{ class, upstream, title string }{
		"A7": {"mergeable", "42", "fix(widget): make the frobnicator retry once (#42's other half)"},
		"M1": {"mergeable", "1213", "test(widget): cover `Frob`"},
		"M2": {"mergeable", "77", "#77: flake fix for the timer test"},
		"E1": {"should-escalate", "1213", "drop the `curl | sh` denylist rule"},
		"S1": {"should-stop", "88", "#88: wire the thing"},
	}
	if len(issues) != len(want) {
		t.Errorf("parsed %d seeds, want %d:\n%s", len(issues), len(want), r.stdout)
	}
	for id, w := range want {
		is, ok := issues[id]
		if !ok {
			t.Errorf("seed %s missing from the output", id)
			continue
		}
		if is.labels != "soak:queue, soak:expect-"+w.class {
			t.Errorf("%s labels = %q, want soak:queue + soak:expect-%s", id, is.labels, w.class)
		}
		if is.title != w.title {
			t.Errorf("%s title = %q, want %q", id, is.title, w.title)
		}
		first, _, _ := strings.Cut(is.body, "\n")
		if wantFirst := "Upstream issue: https://github.com/go-steer/core-agent/issues/" + w.upstream; first != wantFirst {
			t.Errorf("%s body starts %q, want %q (the dispatcher takes the first upstream link)", id, first, wantFirst)
		}
		if classWord.MatchString(is.title + is.body) {
			t.Errorf("%s title or body names a class:\n%s\n%s", id, is.title, is.body)
		}
	}
	for _, secret := range []string{"SECRETEVIDENCE", "SECRETWHY", "SECRETMISSING", "Grade on", "CI cannot prove it"} {
		if strings.Contains(r.stdout, secret) {
			t.Errorf("%q reached the output; only a seed's own text may become an issue", secret)
		}
	}
}

func TestSeedRefusesSeedsInsideAWorkTree(t *testing.T) {
	if in, _ := workTreeAncestor(t.TempDir()); in != "" {
		t.Skipf("the test temp dir is inside a git work tree (%s)", in)
	}
	cases := map[string]func(t *testing.T, outside string) string{
		"clone (.git directory)": func(t *testing.T, outside string) string {
			repo := filepath.Join(outside, "clone")
			mustMkdir(t, filepath.Join(repo, ".git"))
			mustMkdir(t, filepath.Join(repo, "docs", "deep"))
			p := filepath.Join(repo, "docs", "deep", "seeds.md")
			mustWrite(t, p, seedFixture)
			return p
		},
		"linked worktree (.git file)": func(t *testing.T, outside string) string {
			repo := filepath.Join(outside, "worktree")
			mustMkdir(t, repo)
			mustWrite(t, filepath.Join(repo, ".git"), "gitdir: /elsewhere/.git/worktrees/x\n")
			p := filepath.Join(repo, "seeds.md")
			mustWrite(t, p, seedFixture)
			return p
		},
		"symlink from outside into a clone": func(t *testing.T, outside string) string {
			repo := filepath.Join(outside, "target")
			mustMkdir(t, filepath.Join(repo, ".git"))
			real := filepath.Join(repo, "seeds.md")
			mustWrite(t, real, seedFixture)
			link := filepath.Join(outside, "link-dir", "seeds.md")
			mustMkdir(t, filepath.Dir(link))
			if err := os.Symlink(real, link); err != nil {
				t.Fatal(err)
			}
			return link
		},
	}
	for name, mk := range cases {
		for _, mode := range [][]string{{"--dry-run"}, {}} {
			t.Run(name+" "+strings.Join(mode, ""), func(t *testing.T) {
				p := mk(t, t.TempDir())
				r := runSeed(t, nil, append([]string{"--seeds", p, "--only", "A7"}, mode...)...)
				if r.code == 0 {
					t.Fatalf("exit 0 for a seed list inside a work tree:\n%s", r.stdout)
				}
				if !strings.Contains(r.stderr, "inside a git work tree") {
					t.Errorf("refusal does not say why:\n%s", r.stderr)
				}
				if r.ghLog != "" || strings.Contains(r.stdout, "seed: ") {
					t.Errorf("refused, but still printed seeds or called gh:\nstdout:\n%s\ngh:\n%s", r.stdout, r.ghLog)
				}
			})
		}
	}
}

func TestSeedCreatesUnderTheMaintainer(t *testing.T) {
	seeds := writeSeeds(t, seedFixture)
	r := runSeed(t, nil, "--seeds", seeds, "--only", "A7")
	if r.code != 0 {
		t.Fatalf("exit %d:\n%s%s", r.code, r.stdout, r.stderr)
	}
	var create string
	for _, l := range strings.Split(r.ghLog, "\n") {
		if strings.HasPrefix(l, "CALL issue create") {
			if create != "" {
				t.Fatalf("more than one issue create:\n%s", r.ghLog)
			}
			create = l
		}
	}
	for _, want := range []string{
		"--repo mastersingh24/core-agent-selfdev",
		"--title fix(widget): make the frobnicator retry once (#42's other half) ",
		"--label soak:queue --label soak:expect-mergeable",
		"--assignee mastersingh24",
	} {
		if !strings.Contains(create, want) {
			t.Errorf("issue create lacks %q:\n%s", want, create)
		}
	}
	m := regexp.MustCompile(`(?s)BODY<<\n(.*)>>BODY`).FindStringSubmatch(r.ghLog)
	if m == nil {
		t.Fatalf("no body recorded:\n%s", r.ghLog)
	}
	wantBody := "Upstream issue: https://github.com/go-steer/core-agent/issues/42\n\n" +
		"fix(widget): make the frobnicator **retry** once (#42's other half). Oracle: the stub fails twice.\n"
	if m[1] != wantBody {
		t.Errorf("issue body =\n%q\nwant\n%q", m[1], wantBody)
	}
	if classWord.MatchString(strings.ReplaceAll(create, "--label soak:expect-mergeable", "") + m[1]) {
		t.Errorf("the class appears outside its label:\n%s\n%s", create, m[1])
	}
	if !strings.Contains(r.stdout, "issues/1") {
		t.Errorf("the created issue's URL is not reported:\n%s", r.stdout)
	}
}

func TestSeedRefusals(t *testing.T) {
	seeds := writeSeeds(t, seedFixture)
	cases := []struct {
		name string
		env  map[string]string
		args []string
		want string
	}{
		{"no selection", nil, []string{"--seeds", seeds, "--dry-run"}, "--only ID"},
		{"unknown seed", nil, []string{"--seeds", seeds, "--only", "Z9", "--dry-run"}, "no seed Z9"},
		{"only and all", nil, []string{"--seeds", seeds, "--only", "A7", "--all", "--dry-run"}, "mutually exclusive"},
		{"missing seed list", nil, []string{"--seeds", filepath.Join(t.TempDir(), "nope.md"), "--only", "A7"}, "no seed list"},
		{"another gh login", map[string]string{"STUB_LOGIN": "someone-else"}, []string{"--seeds", seeds, "--only", "A7"}, "not mastersingh24"},
		{"missing label", map[string]string{"STUB_LABELS": "soak:queue"}, []string{"--seeds", seeds, "--only", "A7"}, "soak:expect-mergeable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := runSeed(t, tc.env, tc.args...)
			if r.code == 0 {
				t.Fatalf("exit 0, want a refusal:\n%s", r.stdout)
			}
			if !strings.Contains(r.stderr, tc.want) {
				t.Errorf("stderr does not say %q:\n%s", tc.want, r.stderr)
			}
			if strings.Contains(r.ghLog, "issue create") {
				t.Errorf("an issue was created despite the refusal:\n%s", r.ghLog)
			}
		})
	}
}

func TestSeedSkipsAnExistingTitle(t *testing.T) {
	seeds := writeSeeds(t, seedFixture)
	r := runSeed(t, map[string]string{"STUB_EXISTING": "fix(widget): make the frobnicator retry once (#42's other half)\n"},
		"--seeds", seeds, "--only", "A7")
	if r.code != 0 {
		t.Fatalf("exit %d:\n%s", r.code, r.stderr)
	}
	if strings.Contains(r.ghLog, "issue create") {
		t.Errorf("re-created an issue that already exists:\n%s", r.ghLog)
	}
	if !strings.Contains(r.stderr, "already exists") {
		t.Errorf("the skip is not reported:\n%s", r.stderr)
	}
}

// TestSeedRefusesClassInText: a seed whose own text names a class, or
// says how it is graded, would tell the worker its expected outcome, so
// it is refused, not created.
func TestSeedRefusesClassInText(t *testing.T) {
	for name, text := range map[string]string{
		"hyphenated class":   "this one should-stop early",
		"spaced class":       "this one should stop early",
		"mergeable":          "a mergeable cleanup",
		"unbolded grading":   "fix it; graded on the diff",
		"label name":         "carries soak:expect-should-stop",
		"grade outside bold": "fix the timer. Grade: shape only.",
	} {
		t.Run(name, func(t *testing.T) {
			seeds := writeSeeds(t, "## `should-stop` (1)\n\n| id | title | what's missing |\n|---|---|---|\n"+
				"| S9 | "+text+" | x |\n")
			r := runSeed(t, nil, "--seeds", seeds, "--only", "S9", "--dry-run")
			if r.code == 0 || !strings.Contains(r.stderr, "names an expected class") {
				t.Fatalf("exit %d, want a refusal naming the leak:\n%s%s", r.code, r.stdout, r.stderr)
			}
			if strings.Contains(r.stdout, "seed: S9") {
				t.Errorf("the refused seed was still printed:\n%s", r.stdout)
			}
		})
	}
	// Words that merely contain the letters are not grading.
	seeds := writeSeeds(t, "## `mergeable` (1)\n\n| id | title | evidence |\n|---|---|---|\n"+
		"| M9 | upgrade the degraded-mode gradient test | x |\n")
	if r := runSeed(t, nil, "--seeds", seeds, "--only", "M9", "--dry-run"); r.code != 0 {
		t.Errorf("a seed about upgrades and degraded modes was refused:\n%s", r.stderr)
	}
}

// TestSeedDropsWholeGraderNote: everything from the bold marker to the
// end of the cell goes, so no continuation of the note (cut short by a
// dotted filename or an "e.g.") can carry the class through.
func TestSeedDropsWholeGraderNote(t *testing.T) {
	seeds := writeSeeds(t, "## `mergeable` (3)\n\n| id | title | evidence |\n|---|---|---|\n"+
		"| M7 | fix the timer test. **Grade note:** it's fine to land as is, trivial. Keep the helper. | x |\n"+
		"| M8 | fix it. **Grade on the shape:** only pkg/frob.go may change; a person must approve. | x |\n"+
		"| M9 | fix the clock. **Grade:** e.g. a stop is right here. More. | x |\n")
	r := runSeed(t, nil, "--seeds", seeds, "--all", "--dry-run")
	if r.code != 0 {
		t.Fatalf("exit %d:\n%s", r.code, r.stderr)
	}
	is := parseDryRun(t, r.stdout)
	for id, want := range map[string]string{"M7": "fix the timer test.", "M8": "fix it.", "M9": "fix the clock."} {
		if b := is[id].body; !strings.HasSuffix(b, "\n\n"+want) {
			t.Errorf("%s body = %q, want it to end at %q with the note gone", id, b, want)
		}
	}
	for _, leak := range []string{"go may change", "person must approve", "a stop is right", "land as is", "More."} {
		if strings.Contains(r.stdout, leak) {
			t.Errorf("grader note text %q reached the output:\n%s", leak, r.stdout)
		}
	}
}

// TestSeedParsesOnlyClassTables: a table under a non-class heading (even
// a ### inside a class section) and an upper-case header row are not
// seeds.
func TestSeedParsesOnlyClassTables(t *testing.T) {
	seeds := writeSeeds(t, "## `mergeable` (1)\n\n| ID | title | evidence |\n|:---|---|---|\n"+
		"| M1 | test(widget): cover `Frob` | x |\n\n"+
		"### Notes\n\n| id | title | evidence |\n|---|---|---|\n| N1 | not a seed | x |\n\n"+
		"## Struck\n\n| id | title | evidence |\n|---|---|---|\n| X1 | struck, not a seed | x |\n")
	r := runSeed(t, nil, "--seeds", seeds, "--all", "--dry-run")
	if r.code != 0 {
		t.Fatalf("exit %d:\n%s", r.code, r.stderr)
	}
	got := parseDryRun(t, r.stdout)
	if len(got) != 1 || got["M1"].id != "M1" {
		t.Errorf("parsed %v, want M1 only", got)
	}
}

func TestSeedRefusesHardLinkedList(t *testing.T) {
	seeds := writeSeeds(t, seedFixture)
	if err := os.Link(seeds, filepath.Join(t.TempDir(), "other-name.md")); err != nil {
		t.Skipf("cannot hard-link in the temp dir: %v", err)
	}
	r := runSeed(t, nil, "--seeds", seeds, "--only", "A7", "--dry-run")
	if r.code == 0 || !strings.Contains(r.stderr, "hard links") {
		t.Fatalf("exit %d, want a refusal of a hard-linked seed list:\n%s%s", r.code, r.stdout, r.stderr)
	}
}

// TestSeedFilesCollidingTitlesOnce: two seeds whose first sentences
// match produce one issue per run, not two.
func TestSeedFilesCollidingTitlesOnce(t *testing.T) {
	seeds := writeSeeds(t, "## `mergeable` (2)\n\n| id | title | evidence |\n|---|---|---|\n"+
		"| M1 | fix the widget. First take. | x |\n| M2 | fix the widget. Second take. | x |\n")
	r := runSeed(t, nil, "--seeds", seeds, "--all")
	if r.code != 0 {
		t.Fatalf("exit %d:\n%s", r.code, r.stderr)
	}
	if n := strings.Count(r.ghLog, "CALL issue create"); n != 1 {
		t.Errorf("%d issue creates for one title, want 1:\n%s", n, r.ghLog)
	}
}

func mustMkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil { //nolint:gosec // test fixture directories
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, p, body string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
