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

package permissions

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"

	"github.com/go-steer/core-agent/v2/pkg/childenv"
)

// ErrCredentialFile is the sentinel wrapped by every refusal of a call
// that names one of the daemon's credential files. A caller can tell
// it apart from an ordinary out-of-scope denial, which an operator can
// approve; this one nobody can.
var ErrCredentialFile = errors.New("permissions: the daemon's credential file is not reachable from agent tools")

// credentialFiles is the set of files the daemon reads a caller
// credential from — today the multi-session bearer table — that no
// agent tool may read or write (#1201 item 4).
//
// The threat is the one #1157 closed for environment variables: the
// file holds the credential that answers the agent's own permission
// prompts, and the agent runs as the user the daemon runs as, so file
// permissions do not stop it. A model that reads the table can approve
// its own pending call; one that writes it can add its own token for
// the next boot.
//
// Matching (path and inode, current and at registration) is
// childenv.FileSet's. The bash patterns are the gate's own, compiled at
// registration against the working directory the bash tool runs in.
//
// Shared by reference between a template gate and every gate derived
// from it, so a path registered after a session was derived still
// covers that session.
type credentialFiles struct {
	set childenv.FileSet

	mu      sync.RWMutex
	bashPat []*regexp.Regexp
}

// ProtectCredentialFiles adds paths to the set no agent tool may read
// or write, in any permission mode. Empty paths are ignored so callers
// can pass optional config fields straight through. Relative paths
// resolve against the process working directory, the directory the
// daemon itself opens them from and the one the bash tool runs in.
// Safe for concurrent use; there is no way to remove a path.
func (g *Gate) ProtectCredentialFiles(paths ...string) {
	if g == nil || g.creds == nil {
		return
	}
	// One critical section for add, spell and store, so two concurrent
	// registrations cannot leave the older pattern list in place.
	g.creds.mu.Lock()
	defer g.creds.mu.Unlock()
	g.creds.set.Add(paths...)
	wd, _ := os.Getwd()
	spellings := g.creds.set.Spellings(wd)
	pats := make([]*regexp.Regexp, 0, len(spellings))
	for _, forms := range spellings {
		pats = append(pats, bashMentionPattern(forms))
	}
	g.creds.bashPat = pats
}

// CredentialFiles returns the protected paths (absolute), sorted, for
// startup reporting.
func (g *Gate) CredentialFiles() []string {
	if g == nil || g.creds == nil {
		return nil
	}
	return g.creds.set.Paths()
}

// credentialFileDenial refuses a file-tool call whose path is one of the
// credential files. It runs before the path scope, the mode, every
// session grant and the prompter: an out-of-scope read is something an
// operator may approve, and this is not — in yolo, allow mode or under
// an allow-always grant there is no operator to ask, and the scope and
// the grants are exactly what the credential would let the agent change.
func (g *Gate) credentialFileDenial(toolName, path string) error {
	if g.creds == nil {
		return nil
	}
	if _, hit := g.creds.set.Match(path); hit {
		return credentialRefusal(toolName, path)
	}
	return nil
}

// credentialRefusal names the path the caller used, not the file it
// resolved to: a model that came in through a link learns nothing new.
func credentialRefusal(toolName, path string) error {
	return fmt.Errorf("%s refused: %s is one of the daemon's credential files, which no agent tool may read or write in any permission mode (%w). An operator cannot approve this; do not retry it or ask for it", toolName, path, ErrCredentialFile)
}

// credentialBashDenial refuses a bash command whose text names a
// credential file.
//
// This is a seatbelt, not a boundary, for the reason this package's doc
// gives for the bash denylist: a shell can name a file in unboundedly
// many ways — `cd` and a bare name, a glob, a variable, a script the
// agent wrote that holds the path. It catches the direct spelling
// (absolute, symlink-resolved, or relative to the working directory the
// bash tool runs in), which is the one a model reaches for first, and
// nothing else. The boundary for bash is the operating system: the file
// must not be readable by the user the daemon runs as once the daemon
// has read it. cmd/core-agent warns at startup when it is.
//
// Hooks reach the gate through CheckBash too, so a hook command that
// names a credential file is refused as well. That is deliberate: a
// hook is a shell command the daemon runs as its own user, and a hook
// that reads the table is the same route with a different trigger.
func (g *Gate) credentialBashDenial(command string) error {
	if g.creds == nil {
		return nil
	}
	g.creds.mu.RLock()
	defer g.creds.mu.RUnlock()
	for _, p := range g.creds.bashPat {
		if m := p.FindString(command); m != "" {
			return credentialRefusal("bash", strings.Trim(m, " \t\n'\"=<>(){};|&`"))
		}
	}
	return nil
}

// bashMentionPattern matches any of forms as a whole shell word:
// preceded by the start of the command, whitespace, a quote or a shell
// operator (optionally followed by "./"), and followed by the end,
// whitespace, a quote or an operator. So "testdata/users.json" does not
// mention a credential file named users.json in the working directory,
// and "/etc/core-agent/users.json.bak" does not mention
// "/etc/core-agent/users.json".
func bashMentionPattern(forms []string) *regexp.Regexp {
	quoted := make([]string, 0, len(forms))
	for _, f := range forms {
		quoted = append(quoted, regexp.QuoteMeta(f))
	}
	const before = "(?:^|[\\s'\"=<>(){};|&`])(?:\\./)?"
	const after = "(?:$|[\\s'\"<>(){};|&`])"
	return regexp.MustCompile(before + "(?:" + strings.Join(quoted, "|") + ")" + after)
}
