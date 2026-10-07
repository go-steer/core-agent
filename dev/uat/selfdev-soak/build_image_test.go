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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// stubDocker is put first on PATH. It records every invocation, one per
// line, and for a build also records how many entries the context
// directory (the last argument) holds. It never builds or pushes.
const stubDocker = `#!/usr/bin/env bash
printf '%s\n' "$*" >> "${STUB_LOG}"
if [[ "$1" == build ]]; then
  ctx="${@: -1}"
  printf 'context-entries=%s\n' "$(ls -A "${ctx}" | wc -l | tr -d ' ')" >> "${STUB_LOG}"
fi
`

// stubGit answers `git ls-remote <url> refs/tags/T refs/tags/T^{}` the way
// GitHub does: an annotated tag lists the tag object and its peeled
// commit, a lightweight tag only the commit, a missing tag nothing.
// Anything else it is asked fails, so the script cannot reach the
// network through it.
var stubGit = `#!/usr/bin/env bash
[[ "$1" == ls-remote ]] || exit 99
tag="${3#refs/tags/}"
case "${tag}" in
  v0.0.0-missing) ;;
  v9.9.9-light) printf '%s\trefs/tags/%s\n' "` + lightCommit + `" "${tag}" ;;
  *) printf '%s\trefs/tags/%s\n%s\trefs/tags/%s^{}\n' "` + tagObject + `" "${tag}" "` + peeledCommit + `" "${tag}" ;;
esac
`

var (
	tagObject    = strings.Repeat("1", 40)
	peeledCommit = strings.Repeat("2", 40)
	lightCommit  = strings.Repeat("3", 40)
)

// runBuildImage executes build-image.sh with the stub docker and
// returns its exit code, combined output, and the stub's log.
func runBuildImage(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	bin := t.TempDir()
	for name, body := range map[string]string{"docker": stubDocker, "git": stubGit} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil { //nolint:gosec // an executable test stub
			t.Fatal(err)
		}
	}
	logPath := filepath.Join(t.TempDir(), "docker.log")
	cmd := exec.Command("bash", append([]string{"build-image.sh"}, args...)...)
	cmd.Env = append(os.Environ(),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"STUB_LOG="+logPath)
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run build-image.sh: %v", err)
	}
	log, _ := os.ReadFile(logPath)
	return code, string(out), string(log)
}

// TestBuildImageRefusals: every refusal happens before docker starts.
// The stub log being empty is the assertion that matters — a refusal
// that printed its message after starting a build would still pass a
// message check.
func TestBuildImageRefusals(t *testing.T) {
	sha := strings.Repeat("a", 40)
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"no ref", nil, "--ref is required"},
		{"empty ref", []string{"--ref", ""}, "--ref is required"},
		{"branch", []string{"--ref", "main"}, "is neither a release tag"},
		{"short sha", []string{"--ref", sha[:12]}, "is neither a release tag"},
		{"tag without v", []string{"--ref", "2.10.0"}, "is neither a release tag"},
		{"registry without push", []string{"--ref", "v2.10.0", "--registry", "example.test/soak"}, "--registry given without --push"},
		{"push without registry", []string{"--ref", "v2.10.0", "--push"}, "--push needs --registry"},
		{"push with empty registry", []string{"--ref", sha, "--push", "--registry="}, "--push needs --registry"},
		{"unknown flag", []string{"--ref", "v2.10.0", "--yes"}, "unknown argument"},
		{"tag not upstream", []string{"--ref", "v0.0.0-missing"}, "does not exist on"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, out, log := runBuildImage(t, tc.args...)
			if code == 0 {
				t.Fatalf("exit 0, want a refusal; output:\n%s", out)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("output does not say %q:\n%s", tc.want, out)
			}
			if log != "" {
				t.Errorf("docker was invoked despite the refusal:\n%s", log)
			}
		})
	}
}

func TestBuildImageBuildsWithoutPushing(t *testing.T) {
	code, out, log := runBuildImage(t, "--ref", "v2.10.0-dev.1")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if !strings.Contains(log, "--build-arg CORE_AGENT_REF=v2.10.0-dev.1") {
		t.Errorf("build did not pass the ref as CORE_AGENT_REF:\n%s", log)
	}
	if !strings.Contains(log, "--build-arg CORE_AGENT_COMMIT="+peeledCommit) {
		t.Errorf("build did not pass the annotated tag's peeled commit as CORE_AGENT_COMMIT:\n%s", log)
	}
	if !strings.Contains(out, "(commit "+peeledCommit+")") {
		t.Errorf("the script did not print the resolved commit:\n%s", out)
	}
	if !strings.Contains(log, "--tag core-agent-selfdev-soak:v2.10.0-dev.1") {
		t.Errorf("build did not tag core-agent-selfdev-soak:<ref>:\n%s", log)
	}
	if !strings.Contains(log, "context-entries=0") {
		t.Errorf("the build context was not empty; the local checkout could leak into the image:\n%s", log)
	}
	for _, line := range strings.Split(log, "\n") {
		if strings.HasPrefix(line, "push") || strings.HasPrefix(line, "tag ") {
			t.Errorf("pushed without --push: %q", line)
		}
	}
}

func TestBuildImagePushesOnlyWhenAsked(t *testing.T) {
	sha := strings.Repeat("0123456789", 4)
	code, out, log := runBuildImage(t, "--ref", sha, "--push", "--registry", "example.test/soak/")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	remote := "example.test/soak/core-agent-selfdev-soak:" + sha
	tagAt := strings.Index(log, "tag core-agent-selfdev-soak:"+sha+" "+remote+"\n")
	pushAt := strings.Index(log, "push "+remote+"\n")
	switch {
	case pushAt < 0:
		t.Errorf("stub log has no push of %s:\n%s", remote, log)
	case tagAt < 0 || tagAt > pushAt:
		t.Errorf("the built image was not tagged as %s before the push:\n%s", remote, log)
	}
}

func TestBuildImageResolvesCommit(t *testing.T) {
	sha := strings.Repeat("ab", 20)
	for _, tc := range []struct{ ref, want string }{
		{"v9.9.9-light", lightCommit}, // lightweight tag: no peeled line
		{sha, sha},                    // a SHA is its own commit; no lookup
	} {
		code, out, log := runBuildImage(t, "--ref", tc.ref)
		if code != 0 {
			t.Fatalf("--ref %s: exit %d:\n%s", tc.ref, code, out)
		}
		if !strings.Contains(log, "--build-arg CORE_AGENT_COMMIT="+tc.want+" ") {
			t.Errorf("--ref %s: CORE_AGENT_COMMIT is not %s:\n%s", tc.ref, tc.want, log)
		}
	}
}
