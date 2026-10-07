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
	"errors"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// #1201 item 5: the non-dumpable flag stops only processes without
// CAP_SYS_PTRACE. The warning fires exactly when the agent's children
// will hold it AND there is something to steal.
func TestPtraceWarning(t *testing.T) {
	t.Parallel()
	readErr := errors.New("EPERM")
	cases := []struct {
		name  string
		holds bool
		root  bool
		caps  PtraceCaps
		err   error
		want  string // substring; "" means no warning
	}{
		{"root with ptrace in bounding", true, true, PtraceCaps{Bounding: true}, nil, "runs as root with CAP_SYS_PTRACE in its capability bounding set"},
		{"root, dropped from bounding but still inheritable", true, true, PtraceCaps{Inheritable: true}, nil, "inheritable capability set"},
		{"root, dropped everywhere (Docker default)", true, true, PtraceCaps{}, nil, ""},
		{"non-root, ptrace only in bounding", true, false, PtraceCaps{Bounding: true}, nil, ""},
		{"non-root, ptrace only inheritable", true, false, PtraceCaps{Inheritable: true}, nil, ""},
		{"non-root, ptrace ambient", true, false, PtraceCaps{Ambient: true}, nil, "ambient capability set"},
		{"root, caps unreadable", true, true, PtraceCaps{}, readErr, "could not be read (EPERM)"},
		{"non-root, caps unreadable", true, false, PtraceCaps{}, readErr, ""},
		{"nothing held", false, true, PtraceCaps{Bounding: true, Inheritable: true, Ambient: true}, nil, ""},
	}
	for _, c := range cases {
		got := PtraceWarning(c.holds, c.root, c.caps, c.err)
		switch {
		case c.want == "" && got != "":
			t.Errorf("%s: unexpected warning %q", c.name, got)
		case c.want != "" && !strings.Contains(got, c.want):
			t.Errorf("%s: warning %q does not contain %q", c.name, got, c.want)
		case c.want != "" && !strings.Contains(got, "SYS_PTRACE") || c.want != "" && !strings.Contains(got, "#1201"):
			t.Errorf("%s: warning %q does not name the fix", c.name, got)
		}
	}
}

// The reader must work unprivileged — the startup check runs on every
// daemon that holds a credential, root or not.
func TestReadPtraceCapsUnprivileged(t *testing.T) {
	t.Parallel()
	if runtime.GOOS != "linux" {
		t.Skip("Linux-only reader")
	}
	caps, err := ReadPtraceCaps()
	if err != nil {
		t.Fatalf("ReadPtraceCaps: %v", err)
	}
	// Cross-check against the kernel's other report of the same set, so
	// a wrong prctl option or capability number cannot pass silently.
	status, err := os.ReadFile("/proc/self/status")
	if err != nil {
		t.Skipf("no /proc/self/status to cross-check against: %v", err)
	}
	got := map[string]bool{"CapBnd": caps.Bounding, "CapInh": caps.Inheritable, "CapAmb": caps.Ambient}
	checked := 0
	for _, line := range strings.Split(string(status), "\n") {
		key, hex, ok := strings.Cut(line, ":")
		have, wanted := got[key]
		if !ok || !wanted {
			continue
		}
		mask, err := strconv.ParseUint(strings.TrimSpace(hex), 16, 64)
		if err != nil {
			t.Fatalf("parse %s %q: %v", key, hex, err)
		}
		const capSysPtrace = 19
		if want := mask&(1<<capSysPtrace) != 0; have != want {
			t.Errorf("ReadPtraceCaps() says %v for %s, /proc/self/status says %v", have, key, want)
		}
		checked++
	}
	if checked == 0 {
		t.Skip("no capability lines in /proc/self/status to cross-check against")
	}
}
