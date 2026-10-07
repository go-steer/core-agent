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

import "fmt"

// PtraceCaps is where CAP_SYS_PTRACE sits in this process's capability
// sets, as far as it decides what the processes it starts will hold.
//
// Protect's guarantee is "no process without CAP_SYS_PTRACE can read
// me". Whether the agent's children have that capability is decided at
// execve (capabilities(7)). When the parent's real or effective uid is
// 0, the kernel treats the new program as carrying every file
// capability, so the child's permitted set is the parent's inheritable
// set OR its bounding set (OR ambient). For any parent, the ambient set
// passes through. Those are the three fields here. The daemon's own
// effective set does not reach a non-root child, so it is not read.
type PtraceCaps struct {
	Bounding    bool // CAP_SYS_PTRACE is in the bounding set
	Inheritable bool // CAP_SYS_PTRACE is in the inheritable set
	Ambient     bool // CAP_SYS_PTRACE is in the ambient set
}

// PtraceWarning returns the startup warning for a daemon whose children
// can read its memory and /proc entries despite Protect (#1201 item 5),
// or "" when there is nothing to warn about.
//
// holds is whether the daemon holds a credential worth protecting — a
// withheld env var that is set, or a configured credential file. root
// is whether its real or effective uid is 0. caps and capsErr are
// ReadPtraceCaps' result. The function is pure so every arm is testable
// without root.
//
// It warns rather than refuses on purpose; see
// docs/credential-files-design.md ("Settled decisions").
func PtraceWarning(holds, root bool, caps PtraceCaps, capsErr error) string {
	if !holds {
		return ""
	}
	const tail = "so every process the agent starts can read this daemon's memory and /proc/<pid>/environ, and with them the credential that answers its permission prompts; the non-dumpable flag does not stop a process holding CAP_SYS_PTRACE. Run the daemon as a non-root user, or drop CAP_SYS_PTRACE (Kubernetes: securityContext.capabilities.drop [SYS_PTRACE] or [ALL]; not a privileged pod) (#1201)"
	switch {
	case root && capsErr != nil:
		return fmt.Sprintf("core-agent: warning: this daemon runs as root and its capabilities could not be read (%v); if it holds CAP_SYS_PTRACE, %s", capsErr, tail)
	case root && caps.Bounding:
		return "core-agent: warning: this daemon runs as root with CAP_SYS_PTRACE in its capability bounding set, " + tail
	case root && caps.Inheritable:
		return "core-agent: warning: this daemon runs as root with CAP_SYS_PTRACE in its inheritable capability set, " + tail
	case caps.Ambient:
		return "core-agent: warning: this daemon has CAP_SYS_PTRACE in its ambient capability set, which every process it starts inherits, " + tail
	}
	return ""
}
