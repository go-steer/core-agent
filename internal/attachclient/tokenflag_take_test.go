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

package attachclient

import (
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
)

const (
	takeChildFlag = "CORE_AGENT_1201_TAKE_CHILD"
	takeVar       = "CORE_AGENT_1201_CLIENT_TOKEN"
	takeSecret    = "s3cret-1201-client"
)

// #1201: an attach client (core-agent-tui, `core-agent attach`, `ls`)
// read its token from the environment and kept it there, in a dumpable
// process. Any same-user process could read it back — including the
// agent it was attached to, if that agent has a shell on the machine,
// which is exactly the self-development rig's shape. With the token the
// agent can answer its own permission prompts.
//
// The client now takes the token: reads it, unsets the variable, and
// makes itself non-dumpable. This drives the real resolver in a
// re-exec'd process, then asks for the token both ways a child could get
// it. A control read of /proc runs first, before resolving, so the
// assertion cannot pass on a host where /proc is unreadable anyway.
func TestResolveTokenEnvTakesTheTokenOutOfReach(t *testing.T) {
	if os.Getenv(takeChildFlag) == "1" {
		takeChild()
		return
	}
	if runtime.GOOS != "linux" {
		t.Skip("the /proc half of this property is Linux-only")
	}
	t.Parallel()
	cmd := exec.Command(os.Args[0], "-test.run=^TestResolveTokenEnvTakesTheTokenOutOfReach$", "-test.count=1")
	cmd.Env = append(os.Environ(), takeChildFlag+"=1", takeVar+"="+takeSecret)
	out, err := cmd.CombinedOutput()
	got := string(out)
	if err != nil {
		t.Fatalf("child failed: %v\n%s", err, got)
	}
	if !strings.Contains(got, "RESOLVED="+takeSecret) {
		t.Fatalf("the resolver no longer returns the token, so the client cannot authenticate:\n%s", got)
	}
	if !strings.Contains(got, "CONTROL="+takeSecret) {
		t.Fatalf("control failed: /proc/<pid>/environ was unreadable before resolving, so the /proc assertion would be vacuous:\n%s", got)
	}
	if strings.Contains(got, "INHERITED="+takeSecret) {
		t.Errorf("a child of the client inherited the token after it was resolved:\n%s", got)
	}
	if strings.Contains(got, "PROC="+takeSecret) {
		t.Errorf("a same-user process read the token out of the client's /proc/<pid>/environ:\n%s", got)
	}
}

func takeChild() {
	probe := func(label string) string {
		out, _ := exec.Command("/bin/sh", "-c",
			`printf '`+label+`=%s\n' "$(tr '\0' '\n' < /proc/$PPID/environ 2>/dev/null | sed -n 's/^`+takeVar+`=//p')"; `+
				`printf 'INHERITED=%s\n' "$`+takeVar+`"`).CombinedOutput()
		return string(out)
	}
	control := probe("CONTROL")
	// Only the /proc half of the control matters; the child inheriting
	// it before resolving is expected.
	control = strings.SplitN(control, "\n", 2)[0] + "\n"
	tok := ResolveTokenEnv("test", takeVar, "", io.Discard)
	os.Stdout.WriteString(control + "RESOLVED=" + tok + "\n" + probe("PROC"))
	os.Exit(0)
}
