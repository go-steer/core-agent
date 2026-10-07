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

package childenv

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func secretFile(t *testing.T, content string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil { // WriteFile's mode is umask-filtered
		t.Fatal(err)
	}
	return p
}

func TestReadSecretFileContentRules(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, content, want, errSubstr string
	}{
		{name: "trailing newline is trimmed", content: "tok-123\n", want: "tok-123"},
		{name: "CRLF is trimmed", content: "tok-123\r\n", want: "tok-123"},
		{name: "no newline at all", content: "tok-123", want: "tok-123"},
		{name: "surrounding blank lines", content: "\n  tok-123  \n\n", want: "tok-123"},
		{name: "empty", content: "", errSubstr: "is empty"},
		{name: "whitespace only", content: " \n\t\n", errSubstr: "is empty"},
		{name: "two lines is a different file", content: "tok-123\nsecond\n", errSubstr: "alone on one line"},
		{name: "internal space", content: "Bearer tok-123\n", errSubstr: "alone on one line"},
		{name: "control character", content: "tok\x00123", errSubstr: "control character"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ReadSecretFile(secretFile(t, tc.content, 0o600))
			if tc.errSubstr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.errSubstr) {
					t.Fatalf("err = %v, want one containing %q", err, tc.errSubstr)
				}
				if strings.Contains(err.Error(), "tok-123") || strings.Contains(err.Error(), "second") {
					t.Errorf("the error echoes file content, which is the secret or most of it: %v", err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("got %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

func TestReadSecretFileModeRules(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits")
	}
	cases := []struct {
		mode os.FileMode
		ok   bool
	}{
		{0o600, true},
		{0o400, true},
		{0o640, true}, // group read: a k8s secret volume with fsGroup + defaultMode 0440
		{0o440, true},
		{0o620, false}, // group write: someone else can substitute the token
		{0o604, false}, // world-readable
		{0o644, false}, // the k8s secret-volume default, which is world-readable
		{0o602, false}, // world-writable
		{0o601, false},
	}
	for _, tc := range cases {
		t.Run(tc.mode.String(), func(t *testing.T) {
			t.Parallel()
			_, err := ReadSecretFile(secretFile(t, "tok\n", tc.mode))
			if tc.ok && err != nil {
				t.Fatalf("mode %04o refused: %v", tc.mode, err)
			}
			if !tc.ok && (err == nil || !strings.Contains(err.Error(), "accessible to other users")) {
				t.Fatalf("mode %04o: err = %v, want an 'accessible to other users' refusal", tc.mode, err)
			}
		})
	}
}

func TestReadSecretFileRefusesNonTokens(t *testing.T) {
	t.Parallel()
	if MayBlock(secretFile(t, "tok\n", 0o600)) || MayBlock(t.TempDir()) || MayBlock(filepath.Join(t.TempDir(), "missing")) {
		t.Error("MayBlock is true for a regular file, a directory or a missing path")
	}
	if _, err := ReadSecretFile(""); err == nil {
		t.Error("an empty path read without error")
	}
	if _, err := ReadSecretFile(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("a missing file read without error")
	}
	if _, err := ReadSecretFile(t.TempDir()); err == nil || !strings.Contains(err.Error(), "directory") {
		t.Errorf("a directory: err = %v", err)
	}
	big := secretFile(t, strings.Repeat("a", MaxSecretFileBytes+1), 0o600)
	if _, err := ReadSecretFile(big); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Errorf("an oversized file: err = %v", err)
	}
}

// A FIFO — and /dev/fd/N from a bash process substitution, which is a
// pipe — is the posture that leaves nothing on disk. Its mode bits say
// nothing about who can read the value, so they are not checked: a FIFO
// created 0644 must still be accepted.
func TestReadSecretFileReadsAFIFOWithoutAModeCheck(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("no FIFOs")
	}
	p := filepath.Join(t.TempDir(), "fifo")
	if err := mkfifo(p, 0o644); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	go func() {
		f, err := os.OpenFile(p, os.O_WRONLY, 0)
		if err != nil {
			return
		}
		_, _ = f.WriteString("fifo-token\n")
		_ = f.Close()
	}()
	if !MayBlock(p) {
		t.Error("MayBlock(FIFO) = false; a startup waiting on it would hang silently")
	}
	got, err := ReadSecretFile(p)
	if err != nil || got != "fifo-token" {
		t.Fatalf("got %q, %v; want fifo-token", got, err)
	}
}

const takeFileChildFlag = "CORE_AGENT_1201_TAKEFILE_CHILD"

// TakeFile must make the process non-dumpable: the token now lives in
// this process's memory, and /proc/<pid>/mem is as open to a same-user
// child as /proc/<pid>/environ. The re-exec'd child reads its own
// environ through a grandchild before and after; before must succeed
// (the control), after must be refused.
func TestTakeFileMakesTheProcessNonDumpable(t *testing.T) {
	if os.Getenv(takeFileChildFlag) != "" {
		takeFileChild(os.Getenv(takeFileChildFlag))
		return
	}
	if runtime.GOOS != "linux" {
		t.Skip("Protect is Linux-only")
	}
	if os.Geteuid() == 0 {
		t.Skip("root has CAP_SYS_PTRACE, which non-dumpable does not stop")
	}
	t.Parallel()
	p := secretFile(t, "tok\n", 0o600)
	cmd := exec.Command(os.Args[0], "-test.run=^TestTakeFileMakesTheProcessNonDumpable$", "-test.count=1")
	cmd.Env = append(os.Environ(), takeFileChildFlag+"="+p)
	out, err := cmd.CombinedOutput()
	got := string(out)
	if err != nil {
		t.Fatalf("child: %v\n%s", err, got)
	}
	if !strings.Contains(got, "BEFORE=ok") {
		t.Fatalf("control failed: /proc/<pid>/environ was unreadable before TakeFile, so the assertion would be vacuous:\n%s", got)
	}
	if !strings.Contains(got, "TOKEN=tok") {
		t.Fatalf("TakeFile did not return the token:\n%s", got)
	}
	if !strings.Contains(got, "AFTER=denied") {
		t.Errorf("after TakeFile a same-user process could still read this process's /proc entries:\n%s", got)
	}
}

func takeFileChild(path string) {
	probe := func(label string) string {
		out, _ := exec.Command("/bin/sh", "-c",
			`if cat /proc/$PPID/environ >/dev/null 2>&1; then echo `+label+`=ok; else echo `+label+`=denied; fi`).Output()
		return string(out)
	}
	before := probe("BEFORE")
	tok, protectErr, err := TakeFile(path)
	if err != nil {
		os.Stdout.WriteString("ERR=" + err.Error() + "\n")
	}
	if protectErr != nil {
		os.Stdout.WriteString("PROTECT_ERR=" + protectErr.Error() + "\n")
	}
	os.Stdout.WriteString(before + "TOKEN=" + tok + "\n" + probe("AFTER"))
	os.Exit(0)
}
