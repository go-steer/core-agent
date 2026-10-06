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

package gkeplatformagent_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Every path content.Dockerfile COPYs out of the build context must be
// staged into that context by build-content-image.sh, which builds from a
// /tmp copy rather than the recipe tree.
//
// #1105 added `COPY gated-apply/` to the Dockerfile and not the matching
// `cp -a` to the script, so every content build failed at that COPY and
// no image after v4 carried the gated-apply content root. Nothing noticed
// until a live D1 deploy on v4 could not start its container: the
// read-only image volume had no mountpoint for gated-apply's plans
// emptyDir. A build that fails is loud; a recipe whose default tag
// silently predates its own Dockerfile is not.
//
// This is a "must find it" check: a missed match fails loudly, a false
// match passes a broken build, so the false match is the direction it
// must not err in. It matches comment-stripped text and NOT shellCode(),
// which blanks quoted spans — every staging line quotes its path, and
// shellCode would hide all of them. It does not tell a call from a
// mention (a cp inside a heredoc or an uncalled function would satisfy
// it), so it pins the shape it can see instead: the line must copy to
// the matching ${STAGE} destination, the script must have no heredoc,
// and the Dockerfile may use only single-source COPY and no ADD — each
// of which would otherwise slip a source past the check.
func TestEveryDockerfileCopyIsStagedByTheBuildScript(t *testing.T) {
	df, err := os.ReadFile(filepath.Join("deploy", "content.Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	sh, err := os.ReadFile(filepath.Join("scripts", "build-content-image.sh"))
	if err != nil {
		t.Fatal(err)
	}

	var sources []string
	for _, l := range strings.Split(string(df), "\n") {
		f := strings.Fields(l)
		if len(f) == 0 || strings.HasPrefix(f[0], "#") {
			continue
		}
		switch strings.ToUpper(f[0]) {
		case "ADD":
			t.Fatalf("content.Dockerfile uses ADD (%q); this check reads COPY only — use COPY", l)
		case "COPY":
		default:
			continue
		}
		var args []string
		fromStage := false
		for _, a := range f[1:] {
			if strings.HasPrefix(a, "--from") {
				fromStage = true
			}
			if !strings.HasPrefix(a, "--") {
				args = append(args, a)
			}
		}
		if fromStage {
			continue
		}
		if len(args) != 2 || strings.HasPrefix(args[0], "[") || args[0] == "\\" {
			t.Fatalf("content.Dockerfile COPY %q is not `COPY <one source> <dest>`; this check cannot read it", l)
		}
		sources = append(sources, strings.TrimSuffix(strings.TrimPrefix(args[0], "./"), "/"))
	}
	if len(sources) == 0 {
		t.Fatal("found no COPY in content.Dockerfile; the test would check nothing")
	}

	if regexp.MustCompile(`<<-?\s*['"]?\w`).Match(sh) {
		t.Fatal("build-content-image.sh has a heredoc; this check cannot tell a staging line inside one from a real one")
	}
	var code strings.Builder
	for _, l := range strings.Split(string(sh), "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "#") {
			continue
		}
		code.WriteString(l)
		code.WriteByte('\n')
	}
	for _, src := range sources {
		q := regexp.QuoteMeta(src)
		stage := regexp.MustCompile(`(?m)^\s*cp\s+-a\s+"\$\{RECIPE_ROOT\}/` + q + `"\s+"\$\{STAGE\}/` + q + `"\s*$`)
		if !stage.MatchString(code.String()) {
			t.Errorf("content.Dockerfile COPYs %q but build-content-image.sh never stages it "+
				"(expected a `cp -a \"${RECIPE_ROOT}/%s\" ...` line); the build fails at that COPY", src, src)
		}
	}
}
