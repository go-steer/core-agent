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
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func clientTokenFile(t *testing.T, content string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

// Flag precedence for the attach clients' one resolver. Not parallel:
// t.Setenv, and the file cases make the test process non-dumpable,
// which is harmless here.
func TestResolveTokenPrecedence(t *testing.T) {
	t.Setenv("CORE_AGENT_1201_RT_ENV", "env-token")
	file := clientTokenFile(t, "file-token\n", 0o600)

	t.Run("file alone", func(t *testing.T) {
		var warn bytes.Buffer
		got, err := ResolveToken("test", file, "", "", &warn)
		if err != nil || got != "file-token" {
			t.Fatalf("got %q, %v; want file-token", got, err)
		}
	})
	t.Run("env alone still works through ResolveTokenEnv", func(t *testing.T) {
		got, err := ResolveToken("test", "", "CORE_AGENT_1201_RT_ENV", "", nil)
		if err != nil || got != "env-token" {
			t.Fatalf("got %q, %v; want env-token", got, err)
		}
	})
	t.Run("nothing is the supported no-token posture", func(t *testing.T) {
		got, err := ResolveToken("test", "", "", "", nil)
		if err != nil || got != "" {
			t.Fatalf("got %q, %v; want \"\", nil", got, err)
		}
	})
	for _, tc := range []struct{ name, env, legacy string }{
		{"file and --token-env", "CORE_AGENT_1201_RT_ENV", ""},
		{"file and deprecated --token", "", "CORE_AGENT_1201_RT_ENV"},
	} {
		t.Run(tc.name+" is an error, not a precedence rule", func(t *testing.T) {
			got, err := ResolveToken("test", file, tc.env, tc.legacy, nil)
			if err == nil || !strings.Contains(err.Error(), "mutually exclusive") || got != "" {
				t.Fatalf("got %q, %v; want a mutually-exclusive error", got, err)
			}
		})
	}
	t.Run("an unreadable or loose file is an error, never an anonymous request", func(t *testing.T) {
		for _, p := range []string{filepath.Join(t.TempDir(), "missing"), clientTokenFile(t, "tok\n", 0o644)} {
			got, err := ResolveToken("test", p, "", "", nil)
			if err == nil || !strings.Contains(err.Error(), "--token-file") || got != "" {
				t.Errorf("%s: got %q, %v; want a --token-file error", p, got, err)
			}
		}
	})
}

const tokenFileChildFlag = "CORE_AGENT_1201_TOKENFILE_CHILD"

// #1201 item 3, client half. --token-env can only shorten the window in
// which a client's environment holds the token; --token-file removes it:
// the client starts with no token anywhere in its environment, and once
// it has read the file it is non-dumpable, so the token in its memory is
// out of a same-user process's reach too. The control read before
// resolving keeps the /proc assertion from passing on a host where /proc
// is unreadable anyway.
func TestResolveTokenFromAFileLeavesNothingInReach(t *testing.T) {
	if p := os.Getenv(tokenFileChildFlag); p != "" {
		tokenFileChild(p)
		return
	}
	if runtime.GOOS != "linux" {
		t.Skip("the /proc half of this property is Linux-only")
	}
	if os.Geteuid() == 0 {
		t.Skip("root has CAP_SYS_PTRACE, which non-dumpable does not stop")
	}
	t.Parallel()
	const secret = "s3cret-1201-token-file-client"
	file := clientTokenFile(t, secret+"\n", 0o600)
	cmd := exec.Command(os.Args[0], "-test.run=^TestResolveTokenFromAFileLeavesNothingInReach$", "-test.count=1")
	cmd.Env = append(os.Environ(), tokenFileChildFlag+"="+file)
	out, err := cmd.CombinedOutput()
	got := string(out)
	if err != nil {
		t.Fatalf("child failed: %v\n%s", err, got)
	}
	if !strings.Contains(got, "RESOLVED="+secret) {
		t.Fatalf("the resolver did not return the token from the file:\n%s", got)
	}
	if !strings.Contains(got, "CONTROL=readable") {
		t.Fatalf("control failed: /proc/<pid>/environ was unreadable before resolving:\n%s", got)
	}
	if !strings.Contains(got, "AFTER=denied") {
		t.Errorf("after reading the token file the client is still dumpable, so a same-user process can read the token from its memory:\n%s", got)
	}
	if strings.Count(got, secret) != 1 {
		t.Errorf("the token appeared somewhere other than the resolver's return value (a child's environment or the client's /proc environ):\n%s", got)
	}
}

func tokenFileChild(path string) {
	probe := func(label string) string {
		out, _ := exec.Command("/bin/sh", "-c",
			`if e=$(tr '\0' '\n' < /proc/$PPID/environ 2>/dev/null); then printf '`+label+`=readable\n%s\n' "$e" | grep -v '^`+tokenFileChildFlag+`='; else echo `+label+`=denied; fi`).Output()
		return string(out)
	}
	control := probe("CONTROL")
	tok, err := ResolveToken("test", path, "", "", os.Stderr)
	if err != nil {
		os.Stdout.WriteString("ERR=" + err.Error() + "\n")
	}
	os.Stdout.WriteString(control + "RESOLVED=" + tok + "\n" + probe("AFTER"))
	os.Exit(0)
}
