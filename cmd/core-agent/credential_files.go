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
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/go-steer/core-agent/v2/pkg/childenv"
	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/permissions"
)

// credentialProbes is everything guardCredentialFiles asks the host, so
// a test can answer for a root daemon, a readable file or a failing
// prctl without being root or touching the shared test process.
type credentialProbes struct {
	readable  func(path string) bool
	root      func() bool // real or effective uid is 0
	caps      func() (childenv.PtraceCaps, error)
	protect   func() error
	supported bool
}

var hostCredentialProbes = credentialProbes{
	readable: func(path string) bool {
		// Only a regular file can be read again. A FIFO or process
		// substitution (an --attach-token-file <(...)) was drained at
		// startup and has nothing left; opening one here would block
		// startup until some writer appeared.
		if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
			return false
		}
		f, err := os.Open(path) //nolint:gosec // operator-configured credential path, opened only to probe readability
		if err != nil {
			return false
		}
		_ = f.Close()
		return true
	},
	root:      func() bool { return os.Getuid() == 0 || os.Geteuid() == 0 },
	caps:      childenv.ReadPtraceCaps,
	protect:   childenv.Protect,
	supported: childenv.Supported,
}

// withholdCredentialFiles registers the config's credential files with
// the process-wide set the instruction loader consults, so no
// `@include` can splice one into a system prompt (#1201). It must run
// before the first instruction load, which is why it sits directly after
// withholdDaemonCredentials in run(), several hundred lines ahead of
// guardCredentialFiles: the boot-time instruction.LoadForSession runs in
// between. permissions.FromConfig also registers the same paths (for
// library hosts), and today it runs ahead of that load too; this call
// makes run() not depend on that ordering. Both are add-only.
//
// tokenFile is attachCfg.TokenFile: the attach token file after
// --attach-token-file and ${VAR} expansion, which cfg alone does not
// carry when it came from the flag.
func withholdCredentialFiles(cfg *config.Config, tokenFile string) {
	childenv.WithholdFiles(cfg.CredentialFiles()...)
	childenv.WithholdFiles(tokenFile)
}

// guardCredentialFiles reports and hardens what #1201 items 4 and 5
// leave to the deployment, once the gate knows the daemon's credential
// files and the build knows whether it registers bash.
//
// Enforced elsewhere, reported here: permissions.FromConfig already put
// every credential file on the gate's refusal list (agent file tools
// refused in every mode; bash commands that name it directly refused),
// and withholdCredentialFiles kept it out of every @include.
//
// Enforced here: a daemon configured with a credential file holds its
// contents in memory for the life of the process, so it is made
// non-dumpable exactly as withholdDaemonCredentials does for an env
// credential — otherwise a same-user child could read the table back
// out of /proc/<pid>/mem wherever Yama does not stop it.
//
// Warned here, because only the operating system can enforce them:
// a credential file the agent's bash can still read (bash is not
// path-scoped, so the gate's check of its command text is a seatbelt),
// and a daemon whose children will hold CAP_SYS_PTRACE.
//
// Takes the template gate because the set lives there and is shared with
// every derived gate; bashRegistered is tools.BashRegistered(b), the
// same answer the skill loader gets. Must run after both exist and
// before any session's first tool call, which is every point between
// the tool toggles and runner.Run.
//
// tokenFile is attachCfg.TokenFile, registered on the gate here because
// a flag-only path never reaches the config FromConfig read. The set is
// shared by reference, so the sessions derived before this call are
// covered too, and no tool has run yet.
func guardCredentialFiles(gate *permissions.Gate, tokenFile string, bashRegistered bool, stderr io.Writer) {
	guardCredentialFilesWith(gate, tokenFile, bashRegistered, stderr, hostCredentialProbes)
}

func guardCredentialFilesWith(gate *permissions.Gate, tokenFile string, bashRegistered bool, stderr io.Writer, p credentialProbes) {
	gate.ProtectCredentialFiles(tokenFile)
	files := gate.CredentialFiles()
	if len(files) > 0 {
		fmt.Fprintf(stderr, "core-agent: agent tools may not read or write %s (credential files)\n", strings.Join(files, ", "))
		inScope := func(path string) bool {
			access, err := gate.Scope().AccessFor(path)
			return err == nil && access.Allows(permissions.AccessRead)
		}
		for _, f := range files {
			if w := credentialFileExposure(f, p.readable(f), bashRegistered, inScope(f)); w != "" {
				fmt.Fprintln(stderr, w)
			}
		}
		if p.supported {
			if err := p.protect(); err != nil {
				fmt.Fprintf(stderr, "core-agent: warning: could not make the daemon non-dumpable (%v); a child process can read the credential files' contents out of its memory\n", err)
			}
		}
	}
	holds := len(files) > 0 || childenv.HoldsCredential()
	caps, capsErr := p.caps()
	if w := childenv.PtraceWarning(holds, p.root(), caps, capsErr); w != "" {
		fmt.Fprintln(stderr, w)
	}
}

// credentialFileExposure returns the startup warning for one credential
// file, or "" when the agent has no route to it the gate cannot close.
//
// readable is whether the daemon's own user can open it now. The agent
// runs as that user, so file modes never separate the two; a file this
// user cannot read is one the agent cannot read either.
//
// bash is whether the bash tool is registered. bash is not path-scoped
// — the gate sees a command string, not the files the shell will open —
// so for bash "inside the agent's reach" means "readable by this user",
// wherever the file is mounted.
//
// inScope is whether the path scope grants reads there. Agent file
// tools are refused on it regardless (glob and grep skip it), and so is
// an @include of it; the extra risk of an in-scope placement is that
// list_dir shows its name to the model, and that anything that reads
// files without the gate's path check — an MCP filesystem server, a
// skill's scripts run by bash — finds it in the working tree.
func credentialFileExposure(path string, readable, bash, inScope bool) string {
	if !readable || (!bash && !inScope) {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "core-agent: warning: credential file %s is readable by the user this daemon runs as", path)
	if bash {
		b.WriteString(", and so by the agent's bash, which runs as the same user. The gate refuses bash commands that name it directly, but a shell can reach it by other names; only the operating system can keep it out of bash. Disable the bash tool on this daemon, or arrange that this user cannot read the file once the daemon has loaded it")
	}
	if inScope {
		if bash {
			b.WriteString(". It also sits")
		} else {
			b.WriteString(", and it sits")
		}
		b.WriteString(" inside the agent's path scope, where directory listings show it to the model; mount it outside the project tree")
	}
	b.WriteString(" (#1201)")
	return b.String()
}
