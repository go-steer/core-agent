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
	"strings"

	"github.com/go-steer/core-agent/v2/pkg/childenv"
	"github.com/go-steer/core-agent/v2/pkg/config"
)

// withholdDaemonCredentials keeps the daemon's own credentials out of
// every process it starts, and out of reach of those processes'
// /proc reads (#1157).
//
// The withheld set is every env var the daemon resolves BY NAME —
// cfg.EnvRefs(), the same reflective walk over *_env config fields that
// feeds env.yaml's drift report (#764) — plus the attach token's name,
// which --attach-token can set with no config behind it. That set is
// exactly "the variables the daemon itself consumes as credentials":
// the attach token answers the agent's permission prompts; an alert
// target's bearer_env lets the holder post as the daemon; and a url_env
// is a credential too whenever the URL is the secret, which a Slack
// incoming-webhook URL is.
//
// Deliberately NOT withheld: provider API keys and cloud credentials.
// Those are consumed by SDKs rather than by name through config, so
// they are not in this set, and a coding agent's own `go test` or `gh`
// legitimately needs the environment it runs in. #1157's threat is the
// agent answering its own gate, and that is what this closes.
//
// Must run before anything can start a child: tools, hooks, MCP stdio
// servers. In run() it sits directly after the attach options are
// merged, ahead of all three.
func withholdDaemonCredentials(cfg *config.Config, attachTokenEnv string, stderr io.Writer) {
	childenv.Withhold(cfg.EnvRefs()...)
	childenv.Withhold(attachTokenEnv)
	names := childenv.Withheld()
	if len(names) == 0 {
		return
	}
	fmt.Fprintf(stderr, "core-agent: child processes do not inherit %s\n", strings.Join(names, ", "))
	if !childenv.HoldsCredential() {
		return
	}
	if !childenv.Supported {
		fmt.Fprintln(stderr, "core-agent: warning: this platform cannot make the daemon non-dumpable, so a child process can still read its credentials from the process table")
		return
	}
	if err := childenv.Protect(); err != nil {
		// Not fatal: the scrubbed environment still stops inheritance,
		// and refusing to start would turn a hardening step into an
		// outage. But say plainly that the /proc route is open.
		fmt.Fprintf(stderr, "core-agent: warning: could not make the daemon non-dumpable (%v); a child process can still read its credentials from /proc\n", err)
	}
}
