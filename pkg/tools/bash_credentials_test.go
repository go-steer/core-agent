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

package tools

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"

	adkagent "google.golang.org/adk/agent"

	"github.com/go-steer/core-agent/v2/pkg/childenv"
	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/permissions"
)

const (
	credTestChild  = "CORE_AGENT_1157_CHILD"
	credTestVar    = "CORE_AGENT_1157_ATTACH_TOKEN"
	credTestSecret = "s3cret-1157-attach-token"
)

// #1157: the attach token answers the agent's own permission prompts,
// and the bash tool used to hand it to every command the agent ran.
//
// This drives the real bash tool in a daemon-shaped process — the token
// set in the environment, withheld, and the process protected — and
// asks for the token both ways a child could get it: inherited, and
// read back out of the parent's /proc/<pid>/environ. The second is the
// one that matters. Scrubbing the child's environment alone stops only
// an agent that never looks in /proc; the parent's environ there is the
// environment it was STARTED with and does not change when the process
// edits its own.
//
// Runs in a re-exec'd copy of the test binary, because both the
// withheld set and the dumpable flag are process-global, and clearing
// the flag on the shared test process would leak into every other test.
func TestBashCannotReachTheDaemonsAttachToken(t *testing.T) {
	if os.Getenv(credTestChild) == "1" {
		credentialChild()
		return
	}
	if runtime.GOOS != "linux" {
		t.Skip("the /proc half of this property is Linux-only")
	}
	t.Parallel()
	cmd := exec.Command(os.Args[0], "-test.run=^TestBashCannotReachTheDaemonsAttachToken$", "-test.count=1")
	cmd.Env = append(os.Environ(), credTestChild+"=1", credTestVar+"="+credTestSecret)
	out, err := cmd.CombinedOutput()
	got := string(out)
	if err != nil {
		t.Fatalf("child run failed: %v\n%s", err, got)
	}
	if !strings.Contains(got, "CHILD-RAN") {
		t.Fatalf("the child never ran its bash call, so nothing below was tested:\n%s", got)
	}
	if !strings.Contains(got, "CONTROL="+credTestSecret) {
		t.Fatalf("control failed: before Protect the parent's /proc environ was not readable here, so the /proc assertion would pass vacuously:\n%s", got)
	}
	if strings.Contains(got, "INHERITED="+credTestSecret) {
		t.Errorf("a bash command inherited the attach token from the daemon's environment:\n%s", got)
	}
	if strings.Contains(got, "PROC="+credTestSecret) {
		t.Errorf("a bash command read the attach token out of the daemon's /proc/<pid>/environ:\n%s", got)
	}
}

// sciontool is the fourth exec site the daemon owns. A fake binary on
// PATH writes what it inherited to a file; the real runSciontoolStatus
// runs it.
//
// Not parallel: t.Setenv.
func TestSciontoolNeverInheritsAWithheldCredential(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake")
	}
	const name = "CORE_AGENT_1157_SCION_TOKEN"
	dir := t.TempDir()
	out := dir + "/seen"
	fake := "#!/bin/sh\nprintf 'RAN %s\\n' \"$" + name + "\" > " + out + "\n"
	if err := os.WriteFile(dir+"/sciontool", []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(name, "s3cret")
	childenv.Withhold(name)

	runSciontoolStatus("blocked", "msg")

	seen, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("the fake sciontool never ran, so nothing was tested: %v", err)
	}
	if got := strings.TrimSpace(string(seen)); got != "RAN" {
		t.Fatalf("sciontool inherited the withheld %s: %q", name, got)
	}
}

// credentialChild is the daemon side: withhold, protect, then run the
// agent's command through the real tool. It prints what the command saw
// and lets the parent judge, so a failure shows the leaked value.
func credentialChild() {
	childenv.Withhold(credTestVar)
	gate := permissions.New(permissions.Options{Mode: permissions.ModeYolo})
	// Control, before Protect: the same /proc read must succeed here, or
	// the post-Protect absence proves nothing — on a host where /proc is
	// unreadable for some other reason (hidepid, a sandboxed kernel) the
	// check would pass with no protection at all.
	ctl, err := bashFunc(gate, config.DefaultConfig())(adkagent.ToolContext(nil), bashArgs{Command: `printf 'CONTROL=%s\n' "$(tr '\0' '\n' < /proc/$PPID/environ 2>/dev/null | sed -n 's/^` + credTestVar + `=//p')"`})
	if err != nil {
		os.Stdout.WriteString("BASH-FAILED: " + err.Error() + "\n")
		os.Exit(1)
	}
	os.Stdout.WriteString(ctl.Stdout)
	if err := childenv.Protect(); err != nil {
		os.Stdout.WriteString("PROTECT-FAILED: " + err.Error() + "\n")
		os.Exit(1)
	}
	res, err := bashFunc(gate, config.DefaultConfig())(adkagent.ToolContext(nil), bashArgs{Command: `
printf 'CHILD-RAN\n'
printf 'INHERITED=%s\n' "$` + credTestVar + `"
printf 'PROC=%s\n' "$(tr '\0' '\n' < /proc/$PPID/environ 2>/dev/null | sed -n 's/^` + credTestVar + `=//p')"
`})
	if err != nil {
		os.Stdout.WriteString("BASH-FAILED: " + err.Error() + "\n")
		os.Exit(1)
	}
	os.Stdout.WriteString(res.Stdout + res.Stderr)
	os.Exit(0)
}
