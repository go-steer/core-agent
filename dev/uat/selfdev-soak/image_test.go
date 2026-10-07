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

// Package selfdevsoak holds the checks on the self-development soak's
// image and config (#1213, prerequisite P3). There is no library code
// here: the directory ships a Dockerfile, a build script and a config
// overlay, and these tests are what stop them drifting from the rules
// the design doc settled (decision 11's pinned ref, the repo's one Go
// toolchain, a digest-pinned base, a non-root user, and pushes that
// only happen on request).
package selfdevsoak

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

const dockerfilePath = "Dockerfile"

// repoRoot is three levels up: dev/uat/selfdev-soak.
var repoRoot = filepath.Join("..", "..", "..")

// instruction is one Dockerfile instruction with its continuation lines
// joined, comments dropped.
type instruction struct {
	op   string // upper-cased: FROM, ARG, RUN, ...
	args string
}

// parseDockerfile is deliberately small: it knows comments, `\`
// continuations and the instruction keyword, which is all the
// assertions below read. A Dockerfile shape it cannot parse makes those
// assertions fail rather than pass, because each one looks for a
// specific instruction and reports its absence.
func parseDockerfile(t *testing.T) []instruction {
	t.Helper()
	body, err := os.ReadFile(dockerfilePath)
	if err != nil {
		t.Fatal(err)
	}
	var out []instruction
	var cur strings.Builder
	for _, line := range strings.Split(string(body), "\n") {
		trimmed := strings.TrimSpace(line)
		if cur.Len() == 0 && (trimmed == "" || strings.HasPrefix(trimmed, "#")) {
			continue
		}
		if cur.Len() > 0 && strings.HasPrefix(trimmed, "#") {
			continue // a comment inside a continued instruction
		}
		if strings.HasSuffix(trimmed, `\`) {
			cur.WriteString(strings.TrimSuffix(trimmed, `\`))
			cur.WriteString(" ")
			continue
		}
		cur.WriteString(trimmed)
		full := cur.String()
		cur.Reset()
		op, args, _ := strings.Cut(full, " ")
		out = append(out, instruction{op: strings.ToUpper(op), args: strings.TrimSpace(args)})
	}
	return out
}

// stages splits instructions at each FROM. Index 0 is the global
// preamble (ARGs before the first FROM).
func stages(ins []instruction) [][]instruction {
	out := [][]instruction{nil}
	for _, in := range ins {
		if in.op == "FROM" {
			out = append(out, nil)
		}
		out[len(out)-1] = append(out[len(out)-1], in)
	}
	return out
}

// globalArg returns the default of a pre-FROM ARG.
func globalArg(t *testing.T, ins []instruction, name string) string {
	t.Helper()
	for _, in := range stages(ins)[0] {
		if in.op == "ARG" && strings.HasPrefix(in.args, name+"=") {
			return strings.TrimPrefix(in.args, name+"=")
		}
	}
	t.Fatalf("Dockerfile has no global `ARG %s=<default>`", name)
	return ""
}

// resolvedToolchain asks the repo's single resolver, rather than
// re-reading go.mod here: a second parser is how #736 shipped images
// built on the wrong Go.
func resolvedToolchain(t *testing.T) string {
	t.Helper()
	//
	// The path must be absolute. common.sh re-derives the repo root from
	// its own relative dirname after the script has already cd'd to the
	// root, so a relative path resolves against the wrong directory. In
	// a git worktree under .claude/worktrees/ that wrong directory is the
	// main checkout, whose go.mod answered silently, which is how this
	// went unnoticed until the sweep ran inside the soak image.
	script, err := filepath.Abs(filepath.Join(repoRoot, "dev", "tools", "verify-go-toolchain"))
	if err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	cmd := exec.Command("bash", script, "--print")
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("dev/tools/verify-go-toolchain --print: %v\n%s", err, stderr.String())
	}
	return strings.TrimSpace(string(out))
}

func TestDockerfileRequiresCoreAgentRef(t *testing.T) {
	ins := parseDockerfile(t)
	declared := false
	for _, in := range ins {
		if in.op == "ENV" && strings.Contains(in.args, "CORE_AGENT_REF") {
			t.Errorf("ENV sets CORE_AGENT_REF (%q), which would act as a default", in.args)
		}
		if in.op != "ARG" {
			continue
		}
		// One ARG can declare several names (`ARG A=1 CORE_AGENT_REF=x`).
		for _, field := range strings.Fields(in.args) {
			switch {
			case strings.HasPrefix(field, "CORE_AGENT_REF="):
				t.Errorf("CORE_AGENT_REF has a default (ARG %s); decision 11 requires a build that fails without an explicit ref", in.args)
			case field == "CORE_AGENT_REF":
				declared = true
			}
		}
	}
	if !declared {
		t.Fatal("Dockerfile declares no `ARG CORE_AGENT_REF`")
	}
	if !hasRun(ins, `if [[ -z "${CORE_AGENT_REF:-}" ]]; then`) {
		t.Error("no RUN guards an empty CORE_AGENT_REF; an unset build arg would build whatever FETCH_HEAD is")
	}
}

func hasRun(ins []instruction, fragment string) bool {
	for _, in := range ins {
		if in.op == "RUN" && strings.Contains(in.args, fragment) {
			return true
		}
	}
	return false
}

// TestDockerfileBuildsFromUpstreamOnly pins decision 11's other half:
// the source comes from go-steer/core-agent at the ref, never from the
// build context, so neither a local checkout nor the mirror can leak in.
func TestDockerfileBuildsFromUpstreamOnly(t *testing.T) {
	ins := parseDockerfile(t)
	for _, in := range ins {
		if (in.op == "COPY" || in.op == "ADD") && !strings.HasPrefix(in.args, "--from=") {
			t.Errorf("%s %s copies from the build context; the image must build only what it clones from upstream", in.op, in.args)
		}
	}
	if !hasRun(ins, "remote add origin https://github.com/go-steer/core-agent.git") {
		t.Error("the builder no longer clones https://github.com/go-steer/core-agent.git")
	}
	// GitHub serves any object in the repo's network by id, unmerged PR
	// heads included, so a SHA is only "upstream" once it is on main.
	if !hasRun(ins, `merge-base --is-ancestor "${CORE_AGENT_REF}" refs/remotes/origin/main`) {
		t.Error("the builder no longer requires a SHA ref to be on upstream main")
	}
}

var digestRef = regexp.MustCompile(`^golang:([0-9][^@\s]*)-bookworm@sha256:[0-9a-f]{64}$`)

func TestDockerfileBasesPinnedByDigestOnResolvedToolchain(t *testing.T) {
	ins := parseDockerfile(t)
	want := resolvedToolchain(t)
	if got := globalArg(t, ins, "GO_VERSION"); got != want {
		t.Errorf("ARG GO_VERSION=%s, but dev/tools/verify-go-toolchain resolves %s", got, want)
	}
	stageNames := map[string]bool{}
	froms := 0
	var bases []string
	for _, in := range ins {
		if in.op != "FROM" {
			continue
		}
		froms++
		fields := strings.Fields(in.args)
		ref := fields[0]
		if len(fields) == 3 && strings.EqualFold(fields[1], "AS") {
			stageNames[fields[2]] = true
		}
		if stageNames[ref] {
			continue // FROM <earlier stage>: pinned where that stage was
		}
		bases = append(bases, ref)
		m := digestRef.FindStringSubmatch(ref)
		if m == nil {
			t.Errorf("FROM %s is not golang:<version>-bookworm@sha256:<digest>; a base must be pinned by digest and must not take a build-arg", ref)
			continue
		}
		if m[1] != want {
			t.Errorf("FROM %s carries Go %s, but the repo resolves %s; bump the tag AND the digest (docker buildx imagetools inspect golang:%s-bookworm)", ref, m[1], want, want)
		}
	}
	if froms < 2 {
		t.Errorf("found %d FROM lines, want a builder and a runtime stage", froms)
	}
	// The builder checks its digest's Go at build time; the runtime stage
	// is covered by that check only if it is the same image.
	for i := 1; i < len(bases); i++ {
		if b := bases[i]; b != bases[0] {
			t.Errorf("FROM %s differs from the builder's FROM %s; both stages must use the one pinned image", b, bases[0])
		}
	}
	if !hasRun(ins, `if [[ "${have}" != "go${GO_VERSION}" ]]; then`) {
		t.Error("the builder no longer checks the base image's Go against GO_VERSION; a stale digest under a bumped tag would build silently")
	}
}

// TestDockerfileIsUnderTheToolchainGate keeps this Dockerfile inside
// dev/tools/verify-go-toolchain's static check, so a go.mod toolchain
// bump fails that presubmit too, not just this test.
func TestDockerfileIsUnderTheToolchainGate(t *testing.T) {
	body, err := os.ReadFile(filepath.Join(repoRoot, "dev", "tools", "verify-go-toolchain"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^DOCKERFILES=\(([^)]*)\)`).FindSubmatch(body)
	if m == nil {
		t.Fatal("verify-go-toolchain has no DOCKERFILES=(...) list")
	}
	for _, f := range strings.Fields(string(m[1])) {
		if f == "dev/uat/selfdev-soak/Dockerfile" {
			return
		}
	}
	t.Errorf("dev/uat/selfdev-soak/Dockerfile is not in verify-go-toolchain's DOCKERFILES (%s)", m[1])
}

func TestDockerfileRunsAsNonRoot(t *testing.T) {
	st := stages(parseDockerfile(t))
	final := st[len(st)-1]
	user, uidArg := "", ""
	for _, in := range final {
		switch {
		case in.op == "USER":
			user = in.args
		case in.op == "ARG" && strings.HasPrefix(in.args, "SOAK_UID="):
			uidArg = strings.TrimPrefix(in.args, "SOAK_UID=")
		}
	}
	if user == "" {
		t.Fatal("the runtime stage sets no USER; the image would run as root")
	}
	uidStr, gidStr, _ := strings.Cut(user, ":")
	uid, err := strconv.Atoi(uidStr)
	if err != nil {
		t.Fatalf("USER %s is not numeric; a pod's runAsNonRoot cannot verify a user name", user)
	}
	if uid < 10000 {
		t.Errorf("USER uid %d, want >= 10000", uid)
	}
	if gidStr != "" {
		if gid, err := strconv.Atoi(gidStr); err != nil || gid == 0 {
			t.Errorf("USER group %q must be a numeric, non-root gid", gidStr)
		}
	}
	if uidArg != uidStr {
		t.Errorf("USER %s does not match the uid the stage creates (SOAK_UID=%s)", user, uidArg)
	}
}

// TestDockerfileHasNoGitHubClient: the dispatcher pushes and opens PRs
// (decision 7), so gh has no business in the agent's pod.
func TestDockerfileHasNoGitHubClient(t *testing.T) {
	ghWord := regexp.MustCompile(`(^|[\s;&|/])gh(\s|$)|github-cli|cli\.github\.com`)
	for _, in := range parseDockerfile(t) {
		if in.op == "RUN" && ghWord.MatchString(in.args) {
			t.Errorf("a RUN mentions gh: %s", in.args)
		}
	}
}

// TestDockerfileTrustsSharedWorkspace: the dispatcher's pod creates the
// per-issue worktrees on the shared PVC, possibly under another uid, and
// git (and `go build`'s VCS stamping) refuses a repository another uid
// owns. Found by running the presubmit sweep inside the image.
func TestDockerfileTrustsSharedWorkspace(t *testing.T) {
	if !hasRun(parseDockerfile(t), `config --system --add safe.directory '*'`) {
		t.Error("the image no longer marks repositories safe system-wide; every git call fails on a worktree the dispatcher created under another uid")
	}
}

func TestDockerfileEntrypointPinsConfig(t *testing.T) {
	for _, in := range parseDockerfile(t) {
		if in.op == "ENTRYPOINT" {
			if !strings.Contains(in.args, `"-c", "/etc/core-agent-soak/config.json"`) {
				t.Errorf("ENTRYPOINT %s does not pin -c /etc/core-agent-soak/config.json; the workspace clone's committed recipe (mode ask) would be discovered instead", in.args)
			}
			return
		}
	}
	t.Error("no ENTRYPOINT")
}
