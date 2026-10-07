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

//go:build linux

package childenv

import "golang.org/x/sys/unix"

// Protect clears the process's dumpable flag (prctl PR_SET_DUMPABLE 0).
//
// That is the kernel's switch for "may a same-uid, unprivileged process
// inspect me": with it cleared, /proc/<pid>/environ and /proc/<pid>/mem
// fail with EACCES and ptrace attach is refused, for every process
// without CAP_SYS_PTRACE — which is every process the bash tool starts
// unless the daemon itself runs as root with full capabilities, in
// which case its children typically inherit CAP_SYS_PTRACE and this
// protects nothing (PtraceWarning is the startup check for that, #1201).
// Children are unaffected: the flag is reset to
// dumpable on execve, so nothing the agent runs is restricted by it.
//
// The costs, which is why cmd/core-agent calls it only when a withheld
// credential is actually set (HoldsCredential): no core dumps, and a
// same-user debugger cannot attach to a running daemon (launching it
// under the debugger still works, since tracing is established first).
func Protect() error {
	return unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0)
}

// Supported reports whether Protect does anything on this platform.
const Supported = true
