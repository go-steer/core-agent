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

package main

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// containsSeq reports whether argv contains want as a contiguous run.
// Flag ORDER matters to bwrap (--ro-bind / / before --bind ws
// /workspace, or the workspace bind is shadowed), so the jail tests
// assert on sequences rather than set membership.
func containsSeq(argv, want []string) bool {
	for i := 0; i+len(want) <= len(argv); i++ {
		match := true
		for j, w := range want {
			if argv[i+j] != w {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// TestSandboxArgvIsAJail is the security test of this example. The
// bwrap flags ARE the containment boundary for model-authored shell
// commands; if one is dropped, nothing else in the port notices.
func TestSandboxArgvIsAJail(t *testing.T) {
	s := &sandbox{
		Mode:      sandboxModeBwrap,
		Workspace: "/scratch/session-1",
		RunAsUser: sandboxRunAsUser,
		exists:    func(string) bool { return false },
	}
	argv := s.argv("kubectl get pods")

	if got := argv[:5]; !containsSeq(got, []string{"sudo", "-n", "-E", "-u", "agent-runner"}) {
		t.Errorf("argv does not start with the uid drop: %v", got)
	}
	for _, flag := range []string{"--unshare-user", "--unshare-pid", "--unshare-ipc", "--unshare-cgroup"} {
		if !containsSeq(argv, []string{flag}) {
			t.Errorf("argv is missing %s; the jail is weaker than upstream's", flag)
		}
	}
	for _, seq := range [][]string{
		{"--ro-bind", "/", "/"},
		{"--proc", "/proc"},
		{"--dev", "/dev"},
		{"--tmpfs", "/tmp"},
		{"--bind", "/scratch/session-1", "/workspace"},
		{"--chdir", "/workspace"},
	} {
		if !containsSeq(argv, seq) {
			t.Errorf("argv is missing the sequence %v: %v", seq, argv)
		}
	}
	if got := argv[len(argv)-3:]; !containsSeq(got, []string{"bash", "-c", "kubectl get pods"}) {
		t.Errorf("argv does not end with the command: %v", got)
	}
	// The root bind must precede the workspace bind, or the writable
	// workspace is mounted over.
	rootAt, wsAt := -1, -1
	for i := range argv {
		if argv[i] == "--ro-bind" && i+2 < len(argv) && argv[i+1] == "/" {
			rootAt = i
		}
		if argv[i] == "--bind" && i+2 < len(argv) && argv[i+2] == "/workspace" {
			wsAt = i
		}
	}
	if rootAt < 0 || wsAt < 0 || rootAt > wsAt {
		t.Errorf("read-only root bind (%d) must come before the workspace bind (%d)", rootAt, wsAt)
	}
}

func TestSandboxArgvBindsServiceAccountWhenPresent(t *testing.T) {
	s := &sandbox{Mode: sandboxModeBwrap, Workspace: "/ws", RunAsUser: sandboxRunAsUser,
		exists: func(p string) bool { return p == serviceAccountDir }}
	if !containsSeq(s.argv("true"), []string{"--ro-bind", serviceAccountDir, serviceAccountDir}) {
		t.Error("the projected service-account token must be bind-mounted read-only when present")
	}
}

func TestSandboxArgvWithoutUIDDrop(t *testing.T) {
	s := &sandbox{Mode: sandboxModeBwrap, Workspace: "/ws", exists: func(string) bool { return false }}
	argv := s.argv("true")
	if argv[0] != "bwrap" {
		t.Errorf("argv[0] = %q, want bwrap when no RunAsUser is set", argv[0])
	}
	if containsSeq(argv, []string{"sudo"}) {
		t.Error("no sudo should appear when RunAsUser is empty")
	}
}

func TestSandboxArgvNoneMode(t *testing.T) {
	s := newSandbox(sandboxModeNone, "/ws")
	want := []string{"/bin/sh", "-c", "echo hi"}
	got := s.argv("echo hi")
	if len(got) != 3 || !containsSeq(got, want) {
		t.Errorf("argv = %v, want %v", got, want)
	}
}

func TestSandboxRunReportsExitCode(t *testing.T) {
	s := newSandbox(sandboxModeNone, t.TempDir())
	out := s.run(context.Background(), "printf hello; exit 7")
	if out.ExitCode != 7 {
		t.Errorf("ExitCode = %d, want 7", out.ExitCode)
	}
	if out.Stdout != "hello" {
		t.Errorf("Stdout = %q, want %q", out.Stdout, "hello")
	}
	if out.TimedOut {
		t.Error("TimedOut should be false")
	}
}

func TestSandboxRunTruncatesOutput(t *testing.T) {
	s := newSandbox(sandboxModeNone, t.TempDir())
	out := s.run(context.Background(), "head -c 20000 /dev/zero | tr '\\0' 'a'")
	if out.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, stderr=%q", out.ExitCode, out.Stderr)
	}
	if !strings.HasSuffix(out.Stdout, "...[Output Truncated]...") {
		t.Errorf("oversized stdout was not truncated (len %d)", len(out.Stdout))
	}
	if len(out.Stdout) > maxSandboxOutput+64 {
		t.Errorf("truncated stdout is %d bytes, want ~%d", len(out.Stdout), maxSandboxOutput)
	}
}

func TestSandboxRunTimesOut(t *testing.T) {
	s := newSandbox(sandboxModeNone, t.TempDir())
	s.Timeout = 50 * time.Millisecond
	start := time.Now()
	out := s.run(context.Background(), "sleep 30")
	if !out.TimedOut {
		t.Fatalf("expected TimedOut, got %+v", out)
	}
	if out.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1", out.ExitCode)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("run took %s; the timeout did not kill the child", elapsed)
	}
}

func TestSandboxRunUsesWorkspaceAsCWD(t *testing.T) {
	dir := t.TempDir()
	s := newSandbox(sandboxModeNone, dir)
	out := s.run(context.Background(), "pwd")
	if !strings.Contains(out.Stdout, strings.TrimPrefix(dir, "/private")) {
		t.Errorf("pwd = %q, want the workspace %q", strings.TrimSpace(out.Stdout), dir)
	}
}

func TestSandboxRunNilContext(t *testing.T) {
	s := newSandbox(sandboxModeNone, t.TempDir())
	//nolint:staticcheck // deliberately exercising the nil-ctx guard
	if out := s.run(nil, "true"); out.ExitCode != 0 {
		t.Errorf("nil context should fall back to Background, got %+v", out)
	}
}

func TestWorkspacePath(t *testing.T) {
	jailed := &sandbox{Mode: sandboxModeBwrap, Workspace: "/scratch/session-1"}
	if got := jailed.workspacePath(candidateFile); got != "/workspace/candidate.yaml" {
		t.Errorf("workspacePath = %q, want the in-jail bind target", got)
	}
	plain := newSandbox(sandboxModeNone, "/scratch/session-1")
	if got := plain.workspacePath(candidateFile); got != "/scratch/session-1/candidate.yaml" {
		t.Errorf("workspacePath = %q, want the real path when uncontained", got)
	}
}

func TestWaitFuncCapsAndRespectsCancel(t *testing.T) {
	got, err := waitFunc(context.TODO(), waitArgs{Seconds: 0})
	if err != nil || got.WaitedSeconds != 0 || got.Capped {
		t.Errorf("a zero wait should be a no-op, got %+v %v", got, err)
	}
	if got, err := waitFunc(context.TODO(), waitArgs{Seconds: -5}); err != nil || got.WaitedSeconds != 0 {
		t.Errorf("a negative wait should be a no-op, got %+v %v", got, err)
	}

	// A model asking to sleep for a day must not wedge the run: the
	// call is clamped, and the clamp is reported so the model can loop.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := waitFunc(ctx, waitArgs{Seconds: 86400}); err == nil {
		t.Fatal("a cancelled context must abort the wait")
	}
	if maxWaitSeconds != 900 {
		t.Errorf("maxWaitSeconds = %d, want the 900s the checker prompt asks for", maxWaitSeconds)
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("abc", 10); got != "abc" {
		t.Errorf("short strings must pass through, got %q", got)
	}
	got := truncate("abcdef", 3)
	if !strings.HasPrefix(got, "abc") || !strings.HasSuffix(got, "...[Output Truncated]...") {
		t.Errorf("truncate = %q", got)
	}
}

// TestTruncateKeepsValidUTF8 covers the byte-vs-rune slice: a cut that
// lands inside a multi-byte rune would put invalid UTF-8 into a tool
// result, which the model API rejects outright.
func TestTruncateKeepsValidUTF8(t *testing.T) {
	// "日本語" is 3 bytes per rune, so cutting at 4 splits the second.
	for _, max := range []int{1, 2, 3, 4, 5, 6, 7, 8} {
		got := truncate("日本語です", max)
		if !utf8.ValidString(got) {
			t.Errorf("truncate(%d) produced invalid UTF-8: %q", max, got)
		}
	}
	if got := truncate("日本語です", 6); !strings.HasPrefix(got, "日本") {
		t.Errorf("truncate kept %q, want the whole runes that fit", got)
	}
	// Binary output is truncated, not eaten: at most one rune's worth
	// of bytes is trimmed even when nothing is valid UTF-8.
	bin := string([]byte{0xff, 0xfe, 0xfd, 0xfc, 0xfb, 0xfa})
	if got := truncate(bin, 5); len(got) < 2 {
		t.Errorf("binary truncation dropped too much: %q", got)
	}
}

func TestResolveWorkspaceCreatesDir(t *testing.T) {
	dir := t.TempDir() + "/nested/workspace"
	got, err := resolveWorkspace(dir)
	if err != nil {
		t.Fatalf("resolveWorkspace: %v", err)
	}
	if got == "" || !strings.HasSuffix(got, "workspace") {
		t.Errorf("resolveWorkspace = %q", got)
	}
}
